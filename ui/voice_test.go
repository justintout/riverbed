package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/store"
)

type fakeTranscriber struct{ text string }

func (f fakeTranscriber) Transcribe(_ context.Context, audio io.Reader, _ string) (string, error) {
	_, err := io.Copy(io.Discard, audio)
	return f.text, err
}

func (fakeTranscriber) Model() string { return "fake-whisper" }

func TestVoiceInputIsHiddenWithoutATranscriber(t *testing.T) {
	h, _ := newHandler(t, "")
	if got := do(h, "POST", "/api/notes/voice", "audio").Code; got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
		t.Fatalf("voice input must not exist without a transcriber: got %d", got)
	}
	if body := do(h, "GET", "/api/status", "").Body.String(); strings.Contains(body, "voice_model") {
		t.Fatalf("status must not offer voice input without a transcriber: %s", body)
	}
}

func TestVoiceNoteRecordsItsSource(t *testing.T) {
	s, err := store.Open(t.Context(), store.Options{
		Path: filepath.Join(t.TempDir(), "riverbed.db"), PoolSize: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := config.Default()
	cfg.UI.Enabled = true
	h, err := New(Options{Store: s, Config: cfg, Transcriber: fakeTranscriber{text: "call the vet"}})
	if err != nil {
		t.Fatal(err)
	}

	req := do(h, "POST", "/api/notes/voice", "opus bytes")
	if req.Code != http.StatusCreated {
		t.Fatalf("voice note: got %d %s", req.Code, req.Body)
	}
	var created struct{ ID int64 }
	if err := json.Unmarshal(req.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Transcription != "call the vet" || rec.Source != store.SourceWeb || rec.TranscribedBy != "fake-whisper" {
		t.Fatalf("a voice note should keep its transcription, source web and its model: %+v", rec)
	}
	if rec.AudioBytes != int64(len("opus bytes")) {
		t.Fatalf("a voice note should keep its audio when audio.retain is set: %d bytes", rec.AudioBytes)
	}
}
