// Package embedding turns text into vectors.
//
// Three implementations are available. Potion and Goformer run inside this
// process, so a Riverbed binary needs no embedding service. Remote calls an
// OpenAI-compatible /v1/embeddings endpoint for hosted or separately served
// models.
package embedding

import (
	"context"
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/justintout/riverbed/config"
)

// Embedder produces vectors from text.
//
// The signature matches go-sqlite-vector's Embedder, so an Embedder can also be
// registered as the vector_embed SQL function.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Model describes a loaded embedder.
type Model struct {
	Embedder
	// Name identifies the model. It is stored in the database so that a later
	// run cannot mix vectors from different models.
	Name string
	// Dim is the vector length.
	Dim int
	// Close releases any resources the model holds.
	Close func() error
}

// Open loads the embedder described by cfg. It returns a nil Model when no
// model is configured, which leaves retrieval keyword only.
func Open(ctx context.Context, cfg config.Embedding) (*Model, error) {
	if !cfg.Enabled() {
		return nil, nil
	}

	var (
		m   *Model
		err error
	)
	switch cfg.Kind {
	case "potion":
		m, err = openPotion(ctx, cfg.Model)
	case "goformer":
		m, err = openGoformer(cfg.Model)
	case "remote":
		m, err = openRemote(ctx, cfg)
	default:
		return nil, fmt.Errorf("embedding: unknown kind %q", cfg.Kind)
	}
	if err != nil {
		return nil, err
	}

	if cfg.Dim > 0 {
		if cfg.Dim > m.Dim {
			closeModel(m)
			return nil, fmt.Errorf("embedding: %s produces %d dimensions, cannot truncate to %d",
				m.Name, m.Dim, cfg.Dim)
		}
		if cfg.Dim < m.Dim {
			m = truncate(m, cfg.Dim)
		}
	}
	return m, nil
}

func closeModel(m *Model) {
	if m != nil && m.Close != nil {
		_ = m.Close()
	}
}

// truncate shortens and renormalizes every vector. Models trained with
// Matryoshka representation learning, which includes the potion family and
// nomic-embed, stay usable at reduced width.
func truncate(m *Model, dim int) *Model {
	inner := m.Embedder
	return &Model{
		Name:  fmt.Sprintf("%s@%d", m.Name, dim),
		Dim:   dim,
		Close: m.Close,
		Embedder: embedFunc(func(ctx context.Context, text string) ([]float32, error) {
			v, err := inner.Embed(ctx, text)
			if err != nil {
				return nil, err
			}
			return normalize(v[:dim]), nil
		}),
	}
}

// embedFunc adapts a function to the Embedder interface.
type embedFunc func(ctx context.Context, text string) ([]float32, error)

func (f embedFunc) Embed(ctx context.Context, text string) ([]float32, error) { return f(ctx, text) }

// normalize scales a vector to unit length so that a dot product is a cosine
// similarity. It returns the vector unchanged when its norm is zero.
func normalize(v []float32) []float32 {
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(float64(sum)))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// Chunk splits text for embedding. Recordings are short, so the whole
// transcription is one chunk unless it runs long, in which case it is split on
// sentence boundaries with an overlap so a thought spanning a boundary is still
// retrievable.
func Chunk(text string, maxRunes int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = 1200
	}
	if len([]rune(text)) <= maxRunes {
		return []string{text}
	}

	sentences := splitSentences(text)
	var (
		chunks  []string
		current []string
		length  int
	)
	flush := func() {
		if len(current) == 0 {
			return
		}
		chunks = append(chunks, strings.TrimSpace(strings.Join(current, " ")))
		// Carry the final sentence into the next chunk as overlap.
		last := current[len(current)-1]
		current = []string{last}
		length = len([]rune(last))
	}
	for _, s := range sentences {
		n := len([]rune(s))
		if length+n > maxRunes {
			flush()
		}
		current = append(current, s)
		length += n
	}
	if len(current) > 0 {
		chunks = append(chunks, strings.TrimSpace(strings.Join(current, " ")))
	}
	return chunks
}

// splitSentences breaks text after sentence-ending punctuation.
func splitSentences(text string) []string {
	var (
		out   []string
		start int
	)
	runes := []rune(text)
	for i, r := range runes {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// Only break when whitespace follows, so decimals stay intact.
		if i+1 < len(runes) && !unicode.IsSpace(runes[i+1]) {
			continue
		}
		if s := strings.TrimSpace(string(runes[start : i+1])); s != "" {
			out = append(out, s)
		}
		start = i + 1
	}
	if s := strings.TrimSpace(string(runes[start:])); s != "" {
		out = append(out, s)
	}
	return out
}
