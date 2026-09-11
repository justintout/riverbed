package mcpserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/justintout/riverbed/store"
)

// keywordEmbedder gives "lights" one direction and everything else another, so
// semantic matching can be asserted without a model.
type keywordEmbedder struct{}

func (keywordEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if strings.Contains(strings.ToLower(text), "light") ||
		strings.Contains(strings.ToLower(text), "lamp") ||
		strings.Contains(strings.ToLower(text), "illuminat") {
		return []float32{1, 0, 0}, nil
	}
	return []float32{0, 1, 0}, nil
}

func openStore(t *testing.T, dim int) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), store.Options{
		Path: filepath.Join(t.TempDir(), "riverbed.db"), PoolSize: 4,
		EmbedModel: "test", EmbedDim: dim,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// client connects to the server over HTTP with the given token.
func client(t *testing.T, s *Server, token string) *mcp.ClientSession {
	t.Helper()
	httpSrv := httptest.NewServer(s)
	t.Cleanup(httpSrv.Close)

	transport := &mcp.StreamableClientTransport{
		Endpoint: httpSrv.URL,
		HTTPClient: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			if token != "" {
				clone.Header.Set("Authorization", "Bearer "+token)
			}
			return http.DefaultTransport.RoundTrip(clone)
		})},
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func callText(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	if res.IsError {
		t.Logf("tool reported an error: %s", b.String())
	}
	return b.String()
}

