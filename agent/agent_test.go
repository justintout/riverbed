package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/tool"
)

// lightTool is the tool offered in the provider tests.
var lightTool = tool.Tool{
	Name:        "home.turn_on",
	Server:      "home",
	Bare:        "turn_on",
	Description: "Turn a light on",
	InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"entity": map[string]any{"type": "string"}},
		"required":   []string{"entity"},
	},
}

// invoker records calls and returns a fixed result.
func invoker(t *testing.T, seen *[]string) Invoker {
	t.Helper()
	return func(_ context.Context, name string, args map[string]any) (tool.Result, error) {
		encoded, _ := json.Marshal(args)
		*seen = append(*seen, name+" "+string(encoded))
		return tool.Result{Text: "turned on " + fmt.Sprint(args["entity"])}, nil
	}
}

func TestNewRejectsUnknownKind(t *testing.T) {
	if _, err := New(config.Agent{Name: "x", Kind: "telepathy"}, nil); err == nil {
		t.Error("want an error for an unknown kind")
	}
}

func TestDefaults(t *testing.T) {
	o := Options{Config: config.Agent{}}
	if o.system() != DefaultSystem {
		t.Error("an agent with no system prompt should get the default")
	}
	if o.maxTokens() != DefaultMaxTokens || o.maxTurns() != DefaultMaxTurns || o.timeout() != DefaultTimeout {
		t.Error("defaults were not applied")
	}

	o = Options{Config: config.Agent{System: "be brief", MaxTokens: 10, MaxTurns: 2}}
	if o.system() != "be brief" || o.maxTokens() != 10 || o.maxTurns() != 2 {
		t.Error("configured values were not used")
	}
}

// --- Claude ---

func TestClaudeAnswersWithoutTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_1","type":"message","role":"assistant","model":"test",
			"content":[{"type":"text","text":"The lights are on."}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":11,"output_tokens":5}
		}`))
	}))
	defer srv.Close()

	a, err := New(config.Agent{
		Name: "claude", Kind: "claude", Model: "test-model", APIKey: "k", BaseURL: srv.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(t.Context(), Request{Prompt: "are the lights on"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "The lights are on." {
		t.Errorf("text = %q", res.Text)
	}
	if res.InputTokens != 11 || res.OutputTokens != 5 {
		t.Errorf("usage = %d/%d", res.InputTokens, res.OutputTokens)
	}
	if len(res.Calls) != 0 {
		t.Errorf("calls = %v", res.Calls)
	}
}

func TestClaudeRunsToolLoop(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		requests = append(requests, decoded)

		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{
				"id":"msg_1","type":"message","role":"assistant","model":"test",
				"content":[
					{"type":"text","text":"Switching it on."},
					{"type":"tool_use","id":"tu_1","name":"home.turn_on","input":{"entity":"light.kitchen"}}
				],
				"stop_reason":"tool_use",
				"usage":{"input_tokens":20,"output_tokens":10}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"msg_2","type":"message","role":"assistant","model":"test",
			"content":[{"type":"text","text":"Kitchen light is on."}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":30,"output_tokens":8}
		}`))
	}))
	defer srv.Close()

	a, err := New(config.Agent{
		Name: "claude", Kind: "claude", Model: "test-model", APIKey: "k", BaseURL: srv.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	res, err := a.Run(t.Context(), Request{
		Prompt: "turn on the kitchen light",
		Tools:  []tool.Tool{lightTool},
		Invoke: invoker(t, &seen),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Kitchen light is on." {
		t.Errorf("text = %q", res.Text)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "home.turn_on") || !strings.Contains(seen[0], "light.kitchen") {
		t.Errorf("tool invocations = %v", seen)
	}
	if len(res.Calls) != 1 {
		t.Fatalf("calls = %+v", res.Calls)
	}
	call := res.Calls[0]
	if call.Server != "home" || call.Tool != "turn_on" {
		t.Errorf("call split = %q/%q", call.Server, call.Tool)
	}
	if call.Result != "turned on light.kitchen" || call.IsError {
		t.Errorf("call result = %q, isError = %v", call.Result, call.IsError)
	}
	if res.InputTokens != 50 || res.OutputTokens != 18 {
		t.Errorf("usage should accumulate over turns: %d/%d", res.InputTokens, res.OutputTokens)
	}

	// The tool must have been declared, and the second request must replay the
	// tool use and carry its result.
	tools, _ := requests[0]["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools sent = %v", requests[0]["tools"])
	}
	if name := tools[0].(map[string]any)["name"]; name != "home.turn_on" {
		t.Errorf("tool name sent = %v", name)
	}
	messages, _ := requests[1]["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("second request should carry user, assistant and result: %d messages", len(messages))
	}
	if !strings.Contains(string(mustJSON(t, messages[1])), "tool_use") {
		t.Error("the assistant turn must replay the tool use")
	}
	if !strings.Contains(string(mustJSON(t, messages[2])), "tool_result") {
		t.Error("the third message must be the tool result")
	}
}

func TestClaudeStopsAfterMaxTurns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always ask for another tool call, so the loop must stop itself.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"m","type":"message","role":"assistant","model":"t",
			"content":[{"type":"tool_use","id":"tu","name":"home.turn_on","input":{"entity":"light.x"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer srv.Close()

	a, err := New(config.Agent{
		Name: "claude", Kind: "claude", Model: "t", APIKey: "k", BaseURL: srv.URL, MaxTurns: 2,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	_, err = a.Run(t.Context(), Request{
		Prompt: "loop", Tools: []tool.Tool{lightTool}, Invoke: invoker(t, &seen),
	})
	if err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("want a give-up error, got %v", err)
	}
	if len(seen) != 2 {
		t.Errorf("the tool ran %d times, want the 2 turn limit", len(seen))
	}
}

func TestClaudeReportsEmptyReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"t",
			"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`))
	}))
	defer srv.Close()

	a, _ := New(config.Agent{Name: "c", Kind: "claude", Model: "t", APIKey: "k", BaseURL: srv.URL}, nil)
	if _, err := a.Run(t.Context(), Request{Prompt: "hello"}); err == nil {
		t.Error("want an error for an empty reply")
	}
}

