// Package riverbed assembles the system: a webhook receiver, a store, a router,
// agents holding MCP tools, and an MCP server over the resulting journal.
package riverbed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/justintout/riverbed/agent"
	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/embedding"
	"github.com/justintout/riverbed/mcpserve"
	"github.com/justintout/riverbed/pipeline"
	"github.com/justintout/riverbed/route"
	"github.com/justintout/riverbed/store"
	"github.com/justintout/riverbed/tool"
	"github.com/justintout/riverbed/webhook"
)

// App is a wired Riverbed system.
type App struct {
	Config config.Config
	Store  *store.Store
	Model  *embedding.Model
	Tools  *tool.Registry
	OAuth  *tool.OAuth

	pipeline *pipeline.Pipeline
	mux      *http.ServeMux
	log      *slog.Logger
}

// OpenOptions configures Open.
type OpenOptions struct {
	Config  config.Config
	Version string
	Logger  *slog.Logger
	// Prompter lets an interactive command complete an OAuth flow. The daemon
	// leaves it nil, which makes a server needing authorization report what to
	// run instead of waiting for a browser.
	Prompter tool.Prompter
}

// Open builds the system. The caller closes it.
func Open(ctx context.Context, opts OpenOptions) (*App, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	cfg := opts.Config
	app := &App{Config: cfg, log: opts.Logger}

	// The embedder is opened first: the store needs its dimension to register
	// the vector functions.
	model, err := embedding.Open(ctx, cfg.Embedding)
	if err != nil {
		return nil, err
	}
	app.Model = model

	embedName, embedDim := "", 0
	if model != nil {
		embedName, embedDim = model.Name, model.Dim
		opts.Logger.Info("embedding enabled", "model", model.Name, "dimensions", model.Dim)
	} else {
		opts.Logger.Info("embedding disabled, retrieval is keyword only")
	}

	db, err := store.Open(ctx, store.Options{
		Path:       cfg.Store.Path,
		PoolSize:   cfg.Store.PoolSize,
		EmbedModel: embedName,
		EmbedDim:   embedDim,
	})
	if err != nil {
		app.closeModel()
		return nil, err
	}
	app.Store = db

	if err := app.build(ctx, opts); err != nil {
		_ = app.Close()
		return nil, err
	}
	return app, nil
}

// build wires the parts that depend on the store.
func (a *App) build(ctx context.Context, opts OpenOptions) error {
	cfg := a.Config

	// OAuth is only prepared when a server asks for it.
	for _, m := range cfg.MCP {
		if m.Auth != "oauth" {
			continue
		}
		oauth, err := tool.NewOAuth(tool.OAuthOptions{
			Store:    a.Store,
			BaseURL:  cfg.Server.BaseURL,
			Prompter: opts.Prompter,
			Logger:   a.log,
		})
		if err != nil {
			return err
		}
		a.OAuth = oauth
		break
	}

	if len(cfg.MCP) > 0 {
		registry, err := tool.New(tool.Options{
			Servers:    cfg.MCP,
			Authorizer: a.authorizer(),
			ClientName: "riverbed",
			Version:    opts.Version,
			Logger:     a.log,
		})
		if err != nil {
			return err
		}
		a.Tools = registry
	}

	agents := make(map[string]agent.Agent, len(cfg.Agents))
	for _, ac := range cfg.Agents {
		built, err := agent.New(ac, a.log)
		if err != nil {
			return err
		}
		agents[ac.Name] = built
	}

	var classifier agent.Agent
	if cfg.Router.Classifier != "" {
		classifier = agents[cfg.Router.Classifier]
	}
	router, err := route.New(route.Options{
		Config:     cfg.Router,
		Classifier: classifier,
		Targets:    pipeline.AgentNames(agents),
		Logger:     a.log,
	})
	if err != nil {
		return err
	}

	var embedder embedding.Embedder
	if a.Model != nil {
		embedder = a.Model
	}

	a.pipeline, err = pipeline.New(pipeline.Options{
		Store:    a.Store,
		Router:   router,
		Agents:   agents,
		Tools:    a.Tools,
		ToolsFor: a.toolsFor,
		Embedder: embedder,
		Workers:  cfg.Webhook.Workers,
		Logger:   a.log,
	})
	if err != nil {
		return err
	}

	return a.routes(opts, embedder)
}

// authorizer returns the OAuth authorizer, or nil when no server needs one.
// It must return a nil interface rather than a typed nil pointer.
func (a *App) authorizer() tool.Authorizer {
	if a.OAuth == nil {
		return nil
	}
	return a.OAuth
}

// toolsFor returns the MCP servers the named agent may use.
func (a *App) toolsFor(name string) []string {
	servers := a.Config.MCPFor(name)
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	return names
}

// routes builds the HTTP mux.
func (a *App) routes(opts OpenOptions, embedder embedding.Embedder) error {
	cfg := a.Config
	mux := http.NewServeMux()

	receiver, err := webhook.New(webhook.Options{
		Receiver:      a.Store,
		Token:         cfg.Webhook.Token,
		RetainAudio:   cfg.Audio.Retain,
		MaxAudioBytes: cfg.Audio.MaxBytes,
		Notify:        a.pipeline.Notify,
		Logger:        a.log,
	})
	if err != nil {
		return err
	}
	mux.Handle(cfg.Webhook.Path, receiver)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	if cfg.MCPServe.Enabled {
		server, err := mcpserve.New(mcpserve.Options{
			Store:    a.Store,
			Embedder: embedder,
			Token:    cfg.MCPServe.Token,
			Version:  opts.Version,
			Logger:   a.log,
		})
		if err != nil {
			return err
		}
		// Both the bare path and its subtree, because MCP clients differ on
		// whether they append a slash.
		mux.Handle(cfg.MCPServe.Path, server)
		mux.Handle(cfg.MCPServe.Path+"/", server)
		a.log.Info("serving the journal over mcp", "path", cfg.MCPServe.Path)
	}

	a.mux = mux
	return nil
}

// Handler returns the HTTP handler.
func (a *App) Handler() http.Handler { return a.mux }

// Pipeline returns the processing pipeline.
func (a *App) Pipeline() *pipeline.Pipeline { return a.pipeline }

// Serve runs the HTTP server and the pipeline until ctx is cancelled, then shuts
// the listener down gracefully.
func (a *App) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              a.Config.Server.Addr,
		Handler:           a.mux,
		ReadHeaderTimeout: 15 * time.Second,
		// Generous, because a recording carries audio over a home connection.
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 30 * time.Second,
	}

	errs := make(chan error, 2)
	go func() {
		a.log.Info("listening", "addr", srv.Addr, "webhook", a.Config.Webhook.Path)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server: %w", err)
			return
		}
		errs <- nil
	}()
	go func() {
		if err := a.pipeline.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("pipeline: %w", err)
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		if err != nil {
			return err
		}
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	a.log.Info("stopped")
	return nil
}

// Close releases the store, the MCP sessions and the embedder.
func (a *App) Close() error {
	var errs []error
	if a.Tools != nil {
		errs = append(errs, a.Tools.Close())
	}
	if a.Store != nil {
		errs = append(errs, a.Store.Close())
	}
	a.closeModel()
	return errors.Join(errs...)
}

func (a *App) closeModel() {
	if a.Model != nil && a.Model.Close != nil {
		_ = a.Model.Close()
	}
}
