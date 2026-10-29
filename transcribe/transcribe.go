// Package transcribe turns recorded audio into text with an OpenAI-compatible
// /v1/audio/transcriptions endpoint.
package transcribe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/justintout/riverbed/config"
)

// Transcriber sends audio to the configured endpoint.
type Transcriber struct {
	model  string
	client openai.Client
}

// New builds a transcriber. It does no I/O.
func New(cfg config.Transcription) (*Transcriber, error) {
	if !cfg.Enabled() {
		return nil, errors.New("transcribe: no model is configured")
	}
	opts := []option.RequestOption{}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &Transcriber{model: cfg.Model, client: openai.NewClient(opts...)}, nil
}

// Model names the transcription model, which notes record as their transcriber.
func (t *Transcriber) Model() string { return t.model }

// Transcribe returns the text spoken in audio, whose media type is contentType.
func (t *Transcriber) Transcribe(ctx context.Context, audio io.Reader, contentType string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("transcribe: content type %q: %w", contentType, err)
	}
	ext, ok := extensions[mediaType]
	if !ok {
		return "", fmt.Errorf("transcribe: unsupported audio type %q", mediaType)
	}
	// OpenAI identifies the format from the file name, so it needs an extension.
	res, err := t.client.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  openai.File(audio, "audio"+ext, mediaType),
		Model: openai.AudioModel(t.model),
	})
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	return strings.TrimSpace(res.Text), nil
}

// extensions covers what browsers record and the formats OpenAI accepts.
var extensions = map[string]string{
	"audio/webm":  ".webm",
	"audio/ogg":   ".ogg",
	"audio/mp4":   ".m4a",
	"audio/x-m4a": ".m4a",
	"audio/mpeg":  ".mp3",
	"audio/wav":   ".wav",
	"audio/x-wav": ".wav",
	"audio/flac":  ".flac",
}
