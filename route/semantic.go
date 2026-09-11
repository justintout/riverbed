package route

import (
	"context"
	"fmt"
	"math"
)

// utterance is one example of what a route handles, with its embedding.
type utterance struct {
	text   string
	vector []float32
}

// semanticMatch is the best semantic score for one rule.
type semanticMatch struct {
	// Index is the rule's position in the configuration.
	Index int
	Agent string
	// Score is the highest cosine similarity between the transcription and any
	// of the rule's utterances.
	Score float64
	// Utterance is the example that produced Score.
	Utterance string
	// Threshold is the score this rule requires.
	Threshold float64
}

// Matched reports whether the score reached the threshold.
func (m semanticMatch) Matched() bool { return m.Score >= m.Threshold }

// embedUtterances embeds every example of every semantic rule. It runs once, at
// construction, so routing itself embeds only the transcription.
func (r *Router) embedUtterances(ctx context.Context) error {
	for i := range r.rules {
		rule := &r.rules[i]
		if len(rule.cfg.Utterances) == 0 {
			continue
		}
		if r.embedder == nil {
			return fmt.Errorf("route: rule %d matches by meaning, which needs an embedder", rule.index)
		}
		for _, text := range rule.cfg.Utterances {
			vector, err := r.embedder.Embed(ctx, text)
			if err != nil {
				return fmt.Errorf("route: embed utterance %q: %w", text, err)
			}
			if len(vector) == 0 {
				return fmt.Errorf("route: utterance %q produced an empty vector", text)
			}
			rule.utterances = append(rule.utterances, utterance{
				text: text,
				// Stored normalized, so scoring is one dot product.
				vector: normalize(vector),
			})
		}
	}
	return nil
}

// semanticScores scores every semantic rule against a transcription. The slice
// is in configuration order, and is empty when no rule matches by meaning.
func (r *Router) semanticScores(ctx context.Context, text string) ([]semanticMatch, error) {
	if !r.hasSemantic() {
		return nil, nil
	}
	vector, err := r.embedder.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("route: embed transcription: %w", err)
	}
	query := normalize(vector)

	var out []semanticMatch
	for _, rule := range r.rules {
		if len(rule.utterances) == 0 {
			continue
		}
		match := semanticMatch{
			Index:     rule.index,
			Agent:     rule.cfg.Agent,
			Threshold: rule.threshold,
			Score:     -1,
		}
		for _, example := range rule.utterances {
			if len(example.vector) != len(query) {
				return nil, fmt.Errorf(
					"route: utterance %q has %d dimensions but the transcription has %d",
					example.text, len(example.vector), len(query))
			}
			if score := dot(query, example.vector); score > match.Score {
				match.Score = score
				match.Utterance = example.text
			}
		}
		out = append(out, match)
	}
	return out, nil
}

// bestSemantic returns the highest scoring rule that reached its threshold.
// Equal scores are broken by configuration order, so the decision is stable.
func bestSemantic(matches []semanticMatch) (semanticMatch, bool) {
	best := semanticMatch{Score: -1}
	found := false
	for _, match := range matches {
		if !match.Matched() {
			continue
		}
		if !found || match.Score > best.Score {
			best = match
			found = true
		}
	}
	return best, found
}

func (r *Router) hasSemantic() bool {
	for _, rule := range r.rules {
		if len(rule.utterances) > 0 {
			return true
		}
	}
	return false
}

// normalize scales a vector to unit length, so a dot product is a cosine
// similarity. Models differ in whether they return normalized vectors, so this
// does not assume it.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// dot returns the dot product of two normalized vectors, which is their cosine
// similarity.
func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}
