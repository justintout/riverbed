package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/justintout/riverbed/tool"
)

// claudeAgent answers with the Anthropic Messages API.
type claudeAgent struct {
	opts   Options
	client anthropic.Client
}

func newClaude(opts Options) (Agent, error) {
	reqOpts := []option.RequestOption{}
	if opts.Config.APIKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(opts.Config.APIKey))
	}
	if opts.Config.BaseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(opts.Config.BaseURL))
	}
	return &claudeAgent{opts: opts, client: anthropic.NewClient(reqOpts...)}, nil
}

func (a *claudeAgent) Name() string { return a.opts.Config.Name }

func (a *claudeAgent) Run(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.timeout())
	defer cancel()

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.opts.Config.Model),
		MaxTokens: a.opts.maxTokens(),
		System:    []anthropic.TextBlockParam{{Text: a.opts.system()}},
		Tools:     claudeTools(req.Tools),
	}

	out := &Response{}
	for turn := 0; turn < a.opts.maxTurns(); turn++ {
		params.Messages = messages
		msg, err := a.client.Messages.New(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", a.Name(), err)
		}
		out.InputTokens += msg.Usage.InputTokens
		out.OutputTokens += msg.Usage.OutputTokens

		var (
			text     string
			toolUses []anthropic.ContentBlockUnion
		)
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				text += block.Text
			case "tool_use":
				toolUses = append(toolUses, block)
			}
		}

		if len(toolUses) == 0 {
			if text == "" {
				return nil, fmt.Errorf("agent %s: %w", a.Name(), ErrNoReply)
			}
			out.Text = text
			return out, nil
		}

		// The assistant turn must be replayed verbatim, tool uses included, or
		// the next request has no call for the results to answer.
		assistant := make([]anthropic.ContentBlockParamUnion, 0, len(msg.Content))
		if text != "" {
			assistant = append(assistant, anthropic.NewTextBlock(text))
		}
		for _, use := range toolUses {
			assistant = append(assistant, anthropic.ContentBlockParamUnion{
				OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    use.ID,
					Name:  use.Name,
					Input: json.RawMessage(use.Input),
				},
			})
		}
		messages = append(messages, anthropic.MessageParam{
			Role:    anthropic.MessageParamRoleAssistant,
			Content: assistant,
		})

		results := make([]anthropic.ContentBlockParamUnion, 0, len(toolUses))
		for _, use := range toolUses {
			args := map[string]any{}
			if len(use.Input) > 0 {
				if err := json.Unmarshal(use.Input, &args); err != nil {
					a.opts.Logger.Warn("unreadable tool arguments", "tool", use.Name, "error", err)
				}
			}
			call, resultText := invoke(ctx, req, use.Name, args)
			out.Calls = append(out.Calls, call)
			results = append(results, anthropic.NewToolResultBlock(use.ID, resultText, call.IsError))
		}
		messages = append(messages, anthropic.NewUserMessage(results...))
	}

	return nil, fmt.Errorf("agent %s: gave up after %d tool turns", a.Name(), a.opts.maxTurns())
}

// claudeTools converts the registry's tools to Anthropic tool parameters.
func claudeTools(tools []tool.Tool) []anthropic.ToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema := anthropic.ToolInputSchemaParam{}
		if props, required, ok := schemaParts(t.InputSchema); ok {
			schema.Properties = props
			schema.Required = required
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: schema,
		}})
	}
	return out
}
