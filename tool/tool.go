// Package tool connects to MCP servers and offers their tools to agents.
//
// Each configured server becomes a lazily connected session. Tools are exposed
// under a qualified name, "server__tool", so two servers may offer the same tool
// name without colliding, and so a stored tool call says which server ran it.
package tool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justintout/riverbed/config"
)

// NameSeparator joins a server name and a tool name. It uses only characters
// that the Anthropic and OpenAI APIs accept in a tool name, [A-Za-z0-9_-].
const NameSeparator = "__"

// Tool is one callable tool offered by a server.
type Tool struct {
	// Name is the qualified name an agent calls, "server__tool".
	Name string
	// Server and Bare are the two halves of Name.
	Server string
	Bare   string

	Description string
	// InputSchema is the tool's JSON Schema, passed to the model unchanged.
	InputSchema any
}

// Result is the outcome of a tool call.
type Result struct {
	// Text is the textual content of the result, joined across content blocks.
	Text string
	// IsError reports a tool-level error. The agent should see it and may
	// recover, so it is not returned as a Go error.
	IsError bool
}

// Registry holds the configured MCP servers.
type Registry struct {
	servers map[string]*server
	names   []string
	log     *slog.Logger
}

// Authorizer supplies an OAuth handler for a server that needs one.
type Authorizer interface {
	// Handler returns the OAuth handler for the named server.
	Handler(server config.MCP) (auth.OAuthHandler, error)
}

// Options configures a Registry.
type Options struct {
	Servers []config.MCP
	// Authorizer is required only when a server uses OAuth.
	Authorizer Authorizer
	// ClientName identifies Riverbed to the MCP servers.
	ClientName string
	Version    string
	Logger     *slog.Logger
}

// New builds a registry. No connection is made until a tool is listed or
// called, so an unreachable server does not prevent startup.
func New(opts Options) (*Registry, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ClientName == "" {
		opts.ClientName = "riverbed"
	}

	r := &Registry{servers: make(map[string]*server, len(opts.Servers)), log: opts.Logger}
	for _, cfg := range opts.Servers {
		if strings.Contains(cfg.Name, NameSeparator) {
			return nil, fmt.Errorf("tool: mcp server name %q cannot contain %q", cfg.Name, NameSeparator)
		}
		s := &server{
			cfg:        cfg,
			clientName: opts.ClientName,
			version:    opts.Version,
			log:        opts.Logger.With("mcp", cfg.Name),
		}
		if cfg.Auth == "oauth" {
			if opts.Authorizer == nil {
				return nil, fmt.Errorf("tool: mcp server %q uses oauth but no authorizer was provided", cfg.Name)
			}
			handler, err := opts.Authorizer.Handler(cfg)
			if err != nil {
				return nil, fmt.Errorf("tool: mcp server %q: %w", cfg.Name, err)
			}
			s.oauth = handler
		}
		r.servers[cfg.Name] = s
		r.names = append(r.names, cfg.Name)
	}
	sort.Strings(r.names)
	return r, nil
}

// Names returns the configured server names in order.
func (r *Registry) Names() []string { return r.names }

// Tools lists the tools offered by the named servers. A server that cannot be
// reached is logged and skipped: the rest of the system stays useful when one
// integration is down.
func (r *Registry) Tools(ctx context.Context, names []string) []Tool {
	var out []Tool
	for _, name := range names {
		s, ok := r.servers[name]
		if !ok {
			r.log.Warn("unknown mcp server requested", "mcp", name)
			continue
		}
		tools, err := s.tools(ctx)
		if err != nil {
			r.log.Error("list tools", "mcp", name, "error", err)
			continue
		}
		out = append(out, tools...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Call invokes a qualified tool name with JSON arguments.
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (Result, error) {
	serverName, bare, ok := strings.Cut(name, NameSeparator)
	if !ok {
		return Result{}, fmt.Errorf("tool: %q is not a qualified tool name", name)
	}
	s, ok := r.servers[serverName]
	if !ok {
		return Result{}, fmt.Errorf("tool: unknown mcp server %q", serverName)
	}
	return s.call(ctx, bare, args)
}

// Close disconnects every session.
func (r *Registry) Close() error {
	var errs []error
	for _, s := range r.servers {
		if err := s.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// server is one MCP server and its session.
type server struct {
	cfg        config.MCP
	clientName string
	version    string
	oauth      auth.OAuthHandler
	log        *slog.Logger

	mu      sync.Mutex
	session *mcp.ClientSession
}

// connect returns a live session, dialing on first use and redialing after a
// disconnect.
func (s *server) connect(ctx context.Context) (*mcp.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session != nil {
		// Ping confirms the session is still usable; a dead one is replaced.
		if err := s.session.Ping(ctx, nil); err == nil {
			return s.session, nil
		}
		s.log.Info("mcp session lost, reconnecting")
		_ = s.session.Close()
		s.session = nil
	}

	client := mcp.NewClient(&mcp.Implementation{Name: s.clientName, Version: s.version}, nil)
	session, err := client.Connect(ctx, s.transport(), nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", s.cfg.URL, err)
	}
	s.session = session
	return session, nil
}

// transport builds the transport described by the configuration.
func (s *server) transport() mcp.Transport {
	httpClient := &http.Client{Timeout: 2 * time.Minute}
	if s.cfg.Auth == "bearer" {
		httpClient.Transport = &bearerTransport{token: s.cfg.Token, base: http.DefaultTransport}
	}

	if s.cfg.Transport == "sse" {
		return &mcp.SSEClientTransport{Endpoint: s.cfg.URL, HTTPClient: httpClient}
	}
	return &mcp.StreamableClientTransport{
		Endpoint:     s.cfg.URL,
		HTTPClient:   httpClient,
		OAuthHandler: s.oauth,
	}
}

func (s *server) tools(ctx context.Context) ([]Tool, error) {
	session, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	out := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		out = append(out, Tool{
			Name:        s.cfg.Name + NameSeparator + t.Name,
			Server:      s.cfg.Name,
			Bare:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return out, nil
}

func (s *server) call(ctx context.Context, name string, args map[string]any) (Result, error) {
	session, err := s.connect(ctx)
	if err != nil {
		return Result{}, err
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return Result{}, fmt.Errorf("call %s.%s: %w", s.cfg.Name, name, err)
	}
	return Result{Text: contentText(res), IsError: res.IsError}, nil
}

func (s *server) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil
	}
	err := s.session.Close()
	s.session = nil
	return err
}

// contentText joins the textual content of a tool result. Non-text content is
// described rather than dropped, so a model is told something came back.
func contentText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *mcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource %s]", v.URI))
		default:
			parts = append(parts, fmt.Sprintf("[%T]", c))
		}
	}
	return strings.Join(parts, "\n")
}

// bearerTransport adds a static Authorization header. The device's own MCP
// sandbox supports only this, so Riverbed supports it too for the same servers.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The request must not be mutated: RoundTrippers may be retried.
	clone := req.Clone(req.Context())
	token := t.token
	if !strings.Contains(token, " ") {
		token = "Bearer " + token
	}
	clone.Header.Set("Authorization", token)
	return t.base.RoundTrip(clone)
}
