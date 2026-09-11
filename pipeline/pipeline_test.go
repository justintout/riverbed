package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/justintout/riverbed/agent"
	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/embedding"
	"github.com/justintout/riverbed/route"
	"github.com/justintout/riverbed/store"
	"github.com/justintout/riverbed/tool"
)

// fakeAgent records prompts and answers with a fixed reply.
type fakeAgent struct {
	name  string
	reply string
	err   error
	calls []agent.Call

	mu      sync.Mutex
	prompts []string
	tools   []tool.Tool
}

func (f *fakeAgent) Name() string { return f.name }

func (f *fakeAgent) Run(_ context.Context, req agent.Request) (*agent.Response, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, req.Prompt)
	f.tools = req.Tools
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &agent.Response{Text: f.reply, Calls: f.calls, InputTokens: 7, OutputTokens: 3}, nil
}

func (f *fakeAgent) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

// fixedEmbedder returns a constant vector, so storage can be asserted without a
// model.
type fixedEmbedder struct {
	err  error
	dim  int
	seen []string
	mu   sync.Mutex
}

func (f *fixedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	f.seen = append(f.seen, text)
	f.mu.Unlock()
	v := make([]float32, f.dim)
	v[0] = 1
	return v, nil
}

func openStore(t *testing.T, dim int) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), store.Options{
		Path:       filepath.Join(t.TempDir(), "riverbed.db"),
		PoolSize:   4,
		EmbedModel: "test",
		EmbedDim:   dim,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newRouter(t *testing.T, cfg config.Router) *route.Router {
	t.Helper()
	r, err := route.New(route.Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// runOnce drains the queue by running the pipeline until the recording is no
// longer pending, then stops it.
func runOnce(t *testing.T, p *Pipeline, s *store.Store, id int64) store.Recording {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	p.Notify()

	deadline := time.After(10 * time.Second)
	for {
		rec, err := s.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status == store.StatusDone || rec.Status == store.StatusFailed {
			cancel()
			<-done
			return rec
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("recording %d never finished, status %q", id, rec.Status)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func insert(t *testing.T, s *store.Store, text string) int64 {
	t.Helper()
	id, err := s.Insert(t.Context(), store.NewRecording{
		Client: "ring", RecordedAt: time.Now(), Transcription: text,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestJournaledRecordingCallsNoAgent(t *testing.T) {
	s := openStore(t, 0)
	a := &fakeAgent{name: "shelley", reply: "done"}
	p, err := New(Options{
		Store:  s,
		Router: newRouter(t, config.Router{Default: config.JournalAgent}),
		Agents: map[string]agent.Agent{"shelley": a},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := insert(t, s, "a passing thought")
	rec := runOnce(t, p, s, id)

	if rec.Status != store.StatusDone {
		t.Errorf("status = %q, error = %q", rec.Status, rec.Error)
	}
	if rec.Route != config.JournalAgent {
		t.Errorf("route = %q", rec.Route)
	}
	if a.promptCount() != 0 {
		t.Error("the journal route must not call an agent")
	}
}

func TestRoutedRecordingRunsTheAgent(t *testing.T) {
	s := openStore(t, 0)
	a := &fakeAgent{name: "shelley", reply: "Calendar is clear."}
	p, err := New(Options{
		Store: s,
		Router: newRouter(t, config.Router{
			Default: config.JournalAgent,
			Rules:   []config.Rule{{Prefix: "hey shelley", Agent: "shelley", Strip: true, Tags: []string{"shelley"}}},
		}),
		Agents: map[string]agent.Agent{"shelley": a},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := insert(t, s, "Hey Shelley, what's on my calendar?")
	rec := runOnce(t, p, s, id)

	if rec.Status != store.StatusDone {
		t.Fatalf("status = %q, error = %q", rec.Status, rec.Error)
	}
	if rec.Route != "shelley" {
		t.Errorf("route = %q", rec.Route)
	}
	if rec.Prompt != "what's on my calendar?" {
		t.Errorf("prompt = %q, want the prefix stripped", rec.Prompt)
	}

	if a.promptCount() != 1 {
		t.Fatalf("the agent ran %d times", a.promptCount())
	}
	if a.prompts[0] != "what's on my calendar?" {
		t.Errorf("the agent saw %q", a.prompts[0])
	}

	responses, err := s.Responses(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(responses) != 1 || responses[0].Text != "Calendar is clear." {
		t.Errorf("responses = %+v", responses)
	}
	if responses[0].InputTokens != 7 || responses[0].OutputTokens != 3 {
		t.Errorf("usage was not stored: %+v", responses[0])
	}

	tags, err := s.Tags(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0] != "shelley" {
		t.Errorf("tags = %v", tags)
	}
}

func TestToolCallsAreStored(t *testing.T) {
	s := openStore(t, 0)
	now := time.Now().Truncate(time.Millisecond)
	a := &fakeAgent{
		name:  "home",
		reply: "Kitchen light is on.",
		calls: []agent.Call{{
			Name: "homeassistant.turn_on", Server: "homeassistant", Tool: "turn_on",
			Arguments: `{"entity":"light.kitchen"}`, Result: "ok",
			StartedAt: now, EndedAt: now.Add(time.Second),
		}},
	}
	p, err := New(Options{
		Store: s,
		Router: newRouter(t, config.Router{
			Default: config.JournalAgent,
			Rules:   []config.Rule{{Regex: "^turn ", Agent: "home"}},
		}),
		Agents: map[string]agent.Agent{"home": a},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := insert(t, s, "turn on the kitchen light")
	if rec := runOnce(t, p, s, id); rec.Status != store.StatusDone {
		t.Fatalf("status = %q, error = %q", rec.Status, rec.Error)
	}

	calls, err := s.ToolCalls(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Server != "homeassistant" || calls[0].Tool != "turn_on" {
		t.Errorf("call = %+v", calls[0])
	}

	// "a tool was used" must now be queryable.
	used := true
	got, err := s.Search(t.Context(), store.Query{Text: "kitchen", ToolUsed: &used})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != id {
		t.Errorf("tool-used search returned %+v", got)
	}
}

func TestEmbeddingsAreStored(t *testing.T) {
	s := openStore(t, 8)
	e := &fixedEmbedder{dim: 8}
	p, err := New(Options{
		Store:    s,
		Router:   newRouter(t, config.Router{Default: config.JournalAgent}),
		Embedder: e,
	})
	if err != nil {
		t.Fatal(err)
	}

	id := insert(t, s, "a thought worth finding later")
	if rec := runOnce(t, p, s, id); rec.Status != store.StatusDone {
		t.Fatalf("status = %q", rec.Status)
	}

	pending, err := s.PendingEmbedding(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("nothing should be pending, got %v", pending)
	}

	query := make([]float32, 8)
	query[0] = 1
	got, err := s.Search(t.Context(), store.Query{Vector: query})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != id {
		t.Errorf("vector search returned %+v", got)
	}
}

func TestEmbeddingFailureDoesNotFailTheRecording(t *testing.T) {
	s := openStore(t, 8)
	p, err := New(Options{
		Store:    s,
		Router:   newRouter(t, config.Router{Default: config.JournalAgent}),
		Embedder: &fixedEmbedder{dim: 8, err: errors.New("model is unavailable")},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := insert(t, s, "a thought")
	rec := runOnce(t, p, s, id)
	if rec.Status != store.StatusDone {
		t.Errorf("status = %q: losing retrieval quality must not lose the recording", rec.Status)
	}
}

func TestAgentFailureRetriesThenFails(t *testing.T) {
	s := openStore(t, 0)
	a := &fakeAgent{name: "shelley", err: errors.New("model is down")}
	p, err := New(Options{
		Store: s,
		Router: newRouter(t, config.Router{
			Default: "shelley",
		}),
		Agents:      map[string]agent.Agent{"shelley": a},
		MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	id := insert(t, s, "a request")
	rec := runOnce(t, p, s, id)

	if rec.Status != store.StatusFailed {
		t.Errorf("status = %q, want failed", rec.Status)
	}
	if rec.Error == "" {
		t.Error("the failure should be recorded")
	}
	if rec.Attempts != 2 {
		t.Errorf("attempts = %d, want the configured 2", rec.Attempts)
	}
	if a.promptCount() != 2 {
		t.Errorf("the agent was called %d times, want 2", a.promptCount())
	}
}

func TestUnconfiguredAgentFails(t *testing.T) {
	s := openStore(t, 0)
	p, err := New(Options{
		Store:       s,
		Router:      newRouter(t, config.Router{Default: "ghost"}),
		Agents:      map[string]agent.Agent{},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	id := insert(t, s, "a request")
	rec := runOnce(t, p, s, id)
	if rec.Status != store.StatusFailed {
		t.Errorf("status = %q", rec.Status)
	}
}

func TestToolsAreOfferedPerAgent(t *testing.T) {
	s := openStore(t, 0)
	a := &fakeAgent{name: "shelley", reply: "ok"}

	registry, err := tool.New(tool.Options{Servers: []config.MCP{
		{Name: "home", URL: "http://127.0.0.1:1/mcp", Transport: "streamable"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	asked := make(chan string, 1)
	p, err := New(Options{
		Store:  s,
		Router: newRouter(t, config.Router{Default: "shelley"}),
		Agents: map[string]agent.Agent{"shelley": a},
		Tools:  registry,
		ToolsFor: func(name string) []string {
			select {
			case asked <- name:
			default:
			}
			return []string{"home"}
		},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	id := insert(t, s, "a request")
	if rec := runOnce(t, p, s, id); rec.Status != store.StatusDone {
		t.Fatalf("status = %q, error = %q", rec.Status, rec.Error)
	}
	select {
	case name := <-asked:
		if name != "shelley" {
			t.Errorf("tools were resolved for %q", name)
		}
	default:
		t.Error("the pipeline should ask which servers the agent may use")
	}
}

func TestRecoversInterruptedRecordings(t *testing.T) {
	s := openStore(t, 0)
	id := insert(t, s, "interrupted")
	if _, err := s.Claim(t.Context()); err != nil {
		t.Fatal(err)
	}

	p, err := New(Options{
		Store:  s,
		Router: newRouter(t, config.Router{Default: config.JournalAgent}),
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := runOnce(t, p, s, id)
	if rec.Status != store.StatusDone {
		t.Errorf("status = %q: a restart should pick the work back up", rec.Status)
	}
}

func TestBackfill(t *testing.T) {
	s := openStore(t, 8)
	for i := 0; i < 3; i++ {
		insert(t, s, fmt.Sprintf("thought %d", i))
	}

	e := &fixedEmbedder{dim: 8}
	p, err := New(Options{
		Store:    s,
		Router:   newRouter(t, config.Router{Default: config.JournalAgent}),
		Embedder: e,
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := p.Backfill(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("backfilled %d, want 3", n)
	}
	pending, err := s.PendingEmbedding(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v", pending)
	}
}

func TestBackfillWithoutEmbedderDoesNothing(t *testing.T) {
	s := openStore(t, 0)
	insert(t, s, "a thought")
	p, err := New(Options{Store: s, Router: newRouter(t, config.Router{Default: config.JournalAgent})})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := p.Backfill(t.Context(), 10); err != nil || n != 0 {
		t.Errorf("n = %d, err = %v", n, err)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{Router: newRouter(t, config.Router{})}); err == nil {
		t.Error("want an error without a store")
	}
	if _, err := New(Options{Store: openStore(t, 0)}); err == nil {
		t.Error("want an error without a router")
	}
}

func TestAgentNamesIsStable(t *testing.T) {
	agents := map[string]agent.Agent{
		"zeta":  &fakeAgent{name: "zeta"},
		"alpha": &fakeAgent{name: "alpha"},
	}
	got := AgentNames(agents)
	want := []string{config.JournalAgent, "alpha", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("names = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("names = %v, want %v", got, want)
			break
		}
	}
}

func TestNotifyNeverBlocks(t *testing.T) {
	p, err := New(Options{
		Store:  openStore(t, 0),
		Router: newRouter(t, config.Router{Default: config.JournalAgent}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		p.Notify()
	}
}

func TestConcurrentWorkers(t *testing.T) {
	s := openStore(t, 0)
	const n = 12
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, insert(t, s, fmt.Sprintf("thought %d", i)))
	}

	p, err := New(Options{
		Store:   s,
		Router:  newRouter(t, config.Router{Default: config.JournalAgent}),
		Workers: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	p.Notify()

	deadline := time.After(15 * time.Second)
	for {
		remaining := 0
		for _, id := range ids {
			rec, err := s.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Status != store.StatusDone {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("%d recordings never finished", remaining)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

var _ embedding.Embedder = (*fixedEmbedder)(nil)
