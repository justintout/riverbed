package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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

	if got := do(h, "POST", path+"/replay", "").Code; got != http.StatusConflict {
		t.Fatalf("replay of a note a worker may hold: got %d, want 409", got)
	}
	if err := s.Finish(t.Context(), id, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if got := do(h, "POST", path+"/replay", "").Code; got != http.StatusNoContent {
		t.Fatalf("replay of a failed note: got %d", got)
	}
	if rec, _ := s.Get(t.Context(), id); rec.Status != store.StatusPending {
		t.Fatalf("status after replay: %s", rec.Status)
	}

	if got := do(h, "DELETE", path, "").Code; got != http.StatusNoContent {
		t.Fatalf("delete: got %d", got)
	}
	if got := do(h, "GET", path, "").Code; got != http.StatusNotFound {
		t.Fatalf("get after delete: got %d, want 404", got)
	}
}

func TestCreateAndEditNote(t *testing.T) {
	h, s := newHandler(t, "")

	created := do(h, "POST", "/api/notes", `{"text":"call the plumber"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: got %d: %s", created.Code, created.Body)
	}
	if got := do(h, "POST", "/api/notes", `{"text":"  "}`).Code; got != http.StatusBadRequest {
		t.Fatalf("empty note: got %d, want 400", got)
	}
	rec, err := s.Get(t.Context(), 1)
	if err != nil || rec.Client != "web" || rec.Status != store.StatusPending {
		t.Fatalf("stored note: %+v %v", rec, err)
	}

	// Editing without replay changes the text and leaves the status alone.
	if got := do(h, "PUT", "/api/notes/1", `{"text":"call the electrician"}`).Code; got != http.StatusNoContent {
		t.Fatalf("edit: got %d", got)
	}
	if body := do(h, "GET", "/api/notes?q=electrician", "").Body.String(); !strings.Contains(body, "electrician") {
		t.Fatalf("keyword index missed the edit: %s", body)
	}
	if body := do(h, "GET", "/api/notes?q=plumber", "").Body.String(); strings.Contains(body, "plumber") {
		t.Fatalf("keyword index kept the old text: %s", body)
	}

	// A note a worker may hold cannot be replayed, and the text stays as it was.
	if got := do(h, "PUT", "/api/notes/1", `{"text":"changed","replay":true}`).Code; got != http.StatusConflict {
		t.Fatalf("edit with replay while pending: got %d, want 409", got)
	}
	if rec, _ := s.Get(t.Context(), 1); rec.Transcription != "call the electrician" {
		t.Fatalf("a refused edit changed the text: %q", rec.Transcription)
	}

	if err := s.Finish(t.Context(), 1, nil); err != nil {
		t.Fatal(err)
	}
	if got := do(h, "PUT", "/api/notes/1", `{"text":"call the roofer","replay":true}`).Code; got != http.StatusNoContent {
		t.Fatalf("edit with replay: got %d", got)
	}
	if rec, _ := s.Get(t.Context(), 1); rec.Status != store.StatusPending || rec.Transcription != "call the roofer" {
		t.Fatalf("after edit with replay: %+v", rec)
	}
}

func TestAudioSupportsRanges(t *testing.T) {
	h, s := newHandler(t, "")
	id, err := s.Insert(t.Context(), store.NewRecording{
		Client: "device", RecordedAt: time.Now(), Transcription: "x",
		AudioMIME: "audio/wav", Audio: []byte("0123456789"),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/notes/"+strconv.FormatInt(id, 10)+"/audio", nil)
	req.Header.Set("Range", "bytes=2-4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" {
		t.Fatalf("got %d %q, want 206 \"234\"", rec.Code, rec.Body)
	}
}

func TestConfigEditing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "riverbed.toml")
	original := "[webhook]\ntoken = \"abc\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	save := func(h *Handler, text string) int {
		body, _ := json.Marshal(map[string]string{"text": text})
		return do(h, "PUT", "/api/config", string(body)).Code
	}

	open, _ := newHandler(t, "")
	open.opts.ConfigPath = path
	if got := save(open, original+"# x\n"); got != http.StatusForbidden {
		t.Fatalf("save without a password: got %d, want 403", got)
	}

	h, _ := newHandler(t, "pw")
	h.opts.ConfigPath = path
	session := do(h, "POST", "/api/login", `{"password":"pw"}`).Result().Cookies()[0]
	put := func(text string) int {
		body, _ := json.Marshal(map[string]string{"text": text})
		return do(h, "PUT", "/api/config", string(body), session).Code
	}

	if got := put("[webhook\n"); got != http.StatusUnprocessableEntity {
		t.Fatalf("invalid TOML: got %d, want 422", got)
	}
	if got := put("[webhook]\ntoken = \"${RIVERBED_UI_TEST_UNSET}\"\n"); got != http.StatusUnprocessableEntity {
		t.Fatalf("unset variable: got %d, want 422", got)
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatalf("a rejected save changed the file: %q", data)
	}

	updated := original + "# edited\n"
	if got := put(updated); got != http.StatusNoContent {
		t.Fatalf("valid save: got %d", got)
	}
	if data, _ := os.ReadFile(path); string(data) != updated {
		t.Fatalf("file after save: %q", data)
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
