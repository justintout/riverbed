// Command riverbed receives recordings from a voice device, files them, and
// routes the ones that ask for something to an agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/justintout/riverbed"
	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/store"
	"github.com/justintout/riverbed/tool"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Riverbed receives recordings from a voice device, stores them, and routes
the ones that ask for something to an agent.

Usage:
  riverbed <command> [flags]

Commands:
  serve      Receive recordings and process them
  search     Search the recorded notes
  auth       Authorize an MCP server that uses OAuth
  backfill   Embed recordings stored before embedding was enabled
  migrate    Create or migrate the database, then exit
  version    Print the version

Run "riverbed <command> -h" for the flags of a command.

Configuration is read from the file named by -config, or RIVERBED_CONFIG.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "riverbed:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("a command is required")
	}

	command, args := os.Args[1], os.Args[2:]
	switch command {
	case "serve":
		return serve(args)
	case "search":
		return search(args)
	case "auth":
		return authorize(args)
	case "backfill":
		return backfill(args)
	case "migrate":
		return migrate(args)
	case "version":
		fmt.Println("riverbed", version)
		return nil
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

// common holds the flags every command shares.
type common struct {
	configPath string
	logLevel   string
	logFormat  string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.configPath, "config", os.Getenv("RIVERBED_CONFIG"),
		"path to the TOML configuration file")
	fs.StringVar(&c.logLevel, "log-level", envOr("RIVERBED_LOG_LEVEL", "info"),
		"debug, info, warn or error")
	fs.StringVar(&c.logFormat, "log-format", envOr("RIVERBED_LOG_FORMAT", "text"),
		"text or json")
}

// load parses the configuration and installs the logger.
func (c *common) load() (config.Config, *slog.Logger, error) {
	logger, err := c.logger()
	if err != nil {
		return config.Config{}, nil, err
	}
	slog.SetDefault(logger)

	cfg, err := config.Load(c.configPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, logger, nil
}

func (c *common) logger() (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(c.logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", c.logLevel)
	}

	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(c.logFormat) {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", c.logFormat)
	}
}

// signalContext cancels on interrupt or termination, so a container stop is a
// graceful shutdown.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, logger, err := c.load()
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	app, err := riverbed.Open(ctx, riverbed.OpenOptions{
		Config: cfg, Version: version, Logger: logger,
	})
	if err != nil {
		return err
	}
	defer app.Close()

	logger.Info("riverbed starting",
		"version", version,
		"database", cfg.Store.Path,
		"agents", len(cfg.Agents),
		"mcp_servers", len(cfg.MCP))

	return app.Serve(ctx)
}

func search(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	var c common
	c.bind(fs)
	since := fs.Duration("since", 0, "only notes this recent, for example 24h")
	tag := fs.String("tag", "", "only notes carrying this tag")
	route := fs.String("route", "", "only notes handled by this agent")
	toolUsed := fs.String("tool-used", "", "yes or no, to filter on whether a tool ran")
	limit := fs.Int("limit", 20, "how many notes to return")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: riverbed search [flags] [query...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, logger, err := c.load()
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	app, err := riverbed.Open(ctx, riverbed.OpenOptions{
		Config: cfg, Version: version, Logger: logger,
	})
	if err != nil {
		return err
	}
	defer app.Close()

	query := store.Query{
		Text:  strings.Join(fs.Args(), " "),
		Route: *route,
		Limit: *limit,
	}
	if *since > 0 {
		query.Since = time.Now().Add(-*since)
	}
	if *tag != "" {
		query.Tags = []string{*tag}
	}
	switch strings.ToLower(*toolUsed) {
	case "":
	case "yes", "true":
		used := true
		query.ToolUsed = &used
	case "no", "false":
		used := false
		query.ToolUsed = &used
	default:
		return fmt.Errorf("-tool-used must be yes or no, not %q", *toolUsed)
	}

	// Searching by meaning as well as wording needs the query embedded the same
	// way the notes were.
	if app.Model != nil && app.Store.VectorEnabled() && query.Text != "" {
		vector, err := app.Model.Embed(ctx, query.Text)
		if err != nil {
			return fmt.Errorf("embed query: %w", err)
		}
		query.Vector = vector
	}

	results, err := app.Store.Search(ctx, query)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Println("No notes matched.")
		return nil
	}

	for _, r := range results {
		fmt.Printf("%s  #%d", r.Recording.RecordedAt.Format(time.RFC3339), r.Recording.ID)
		if r.Recording.Route != "" {
			fmt.Printf("  [%s]", r.Recording.Route)
		}
		if len(r.Tags) > 0 {
			fmt.Printf("  (%s)", strings.Join(r.Tags, ", "))
		}
		fmt.Println()
		fmt.Println("   ", r.Recording.Transcription)

		responses, err := app.Store.Responses(ctx, r.Recording.ID)
		if err != nil {
			return err
		}
		for _, response := range responses {
			fmt.Printf("    -> %s: %s\n", response.Agent, response.Text)
		}
		calls, err := app.Store.ToolCalls(ctx, r.Recording.ID)
		if err != nil {
			return err
		}
		for _, call := range calls {
			status := ""
			if call.IsError {
				status = " (failed)"
			}
			fmt.Printf("    -> tool %s.%s%s\n", call.Server, call.Tool, status)
		}
		fmt.Println()
	}
	return nil
}

