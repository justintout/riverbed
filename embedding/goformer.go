package embedding

import (
	"context"
	"fmt"

	"github.com/MichaelAyles/goformer"
)

// openGoformer loads a BERT-family model from a HuggingFace model directory
// holding config.json, tokenizer.json and a .safetensors weight file.
//
// This is the contextual option: slower than potion, and better on longer or
// more nuanced text because it actually attends across tokens.
func openGoformer(dir string) (*Model, error) {
	m, err := goformer.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("embedding: load goformer model from %s: %w", dir, err)
	}
	return &Model{
		Name: "goformer/" + dir,
		Dim:  m.Dims(),
		Embedder: embedFunc(func(_ context.Context, text string) ([]float32, error) {
			v, err := m.Embed(text)
			if err != nil {
				return nil, fmt.Errorf("embedding: encode: %w", err)
			}
			return v, nil
		}),
	}, nil
}
