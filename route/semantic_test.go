package route

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/justintout/riverbed/config"
)

// axisEmbedder places text on one of three axes by keyword, so expected
// similarities are known without a model. Each string also gets a small
// deterministic offset, so two texts on the same axis score close to each other
// without scoring exactly 1, which is what a real embedder does. Vectors are
// deliberately not normalized, which exercises normalization too.
type axisEmbedder struct {
	err   error
	calls int
}

func (e *axisEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	e.calls++
	lower := strings.ToLower(text)
	offset := jitter(lower)
	switch {
	case containsAny(lower, "light", "lamp", "bright", "dim", "illuminate"):
		return []float32{3, 0.3 + offset, offset}, nil
	case containsAny(lower, "calendar", "meeting", "schedule", "appointment"):
		return []float32{0.3 + offset, 3, offset}, nil
	default:
		return []float32{offset, offset, 3}, nil
	}
}

// jitter returns a small value in [0, 0.6) that depends on the text.
func jitter(text string) float32 {
	var sum int
	for _, r := range text {
		sum += int(r)
	}
	return float32(sum%60) / 100
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func semanticRouter(t *testing.T, cfg config.Router, embedder *axisEmbedder) *Router {
	t.Helper()
	r, err := New(t.Context(), Options{Config: cfg, Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func homeAndCalendar(threshold float64) config.Router {
	return config.Router{
		Default:           config.JournalAgent,
		SemanticThreshold: threshold,
		Rules: []config.Rule{
			{
				Utterances: []string{"turn on the kitchen light", "dim the lamp"},
				Agent:      "home",
				Tags:       []string{"home"},
			},
			{
				Utterances: []string{"what is on my calendar", "schedule a meeting"},
				Agent:      "assistant",
				Tags:       []string{"calendar"},
			},
		},
	}
}

func TestSemanticMatch(t *testing.T) {
	embedder := &axisEmbedder{}
	r := semanticRouter(t, homeAndCalendar(0.5), embedder)

	// No word here appears in any utterance.
	d := r.Route(t.Context(), "please illuminate the room")
	if d.Agent != "home" {
		t.Fatalf("agent = %q, want home (reason %q)", d.Agent, d.Reason)
	}
	if d.Score <= 0.5 {
		t.Errorf("score = %v, want above the threshold", d.Score)
	}
	if d.Utterance == "" {
		t.Error("the matched example should be reported")
	}
	if !strings.Contains(d.Reason, "semantic") {
		t.Errorf("reason = %q", d.Reason)
	}
	if len(d.Tags) != 1 || d.Tags[0] != "home" {
		t.Errorf("tags = %v, want the matched rule's tags", d.Tags)
	}

	d = r.Route(t.Context(), "do I have an appointment today")
	if d.Agent != "assistant" {
		t.Errorf("agent = %q, want assistant", d.Agent)
	}
}

func TestSemanticBelowThresholdFallsThrough(t *testing.T) {
	embedder := &axisEmbedder{}
	r := semanticRouter(t, homeAndCalendar(0.9), embedder)

	// On the third axis, so similarity to both rules is near zero.
	d := r.Route(t.Context(), "the garden needs weeding")
	if d.Agent != config.JournalAgent {
		t.Errorf("agent = %q, want the default", d.Agent)
	}
	if d.Reason != "default" {
		t.Errorf("reason = %q", d.Reason)
	}
	if d.Score != 0 {
		t.Errorf("score = %v, want zero when nothing matched", d.Score)
	}
}

func TestThresholdDecides(t *testing.T) {
	// The same transcription routes differently depending on the threshold,
	// which is what makes calibration necessary.
	text := "the lamp is too bright"

	lenient := semanticRouter(t, homeAndCalendar(0.3), &axisEmbedder{})
	if d := lenient.Route(t.Context(), text); d.Agent != "home" {
		t.Errorf("lenient threshold: agent = %q, want home", d.Agent)
	}

	strict := semanticRouter(t, homeAndCalendar(0.999), &axisEmbedder{})
	if d := strict.Route(t.Context(), text); d.Agent != config.JournalAgent {
		t.Errorf("strict threshold: agent = %q, want the default", d.Agent)
	}
}

func TestPerRuleThresholdOverridesTheRouter(t *testing.T) {
	cfg := homeAndCalendar(0.2)
	cfg.Rules[0].Threshold = 0.999
	r := semanticRouter(t, cfg, &axisEmbedder{})

	// The home rule now demands near equality, so a paraphrase misses it.
	if d := r.Route(t.Context(), "the lamp is too bright"); d.Agent == "home" {
		t.Errorf("the rule threshold should have excluded it, got %q (%.3f)", d.Agent, d.Score)
	}
}

func TestExactMatchersWinOverSemantic(t *testing.T) {
	cfg := homeAndCalendar(0.1)
	cfg.Rules = append(cfg.Rules, config.Rule{
		Prefix: "hey shelley", Agent: "shelley", Strip: true,
	})
	r := semanticRouter(t, cfg, &axisEmbedder{})

	// This mentions a lamp, so the semantic home rule would match it, but the
	// wake word is exact and must win.
	d := r.Route(t.Context(), "Hey Shelley, is the lamp on?")
	if d.Agent != "shelley" {
		t.Fatalf("agent = %q, want shelley", d.Agent)
	}
	if d.Prompt != "is the lamp on?" {
		t.Errorf("prompt = %q, want the prefix stripped", d.Prompt)
	}
}

func TestHighestScoringRuleWins(t *testing.T) {
	embedder := &axisEmbedder{}
	r := semanticRouter(t, homeAndCalendar(0.1), embedder)

	// Both rules clear a threshold of 0.1, so the closer one must win.
	d := r.Route(t.Context(), "dim the lamp")
	if d.Agent != "home" {
		t.Errorf("agent = %q with score %.3f, want home", d.Agent, d.Score)
	}
}

func TestTranscriptionIsEmbeddedOncePerRouting(t *testing.T) {
	embedder := &axisEmbedder{}
	r := semanticRouter(t, homeAndCalendar(0.5), embedder)

	// Four utterances were embedded at construction.
	if embedder.calls != 4 {
		t.Fatalf("construction made %d embeddings, want 4", embedder.calls)
	}
	before := embedder.calls
	r.Route(t.Context(), "please illuminate the room")
	if got := embedder.calls - before; got != 1 {
		t.Errorf("routing made %d embeddings, want 1", got)
	}
}

func TestExactMatchCostsNoEmbedding(t *testing.T) {
	cfg := homeAndCalendar(0.5)
	cfg.Rules = append(cfg.Rules, config.Rule{Prefix: "note", Agent: config.JournalAgent, Strip: true})
	embedder := &axisEmbedder{}
	r := semanticRouter(t, cfg, embedder)

	before := embedder.calls
	if d := r.Route(t.Context(), "note the lamp is broken"); d.Agent != config.JournalAgent {
		t.Fatalf("agent = %q", d.Agent)
	}
	if embedder.calls != before {
		t.Error("a prefix match should not embed anything")
	}
}

func TestSemanticRuleNeedsAnEmbedder(t *testing.T) {
	_, err := New(t.Context(), Options{Config: homeAndCalendar(0.5)})
	if err == nil || !strings.Contains(err.Error(), "embedder") {
		t.Errorf("want an embedder error, got %v", err)
	}
}

func TestEmbedderFailureAtConstructionIsAnError(t *testing.T) {
	_, err := New(t.Context(), Options{
		Config:   homeAndCalendar(0.5),
		Embedder: &axisEmbedder{err: errors.New("model is unavailable")},
	})
	if err == nil {
		t.Error("a failure embedding utterances must not be ignored")
	}
}

func TestEmbedderFailureWhileRoutingFallsThrough(t *testing.T) {
	embedder := &axisEmbedder{}
	r := semanticRouter(t, homeAndCalendar(0.5), embedder)

	// The model becomes unavailable after startup.
	embedder.err = errors.New("model is unavailable")
	d := r.Route(t.Context(), "please illuminate the room")
	if d.Agent != config.JournalAgent || d.Reason != "default" {
		t.Errorf("decision = %+v, want the default so the recording is kept", d)
	}
}

func TestSemanticScoresAreReported(t *testing.T) {
	r := semanticRouter(t, homeAndCalendar(0.5), &axisEmbedder{})

	decision, scores, err := r.Explain(t.Context(), "please illuminate the room")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Agent != "home" {
		t.Errorf("agent = %q", decision.Agent)
	}
	if len(scores) != 2 {
		t.Fatalf("want a score for each semantic rule, got %d", len(scores))
	}
	for _, score := range scores {
		if score.Score < -1.001 || score.Score > 1.001 {
			t.Errorf("score %v is outside the cosine range", score.Score)
		}
		if score.Utterance == "" {
			t.Error("each score should name its closest example")
		}
		if score.Threshold != 0.5 {
			t.Errorf("threshold = %v", score.Threshold)
		}
	}
	if scores[0].Score <= scores[1].Score {
		t.Errorf("the home rule should score higher: %v vs %v", scores[0].Score, scores[1].Score)
	}
	if !scores[0].Matched() || scores[1].Matched() {
		t.Errorf("matched flags = %v, %v", scores[0].Matched(), scores[1].Matched())
	}
}

func TestNormalizeVector(t *testing.T) {
	got := normalize([]float32{3, 4})
	var sum float64
	for _, x := range got {
		sum += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(sum)-1) > 1e-6 {
		t.Errorf("norm = %v, want 1", math.Sqrt(sum))
	}

	// A zero vector has no direction, so it is returned unchanged.
	zero := normalize([]float32{0, 0})
	if zero[0] != 0 || zero[1] != 0 {
		t.Errorf("zero vector = %v", zero)
	}
}

func TestDotIsCosineForNormalizedVectors(t *testing.T) {
	a := normalize([]float32{1, 0})
	b := normalize([]float32{1, 0})
	if got := dot(a, b); math.Abs(got-1) > 1e-6 {
		t.Errorf("identical vectors scored %v, want 1", got)
	}

	c := normalize([]float32{0, 1})
	if got := dot(a, c); math.Abs(got) > 1e-6 {
		t.Errorf("orthogonal vectors scored %v, want 0", got)
	}

	d := normalize([]float32{-1, 0})
	if got := dot(a, d); math.Abs(got+1) > 1e-6 {
		t.Errorf("opposite vectors scored %v, want -1", got)
	}
}

func TestDimensionMismatchIsReported(t *testing.T) {
	r := semanticRouter(t, homeAndCalendar(0.5), &axisEmbedder{})
	// Replace the embedder with one producing a different width, as a model
	// change would.
	r.embedder = &widthEmbedder{}
	if _, _, err := r.Explain(t.Context(), "anything"); err == nil {
		t.Error("want an error for mismatched dimensions")
	}
}

type widthEmbedder struct{}

func (widthEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{1, 0, 0, 0, 0}, nil
}
