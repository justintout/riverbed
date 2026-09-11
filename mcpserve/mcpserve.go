// Package mcpserve exposes Riverbed's journal as an MCP server.
//
// This is the other direction from package tool: here Riverbed is the server.
// The device's own MCP sandbox can point at it, as can any agent, which makes
// the recorded thoughts retrievable by the assistant that produced them.
//
// The device supports only a static Authorization header, so that is what
// guards this server.
package mcpserve

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justintout/riverbed/embedding"
	"github.com/justintout/riverbed/store"
)

// Options configures the server.
type Options struct {
	Store *store.Store
	// Embedder enables semantic search. Without it search is keyword only.
	Embedder embedding.Embedder
	// Token guards every request.
	Token   string
	Version string
	Logger  *slog.Logger
}

// Server serves Riverbed's journal over MCP.
type Server struct {
	opts    Options
	handler http.Handler
	log     *slog.Logger
}

// searchArgs is the input to the search tool.
type searchArgs struct {
	Query    string `json:"query" jsonschema:"what to look for in the recorded notes; omit to list the most recent"`
	Since    string `json:"since,omitempty" jsonschema:"only notes this recent, as a duration such as 24h or 7d"`
	Tag      string `json:"tag,omitempty" jsonschema:"only notes carrying this tag"`
	Route    string `json:"route,omitempty" jsonschema:"only notes handled by this agent"`
	ToolUsed string `json:"tool_used,omitempty" jsonschema:"set to yes or no to filter on whether a tool was invoked"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many notes to return, at most 50"`
}

// recentArgs is the input to the recent tool.
type recentArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many notes to return, at most 50"`
}

// New builds the MCP server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("mcpserve: store is required")
	}
	if opts.Token == "" {
		return nil, errors.New("mcpserve: token is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	s := &Server{opts: opts, log: opts.Logger}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "riverbed",
		Version: opts.Version,
		Title:   "Riverbed journal",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_journal",
		Description: "Search the recorded voice notes. Matches on wording and, when " +
			"semantic search is available, on meaning as well.",
	}, s.search)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "recent_notes",
		Description: "List the most recently recorded voice notes.",
	}, s.recent)

	s.handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, nil)
	return s, nil
}

// ServeHTTP checks the token, then serves MCP.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.handler.ServeHTTP(w, r)
}

func (s *Server) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	token := header
	if len(header) >= 7 && strings.EqualFold(header[:7], "Bearer ") {
		token = header[7:]
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.opts.Token)) == 1
}

// maxLimit caps how many notes one call may return, so a model cannot ask for
// the whole database.
const maxLimit = 50

func (s *Server) search(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	query := store.Query{
		Text:  strings.TrimSpace(args.Query),
		Route: args.Route,
		Limit: clampLimit(args.Limit),
	}
	if args.Tag != "" {
		query.Tags = []string{args.Tag}
	}
	if args.Since != "" {
		d, err := parseDuration(args.Since)
		if err != nil {
			return toolError(fmt.Sprintf("since %q is not a duration such as 24h or 7d", args.Since)), nil, nil
		}
		query.Since = time.Now().Add(-d)
	}
	switch strings.ToLower(args.ToolUsed) {
	case "", "any":
	case "yes", "true":
		used := true
		query.ToolUsed = &used
	case "no", "false":
		used := false
		query.ToolUsed = &used
	default:
		return toolError(fmt.Sprintf("tool_used %q should be yes or no", args.ToolUsed)), nil, nil
	}

	// Semantic matching is additive: the keyword ranking still applies, and the
	// two are fused by the store.
	if s.opts.Embedder != nil && s.opts.Store.VectorEnabled() && query.Text != "" {
		vector, err := s.opts.Embedder.Embed(ctx, query.Text)
		if err != nil {
			s.log.Warn("embed search query", "error", err)
		} else {
			query.Vector = vector
		}
	}

	results, err := s.opts.Store.Search(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("search: %w", err)
	}
	return textResult(format(results)), nil, nil
}

func (s *Server) recent(ctx context.Context, _ *mcp.CallToolRequest, args recentArgs) (*mcp.CallToolResult, any, error) {
	results, err := s.opts.Store.Search(ctx, store.Query{Limit: clampLimit(args.Limit)})
	if err != nil {
		return nil, nil, fmt.Errorf("recent: %w", err)
	}
	return textResult(format(results)), nil, nil
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return 10
	case limit > maxLimit:
		return maxLimit
	default:
		return limit
	}
}

// parseDuration accepts Go durations plus a day suffix, because "7d" is the
// natural way to ask and time.ParseDuration rejects it.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := time.ParseDuration(rest + "h")
		if err != nil {
			return 0, err
		}
		return days * 24, nil
	}
	return time.ParseDuration(s)
}

// format renders results as the text a model reads.
func format(results []store.Result) string {
	if len(results) == 0 {
		return "No notes matched."
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s", r.Recording.RecordedAt.Format(time.RFC3339))
		if r.Recording.Route != "" {
			fmt.Fprintf(&b, " [%s]", r.Recording.Route)
		}
		if len(r.Tags) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(r.Tags, ", "))
		}
		b.WriteString("\n")
		b.WriteString(r.Recording.Transcription)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func toolError(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}