func insert(t *testing.T, s *store.Store, text string, at time.Time) int64 {
	t.Helper()
	id, err := s.Insert(t.Context(), store.NewRecording{
		Client: "ring", RecordedAt: at, Transcription: text,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestToolsAreAdvertised(t *testing.T) {
	s, err := New(Options{Store: openStore(t, 0), Token: "secret", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	session := client(t, s, "secret")

	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"search_journal", "recent_notes"} {
		if !names[want] {
			t.Errorf("missing tool %q, got %v", want, names)
		}
	}
}

func TestUnauthorizedIsRejected(t *testing.T) {
	s, err := New(Options{Store: openStore(t, 0), Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	for _, auth := range []string{"", "Bearer wrong"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpSrv.URL, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("authorization %q: status = %d, want 401", auth, resp.StatusCode)
		}
	}
}

func TestSearchJournal(t *testing.T) {
	store0 := openStore(t, 0)
	insert(t, store0, "turn on the kitchen lights", time.Now())
	insert(t, store0, "remember to renew my passport", time.Now())

	s, err := New(Options{Store: store0, Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	session := client(t, s, "secret")

	got := callText(t, session, "search_journal", map[string]any{"query": "kitchen"})
	if !strings.Contains(got, "kitchen lights") {
		t.Errorf("result = %q", got)
	}
	if strings.Contains(got, "passport") {
		t.Errorf("the unrelated note should not match: %q", got)
	}
}

func TestSearchJournalSemantic(t *testing.T) {
	st := openStore(t, 3)
	ctx := t.Context()
	lights := insert(t, st, "switch the lamp on in the kitchen", time.Now())
	if err := st.PutEmbeddings(ctx, lights,
		[]string{"switch the lamp on in the kitchen"}, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	passport := insert(t, st, "remember to renew my passport", time.Now())
	if err := st.PutEmbeddings(ctx, passport,
		[]string{"remember to renew my passport"}, [][]float32{{0, 1, 0}}); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{Store: st, Token: "secret", Embedder: keywordEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	session := client(t, s, "secret")

	// "illumination" shares no words with the note, so only semantic matching
	// can find it.
	got := callText(t, session, "search_journal", map[string]any{"query": "illumination"})
	if !strings.Contains(got, "lamp") {
		t.Errorf("semantic search should find the lamp note, got %q", got)
	}
}

func TestSearchFilters(t *testing.T) {
	st := openStore(t, 0)
	ctx := t.Context()
	insert(t, st, "an ancient thought", time.Now().Add(-72*time.Hour))
	recent := insert(t, st, "a recent thought", time.Now())
	if err := st.SetRoute(ctx, recent, "shelley", "rule", "a recent thought"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTags(ctx, recent, []string{"idea"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.AddToolCall(ctx, recent, store.ToolCall{
		Server: "home", Tool: "turn_on", StartedAt: now, EndedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{Store: st, Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	session := client(t, s, "secret")

	if got := callText(t, session, "search_journal", map[string]any{
		"query": "thought", "since": "24h",
	}); strings.Contains(got, "ancient") {
		t.Errorf("the since filter did not apply: %q", got)
	}
	if got := callText(t, session, "search_journal", map[string]any{
		"query": "thought", "since": "7d",
	}); !strings.Contains(got, "ancient") {
		t.Errorf("a day suffix should be understood: %q", got)
	}
	if got := callText(t, session, "search_journal", map[string]any{
		"query": "thought", "tag": "idea",
	}); strings.Contains(got, "ancient") {
		t.Errorf("the tag filter did not apply: %q", got)
	}
	if got := callText(t, session, "search_journal", map[string]any{
		"query": "thought", "route": "shelley",
	}); strings.Contains(got, "ancient") {
		t.Errorf("the route filter did not apply: %q", got)
	}
	if got := callText(t, session, "search_journal", map[string]any{
		"query": "thought", "tool_used": "yes",
	}); strings.Contains(got, "ancient") {
		t.Errorf("the tool filter did not apply: %q", got)
	}
	if got := callText(t, session, "search_journal", map[string]any{
		"query": "thought", "tool_used": "no",
	}); !strings.Contains(got, "ancient") {
		t.Errorf("tool_used no should return the untouched note: %q", got)
	}
}

func TestSearchRejectsBadArguments(t *testing.T) {
	s, err := New(Options{Store: openStore(t, 0), Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	session := client(t, s, "secret")

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "search_journal",
		Arguments: map[string]any{"query": "x", "since": "last tuesday"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("an unreadable duration should be reported to the model")
	}

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "search_journal",
		Arguments: map[string]any{"query": "x", "tool_used": "perhaps"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("an unreadable tool_used should be reported to the model")
	}
}

func TestRecentNotes(t *testing.T) {
	st := openStore(t, 0)
	insert(t, st, "older thought", time.Now().Add(-time.Hour))
	insert(t, st, "newer thought", time.Now())

	s, err := New(Options{Store: st, Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	session := client(t, s, "secret")

	got := callText(t, session, "recent_notes", map[string]any{"limit": 5})
	if !strings.Contains(got, "newer thought") || !strings.Contains(got, "older thought") {
		t.Errorf("result = %q", got)
	}
	if strings.Index(got, "newer") > strings.Index(got, "older") {
		t.Errorf("the newest note should come first:\n%s", got)
	}
}

func TestEmptyResult(t *testing.T) {
	s, err := New(Options{Store: openStore(t, 0), Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	session := client(t, s, "secret")
	if got := callText(t, session, "search_journal", map[string]any{"query": "nothing"}); !strings.Contains(got, "No notes") {
		t.Errorf("result = %q", got)
	}
}

func TestLimitIsClamped(t *testing.T) {
	if clampLimit(0) != 10 {
		t.Error("no limit should mean 10")
	}
	if clampLimit(1000) != maxLimit {
		t.Errorf("a large limit should be clamped to %d", maxLimit)
	}
	if clampLimit(5) != 5 {
		t.Error("a sensible limit should pass through")
	}
}

func TestParseDuration(t *testing.T) {
	if d, err := parseDuration("7d"); err != nil || d != 7*24*time.Hour {
		t.Errorf("7d = %v, %v", d, err)
	}
	if d, err := parseDuration("90m"); err != nil || d != 90*time.Minute {
		t.Errorf("90m = %v, %v", d, err)
	}
	if _, err := parseDuration("whenever"); err == nil {
		t.Error("want an error for unparseable text")
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{Token: "t"}); err == nil {
		t.Error("want an error without a store")
	}
	if _, err := New(Options{Store: openStore(t, 0)}); err == nil {
		t.Error("want an error without a token")
	}
}
