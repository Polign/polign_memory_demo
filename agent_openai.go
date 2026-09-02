package main

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v2"
	"github.com/openai/openai-go/v2/shared"

	"github.com/Polign/polign_memory_demo/memkit"
)

// openaiAgent drives OpenAI models through the Chat Completions tool loop.
// Same toolbox, same store, same printed traces as the Anthropic loop.
type openaiAgent struct {
	client   openai.Client
	model    string
	tb       *toolbox
	system   string
	tools    []openai.ChatCompletionToolUnionParam
	messages []openai.ChatCompletionMessageParamUnion
}

func newOpenAIAgent(model string, store *memkit.Store, wikipedia wikipediaSource, trace bool) *openaiAgent {
	specs := toolSpecs(wikipedia != nil)
	tools := make([]openai.ChatCompletionToolUnionParam, len(specs))
	for i, spec := range specs {
		params := map[string]any{"type": "object", "properties": spec.Properties}
		if len(spec.Required) > 0 {
			params["required"] = spec.Required
		}
		tools[i] = openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        spec.Name,
			Description: openai.String(spec.Description),
			Parameters:  shared.FunctionParameters(params),
		})
	}
	return &openaiAgent{
		client: openai.NewClient(),
		model:  model,
		tb:     &toolbox{store: store, wikipedia: wikipedia, trace: trace},
		system: systemPrompt(store.Registry(), wikipedia != nil),
		tools:  tools,
	}
}

func (a *openaiAgent) Reset() { a.messages = nil }

func (a *openaiAgent) Turn(ctx context.Context, userText string) (AgentReply, error) {
	a.messages = append(a.messages, openai.UserMessage(userText))
	retrievedFrom := make(map[string]bool)

	for {
		history := make([]openai.ChatCompletionMessageParamUnion, 0, len(a.messages)+1)
		history = append(history, openai.SystemMessage(a.system))
		history = append(history, a.messages...)

		params := openai.ChatCompletionNewParams{
			Model:               shared.ChatModel(a.model),
			Messages:            history,
			Tools:               a.tools,
			MaxCompletionTokens: openai.Int(2048),
		}
		// GPT-5 supports minimal reasoning and low verbosity. Both reduce work
		// on this latency-sensitive, tool-oriented demo without changing models.
		if a.model == "gpt-5" {
			params.ReasoningEffort = shared.ReasoningEffortMinimal
			params.Verbosity = openai.ChatCompletionNewParamsVerbosityLow
		}
		resp, err := a.client.Chat.Completions.New(ctx, params)
		if err != nil {
			// Drop the failed turn from history so the conversation can continue.
			a.messages = a.messages[:len(a.messages)-1]
			return AgentReply{}, err
		}
		if len(resp.Choices) == 0 {
			return AgentReply{}, fmt.Errorf("openai returned no choices")
		}
		msg := resp.Choices[0].Message
		a.messages = append(a.messages, msg.ToParam())

		if len(msg.ToolCalls) == 0 {
			return AgentReply{Text: msg.Content, RetrievedFrom: mapKeys(retrievedFrom)}, nil
		}
		for _, tc := range msg.ToolCalls {
			result, isErr := a.tb.run(tc.Function.Name, []byte(tc.Function.Arguments))
			if source := a.tb.retrievalSource(tc.Function.Name, isErr); source != "" {
				retrievedFrom[source] = true
			}
			if isErr {
				// Chat Completions has no error flag on tool results; the
				// prefix is the convention the model reads.
				result = "error: " + result
			}
			a.messages = append(a.messages, openai.ToolMessage(result, tc.ID))
		}
	}
}
