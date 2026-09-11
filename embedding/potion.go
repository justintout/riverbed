package embedding

import (
	"context"
	"fmt"
	"strings"

	potion "github.com/trengrj/go-potion"
)

// openPotion loads a static embedding model from the potion family. Model files
// are downloaded on first use and cached under the user cache directory, or
// wherever GO_POTION_HOME points.
//
// Static embeddings carry no attention, so they are weaker than a transformer
// on long or subtle text. They are also about four orders of magnitude faster,
// which suits short voice notes fused with keyword search.
func openPotion(ctx context.Context, model string) (*Model, error) {
	kind, err := potionModel(model)
	if err != nil {
		return nil, err
	}
	p, err := potion.New(ctx, kind)
	if err != nil {
		return nil, fmt.Errorf("embedding: load potion %s: %w", model, err)
	}
	return &Model{
		Name:  "potion/" + string(kind),
		Dim:   p.Dimensions(),
		Close: p.Close,
		Embedder: embedFunc(func(_ context.Context, text string) ([]float32, error) {
			v, err := p.Encode(text)
			if err != nil {
				return nil, fmt.Errorf("embedding: encode: %w", err)
			}
			return v, nil
		}),
	}, nil
}

// potionModel resolves a configured name to a potion model. Both the library
// constant ("BASE8M") and the HuggingFace-style name ("potion-base-8M") are
// accepted, since the latter is what the model is called everywhere else.
func potionModel(name string) (potion.Model, error) {
	want := normalizeModelName(name)
	for _, m := range potion.Models() {
		if normalizeModelName(string(m)) == want {
			return m, nil
		}
		if normalizeModelName("potion-"+string(m)) == want {
			return m, nil
		}
	}
	// Accept the published names, which embed a hyphenated size.
	for _, m := range potion.Models() {
		if strings.HasSuffix(want, normalizeModelName(string(m))) {
			return m, nil
		}
	}
	names := make([]string, 0, len(potion.Models()))
	for _, m := range potion.Models() {
		names = append(names, string(m))
	}
	return "", fmt.Errorf("embedding: unknown potion model %q; available: %s",
		name, strings.Join(names, ", "))
}

func normalizeModelName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}
