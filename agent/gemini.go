package agent

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	"github.com/justintout/riverbed/tool"
)

// geminiAgent answers with the Gemini API.
type geminiAgent struct {
	opts   Options
	client *genai.Client
}

func newGemini(opts Options) (Agent, error) {
	cfg := &genai.ClientConfig{
		APIKey:  opts.Config.APIKey,
		Backend: genai.BackendGeminiAPI,
	}
	if opts.Config.BaseURL != "" {
		cfg.HTTPOptions.BaseURL = opts.Config.BaseURL
	}
	client, err := genai.NewClient(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", opts.Config.Name, err)
	}
	return &geminiAgent{opts: opts, client: client}, nil
}

func (a *geminiAgent) Name() string { return a.opts.Config.Name }

func (a *geminiAgent) Run(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.timeout())
	defer cancel()

	contents := []*genai.Content{
		genai.NewContentFromText(req.Prompt, genai.RoleUser),
	}
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(a.opts.system(), genai.RoleUser),
		MaxOutputTokens:   int32(a.opts.maxTokens()),
		Tools:             geminiTools(req.Tools),
	}

	out := &Response{}
	for turn := 0; turn < a.opts.maxTurns(); turn++ {
		res, err := a.client.Models.GenerateContent(ctx, a.opts.Config.Model, contents, cfg)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", a.Name(), err)
		}
		if res.UsageMetadata != nil {
			out.InputTokens += int64(res.UsageMetadata.PromptTokenCount)
			out.OutputTokens += int64(res.UsageMetadata.CandidatesTokenCount)
		}

		calls := res.FunctionCalls()
		if len(calls) == 0 {
			text := res.Text()
			if text == "" {
				return nil, fmt.Errorf("agent %s: %w", a.Name(), ErrNoReply)
			}
			out.Text = text
			return out, nil
		}

		// Replay the model's turn, then answer every call in one user turn.
		if len(res.Candidates) > 0 && res.Candidates[0].Content != nil {
			contents = append(contents, res.Candidates[0].Content)
		}
		parts := make([]*genai.Part, 0, len(calls))
		for _, call := range calls {
			record, resultText := invoke(ctx, req, call.Name, call.Args)
			out.Calls = append(out.Calls, record)
			part := genai.NewPartFromFunctionResponse(call.Name, map[string]any{
				"result": resultText,
				"error":  record.IsError,
			})
			part.FunctionResponse.ID = call.ID
			parts = append(parts, part)
		}
		contents = append(contents, genai.NewContentFromParts(parts, genai.RoleUser))
	}

	return nil, fmt.Errorf("agent %s: gave up after %d tool turns", a.Name(), a.opts.maxTurns())
}

// geminiTools declares the registry's tools as function declarations. The raw
// JSON Schema is passed through ParametersJsonSchema rather than translated into
// genai.Schema, so a server's schema reaches the model unchanged.
func geminiTools(tools []tool.Tool) []*genai.Tool {
	if len(tools) == 0 {
		return nil
	}
	declarations := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		declarations = append(declarations, &genai.FunctionDeclaration{
			Name:                 t.Name,
			Description:          t.Description,
			ParametersJsonSchema: schemaObject(t.InputSchema),
		})
	}
	return []*genai.Tool{{FunctionDeclarations: declarations}}
}
