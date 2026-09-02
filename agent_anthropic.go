package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Polign/polign_memory_demo/memkit"
)

// anthropicAgent drives Claude models with a hand-written tool loop.
type anthropicAgent struct {
	client   anthropic.Client
	model    string
	tb       *toolbox
	system   string
	tools    []anthropic.ToolUnionParam
	messages []anthropic.MessageParam
}

func newAnthropicAgent(model string, store *memkit.Store, wikipedia wikipediaSource, trace bool) *anthropicAgent {
	specs := toolSpecs(wikipedia != nil)
	defs := make([]anthropic.ToolParam, len(specs))
	tools := make([]anthropic.ToolUnionParam, len(specs))
	for i, spec := range specs {
		defs[i] = anthropic.ToolParam{
			Name:        spec.Name,
			Description: anthropic.String(spec.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: spec.Properties, Required: spec.Required},
		}
		tools[i] = anthropic.ToolUnionParam{OfTool: &defs[i]}
	}
	return &anthropicAgent{
		client: anthropic.NewClient(),
		model:  model,
		tb:     &toolbox{store: store, wikipedia: wikipedia, trace: trace},
		system: systemPrompt(store.Registry(), wikipedia != nil),
		tools:  tools,
	}
}

func (a *anthropicAgent) Reset() { a.messages = nil }

func (a *anthropicAgent) Turn(ctx context.Context, userText string) (AgentReply, error) {
	a.messages = append(a.messages, anthropic.NewUserMessage(anthropic.NewTextBlock(userText)))
	retrievedFrom := make(map[string]bool)

	for {
		resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(a.model),
			MaxTokens: 16000,
			System:    []anthropic.TextBlockParam{{Text: a.system}},
			Messages:  a.messages,
			Tools:     a.tools,
		})
		if err != nil {
			// Drop the failed turn from history so the conversation can continue.
			a.messages = a.messages[:len(a.messages)-1]
			return AgentReply{}, err
		}
		a.messages = append(a.messages, resp.ToParam())

		var replies []string
		var results []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				replies = append(replies, v.Text)
			case anthropic.ToolUseBlock:
				result, isErr := a.tb.run(v.Name, []byte(v.JSON.Input.Raw()))
				if source := a.tb.retrievalSource(v.Name, isErr); source != "" {
					retrievedFrom[source] = true
				}
				results = append(results, anthropic.NewToolResultBlock(v.ID, result, isErr))
			}
		}

		if resp.StopReason == anthropic.StopReasonRefusal {
			return AgentReply{}, fmt.Errorf("the model declined this request (%s)", resp.StopDetails.Category)
		}
		if resp.StopReason != anthropic.StopReasonToolUse {
			return AgentReply{Text: strings.Join(replies, "\n"), RetrievedFrom: mapKeys(retrievedFrom)}, nil
		}
		a.messages = append(a.messages, anthropic.NewUserMessage(results...))
	}
}
