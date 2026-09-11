// Package agent runs a prompt against a model or a remote harness, letting it
// call MCP tools.
//
// Every provider owns its own tool-use loop, because the three vendor wire
// formats differ enough that a shared loop would be mostly branches. What they
// share is this package's Request, Response and Invoker.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/tool"
)

// Agent answers a prompt, optionally by calling tools.
type Agent interface {
	// Name is the configured agent name.
	Name() string
	// Run answers one prompt.
	Run(ctx context.Context, req Request) (*Response, error)
}

// Request is one prompt for an agent.
type Request struct {
	// Prompt is the text to answer, after any routing prefix was stripped.
	Prompt string
	// Tools are the tools the agent may call.
	Tools []tool.Tool
	// Invoke runs a tool. It is nil when Tools is empty.
	Invoke Invoker
}

// Invoker runs a qualified tool by name.
type Invoker func(ctx context.Context, name string, args map[string]any) (tool.Result, error)

// Response is an agent's answer.
type Response struct {
	// Text is the final reply.
	Text string
	// Calls records every tool the agent invoked, in order.
	Calls []Call
	// InputTokens and OutputTokens are reported when the provider supplies them.
	InputTokens  int64
	OutputTokens int64
}

// Call is one tool invocation made while answering.
type Call struct {
	Name      string
	Server    string
	Tool      string
	Arguments string
	Result    string
	IsError   bool
	StartedAt time.Time
	EndedAt   time.Time
}

// Defaults applied when an agent does not configure them.
const (
	DefaultMaxTokens = 2048
	DefaultMaxTurns  = 8
	DefaultTimeout   = 2 * time.Minute
)

// DefaultSystem is the instruction given to an agent that configures none.
const DefaultSystem = "You are the assistant behind a voice recorder. " +
	"Requests arrive as transcribed speech, so expect informal phrasing and transcription errors. " +
	"Use the available tools to act when the request calls for action. " +
	"Answer in one or two sentences, as the reply is read rather than displayed."

// Options are shared by every provider.
type Options struct {
	Config config.Agent
	Logger *slog.Logger
}

// New builds the agent described by cfg.
func New(cfg config.Agent, logger *slog.Logger) (Agent, error) {
	if logger == nil {
		logger = slog.Default()
	}
	opts := Options{Config: cfg, Logger: logger.With("agent", cfg.Name)}

	switch cfg.Kind {
	case "claude":
		return newClaude(opts)
	case "gemini":
		return newGemini(opts)
	case "openai":
		return newOpenAI(opts)
	case "http":
		return newHTTP(opts)
	default:
		return nil, fmt.Errorf("agent %q: unknown kind %q", cfg.Name, cfg.Kind)
	}
}

// ErrNoReply reports that a provider returned no text, which usually means it
// stopped at a tool call without producing an answer.
var ErrNoReply = errors.New("agent: the model returned no reply")

// system returns the configured instruction, or the default.
func (o Options) system() string {
	if o.Config.System != "" {
		return o.Config.System
	}
	return DefaultSystem
}

func (o Options) maxTokens() int64 {
	if o.Config.MaxTokens > 0 {
		return o.Config.MaxTokens
	}
	return DefaultMaxTokens
}

func (o Options) maxTurns() int {
	if o.Config.MaxTurns > 0 {
		return o.Config.MaxTurns
	}
	return DefaultMaxTurns
}

func (o Options) timeout() time.Duration {
	return o.Config.Timeout.Or(DefaultTimeout)
}

// invoke runs one tool and records the call. A tool failure is returned to the
// model as text so it can explain or retry, rather than ending the turn.
func invoke(ctx context.Context, req Request, name string, args map[string]any) (Call, string) {
	call := Call{Name: name, StartedAt: time.Now()}
	if server, bare, ok := cut(name); ok {
		call.Server, call.Tool = server, bare
	} else {
		call.Tool = name
	}
	if encoded, err := json.Marshal(args); err == nil {
		call.Arguments = string(encoded)
	}

	if req.Invoke == nil {
		call.EndedAt = time.Now()
		call.IsError = true
		call.Result = "no tools are available"
		return call, call.Result
	}

	res, err := req.Invoke(ctx, name, args)
	call.EndedAt = time.Now()
	if err != nil {
		call.IsError = true
		call.Result = err.Error()
		return call, "tool call failed: " + err.Error()
	}
	call.IsError = res.IsError
	call.Result = res.Text
	return call, res.Text
}

func cut(name string) (server, bare string, ok bool) {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i], name[i+1:], true
		}
	}
	return "", "", false
}
