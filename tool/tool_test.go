package tool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/justintout/riverbed/config"
)

// lightArgs is the input schema of the test tool below.
type lightArgs struct {
	Entity string `json:"entity" jsonschema:"the light to switch"`
}

// testServer starts an MCP server over streamable HTTP and returns its URL.
// It reports the Authorization header it last saw, so auth can be asserted.
func testServer(t *testing.T) (url string, lastAuth *atomic.Value, calls *atomic.Int64) {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-home", Version: "1.0.0"}, nil)
	calls = &atomic.Int64{}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "turn_on",
		Description: "Turn a light on",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args lightArgs) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		if args.Entity == "light.broken" {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "that light is unreachable"}},
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "turned on " + args.Entity}},
		}, nil, nil
	})

	lastAuth = &atomic.Value{}
	lastAuth.Store("")
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth.Store(r.Header.Get("Authorization"))
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpSrv.Close)
	return httpSrv.URL, lastAuth, calls
}

func TestToolsAndCall(t *testing.T) {
	url, _, calls := testServer(t)

	r, err := New(Options{
		Servers: []config.MCP{{Name: "home", URL: url, Transport: "streamable", Auth: "none"}},
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if got := r.Names(); len(got) != 1 || got[0] != "home" {
		t.Errorf("names = %v", got)
	}

	tools := r.Tools(t.Context(), []string{"home"})
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "home.turn_on" {
		t.Errorf("name = %q, want home.turn_on", tools[0].Name)
	}
	if tools[0].Server != "home" || tools[0].Bare != "turn_on" {
		t.Errorf("server/bare = %q/%q", tools[0].Server, tools[0].Bare)
	}
	if tools[0].Description != "Turn a light on" {
		t.Errorf("description = %q", tools[0].Description)
	}
	if tools[0].InputSchema == nil {
		t.Error("the input schema must reach the model")
	}

	res, err := r.Call(t.Context(), "home.turn_on", map[string]any{"entity": "light.kitchen"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("unexpected tool error: %s", res.Text)
	}
	if res.Text != "turned on light.kitchen" {
		t.Errorf("text = %q", res.Text)
	}
	if calls.Load() != 1 {
		t.Errorf("tool ran %d times, want 1", calls.Load())
	}
}

func TestToolErrorIsReportedNotReturnedAsError(t *testing.T) {
	url, _, _ := testServer(t)
	r, err := New(Options{Servers: []config.MCP{
		{Name: "home", URL: url, Transport: "streamable"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	res, err := r.Call(t.Context(), "home.turn_on", map[string]any{"entity": "light.broken"})
	if err != nil {
		t.Fatalf("a tool-level error must not be a Go error: %v", err)
	}
	if !res.IsError {
		t.Error("want IsError so the agent can see the failure and recover")
	}
	if !strings.Contains(res.Text, "unreachable") {
		t.Errorf("text = %q", res.Text)
	}
}

func TestBearerAuthIsSent(t *testing.T) {
	url, lastAuth, _ := testServer(t)
	r, err := New(Options{Servers: []config.MCP{
		{Name: "home", URL: url, Transport: "streamable", Auth: "bearer", Token: "token123"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if tools := r.Tools(t.Context(), []string{"home"}); len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	if got := lastAuth.Load().(string); got != "Bearer token123" {
		t.Errorf("authorization = %q, want Bearer token123", got)
	}
}

func TestBearerTokenKeepsItsOwnScheme(t *testing.T) {
	// A configured value that already names a scheme is sent unchanged.
	url, lastAuth, _ := testServer(t)
	r, err := New(Options{Servers: []config.MCP{
		{Name: "home", URL: url, Transport: "streamable", Auth: "bearer", Token: "Token abc123"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	r.Tools(t.Context(), []string{"home"})
	if got := lastAuth.Load().(string); got != "Token abc123" {
		t.Errorf("authorization = %q", got)
	}
}

func TestUnreachableServerIsSkippedNotFatal(t *testing.T) {
	url, _, _ := testServer(t)
	r, err := New(Options{Servers: []config.MCP{
		{Name: "home", URL: url, Transport: "streamable"},
		{Name: "down", URL: "http://127.0.0.1:1/mcp", Transport: "streamable"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tools := r.Tools(ctx, []string{"home", "down"})
	if len(tools) != 1 || tools[0].Server != "home" {
		t.Errorf("the reachable server's tools should survive, got %+v", tools)
	}
}

func TestCallRejectsBadNames(t *testing.T) {
	r, err := New(Options{Servers: []config.MCP{{Name: "home", URL: "http://x", Transport: "streamable"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.Call(t.Context(), "unqualified", nil); err == nil {
		t.Error("want an error for an unqualified name")
	}
	if _, err := r.Call(t.Context(), "ghost.tool", nil); err == nil {
		t.Error("want an error for an unknown server")
	}
}

func TestServerNameCannotContainTheSeparator(t *testing.T) {
	_, err := New(Options{Servers: []config.MCP{{Name: "home.sub", URL: "http://x", Transport: "streamable"}}})
	if err == nil {
		t.Error("want an error: the name would be ambiguous")
	}
}

func TestOAuthNeedsAnAuthorizer(t *testing.T) {
	_, err := New(Options{Servers: []config.MCP{
		{Name: "s", URL: "https://example.invalid/mcp", Transport: "streamable", Auth: "oauth"},
	}})
	if err == nil || !strings.Contains(err.Error(), "authorizer") {
		t.Errorf("want an authorizer error, got %v", err)
	}
}

func TestSessionIsReused(t *testing.T) {
	url, _, _ := testServer(t)
	r, err := New(Options{Servers: []config.MCP{{Name: "home", URL: url, Transport: "streamable"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	first := r.Tools(t.Context(), []string{"home"})
	second := r.Tools(t.Context(), []string{"home"})
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("listing twice should work: %d then %d", len(first), len(second))
	}
	s := r.servers["home"]
	s.mu.Lock()
	live := s.session != nil
	s.mu.Unlock()
	if !live {
		t.Error("the session should be kept for reuse")
	}
}

// memStore is an in-memory TokenStore.
type memStore struct {
	tokens map[string]*oauth2.Token
	writes int
}

func (m *memStore) Token(_ context.Context, server string) (*oauth2.Token, error) {
	tok, ok := m.tokens[server]
	if !ok {
		return nil, errors.New("not found")
	}
	return tok, nil
}

func (m *memStore) PutToken(_ context.Context, server string, tok *oauth2.Token) error {
	if m.tokens == nil {
		m.tokens = map[string]*oauth2.Token{}
	}
	m.tokens[server] = tok
	m.writes++
	return nil
}

func TestNewOAuthValidates(t *testing.T) {
	if _, err := NewOAuth(OAuthOptions{BaseURL: "https://example.invalid"}); err == nil {
		t.Error("want an error without a store")
	}
	if _, err := NewOAuth(OAuthOptions{Store: &memStore{}}); err == nil {
		t.Error("want an error without a base URL")
	}
}

func TestRedirectURL(t *testing.T) {
	o, err := NewOAuth(OAuthOptions{Store: &memStore{}, BaseURL: "https://riverbed.example/"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://riverbed.example" + RedirectPath + "/my%20server"
	if got := o.RedirectURL("my server"); got != want {
		t.Errorf("redirect = %q, want %q", got, want)
	}
}

func TestDaemonReportsAuthorizationRequired(t *testing.T) {
	o, err := NewOAuth(OAuthOptions{Store: &memStore{}, BaseURL: "https://riverbed.example"})
	if err != nil {
		t.Fatal(err)
	}
	// With no prompter, starting a flow must fail with a clear instruction
	// rather than block a headless daemon.
	_, err = o.fetcher("notion")(t.Context(), nil)
	if !errors.Is(err, ErrAuthorizationRequired) {
		t.Fatalf("want ErrAuthorizationRequired, got %v", err)
	}
	if !strings.Contains(err.Error(), "riverbed auth notion") {
		t.Errorf("the error should say what to run: %v", err)
	}
}

func TestHandlerIsCachedPerServer(t *testing.T) {
	o, err := NewOAuth(OAuthOptions{Store: &memStore{}, BaseURL: "https://riverbed.example"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.MCP{Name: "notion", URL: "https://example.invalid/mcp", Transport: "streamable", Auth: "oauth"}
	first, err := o.Handler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := o.Handler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("the same server should reuse one handler, so its token is shared")
	}
}

func TestPersistingSourceStoresRotatedTokens(t *testing.T) {
	store := &memStore{}
	o, err := NewOAuth(OAuthOptions{Store: store, BaseURL: "https://riverbed.example"})
	if err != nil {
		t.Fatal(err)
	}

	rotating := &rotatingSource{tokens: []string{"first", "second"}}
	src := &persistingSource{oauth: o, server: "notion", inner: rotating, last: ""}

	if _, err := src.Token(); err != nil {
		t.Fatal(err)
	}
	if store.writes != 1 || store.tokens["notion"].AccessToken != "first" {
		t.Fatalf("writes = %d, token = %+v", store.writes, store.tokens["notion"])
	}

	// The same token must not be written again.
	rotating.repeat = true
	if _, err := src.Token(); err != nil {
		t.Fatal(err)
	}
	if store.writes != 1 {
		t.Errorf("writes = %d, want no write for an unchanged token", store.writes)
	}

	// A rotated token must be persisted, or a restart would need a browser.
	rotating.repeat = false
	if _, err := src.Token(); err != nil {
		t.Fatal(err)
	}
	if store.writes != 2 || store.tokens["notion"].AccessToken != "second" {
		t.Errorf("writes = %d, token = %+v", store.writes, store.tokens["notion"])
	}
}

// rotatingSource yields the next access token on each call, or repeats the one
// it last issued while repeat is set.
type rotatingSource struct {
	tokens []string
	i      int
	repeat bool
	issued string
}

func (r *rotatingSource) Token() (*oauth2.Token, error) {
	if r.repeat && r.issued != "" {
		return &oauth2.Token{AccessToken: r.issued, Expiry: time.Now().Add(time.Hour)}, nil
	}
	if r.i >= len(r.tokens) {
		r.i = len(r.tokens) - 1
	}
	r.issued = r.tokens[r.i]
	r.i++
	return &oauth2.Token{AccessToken: r.issued, Expiry: time.Now().Add(time.Hour)}, nil
}

func TestStoredSourceSeedsFromTheDatabase(t *testing.T) {
	store := &memStore{tokens: map[string]*oauth2.Token{
		"notion": {AccessToken: "stored", Expiry: time.Now().Add(time.Hour)},
	}}
	o, err := NewOAuth(OAuthOptions{Store: store, BaseURL: "https://riverbed.example"})
	if err != nil {
		t.Fatal(err)
	}
	src := o.stored("notion")
	if src == nil {
		t.Fatal("a stored token should produce a token source")
	}
	tok, err := src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "stored" {
		t.Errorf("access token = %q", tok.AccessToken)
	}

	if o.stored("absent") != nil {
		t.Error("no stored token should produce no source, so the flow starts")
	}
}

func TestContentText(t *testing.T) {
	got := contentText(&mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "first"},
		&mcp.ImageContent{MIMEType: "image/png", Data: []byte("xx")},
		&mcp.TextContent{Text: "last"},
	}})
	for _, want := range []string{"first", "[image image/png, 2 bytes]", "last"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
