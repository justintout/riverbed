package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// open returns a store backed by a temporary file. dim of 0 disables vectors.
func open(t *testing.T, dim int) *Store {
	t.Helper()
	s, err := Open(t.Context(), Options{
		Path:       filepath.Join(t.TempDir(), "riverbed.db"),
		PoolSize:   4,
		EmbedModel: "test-model",
		EmbedDim:   dim,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func insert(t *testing.T, s *Store, text string, at time.Time) int64 {
	t.Helper()
	id, err := s.Insert(t.Context(), NewRecording{
		Client:        "ring",
		RecordedAt:    at,
		Transcription: text,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "riverbed.db")
	for i := 0; i < 2; i++ {
		s, err := Open(t.Context(), Options{Path: path, PoolSize: 2})
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInsertAndGet(t *testing.T) {
	s := open(t, 0)
	recorded := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	id, err := s.Insert(t.Context(), NewRecording{
		Client:        "ring",
		RecordedAt:    recorded,
		Transcription: "turn on the kitchen lights",
		AudioMIME:     "audio/mp4",
		Audio:         []byte("fake m4a payload"),
	})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Transcription != "turn on the kitchen lights" {
		t.Errorf("transcription = %q", rec.Transcription)
	}
	if !rec.RecordedAt.Equal(recorded) {
		t.Errorf("recordedAt = %v, want %v", rec.RecordedAt, recorded)
	}
	if rec.Status != StatusPending {
		t.Errorf("status = %q, want pending", rec.Status)
	}
	if rec.AudioBytes != 16 {
		t.Errorf("audioBytes = %d, want 16", rec.AudioBytes)
	}

	audio, err := s.Audio(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "fake m4a payload" {
		t.Errorf("audio = %q", audio)
	}
}

func TestRecordingWithoutTranscriptionIsDone(t *testing.T) {
	s := open(t, 0)
	id, err := s.Insert(t.Context(), NewRecording{Client: "ring", RecordedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusDone {
		t.Errorf("status = %q, want done: there is nothing to route", rec.Status)
	}
	if _, err := s.Claim(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("it should not be claimable, got %v", err)
	}
}

func TestGetMissing(t *testing.T) {
	s := open(t, 0)
	if _, err := s.Get(t.Context(), 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if _, err := s.Audio(t.Context(), 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound for audio, got %v", err)
	}
}

func TestClaimIsExclusiveAndOrdered(t *testing.T) {
	s := open(t, 0)
	first := insert(t, s, "first", time.Now())
	second := insert(t, s, "second", time.Now())

	a, err := s.Claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != first {
		t.Errorf("claimed %d, want the oldest %d", a.ID, first)
	}
	if a.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", a.Attempts)
	}

	b, err := s.Claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != second {
		t.Errorf("claimed %d, want %d: a running recording must not be reclaimed", b.ID, second)
	}

	if _, err := s.Claim(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("queue should be empty, got %v", err)
	}
}

func TestRequeueRunning(t *testing.T) {
	s := open(t, 0)
	insert(t, s, "interrupted", time.Now())
	if _, err := s.Claim(t.Context()); err != nil {
		t.Fatal(err)
	}
	n, err := s.RequeueRunning(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("requeued %d, want 1", n)
	}
	if _, err := s.Claim(t.Context()); err != nil {
		t.Errorf("it should be claimable again: %v", err)
	}
}

func TestFinishRecordsFailure(t *testing.T) {
	s := open(t, 0)
	id := insert(t, s, "doomed", time.Now())
	if err := s.Finish(t.Context(), id, errors.New("agent unreachable")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusFailed {
		t.Errorf("status = %q, want failed", rec.Status)
	}
	if rec.Error != "agent unreachable" {
		t.Errorf("error = %q", rec.Error)
	}
}

func TestTagsResponsesAndToolCalls(t *testing.T) {
	s := open(t, 0)
	id := insert(t, s, "turn on the lights", time.Now())

	if err := s.AddTags(t.Context(), id, []string{"home", "note", "home"}); err != nil {
		t.Fatal(err)
	}
	tags, err := s.Tags(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0] != "home" || tags[1] != "note" {
		t.Errorf("tags = %v, want [home note] with the duplicate dropped", tags)
	}

	if err := s.AddResponse(t.Context(), id, Response{
		Agent: "claude", Text: "Done.", InputTokens: 12, OutputTokens: 3,
	}); err != nil {
		t.Fatal(err)
	}
	responses, err := s.Responses(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(responses) != 1 || responses[0].Text != "Done." || responses[0].InputTokens != 12 {
		t.Errorf("responses = %+v", responses)
	}

	now := time.Now().Truncate(time.Millisecond)
	if err := s.AddToolCall(t.Context(), id, ToolCall{
		Server: "homeassistant", Tool: "turn_on", Arguments: `{"entity":"light.kitchen"}`,
		Result: "ok", StartedAt: now, EndedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	calls, err := s.ToolCalls(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Tool != "turn_on" || calls[0].IsError {
		t.Errorf("tool calls = %+v", calls)
	}
}

func TestKeywordSearch(t *testing.T) {
	s := open(t, 0)
	lights := insert(t, s, "turn on the kitchen lights", time.Now())
	insert(t, s, "remember to renew my passport", time.Now())

	got, err := s.Search(t.Context(), Query{Text: "kitchen"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != lights {
		t.Fatalf("want only the lights recording, got %+v", got)
	}
	if got[0].KeywordRank != 1 {
		t.Errorf("keywordRank = %d, want 1", got[0].KeywordRank)
	}
}

func TestKeywordSearchHandlesPunctuationAndOperators(t *testing.T) {
	s := open(t, 0)
	insert(t, s, "call the dentist about the crown", time.Now())
	// Bare FTS5 syntax in speech must not produce an error.
	for _, text := range []string{`dentist.`, `"dentist`, `dentist OR`, `NEAR(dentist)`, `dent`} {
		if _, err := s.Search(t.Context(), Query{Text: text}); err != nil {
			t.Errorf("search %q: %v", text, err)
		}
	}
}

func TestSearchFilters(t *testing.T) {
	s := open(t, 0)
	ctx := t.Context()
	old := insert(t, s, "ancient thought about lights", time.Now().Add(-72*time.Hour))
	recent := insert(t, s, "fresh thought about lights", time.Now())

	if err := s.SetRoute(ctx, recent, "claude", "rule 0", "fresh thought about lights"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTags(ctx, recent, []string{"home"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.AddToolCall(ctx, recent, ToolCall{
		Server: "homeassistant", Tool: "turn_on", StartedAt: now, EndedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	ids := func(rs []Result) []int64 {
		out := make([]int64, len(rs))
		for i, r := range rs {
			out[i] = r.Recording.ID
		}
		return out
	}

	got, err := s.Search(ctx, Query{Text: "lights", Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != recent {
		t.Errorf("since filter: got %v, want [%d]", ids(got), recent)
	}

	got, err = s.Search(ctx, Query{Text: "lights", Until: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != old {
		t.Errorf("until filter: got %v, want [%d]", ids(got), old)
	}

	got, err = s.Search(ctx, Query{Text: "lights", Route: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != recent {
		t.Errorf("route filter: got %v", ids(got))
	}

	got, err = s.Search(ctx, Query{Text: "lights", Tags: []string{"home"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != recent {
		t.Errorf("tag filter: got %v", ids(got))
	}

	used := true
	got, err = s.Search(ctx, Query{Text: "lights", ToolUsed: &used})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != recent {
		t.Errorf("tool used filter: got %v", ids(got))
	}

	unused := false
	got, err = s.Search(ctx, Query{Text: "lights", ToolUsed: &unused})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recording.ID != old {
		t.Errorf("tool unused filter: got %v", ids(got))
	}
}

func TestRecentWhenQueryIsEmpty(t *testing.T) {
	s := open(t, 0)
	insert(t, s, "older", time.Now().Add(-time.Hour))
	newest := insert(t, s, "newer", time.Now())

	got, err := s.Search(t.Context(), Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want both recordings, got %d", len(got))
	}
	if got[0].Recording.ID != newest {
		t.Errorf("first = %d, want the newest %d", got[0].Recording.ID, newest)
	}
}

func TestVectorSearchNeedsAnEmbedder(t *testing.T) {
	s := open(t, 0)
	if _, err := s.Search(t.Context(), Query{Vector: []float32{1, 0, 0}}); err == nil {
		t.Error("want an error when vectors are not enabled")
	}
	if err := s.PutEmbeddings(t.Context(), 1, []string{"x"}, [][]float32{{1, 0, 0}}); err == nil {
		t.Error("want an error when storing vectors without an embedder")
	}
}

func TestVectorSearch(t *testing.T) {
	s := open(t, 3)
	ctx := t.Context()
	lights := insert(t, s, "turn on the kitchen lights", time.Now())
	passport := insert(t, s, "remember to renew my passport", time.Now())

	if err := s.PutEmbeddings(ctx, lights, []string{"turn on the kitchen lights"}, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEmbeddings(ctx, passport, []string{"remember to renew my passport"}, [][]float32{{0, 1, 0}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Search(ctx, Query{Vector: []float32{0.9, 0.1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want both recordings ranked, got %d", len(got))
	}
	if got[0].Recording.ID != lights {
		t.Errorf("nearest = %d, want %d", got[0].Recording.ID, lights)
	}
	if got[0].Snippet != "turn on the kitchen lights" {
		t.Errorf("snippet = %q", got[0].Snippet)
	}
	if got[0].VectorRank != 1 || got[1].VectorRank != 2 {
		t.Errorf("vector ranks = %d, %d", got[0].VectorRank, got[1].VectorRank)
	}
}

func TestHybridSearchFusesRankings(t *testing.T) {
	s := open(t, 3)
	ctx := t.Context()

	// keywordOnly matches the words but carries no embedding; vectorOnly is
	// nearest in vector space but shares no words; both appears in each
	// ranking, so fusion must lift it above either single-ranking match.
	insert(t, s, "kitchen lights inventory", time.Now())
	vectorOnly := insert(t, s, "illumination downstairs", time.Now())
	both := insert(t, s, "kitchen lights please", time.Now())

	for id, vec := range map[int64][]float32{
		vectorOnly: {1, 0, 0},
		both:       {0.95, 0.05, 0},
	} {
		rec, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutEmbeddings(ctx, id, []string{rec.Transcription}, [][]float32{vec}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Search(ctx, Query{Text: "kitchen lights", Vector: []float32{1, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 results, got %d", len(got))
	}
	if got[0].Recording.ID != both {
		t.Errorf("top result = %d, want %d: appearing in both rankings must win",
			got[0].Recording.ID, both)
	}
	if got[0].KeywordRank == 0 || got[0].VectorRank == 0 {
		t.Errorf("top result should carry both ranks, got keyword=%d vector=%d",
			got[0].KeywordRank, got[0].VectorRank)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Errorf("results are not ordered by score: %v", got)
		}
	}
}

func TestPutEmbeddingsReplacesAndValidates(t *testing.T) {
	s := open(t, 3)
	ctx := t.Context()
	id := insert(t, s, "a thought", time.Now())

	if err := s.PutEmbeddings(ctx, id, []string{"a", "b"}, [][]float32{{1, 0, 0}, {0, 1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEmbeddings(ctx, id, []string{"a"}, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingEmbedding(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("nothing should be pending, got %v", pending)
	}

	if err := s.PutEmbeddings(ctx, id, []string{"a"}, [][]float32{{1, 0}}); err == nil {
		t.Error("want an error for the wrong dimension")
	}
	if err := s.PutEmbeddings(ctx, id, []string{"a", "b"}, [][]float32{{1, 0, 0}}); err == nil {
		t.Error("want an error when chunks and vectors disagree")
	}
}

func TestPendingEmbedding(t *testing.T) {
	s := open(t, 3)
	id := insert(t, s, "needs embedding", time.Now())
	pending, err := s.PendingEmbedding(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != id {
		t.Errorf("pending = %v, want [%d]", pending, id)
	}
}

func TestEmbeddingModelIsPinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "riverbed.db")
	s, err := Open(t.Context(), Options{Path: path, PoolSize: 2, EmbedModel: "model-a", EmbedDim: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(t.Context(), Options{Path: path, PoolSize: 2, EmbedModel: "model-b", EmbedDim: 3}); err == nil {
		t.Error("changing the model must be refused")
	}
	if _, err := Open(t.Context(), Options{Path: path, PoolSize: 2, EmbedModel: "model-a", EmbedDim: 4}); err == nil {
		t.Error("changing the dimension must be refused")
	}
	again, err := Open(t.Context(), Options{Path: path, PoolSize: 2, EmbedModel: "model-a", EmbedDim: 3})
	if err != nil {
		t.Errorf("reopening with the same model must work: %v", err)
	} else if err := again.Close(); err != nil {
		t.Error(err)
	}
}

func TestTokens(t *testing.T) {
	s := open(t, 0)
	ctx := t.Context()

	if _, err := s.Token(ctx, "example"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}

	expiry := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	want := &oauth2.Token{AccessToken: "at", RefreshToken: "rt", TokenType: "Bearer", Expiry: expiry}
	if err := s.PutToken(ctx, "example", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Token(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "at" || got.RefreshToken != "rt" || !got.Expiry.Equal(expiry) {
		t.Errorf("token = %+v, want %+v", got, want)
	}

	if err := s.PutToken(ctx, "example", &oauth2.Token{AccessToken: "at2"}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Token(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "at2" || !got.Expiry.IsZero() {
		t.Errorf("replaced token = %+v", got)
	}

	if err := s.DeleteToken(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx, "example"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound after delete, got %v", err)
	}
}

func TestConcurrentClaims(t *testing.T) {
	s := open(t, 0)
	const n = 8
	for i := 0; i < n; i++ {
		insert(t, s, "thought", time.Now())
	}

	type result struct {
		id  int64
		err error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		go func() {
			rec, err := s.Claim(context.Background())
			results <- result{rec.ID, err}
		}()
	}

	seen := make(map[int64]bool)
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("claim: %v", r.err)
			continue
		}
		if seen[r.id] {
			t.Errorf("recording %d was claimed twice", r.id)
		}
		seen[r.id] = true
	}
}
