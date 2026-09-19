package ui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/store"
)

func newHandler(t *testing.T, password string) (*Handler, *store.Store) {
	t.Helper()
	s, err := store.Open(t.Context(), store.Options{
		Path: filepath.Join(t.TempDir(), "riverbed.db"), PoolSize: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cfg := config.Default()
	cfg.UI = config.UI{Enabled: true, Password: password}
	h, err := New(Options{Store: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return h, s
}

func do(h http.Handler, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPasswordGuardsTheAPI(t *testing.T) {
	h, _ := newHandler(t, "hunter2")

	if got := do(h, "GET", "/api/notes", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("without a session: got %d, want 401", got)
	}
	if got := do(h, "GET", "/", "").Code; got != http.StatusOK {
		t.Fatalf("the page must load so it can show the login form: got %d", got)
	}
	if got := do(h, "POST", "/api/login", `{"password":"wrong"}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", got)
	}

	login := do(h, "POST", "/api/login", `{"password":"hunter2"}`)
	if login.Code != http.StatusNoContent {
		t.Fatalf("login: got %d", login.Code)
	}
	session := login.Result().Cookies()[0]
	if got := do(h, "GET", "/api/notes", "", session).Code; got != http.StatusOK {
		t.Fatalf("with a session: got %d, want 200", got)
	}

	do(h, "POST", "/api/logout", "", session)
	if got := do(h, "GET", "/api/notes", "", session).Code; got != http.StatusUnauthorized {
		t.Fatalf("after logout: got %d, want 401", got)
	}
}

func TestNoPasswordLeavesTheAPIOpen(t *testing.T) {
	h, _ := newHandler(t, "")
	if got := do(h, "GET", "/api/notes", "").Code; got != http.StatusOK {
		t.Fatalf("got %d, want 200", got)
	}
}

func TestManageANote(t *testing.T) {
	h, s := newHandler(t, "")
	id, err := s.Insert(t.Context(), store.NewRecording{
		Client: "device", RecordedAt: time.Now(), Transcription: "buy oat milk",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/notes/" + strconv.FormatInt(id, 10)

	if got := do(h, "POST", path+"/tags", `{"tag":"errand"}`).Code; got != http.StatusNoContent {
		t.Fatalf("add tag: got %d", got)
	}
	if body := do(h, "GET", "/api/notes?tag=errand&q=milk", "").Body.String(); !strings.Contains(body, "oat milk") {
		t.Fatalf("search by tag and text did not find the note: %s", body)
	}
	if got := do(h, "DELETE", path+"/tags/errand", "").Code; got != http.StatusNoContent {
		t.Fatalf("remove tag: got %d", got)
	}

	if got := do(h, "POST", path+"/requeue", "").Code; got != http.StatusConflict {
		t.Fatalf("requeue of a note that has not failed: got %d, want 409", got)
	}
	if err := s.Finish(t.Context(), id, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if got := do(h, "POST", path+"/requeue", "").Code; got != http.StatusNoContent {
		t.Fatalf("requeue of a failed note: got %d", got)
	}
	if rec, _ := s.Get(t.Context(), id); rec.Status != store.StatusPending {
		t.Fatalf("status after requeue: %s", rec.Status)
	}

	if got := do(h, "DELETE", path, "").Code; got != http.StatusNoContent {
		t.Fatalf("delete: got %d", got)
	}
	if got := do(h, "GET", path, "").Code; got != http.StatusNotFound {
		t.Fatalf("get after delete: got %d, want 404", got)
	}
}

func TestStatusOmitsSecrets(t *testing.T) {
	h, _ := newHandler(t, "")
	h.opts.Config.Webhook.Token = "webhook-secret"
	h.opts.Config.Agents = []config.Agent{{Name: "claude", Kind: "claude", APIKey: "sk-secret"}}
	h.opts.Config.MCP = []config.MCP{{Name: "ha", URL: "http://ha", Token: "mcp-secret"}}

	body := do(h, "GET", "/api/status", "").Body.String()
	for _, secret := range []string{"webhook-secret", "sk-secret", "mcp-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("status leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, `"claude"`) {
		t.Fatalf("status lacks the agent: %s", body)
	}
}
