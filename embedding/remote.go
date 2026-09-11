package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/justintout/riverbed/config"
)

// openRemote prepares an embedder backed by an OpenAI-compatible
// /v1/embeddings endpoint. The dimension is probed once, because the endpoint
// does not otherwise advertise it.
func openRemote(ctx context.Context, cfg config.Embedding) (*Model, error) {
	c := &remote{
		url:    strings.TrimSuffix(cfg.BaseURL, "/") + "/embeddings",
		apiKey: cfg.APIKey,
		model:  cfg.Model,
		client: &http.Client{Timeout: 30 * time.Second},
	}
	probe, err := c.Embed(ctx, "dimension probe")
	if err != nil {
		return nil, fmt.Errorf("embedding: probe %s: %w", c.url, err)
	}
	if len(probe) == 0 {
		return nil, fmt.Errorf("embedding: %s returned an empty vector", c.url)
	}
	return &Model{
		Embedder: c,
		Name:     "remote/" + cfg.Model,
		Dim:      len(probe),
	}, nil
}

// remote calls an OpenAI-compatible embeddings endpoint.
type remote struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

func (r *remote) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{"model": r.model, "input": text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("embeddings endpoint returned %s: %s",
			resp.Status, strings.TrimSpace(string(detail)))
	}

	var payload struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("embeddings endpoint returned no data")
	}
	return payload.Data[0].Embedding, nil
}
