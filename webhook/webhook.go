// Package webhook receives recordings from the device.
//
// The wire format is the one the Pebble Index 01 sends: an HTTPS POST of
// multipart/form-data carrying the fields recordedAt and client always, plus
// transcription and audio when configured. An X-Audio-Size header accompanies
// audio. Authorization is whatever header the device was configured to send; a
// bearer token is what this package checks.
package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/justintout/riverbed/store"
)

// Receiver stores incoming recordings and signals that work is waiting.
type Receiver interface {
	Insert(ctx context.Context, r store.NewRecording) (int64, error)
}

// Options configures a Handler.
type Options struct {
	Receiver Receiver
	// Token must match the bearer token in the Authorization header.
	Token string
	// RetainAudio stores the audio part. When false the audio is counted and
	// discarded.
	RetainAudio bool
	// MaxAudioBytes rejects recordings whose audio exceeds this size.
	MaxAudioBytes int64
	// Notify is called after a recording is stored, so a worker can wake
	// without polling. It must not block.
	Notify func()
	Logger *slog.Logger
}

// Handler receives recordings over HTTP.
type Handler struct {
	opts Options
	log  *slog.Logger
}

// New returns a handler for the device webhook.
func New(opts Options) (*Handler, error) {
	if opts.Receiver == nil {
		return nil, errors.New("webhook: receiver is required")
	}
	if opts.Token == "" {
		return nil, errors.New("webhook: token is required")
	}
	if opts.MaxAudioBytes <= 0 {
		opts.MaxAudioBytes = 32 << 20
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Handler{opts: opts, log: opts.Logger}, nil
}

// maxFieldBytes caps each text field. A transcription is prose, so a megabyte
// is already far beyond anything a recording produces.
const maxFieldBytes = 1 << 20

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(r) {
		// No detail: an unauthenticated caller learns nothing about why.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rec, err := h.parse(r)
	if err != nil {
		h.log.Warn("rejected recording", "error", err, "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := h.opts.Receiver.Insert(r.Context(), rec)
	if err != nil {
		h.log.Error("store recording", "error", err)
		http.Error(w, "could not store recording", http.StatusInternalServerError)
		return
	}

	h.log.Info("received recording",
		"id", id,
		"client", rec.Client,
		"recorded_at", rec.RecordedAt,
		"transcribed", rec.Transcription != "",
		"audio_bytes", len(rec.Audio))

	if h.opts.Notify != nil {
		h.opts.Notify()
	}

	// The device is waiting, so acknowledge as soon as the recording is
	// durable and let a worker do the slow part.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
}

// authorized compares the bearer token in constant time.
func (h *Handler) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	token := header
	if after, ok := cutPrefixFold(header, "Bearer "); ok {
		token = after
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.opts.Token)) == 1
}

// cutPrefixFold is strings.CutPrefix with a case-insensitive prefix, because
// the scheme name in an Authorization header is not case sensitive.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// parse reads the multipart body into a recording. Parts are streamed, so a
// large audio payload is never buffered twice.
func (h *Handler) parse(r *http.Request) (store.NewRecording, error) {
	var rec store.NewRecording

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return rec, fmt.Errorf("unreadable Content-Type: %w", err)
	}
	if mediaType != "multipart/form-data" {
		return rec, fmt.Errorf("expected multipart/form-data, got %s", mediaType)
	}
	boundary, ok := params["boundary"]
	if !ok {
		return rec, errors.New("Content-Type has no boundary")
	}

	// Reject oversized audio before reading it, when the device declares a size.
	if declared := r.Header.Get("X-Audio-Size"); declared != "" {
		size, err := strconv.ParseInt(declared, 10, 64)
		if err != nil {
			return rec, fmt.Errorf("unreadable X-Audio-Size %q: %w", declared, err)
		}
		if size > h.opts.MaxAudioBytes {
			return rec, fmt.Errorf("audio of %d bytes exceeds the %d byte limit", size, h.opts.MaxAudioBytes)
		}
	}

	var recordedAt string
	parts := multipart.NewReader(r.Body, boundary)
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return rec, fmt.Errorf("reading multipart body: %w", err)
		}

		switch part.FormName() {
		case "audio":
			rec.AudioMIME = part.Header.Get("Content-Type")
			if rec.AudioMIME == "" {
				rec.AudioMIME = "audio/mp4"
			}
			// Read one byte past the limit so an oversized payload is caught
			// even when the device sent no X-Audio-Size.
			audio, err := io.ReadAll(io.LimitReader(part, h.opts.MaxAudioBytes+1))
			if err != nil {
				return rec, fmt.Errorf("reading audio: %w", err)
			}
			if int64(len(audio)) > h.opts.MaxAudioBytes {
				return rec, fmt.Errorf("audio exceeds the %d byte limit", h.opts.MaxAudioBytes)
			}
			if h.opts.RetainAudio {
				rec.Audio = audio
			} else {
				rec.AudioMIME = ""
			}
		case "transcription":
			text, err := readField(part)
			if err != nil {
				return rec, fmt.Errorf("reading transcription: %w", err)
			}
			rec.Transcription = strings.TrimSpace(text)
		case "recordedAt":
			recordedAt, err = readField(part)
			if err != nil {
				return rec, fmt.Errorf("reading recordedAt: %w", err)
			}
		case "client":
			client, err := readField(part)
			if err != nil {
				return rec, fmt.Errorf("reading client: %w", err)
			}
			rec.Client = strings.TrimSpace(client)
		default:
			// Ignore unknown parts: the device may add fields later.
			_, _ = io.Copy(io.Discard, io.LimitReader(part, maxFieldBytes))
		}
		_ = part.Close()
	}

	if rec.Client == "" {
		return rec, errors.New("client is required")
	}
	if recordedAt == "" {
		return rec, errors.New("recordedAt is required")
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(recordedAt), 10, 64)
	if err != nil {
		return rec, fmt.Errorf("recordedAt %q is not unix milliseconds: %w", recordedAt, err)
	}
	rec.RecordedAt = time.UnixMilli(ms)

	return rec, nil
}

func readField(part *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
