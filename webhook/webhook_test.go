package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justintout/riverbed/store"
)

// recorder captures what the handler stored.
type recorder struct {
	got  []store.NewRecording
	err  error
	next int64
}

func (r *recorder) Insert(_ context.Context, rec store.NewRecording) (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.got = append(r.got, rec)
	r.next++
	return r.next, nil
}

// deviceRequest builds the request the Pebble Index 01 sends.
func deviceRequest(t *testing.T, transcription string, recordedAt time.Time, audio []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	if audio != nil {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="audio"; filename="recording.m4a"`}
		h["Content-Type"] = []string{"audio/mp4"}
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(audio); err != nil {
			t.Fatal(err)
		}
	}
	if transcription != "" {
		if err := mw.WriteField("transcription", transcription); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.WriteField("recordedAt", strconv.FormatInt(recordedAt.UnixMilli(), 10)); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("client", "ring"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook/recording", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer shared-secret")
	if audio != nil {
		req.Header.Set("X-Audio-Size", strconv.Itoa(len(audio)))
	}
	return req
}

func handler(t *testing.T, rec *recorder, tweak func(*Options)) *Handler {
	t.Helper()
	opts := Options{Receiver: rec, Token: "shared-secret", RetainAudio: true, MaxAudioBytes: 1 << 20}
	if tweak != nil {
		tweak(&opts)
	}
	h, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestReceivesDeviceRequest(t *testing.T) {
	rec := &recorder{}
	notified := 0
	h := handler(t, rec, func(o *Options) { o.Notify = func() { notified++ } })

	recordedAt := time.Now().Add(-30 * time.Second).Truncate(time.Millisecond)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest(t, "turn on the kitchen lights", recordedAt, []byte("m4a bytes")))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	var reply struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.ID != 1 {
		t.Errorf("id = %d, want 1", reply.ID)
	}
	if notified != 1 {
		t.Errorf("notified %d times, want 1", notified)
	}

	if len(rec.got) != 1 {
		t.Fatalf("stored %d recordings", len(rec.got))
	}
	got := rec.got[0]
	if got.Client != "ring" {
		t.Errorf("client = %q", got.Client)
	}
	if got.Transcription != "turn on the kitchen lights" {
		t.Errorf("transcription = %q", got.Transcription)
	}
	if !got.RecordedAt.Equal(recordedAt) {
		t.Errorf("recordedAt = %v, want %v", got.RecordedAt, recordedAt)
	}
	if string(got.Audio) != "m4a bytes" {
		t.Errorf("audio = %q", got.Audio)
	}
	if got.AudioMIME != "audio/mp4" {
		t.Errorf("audioMIME = %q", got.AudioMIME)
	}
}

func TestAudioOnlyRecording(t *testing.T) {
	rec := &recorder{}
	h := handler(t, rec, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest(t, "", time.Now(), []byte("m4a bytes")))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if rec.got[0].Transcription != "" {
		t.Errorf("transcription = %q, want empty", rec.got[0].Transcription)
	}
}

func TestTranscriptionOnlyRecording(t *testing.T) {
	rec := &recorder{}
	h := handler(t, rec, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest(t, "a passing thought", time.Now(), nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(rec.got[0].Audio) != 0 {
		t.Errorf("audio = %q, want none", rec.got[0].Audio)
	}
}

func TestDiscardsAudioWhenRetentionIsOff(t *testing.T) {
	rec := &recorder{}
	h := handler(t, rec, func(o *Options) { o.RetainAudio = false })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest(t, "a thought", time.Now(), []byte("m4a bytes")))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(rec.got[0].Audio) != 0 {
		t.Errorf("audio was retained: %q", rec.got[0].Audio)
	}
	if rec.got[0].Transcription != "a thought" {
		t.Errorf("the transcription must survive: %q", rec.got[0].Transcription)
	}
}

func TestRejectsUnknownToken(t *testing.T) {
	rec := &recorder{}
	h := handler(t, rec, nil)
	for _, auth := range []string{"", "Bearer wrong", "wrong", "Basic shared-secret"} {
		req := deviceRequest(t, "a thought", time.Now(), nil)
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("authorization %q: status = %d, want 401", auth, w.Code)
		}
	}
	if len(rec.got) != 0 {
		t.Error("nothing should have been stored")
	}
}

func TestAcceptsBareToken(t *testing.T) {
	// The device allows arbitrary headers, so a bare token is plausible.
	rec := &recorder{}
	h := handler(t, rec, nil)
	req := deviceRequest(t, "a thought", time.Now(), nil)
	req.Header.Set("Authorization", "shared-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202: %s", w.Code, w.Body)
	}
}

func TestAcceptsLowercaseBearer(t *testing.T) {
	rec := &recorder{}
	h := handler(t, rec, nil)
	req := deviceRequest(t, "a thought", time.Now(), nil)
	req.Header.Set("Authorization", "bearer shared-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202: %s", w.Code, w.Body)
	}
}

func TestRejectsNonPost(t *testing.T) {
	h := handler(t, &recorder{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/webhook/recording", nil)
	req.Header.Set("Authorization", "Bearer shared-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestRejectsMalformedBodies(t *testing.T) {
	h := handler(t, &recorder{}, nil)

	for name, build := range map[string]func() *http.Request{
		"not multipart": func() *http.Request {
			req := httptest.NewRequest(http.MethodPost, "/w", strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			return req
		},
		"no boundary": func() *http.Request {
			req := httptest.NewRequest(http.MethodPost, "/w", strings.NewReader("body"))
			req.Header.Set("Content-Type", "multipart/form-data")
			return req
		},
		"missing client": func() *http.Request {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("recordedAt", "1757000000000")
			_ = mw.Close()
			req := httptest.NewRequest(http.MethodPost, "/w", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			return req
		},
		"missing recordedAt": func() *http.Request {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("client", "ring")
			_ = mw.Close()
			req := httptest.NewRequest(http.MethodPost, "/w", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			return req
		},
		"recordedAt not a number": func() *http.Request {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("client", "ring")
			_ = mw.WriteField("recordedAt", "yesterday")
			_ = mw.Close()
			req := httptest.NewRequest(http.MethodPost, "/w", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			return req
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := build()
			req.Header.Set("Authorization", "Bearer shared-secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", w.Code, w.Body)
			}
		})
	}
}

func TestRejectsOversizedAudioByHeader(t *testing.T) {
	h := handler(t, &recorder{}, func(o *Options) { o.MaxAudioBytes = 16 })
	req := deviceRequest(t, "a thought", time.Now(), bytes.Repeat([]byte("x"), 64))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "exceeds") {
		t.Errorf("body = %q", w.Body)
	}
}

func TestRejectsOversizedAudioWithoutHeader(t *testing.T) {
	h := handler(t, &recorder{}, func(o *Options) { o.MaxAudioBytes = 16 })
	req := deviceRequest(t, "a thought", time.Now(), bytes.Repeat([]byte("x"), 64))
	req.Header.Del("X-Audio-Size")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestIgnoresUnknownParts(t *testing.T) {
	rec := &recorder{}
	h := handler(t, rec, nil)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("client", "ring")
	_ = mw.WriteField("recordedAt", "1757000000000")
	_ = mw.WriteField("somethingNew", "from a later firmware")
	_ = mw.WriteField("transcription", "a thought")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/w", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer shared-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	if rec.got[0].Transcription != "a thought" {
		t.Errorf("transcription = %q", rec.got[0].Transcription)
	}
}

func TestStoreFailureIsServerError(t *testing.T) {
	rec := &recorder{err: fmt.Errorf("disk is full")}
	h := handler(t, rec, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest(t, "a thought", time.Now(), nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "disk is full") {
		t.Error("the internal error must not reach the client")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(Options{Token: "t"}); err == nil {
		t.Error("want an error without a receiver")
	}
	if _, err := New(Options{Receiver: &recorder{}}); err == nil {
		t.Error("want an error without a token")
	}
}

func TestLargeAudioIsStreamed(t *testing.T) {
	// A payload at the limit must be accepted, confirming the limit is
	// inclusive and the reader does not truncate.
	rec := &recorder{}
	h := handler(t, rec, func(o *Options) { o.MaxAudioBytes = 1024 })
	audio := bytes.Repeat([]byte("a"), 1024)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest(t, "a thought", time.Now(), audio))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(rec.got[0].Audio) != 1024 {
		t.Errorf("stored %d bytes, want 1024", len(rec.got[0].Audio))
	}
}
