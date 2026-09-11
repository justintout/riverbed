package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/justintout/riverbed/tool"
)

// openAIAgent answers with an OpenAI-compatible chat completions endpoint.
// DeepSeek, llama.cpp, LM Studio and Ollama all speak this, so one provider
// covers hosted and local models alike.
type openAIAgent struct {
	opts   Options
	client openai.Client
}

func newOpenAI(opts Options) (Agent, error) {
	reqOpts := []option.RequestOption{}
	if opts.Config.APIKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(opts.Config.APIKey))
	}
	if opts.Config.BaseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(opts.Config.BaseURL))
	}
	return &openAIAgent{opts: opts, client: openai.NewClient(reqOpts...)}, nil
}

func (a *openAIAgent) Name() string { return a.opts.Config.Name }

func (a *openAIAgent) Run(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.timeout())
	defer cancel()

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(a.opts.system()),
		openai.UserMessage(req.Prompt),
	}
	params := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(a.opts.Config.Model),
		MaxCompletionTokens: openai.Int(a.opts.maxTokens()),
		Tools:               openAITools(req.Tools),
	}

	out := &Response{}
	for turn := 0; turn < a.opts.maxTurns(); turn++ {
		params.Messages = messages
		completion, err := a.client.Chat.Completions.New(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", a.Name(), err)
		}
		if len(completion.Choices) == 0 {
			return nil, fmt.Errorf("agent %s: %w", a.Name(), ErrNoReply)
		}
		out.InputTokens += completion.Usage.PromptTokens
		out.OutputTokens += completion.Usage.CompletionTokens

		message := completion.Choices[0].Message
		if len(message.ToolCalls) == 0 {
			if message.Content == "" {
				return nil, fmt.Errorf("agent %s: %w", a.Name(), ErrNoReply)
			}
			out.Text = message.Content
			return out, nil
		}

		messages = append(messages, message.ToParam())
		for _, call := range message.ToolCalls {
			fn := call.Function
			args := map[string]any{}
			if fn.Arguments != "" {
				if err := json.Unmarshal([]byte(fn.Arguments), &args); err != nil {
					a.opts.Logger.Warn("unreadable tool arguments", "tool", fn.Name, "error", err)
				}
			}
			record, resultText := invoke(ctx, req, fn.Name, args)
			out.Calls = append(out.Calls, record)
			messages = append(messages, openai.ToolMessage(resultText, call.ID))
		}
	}

	return nil, fmt.Errorf("agent %s: gave up after %d tool turns", a.Name(), a.opts.maxTurns())
}

// openAITools converts the registry's tools to function definitions.
func openAITools(tools []tool.Tool) []openai.ChatCompletionToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  shared.FunctionParameters(schemaObject(t.InputSchema)),
		}))
	}
	return out
}
