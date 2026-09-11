package embedding

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justintout/riverbed/config"
)

func TestOpenDisabledWithoutModel(t *testing.T) {
	m, err := Open(t.Context(), config.Embedding{Kind: "potion"})
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Error("no model name should mean no embedder")
	}
}

func TestPotionModelNames(t *testing.T) {
	for _, name := range []string{"BASE8M", "base8m", "potion-base-8M", "potion-base-8m"} {
		got, err := potionModel(name)
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		if got != "BASE8M" {
			t.Errorf("%q resolved to %q, want BASE8M", name, got)
		}
	}
	if _, err := potionModel("not-a-model"); err == nil {
		t.Error("want an error for an unknown model")
	} else if !strings.Contains(err.Error(), "BASE8M") {
		t.Errorf("the error should list available models: %v", err)
	}
}

func TestRemoteEmbedder(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.0,3.0,4.0]}]}`))
	}))
	defer srv.Close()

	m, err := Open(t.Context(), config.Embedding{
		Kind: "remote", Model: "text-embed", BaseURL: srv.URL + "/v1", APIKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Dim != 3 {
		t.Errorf("dim = %d, want the probed 3", m.Dim)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotModel != "text-embed" {
		t.Errorf("model = %q", gotModel)
	}

	v, err := m.Embed(t.Context(), "anything")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 3 || v[2] != 4 {
		t.Errorf("vector = %v", v)
	}
}

func TestRemoteEmbedderReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := Open(t.Context(), config.Embedding{Kind: "remote", Model: "missing", BaseURL: srv.URL + "/v1"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("the error should carry the server detail: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.0,3.0,4.0,12.0]}]}`))
	}))
	defer srv.Close()

	m, err := Open(t.Context(), config.Embedding{
		Kind: "remote", Model: "text-embed", BaseURL: srv.URL + "/v1", Dim: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Dim != 3 {
		t.Fatalf("dim = %d, want 3", m.Dim)
	}
	v, err := m.Embed(t.Context(), "anything")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 3 {
		t.Fatalf("len = %d, want 3", len(v))
	}
	// The truncated vector must be renormalized: [0,3,4] has norm 5.
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-6 {
		t.Errorf("norm = %v, want 1", math.Sqrt(norm))
	}
}

func TestTruncateRejectsWidening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1.0,0.0]}]}`))
	}))
	defer srv.Close()

	_, err := Open(t.Context(), config.Embedding{
		Kind: "remote", Model: "m", BaseURL: srv.URL + "/v1", Dim: 8,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot truncate") {
		t.Errorf("want a truncation error, got %v", err)
	}
}

func TestChunk(t *testing.T) {
	if got := Chunk("   ", 100); got != nil {
		t.Errorf("blank text should produce no chunks, got %v", got)
	}
	if got := Chunk("one short thought", 100); len(got) != 1 || got[0] != "one short thought" {
		t.Errorf("short text should be one chunk, got %v", got)
	}

	long := "Alpha sentence here. Beta sentence here. Gamma sentence here. Delta sentence here."
	got := Chunk(long, 45)
	if len(got) < 2 {
		t.Fatalf("long text should split, got %v", got)
	}
	for i, c := range got {
		if len([]rune(c)) > 90 {
			t.Errorf("chunk %d is too long: %q", i, c)
		}
	}
	// Overlap: the last sentence of one chunk opens the next.
	for i := 1; i < len(got); i++ {
		prev := strings.Fields(got[i-1])
		if len(prev) == 0 {
			continue
		}
		if !strings.HasPrefix(got[i], prev[len(prev)-3]) && !strings.Contains(got[i-1], strings.Fields(got[i])[0]) {
			t.Errorf("chunk %d does not overlap chunk %d:\n%q\n%q", i, i-1, got[i-1], got[i])
		}
	}
}

func TestChunkKeepsDecimalsIntact(t *testing.T) {
	got := splitSentences("Set it to 21.5 degrees. Then leave it.")
	if len(got) != 2 {
		t.Fatalf("want 2 sentences, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "21.5") {
		t.Errorf("the decimal should stay in one sentence: %v", got)
	}
}