// --- OpenAI compatible ---

func TestOpenAIRunsToolLoop(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		requests = append(requests, decoded)

		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{
				"id":"c1","object":"chat.completion","model":"test",
				"choices":[{"index":0,"finish_reason":"tool_calls","message":{
					"role":"assistant","content":"",
					"tool_calls":[{"id":"tc_1","type":"function","function":{
						"name":"home.turn_on","arguments":"{\"entity\":\"light.kitchen\"}"}}]
				}}],
				"usage":{"prompt_tokens":20,"completion_tokens":10}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"c2","object":"chat.completion","model":"test",
			"choices":[{"index":0,"finish_reason":"stop","message":{
				"role":"assistant","content":"Kitchen light is on."}}],
			"usage":{"prompt_tokens":30,"completion_tokens":8}
		}`))
	}))
	defer srv.Close()

	a, err := New(config.Agent{
		Name: "local", Kind: "openai", Model: "test", APIKey: "k", BaseURL: srv.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	res, err := a.Run(t.Context(), Request{
		Prompt: "turn on the kitchen light",
		Tools:  []tool.Tool{lightTool},
		Invoke: invoker(t, &seen),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Kitchen light is on." {
		t.Errorf("text = %q", res.Text)
	}
	if len(seen) != 1 {
		t.Fatalf("tool invocations = %v", seen)
	}
	if res.InputTokens != 50 || res.OutputTokens != 18 {
		t.Errorf("usage = %d/%d", res.InputTokens, res.OutputTokens)
	}

	tools, _ := requests[0]["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools sent = %v", requests[0]["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "home.turn_on" {
		t.Errorf("function name = %v", fn["name"])
	}
	if fn["parameters"] == nil {
		t.Error("the schema must be sent as parameters")
	}
	messages, _ := requests[1]["messages"].([]any)
	if len(messages) != 4 {
		t.Errorf("second request should carry system, user, assistant and tool: %d", len(messages))
	}
	if role := messages[3].(map[string]any)["role"]; role != "tool" {
		t.Errorf("last message role = %v, want tool", role)
	}
}

func TestOpenAIWithoutTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &decoded)
		if _, ok := decoded["tools"]; ok {
			t.Error("no tools should be sent when none are offered")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"t",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"noted"}}],
			"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	a, _ := New(config.Agent{Name: "l", Kind: "openai", Model: "t", APIKey: "k", BaseURL: srv.URL}, nil)
	res, err := a.Run(t.Context(), Request{Prompt: "a thought"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "noted" {
		t.Errorf("text = %q", res.Text)
	}
}

// --- Gemini ---

func TestGeminiRunsToolLoop(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		requests = append(requests, decoded)

		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{
				"candidates":[{"content":{"role":"model","parts":[
					{"functionCall":{"name":"home.turn_on","args":{"entity":"light.kitchen"}}}
				]},"finishReason":"STOP"}],
				"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":10}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"candidates":[{"content":{"role":"model","parts":[
				{"text":"Kitchen light is on."}
			]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":8}
		}`))
	}))
	defer srv.Close()

	a, err := New(config.Agent{
		Name: "gemini", Kind: "gemini", Model: "gemini-test", APIKey: "k", BaseURL: srv.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	res, err := a.Run(t.Context(), Request{
		Prompt: "turn on the kitchen light",
		Tools:  []tool.Tool{lightTool},
		Invoke: invoker(t, &seen),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Kitchen light is on." {
		t.Errorf("text = %q", res.Text)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "light.kitchen") {
		t.Errorf("tool invocations = %v", seen)
	}
	if res.InputTokens != 50 || res.OutputTokens != 18 {
		t.Errorf("usage = %d/%d", res.InputTokens, res.OutputTokens)
	}

	if len(requests) != 2 {
		t.Fatalf("want 2 requests, got %d", len(requests))
	}
	encoded := string(mustJSON(t, requests[1]))
	if !strings.Contains(encoded, "functionResponse") {
		t.Error("the second request must carry the function response")
	}
	if !strings.Contains(string(mustJSON(t, requests[0])), "home.turn_on") {
		t.Error("the tool must be declared")
	}
}

// --- HTTP harness ---

func TestHTTPAgent(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Trace") != "on" {
			t.Errorf("configured headers were not sent: %v", r.Header)
		}
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"reply":"  Noted it down.  "}`))
	}))
	defer srv.Close()

	a, err := New(config.Agent{
		Name: "shelley", Kind: "http", BaseURL: srv.URL, APIKey: "key",
		Headers: map[string]string{"X-Trace": "on"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(t.Context(), Request{Prompt: "remember the milk"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Noted it down." {
		t.Errorf("text = %q, want it trimmed", res.Text)
	}
	if got["prompt"] != "remember the milk" {
		t.Errorf("prompt sent = %v", got["prompt"])
	}
	if got["source"] != "riverbed" {
		t.Errorf("source = %v", got["source"])
	}
}

func TestHTTPAgentReplyShapes(t *testing.T) {
	for name, body := range map[string]string{
		"plain text": "just text",
		"reply":      `{"reply":"from reply"}`,
		"text":       `{"text":"from text"}`,
		"response":   `{"response":"from response"}`,
		"content":    `{"content":"from content"}`,
		"message":    `{"message":"from message"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			a, _ := New(config.Agent{Name: "h", Kind: "http", BaseURL: srv.URL}, nil)
			res, err := a.Run(t.Context(), Request{Prompt: "x"})
			if err != nil {
				t.Fatal(err)
			}
			if res.Text == "" {
				t.Errorf("no text extracted from %s", body)
			}
		})
	}
}

func TestHTTPAgentReportsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "harness is busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a, _ := New(config.Agent{Name: "h", Kind: "http", BaseURL: srv.URL}, nil)
	_, err := a.Run(t.Context(), Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "harness is busy") {
		t.Errorf("want the server detail in the error, got %v", err)
	}
}

func TestHTTPAgentRejectsEmptyReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
	}))
	defer srv.Close()

	a, _ := New(config.Agent{Name: "h", Kind: "http", BaseURL: srv.URL}, nil)
	if _, err := a.Run(t.Context(), Request{Prompt: "x"}); err == nil {
		t.Error("want an error when no reply can be found")
	}
}

// --- shared helpers ---

func TestInvokeWithoutTools(t *testing.T) {
	call, text := invoke(t.Context(), Request{}, "home.turn_on", map[string]any{"entity": "x"})
	if !call.IsError {
		t.Error("calling a tool with no invoker must be an error")
	}
	if text == "" {
		t.Error("the model should be told what happened")
	}
	if call.Server != "home" || call.Tool != "turn_on" {
		t.Errorf("name split = %q/%q", call.Server, call.Tool)
	}
}

func TestInvokeRecordsToolFailure(t *testing.T) {
	req := Request{Invoke: func(context.Context, string, map[string]any) (tool.Result, error) {
		return tool.Result{}, fmt.Errorf("server unreachable")
	}}
	call, text := invoke(t.Context(), req, "home.turn_on", nil)
	if !call.IsError || !strings.Contains(call.Result, "unreachable") {
		t.Errorf("call = %+v", call)
	}
	if !strings.Contains(text, "unreachable") {
		t.Errorf("the model should see the failure: %q", text)
	}
}

func TestSchemaHelpers(t *testing.T) {
	props, required, ok := schemaParts(map[string]any{
		"type":       "object",
		"properties": map[string]any{"entity": map[string]any{"type": "string"}},
		"required":   []string{"entity"},
	})
	if !ok || props["entity"] == nil || len(required) != 1 {
		t.Errorf("props = %v, required = %v, ok = %v", props, required, ok)
	}

	if _, _, ok := schemaParts(nil); ok {
		t.Error("a nil schema has no parts")
	}

	obj := schemaObject(nil)
	if obj["type"] != "object" || obj["properties"] == nil {
		t.Errorf("a nil schema should become an empty object schema, got %v", obj)
	}

	obj = schemaObject(map[string]any{"properties": map[string]any{"a": true}})
	if obj["type"] != "object" {
		t.Errorf("a type should be filled in: %v", obj)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
