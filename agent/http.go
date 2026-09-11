package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// httpAgent posts a prompt to a remote harness and reads back a reply.
//
// This is the hook for an agent that already runs elsewhere and holds its own
// tools and memory, such as a self-hosted assistant. Riverbed sends the prompt
// and stores the answer; it does not drive a tool loop, because the harness owns
// that.
//
// The request body is {"prompt": "...", "source": "riverbed"}. A reply may be
// plain text, or JSON carrying any of "reply", "text", "response", "content" or
// "message", which covers the shapes these harnesses usually return.
type httpAgent struct {
	opts   Options
	client *http.Client
}

func newHTTP(opts Options) (Agent, error) {
	return &httpAgent{
		opts:   opts,
		client: &http.Client{Timeout: opts.timeout()},
	}, nil
}

func (a *httpAgent) Name() string { return a.opts.Config.Name }

func (a *httpAgent) Run(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.timeout())
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"prompt": req.Prompt,
		"system": a.opts.system(),
		"source": "riverbed",
	})
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.opts.Config.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/plain")
	if a.opts.Config.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+a.opts.Config.APIKey)
	}
	for key, value := range a.opts.Config.Headers {
		request.Header.Set(key, value)
	}

	resp, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", a.Name(), err)
	}
	defer resp.Body.Close()

	// A reply is prose, so a megabyte is already generous.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("agent %s: reading reply: %w", a.Name(), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("agent %s: %s: %s", a.Name(), resp.Status,
			strings.TrimSpace(string(payload)))
	}

	text := replyText(payload)
	if text == "" {
		return nil, fmt.Errorf("agent %s: %w", a.Name(), ErrNoReply)
	}
	return &Response{Text: text}, nil
}

// replyText extracts the reply from a response body.
func replyText(payload []byte) string {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] != '{' {
		return string(trimmed)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return string(trimmed)
	}
	for _, key := range []string{"reply", "text", "response", "content", "message"} {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
