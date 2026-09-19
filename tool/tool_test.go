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
	"github.com/justintout/riverbed/store"
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
	if tools[0].Name != "home__turn_on" {
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

	res, err := r.Call(t.Context(), "home__turn_on", map[string]any{"entity": "light.kitchen"})
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

	res, err := r.Call(t.Context(), "home__turn_on", map[string]any{"entity": "light.broken"})
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
	if _, err := r.Call(t.Context(), "ghost__tool", nil); err == nil {
		t.Error("want an error for an unknown server")
	}
}

func TestServerNameCannotContainTheSeparator(t *testing.T) {
	_, err := New(Options{Servers: []config.MCP{{Name: "home__sub", URL: "http://x", Transport: "streamable"}}})
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
	tokens       map[string]*oauth2.Token
	clients      map[string]*store.OAuthClient
	writes       int
	clientWrites int
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

func (m *memStore) Client(_ context.Context, server string) (*store.OAuthClient, error) {
	client, ok := m.clients[server]
	if !ok {
		return nil, errors.New("not found")
	}
	return client, nil
}

func (m *memStore) PutClient(_ context.Context, server string, client *store.OAuthClient) error {
	if m.clients == nil {
		m.clients = map[string]*store.OAuthClient{}
	}
	m.clients[server] = client
	m.clientWrites++
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

func TestStoredSourceRefreshesWithoutABrowser(t *testing.T) {
	// A token endpoint that honours one refresh_token grant.
	var grantType, sentClientID, sentRefresh string
	refreshes := 0
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		grantType = r.Form.Get("grant_type")
		sentRefresh = r.Form.Get("refresh_token")
		sentClientID = r.Form.Get("client_id")
		if sentClientID == "" {
			// client_secret_basic puts the id in the Authorization header.
			if id, _, ok := r.BasicAuth(); ok {
				sentClientID = id
			}
		}
		refreshes++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-access","refresh_token":"fresh-refresh",` +
			`"token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	// The state a restart would find: an expired access token, a refresh token,
	// and the registration that produced them.
	memory := &memStore{
		tokens: map[string]*oauth2.Token{"notion": {
			AccessToken:  "stale-access",
			RefreshToken: "stored-refresh",
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(-time.Hour),
		}},
		clients: map[string]*store.OAuthClient{"notion": {
			ClientID: "registered-client", ClientSecret: "registered-secret",
			AuthURL: "https://example.invalid/authorize", TokenURL: tokenSrv.URL,
			Scopes: []string{"read"},
		}},
	}

	// No prompter, as in the daemon: needing a browser here would be a failure.
	o, err := NewOAuth(OAuthOptions{Store: memory, BaseURL: "https://riverbed.example"})
	if err != nil {
		t.Fatal(err)
	}

	source := o.stored("notion")
	if source == nil {
		t.Fatal("a stored token should produce a source")
	}
	tok, err := source.Token()
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}

	if tok.AccessToken != "fresh-access" {
		t.Errorf("access token = %q, want the refreshed one", tok.AccessToken)
	}
	if refreshes != 1 {
		t.Errorf("the token endpoint was called %d times, want 1", refreshes)
	}
	if grantType != "refresh_token" {
		t.Errorf("grant_type = %q", grantType)
	}
	if sentRefresh != "stored-refresh" {
		t.Errorf("refresh_token sent = %q", sentRefresh)
	}
	if sentClientID != "registered-client" {
		t.Errorf("client_id sent = %q, want the stored registration", sentClientID)
	}

	// The refreshed token must reach the database, or the next restart repeats
	// this work.
	stored, err := memory.Token(t.Context(), "notion")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "fresh-access" || stored.RefreshToken != "fresh-refresh" {
		t.Errorf("stored token = %+v", stored)
	}
}

func TestStoredSourceWithoutARegistrationCannotRefresh(t *testing.T) {
	// Before this change there was no registration to refresh against. The
	// token is then served as it stands so the transport sees a 401.
	expired := &oauth2.Token{AccessToken: "stale", Expiry: time.Now().Add(-time.Hour)}
	memory := &memStore{tokens: map[string]*oauth2.Token{"notion": expired}}

	o, err := NewOAuth(OAuthOptions{Store: memory, BaseURL: "https://riverbed.example"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := o.stored("notion").Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "stale" {
		t.Errorf("access token = %q", tok.AccessToken)
	}
	if tok.Valid() {
		t.Error("the token should still read as expired")
	}
}

func TestRegistrationIsRecordedOnAuthorization(t *testing.T) {
	memory := &memStore{}
	o, err := NewOAuth(OAuthOptions{Store: memory, BaseURL: "https://riverbed.example"})
	if err != nil {
		t.Fatal(err)
	}

	// This is the config the SDK hands over after a successful exchange.
	cfg := &oauth2.Config{
		ClientID:     "issued-client",
		ClientSecret: "issued-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://as.example.invalid/authorize",
			TokenURL:  "https://as.example.invalid/token",
			AuthStyle: oauth2.AuthStyleInHeader,
		},
		Scopes: []string{"read", "write"},
	}
	tok := &oauth2.Token{AccessToken: "first", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}

	if _, err := o.persisting("notion")(t.Context(), cfg, tok); err != nil {
		t.Fatal(err)
	}

	client, err := memory.Client(t.Context(), "notion")
	if err != nil {
		t.Fatalf("the registration should have been stored: %v", err)
	}
	if client.ClientID != "issued-client" || client.ClientSecret != "issued-secret" {
		t.Errorf("client = %+v", client)
	}
	if client.TokenURL != "https://as.example.invalid/token" {
		t.Errorf("token URL = %q", client.TokenURL)
	}
	if client.AuthStyle != int(oauth2.AuthStyleInHeader) {
		t.Errorf("auth style = %d", client.AuthStyle)
	}
	if len(client.Scopes) != 2 {
		t.Errorf("scopes = %v", client.Scopes)
	}
}

func TestStoredRegistrationIsOfferedInsteadOfRegisteringAgain(t *testing.T) {
	memory := &memStore{clients: map[string]*store.OAuthClient{"notion": {
		ClientID: "registered-client", ClientSecret: "registered-secret",
		TokenURL: "https://as.example.invalid/token",
	}}}
	o, err := NewOAuth(OAuthOptions{Store: memory, BaseURL: "https://riverbed.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Handler(config.MCP{
		Name: "notion", URL: "https://mcp.example.invalid/mcp", Transport: "streamable", Auth: "oauth",
	}); err != nil {
		t.Fatal(err)
	}

	// The handler keeps its configuration private, so the conversion is checked
	// directly: it is what decides whether the SDK registers again.
	credentials := preregistered(memory.clients["notion"])
	if credentials.ClientID != "registered-client" {
		t.Errorf("client id = %q", credentials.ClientID)
	}
	if credentials.ClientSecretAuth == nil || credentials.ClientSecretAuth.ClientSecret != "registered-secret" {
		t.Errorf("secret auth = %+v", credentials.ClientSecretAuth)
	}
	if credentials.Issuer != "" {
		t.Error("issuer must stay empty; the SDK does not report it, so it cannot be stored")
	}
	if err := credentials.Validate(); err != nil {
		t.Errorf("the SDK must accept these credentials: %v", err)
	}
}

func TestPublicClientHasNoSecretAuth(t *testing.T) {
	credentials := preregistered(&store.OAuthClient{ClientID: "public-client"})
	if credentials.ClientSecretAuth != nil {
		t.Error("a client with no secret is public, so secret auth must be absent")
	}
	if err := credentials.Validate(); err != nil {
		t.Errorf("a public client must validate: %v", err)
	}
}
