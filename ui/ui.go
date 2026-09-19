// Package ui serves Riverbed's web interface: a JSON API over the journal and a
// single embedded page that uses it.
package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/embedding"
	"github.com/justintout/riverbed/store"
)

//go:embed index.html
var page []byte

const (
	cookieName = "riverbed_session"
	sessionTTL = 30 * 24 * time.Hour
	// failDelay slows password guessing without needing per-client state.
	failDelay = time.Second
	maxLimit  = 200
)

// Options configures the interface.
type Options struct {
	Store    *store.Store
	Embedder embedding.Embedder
	Config   config.Config
	Version  string
	// Notify wakes a worker after a recording is queued.
	Notify func()
	// Embed re-embeds one recording after its text changed. Nil when embedding
	// is off.
	Embed func(ctx context.Context, id int64) error
	// ConfigPath is the file the configuration page edits. Empty disables it.
	ConfigPath string
	Logger     *slog.Logger
}

// Handler serves the interface. Mount it at "GET /{$}" and "/api/".
type Handler struct {
	opts Options
	mux  *http.ServeMux
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[string]time.Time
}

// New builds the handler.
func New(opts Options) (*Handler, error) {
	if opts.Store == nil {
		return nil, errors.New("ui: store is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	h := &Handler{opts: opts, mux: http.NewServeMux(), log: opts.Logger, sessions: map[string]time.Time{}}

	h.mux.HandleFunc("GET /{$}", h.index)
	h.mux.HandleFunc("POST /api/login", h.login)
	h.mux.HandleFunc("POST /api/logout", h.logout)
	h.mux.HandleFunc("GET /api/status", h.guard(h.status))
	h.mux.HandleFunc("GET /api/notes", h.guard(h.list))
	h.mux.HandleFunc("POST /api/notes", h.guard(h.create))
	h.mux.HandleFunc("GET /api/notes/{id}", h.guard(h.get))
	h.mux.HandleFunc("PUT /api/notes/{id}", h.guard(h.edit))
	h.mux.HandleFunc("GET /api/notes/{id}/audio", h.guard(h.audio))
	h.mux.HandleFunc("DELETE /api/notes/{id}", h.guard(h.remove))
	h.mux.HandleFunc("POST /api/notes/{id}/replay", h.guard(h.replay))
	h.mux.HandleFunc("POST /api/notes/{id}/tags", h.guard(h.addTag))
	h.mux.HandleFunc("DELETE /api/notes/{id}/tags/{tag}", h.guard(h.removeTag))
	h.mux.HandleFunc("GET /api/config", h.guard(h.getConfig))
	h.mux.HandleFunc("PUT /api/config", h.guard(h.putConfig))
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// Authentication.

func (h *Handler) authRequired() bool { return h.opts.Config.UI.Password != "" }

func (h *Handler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authRequired() && !h.validSession(r) {
			fail(w, http.StatusUnauthorized, "login required")
			return
		}
		next(w, r)
	}
}

func (h *Handler) validSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	expiry, ok := h.sessions[c.Value]
	if ok && time.Now().After(expiry) {
		delete(h.sessions, c.Value)
		return false
	}
	return ok
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	want := []byte(h.opts.Config.UI.Password)
	if len(want) == 0 || subtle.ConstantTimeCompare([]byte(body.Password), want) != 1 {
		select {
		case <-time.After(failDelay):
		case <-r.Context().Done():
			return
		}
		fail(w, http.StatusUnauthorized, "wrong password")
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fail(w, http.StatusInternalServerError, "create session")
		return
	}
	token := hex.EncodeToString(raw)
	h.mu.Lock()
	now := time.Now()
	for t, expiry := range h.sessions {
		if now.After(expiry) {
			delete(h.sessions, t)
		}
	}
	h.sessions[token] = now.Add(sessionTTL)
	h.mu.Unlock()

	http.SetCookie(w, h.cookie(r, token, int(sessionTTL.Seconds())))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		h.mu.Lock()
		delete(h.sessions, c.Value)
		h.mu.Unlock()
	}
	http.SetCookie(w, h.cookie(r, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

// cookie uses SameSite=Strict as the CSRF defence, since every state change is
// a non-GET request.
func (h *Handler) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	}
}

// Notes.

type noteJSON struct {
	ID         int64     `json:"id"`
	RecordedAt time.Time `json:"recorded_at"`
	Status     string    `json:"status"`
	Route      string    `json:"route,omitempty"`
	Tags       []string  `json:"tags"`
	Text       string    `json:"text"`
	Snippet    string    `json:"snippet,omitempty"`
}

type responseJSON struct {
	Agent        string `json:"agent"`
	Text         string `json:"text"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

type toolCallJSON struct {
	Server    string    `json:"server"`
	Tool      string    `json:"tool"`
	Arguments string    `json:"arguments,omitempty"`
	Result    string    `json:"result,omitempty"`
	IsError   bool      `json:"is_error"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

type detailJSON struct {
	noteJSON
	Client      string         `json:"client"`
	ReceivedAt  time.Time      `json:"received_at"`
	RouteReason string         `json:"route_reason,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	Error       string         `json:"error,omitempty"`
	Attempts    int            `json:"attempts"`
	AudioBytes  int64          `json:"audio_bytes"`
	Responses   []responseJSON `json:"responses"`
	ToolCalls   []toolCallJSON `json:"tool_calls"`
}

func note(rec store.Recording, tags []string) noteJSON {
	if tags == nil {
		tags = []string{}
	}
	return noteJSON{
		ID: rec.ID, RecordedAt: rec.RecordedAt, Status: rec.Status,
		Route: rec.Route, Tags: tags, Text: rec.Transcription,
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query()
	q := store.Query{
		Text:   strings.TrimSpace(p.Get("q")),
		Route:  p.Get("route"),
		Status: p.Get("status"),
		Limit:  20,
	}
	if tag := p.Get("tag"); tag != "" {
		q.Tags = []string{tag}
	}
	if v := p.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		q.Limit = min(n, maxLimit)
	}
	if v := p.Get("since_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, "since_days must be a positive integer")
			return
		}
		q.Since = time.Now().AddDate(0, 0, -n)
	}

	if h.opts.Embedder != nil && h.opts.Store.VectorEnabled() && q.Text != "" {
		vector, err := h.opts.Embedder.Embed(r.Context(), q.Text)
		if err != nil {
			h.log.Error("embed search query", "error", err)
			fail(w, http.StatusBadGateway, "embed the query: "+err.Error())
			return
		}
		q.Vector = vector
	}

	results, err := h.opts.Store.Search(r.Context(), q)
	if err != nil {
		h.serverError(w, "search", err)
		return
	}
	out := make([]noteJSON, len(results))
	for i, res := range results {
		out[i] = note(res.Recording, res.Tags)
		out[i].Snippet = res.Snippet
	}
	respond(w, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	rec, err := h.opts.Store.Get(ctx, id)
	if err != nil {
		h.storeError(w, "get note", err)
		return
	}
	tags, err := h.opts.Store.Tags(ctx, id)
	if err != nil {
		h.serverError(w, "get tags", err)
		return
	}
	responses, err := h.opts.Store.Responses(ctx, id)
	if err != nil {
		h.serverError(w, "get responses", err)
		return
	}
	calls, err := h.opts.Store.ToolCalls(ctx, id)
	if err != nil {
		h.serverError(w, "get tool calls", err)
		return
	}

	d := detailJSON{
		noteJSON: note(rec, tags), Client: rec.Client, ReceivedAt: rec.ReceivedAt,
		RouteReason: rec.RouteReason, Prompt: rec.Prompt, Error: rec.Error,
		Attempts: rec.Attempts, AudioBytes: rec.AudioBytes,
		Responses: []responseJSON{}, ToolCalls: []toolCallJSON{},
	}
	for _, x := range responses {
		d.Responses = append(d.Responses, responseJSON(x))
	}
	for _, c := range calls {
		d.ToolCalls = append(d.ToolCalls, toolCallJSON(c))
	}
	respond(w, d)
}

func (h *Handler) audio(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rec, err := h.opts.Store.Get(r.Context(), id)
	if err != nil {
		h.storeError(w, "get note", err)
		return
	}
	data, err := h.opts.Store.Audio(r.Context(), id)
	if err != nil {
		h.storeError(w, "get audio", err)
		return
	}
	if rec.AudioMIME != "" {
		w.Header().Set("Content-Type", rec.AudioMIME)
	}
	// ServeContent answers range requests, which Safari needs to play audio.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.opts.Store.Delete(r.Context(), id); err != nil {
		h.storeError(w, "delete note", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// create stores a note typed in the browser. It has no audio, and it goes
// through the pipeline like a spoken one, so it is routed and embedded.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		fail(w, http.StatusBadRequest, "text is required")
		return
	}
	id, err := h.opts.Store.Insert(r.Context(), store.NewRecording{
		Client: "web", RecordedAt: time.Now(), Transcription: text,
	})
	if err != nil {
		h.serverError(w, "create note", err)
		return
	}
	h.notify()
	w.WriteHeader(http.StatusCreated)
	respond(w, map[string]int64{"id": id})
}

// edit replaces a note's text. With replay set the note is queued again, and
// the pipeline re-embeds it. Without, the embeddings are rebuilt here.
func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Text   string `json:"text"`
		Replay bool   `json:"replay"`
	}
	if !decode(w, r, &body) {
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		fail(w, http.StatusBadRequest, "text is required")
		return
	}
	rec, err := h.opts.Store.Get(r.Context(), id)
	if err != nil {
		h.storeError(w, "get note", err)
		return
	}
	if body.Replay && !replayable(rec) {
		fail(w, http.StatusConflict, "this note is still being processed")
		return
	}
	if err := h.opts.Store.SetTranscription(r.Context(), id, text); err != nil {
		h.storeError(w, "edit note", err)
		return
	}
	switch {
	case body.Replay:
		if err := h.opts.Store.Replay(r.Context(), id); err != nil {
			h.serverError(w, "replay", err)
			return
		}
		h.notify()
	case h.opts.Embed != nil:
		if err := h.opts.Embed(r.Context(), id); err != nil {
			h.serverError(w, "embed the edited note", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// replayable reports whether a note is idle. Replaying one a worker holds would
// run it twice at once.
func replayable(rec store.Recording) bool {
	return rec.Status == store.StatusDone || rec.Status == store.StatusFailed
}

// replay runs a finished or failed note through routing and its agent again.
func (h *Handler) replay(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rec, err := h.opts.Store.Get(r.Context(), id)
	if err != nil {
		h.storeError(w, "get note", err)
		return
	}
	if !replayable(rec) {
		fail(w, http.StatusConflict, "this note is still being processed")
		return
	}
	if err := h.opts.Store.Replay(r.Context(), id); err != nil {
		h.serverError(w, "replay", err)
		return
	}
	h.notify()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) notify() {
	if h.opts.Notify != nil {
		h.opts.Notify()
	}
}

func (h *Handler) addTag(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if !decode(w, r, &body) {
		return
	}
	tag := strings.TrimSpace(body.Tag)
	if tag == "" {
		fail(w, http.StatusBadRequest, "tag is required")
		return
	}
	if _, err := h.opts.Store.Get(r.Context(), id); err != nil {
		h.storeError(w, "get note", err)
		return
	}
	if err := h.opts.Store.AddTags(r.Context(), id, []string{tag}); err != nil {
		h.serverError(w, "add tag", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeTag(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.opts.Store.RemoveTag(r.Context(), id, r.PathValue("tag")); err != nil {
		h.serverError(w, "remove tag", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Configuration.

// configBlocked explains why the configuration cannot be edited here, or returns
// "". Editing needs a password because the file decides where credentials are
// sent, so an open listener must not offer it.
func (h *Handler) configBlocked() string {
	switch {
	case h.opts.ConfigPath == "":
		return "Riverbed was started without a configuration file."
	case !h.authRequired():
		return "Set ui.password to edit the configuration from the browser."
	}
	return ""
}

func (h *Handler) getConfig(w http.ResponseWriter, _ *http.Request) {
	if reason := h.configBlocked(); reason != "" {
		respond(w, map[string]any{"editable": false, "reason": reason})
		return
	}
	data, err := os.ReadFile(h.opts.ConfigPath)
	if err != nil {
		h.serverError(w, "read the configuration", err)
		return
	}
	respond(w, map[string]any{"editable": true, "path": h.opts.ConfigPath, "text": string(data)})
}

// putConfig validates the text as Riverbed would at startup, then replaces the
// file. The running system is not changed: it reads the file only when it starts.
func (h *Handler) putConfig(w http.ResponseWriter, r *http.Request) {
	if reason := h.configBlocked(); reason != "" {
		fail(w, http.StatusForbidden, reason)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "request body must be JSON")
		return
	}
	if _, err := config.Parse(body.Text); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := writeFile(h.opts.ConfigPath, []byte(body.Text)); err != nil {
		h.serverError(w, "write the configuration", err)
		return
	}
	h.log.Info("configuration saved from the web interface, restart to apply", "path", h.opts.ConfigPath)
	w.WriteHeader(http.StatusNoContent)
}

// writeFile replaces path through a temporary file, so a crash cannot leave a
// half-written configuration.
func writeFile(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".riverbed-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Status.

type statusJSON struct {
	Version   string         `json:"version"`
	Auth      bool           `json:"auth"`
	Counts    map[string]int `json:"counts"`
	Embedding embeddingJSON  `json:"embedding"`
	Audio     bool           `json:"audio_retained"`
	MCPServe  bool           `json:"mcp_serve"`
	Router    routerJSON     `json:"router"`
	Agents    []agentJSON    `json:"agents"`
	MCP       []mcpJSON      `json:"mcp"`
}

type embeddingJSON struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind,omitempty"`
	Model   string `json:"model,omitempty"`
}

type routerJSON struct {
	Default    string     `json:"default"`
	Classifier string     `json:"classifier,omitempty"`
	Rules      []ruleJSON `json:"rules"`
}

type ruleJSON struct {
	Match string   `json:"match"`
	Agent string   `json:"agent"`
	Strip bool     `json:"strip"`
	Tags  []string `json:"tags"`
}

type agentJSON struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Model string `json:"model,omitempty"`
}

type mcpJSON struct {
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Transport string   `json:"transport"`
	Auth      string   `json:"auth"`
	Agents    []string `json:"agents"`
}

// status reports the running configuration. It copies fields one at a time so
// that a secret added to the configuration later is not exposed by accident.
func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	counts, err := h.opts.Store.CountByStatus(r.Context())
	if err != nil {
		h.serverError(w, "count notes", err)
		return
	}
	cfg := h.opts.Config
	s := statusJSON{
		Version:   h.opts.Version,
		Auth:      h.authRequired(),
		Counts:    counts,
		Embedding: embeddingJSON{Enabled: cfg.Embedding.Enabled(), Kind: cfg.Embedding.Kind, Model: cfg.Embedding.Model},
		Audio:     cfg.Audio.Retain,
		MCPServe:  cfg.MCPServe.Enabled,
		Router:    routerJSON{Default: cfg.Router.Default, Classifier: cfg.Router.Classifier, Rules: []ruleJSON{}},
		Agents:    []agentJSON{},
		MCP:       []mcpJSON{},
	}
	for _, rule := range cfg.Router.Rules {
		var match string
		switch {
		case rule.Prefix != "":
			match = "prefix " + strconv.Quote(rule.Prefix)
		case rule.Regex != "":
			match = "regex " + rule.Regex
		default:
			match = fmt.Sprintf("meaning of %d example phrases", len(rule.Utterances))
		}
		tags := rule.Tags
		if tags == nil {
			tags = []string{}
		}
		s.Router.Rules = append(s.Router.Rules, ruleJSON{Match: match, Agent: rule.Agent, Strip: rule.Strip, Tags: tags})
	}
	for _, a := range cfg.Agents {
		s.Agents = append(s.Agents, agentJSON{Name: a.Name, Kind: a.Kind, Model: a.Model})
	}
	for _, m := range cfg.MCP {
		agents := m.Agents
		if agents == nil {
			agents = []string{}
		}
		s.MCP = append(s.MCP, mcpJSON{Name: m.Name, URL: m.URL, Transport: m.Transport, Auth: m.Auth, Agents: agents})
	}
	respond(w, s)
}

// Helpers.

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		fail(w, http.StatusBadRequest, "note id must be a positive integer")
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "request body must be JSON")
		return false
	}
	return true
}

func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (h *Handler) storeError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	h.serverError(w, what, err)
}

func (h *Handler) serverError(w http.ResponseWriter, what string, err error) {
	h.log.Error("ui: "+what, "error", err)
	fail(w, http.StatusInternalServerError, what+" failed")
}