func authorize(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	var c common
	c.bind(fs)
	reset := fs.Bool("reset", false, "discard the stored token before authorizing")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: riverbed auth [flags] <mcp-server-name>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("name exactly one MCP server")
	}
	name := fs.Arg(0)

	cfg, logger, err := c.load()
	if err != nil {
		return err
	}

	var target *config.MCP
	for i := range cfg.MCP {
		if cfg.MCP[i].Name == name {
			target = &cfg.MCP[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no MCP server named %q is configured", name)
	}
	if target.Auth != "oauth" {
		return fmt.Errorf("MCP server %q uses auth %q, which needs no authorization",
			name, target.Auth)
	}

	ctx, stop := signalContext()
	defer stop()

	// The redirect listener has to match the configured base URL, because that
	// is the redirect the authorization server was registered with.
	prompter, err := tool.LocalPrompter(cfg.Server.BaseURL, func(url string) {
		fmt.Println("Open this URL to authorize Riverbed:")
		fmt.Println()
		fmt.Println("   ", url)
		fmt.Println()
		fmt.Println("Waiting for the redirect...")
	})
	if err != nil {
		return err
	}

	app, err := riverbed.Open(ctx, riverbed.OpenOptions{
		Config: cfg, Version: version, Logger: logger, Prompter: prompter,
	})
	if err != nil {
		return err
	}
	defer app.Close()

	if *reset {
		if err := app.Store.DeleteToken(ctx, name); err != nil {
			return err
		}
		fmt.Printf("Discarded the stored token for %q.\n", name)
	}

	// Listing the tools drives the flow: the transport authorizes on the first
	// unauthorized response.
	tools := app.Tools.Tools(ctx, []string{name})
	if len(tools) == 0 {
		return fmt.Errorf("authorization did not complete: %q offered no tools", name)
	}

	if _, err := app.Store.Token(ctx, name); err != nil {
		return fmt.Errorf("no token was stored for %q: %w", name, err)
	}

	fmt.Printf("Authorized %q. %d tools are available:\n", name, len(tools))
	for _, t := range tools {
		fmt.Printf("  %s\n", t.Name)
	}
	return nil
}

func backfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	var c common
	c.bind(fs)
	batch := fs.Int("batch", 100, "how many recordings to load at a time")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, logger, err := c.load()
	if err != nil {
		return err
	}
	if !cfg.Embedding.Enabled() {
		return errors.New("no embedding model is configured, so there is nothing to backfill")
	}

	ctx, stop := signalContext()
	defer stop()

	app, err := riverbed.Open(ctx, riverbed.OpenOptions{
		Config: cfg, Version: version, Logger: logger,
	})
	if err != nil {
		return err
	}
	defer app.Close()

	n, err := app.Pipeline().Backfill(ctx, *batch)
	if err != nil {
		return fmt.Errorf("after embedding %d recordings: %w", n, err)
	}
	fmt.Printf("Embedded %d recordings.\n", n)
	return nil
}

func migrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	var c common
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, logger, err := c.load()
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	app, err := riverbed.Open(ctx, riverbed.OpenOptions{
		Config: cfg, Version: version, Logger: logger,
	})
	if err != nil {
		return err
	}
	defer app.Close()

	fmt.Printf("Database %s is ready.\n", cfg.Store.Path)
	return nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
