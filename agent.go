package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	dim   = "\x1b[2m"
	cyan  = "\x1b[36m"
	reset = "\x1b[0m"
)

// Agent is a Claude conversation wired to the typed memory store. The tool
// loop is written out by hand on purpose: every tool call and result is
// printed, so the audience sees the whole path (model, typed tool,
// validation, database) instead of a chatbot that magically remembers.
type Agent struct {
	client   anthropic.Client
	model    string
	store    *Store
	system   string
	messages []anthropic.MessageParam
}

func NewAgent(client anthropic.Client, model string, store *Store) *Agent {
	return &Agent{
		client: client,
		model:  model,
		store:  store,
		system: systemPrompt(store.registry),
	}
}

func systemPrompt(registry Registry) string {
	return `You are a personal assistant with a typed, durable memory store.

Memory is not text you paste into your context. It is a database of typed
records, each one: kind (fact or preference), subject, predicate, value,
confidence, source, status. The store enforces the schema; if a write is
rejected, read the error and correct the call.

Predicates are a closed registry. Use exactly these:

` + registry.PromptTable() + `
Rules:
- When the user states a durable fact or preference about themselves or
  someone else, store it with remember_fact or remember_preference. Subjects
  are lowercase (a first name, or an entity name).
- Single-valued predicates supersede: the store handles that and tells you
  what was replaced. When that happens, acknowledge the change naturally.
- Before answering a question about the user or anything you may have been
  told before, call recall first. Trust the store over your conversation
  context; the store survives restarts and your context does not.
- Use forget only when the user explicitly asks you to forget something.
- Do not store trivia from the conversation flow, only durable statements.
- Keep replies short and conversational.`
}

// tools returns the typed tool surface. The JSON schemas are the first layer
// of enforcement; the store's validation is the second.
func tools() []anthropic.ToolUnionParam {
	rememberProps := map[string]any{
		"subject":   map[string]any{"type": "string", "description": "Who or what this is about, lowercase (e.g. \"anup\")"},
		"predicate": map[string]any{"type": "string", "description": "A registered snake_case predicate (e.g. \"prefers_editor\")"},
		"value":     map[string]any{"type": "string", "description": "The value (e.g. \"neovim\")"},
		"confidence": map[string]any{
			"type": "number", "description": "How certain this is, 0 to 1. Default 1.",
		},
		"source": map[string]any{
			"type": "string", "enum": []string{"user_stated", "agent_inferred", "tool_result"},
			"description": "Where this came from. Default user_stated.",
		},
	}
	rememberRequired := []string{"subject", "predicate", "value"}

	defs := []anthropic.ToolParam{
		{
			Name:        "remember_fact",
			Description: anthropic.String("Store one durable fact as a typed record. Single-valued predicates supersede any previous value; the result reports what was replaced."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: rememberProps, Required: rememberRequired},
		},
		{
			Name:        "remember_preference",
			Description: anthropic.String("Store one durable preference as a typed record. Same semantics as remember_fact."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: rememberProps, Required: rememberRequired},
		},
		{
			Name:        "recall",
			Description: anthropic.String("Query the memory store. Pass query for an open-ended semantic search, or structural filters (subject, predicate, kind, min_confidence) for an exact lookup; both can be combined. Only active records return unless include_history is true."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{
				"query":           map[string]any{"type": "string", "description": "Free-text semantic query (e.g. \"dev environment setup\")"},
				"subject":         map[string]any{"type": "string"},
				"predicate":       map[string]any{"type": "string"},
				"kind":            map[string]any{"type": "string", "enum": []string{"fact", "preference"}},
				"min_confidence":  map[string]any{"type": "number"},
				"include_history": map[string]any{"type": "boolean", "description": "Also return superseded records, with what replaced them"},
			}},
		},
		{
			Name:        "forget",
			Description: anthropic.String("Permanently delete records for a subject and predicate. With value set, only that record; without it, every record including history."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{
				"subject":   map[string]any{"type": "string"},
				"predicate": map[string]any{"type": "string"},
				"value":     map[string]any{"type": "string"},
			}, Required: []string{"subject", "predicate"}},
		},
	}

	out := make([]anthropic.ToolUnionParam, len(defs))
	for i := range defs {
		out[i] = anthropic.ToolUnionParam{OfTool: &defs[i]}
	}
	return out
}

// dispatch runs one tool call against the store and returns the JSON result
// Claude reads. Errors return as (message, true) so the model can self-repair
// against the store's validation.
func (a *Agent) dispatch(name string, input []byte) (string, bool) {
	fail := func(err error) (string, bool) { return err.Error(), true }
	ok := func(v any) (string, bool) {
		raw, err := json.Marshal(v)
		if err != nil {
			return fail(err)
		}
		return string(raw), false
	}

	switch name {
	case "remember_fact", "remember_preference":
		var in struct {
			Subject    string  `json:"subject"`
			Predicate  string  `json:"predicate"`
			Value      string  `json:"value"`
			Confidence float64 `json:"confidence"`
			Source     string  `json:"source"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return fail(err)
		}
		kind := "fact"
		if name == "remember_preference" {
			kind = "preference"
		}
		res, err := a.store.Remember(kind, in.Subject, in.Predicate, in.Value, in.Confidence, in.Source)
		if err != nil {
			return fail(err)
		}
		return ok(res)

	case "recall":
		var in struct {
			Query          string  `json:"query"`
			Subject        string  `json:"subject"`
			Predicate      string  `json:"predicate"`
			Kind           string  `json:"kind"`
			MinConfidence  float64 `json:"min_confidence"`
			IncludeHistory bool    `json:"include_history"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return fail(err)
		}
		records, err := a.store.Recall(RecallQuery{
			Query: in.Query, Subject: in.Subject, Predicate: in.Predicate,
			Kind: in.Kind, MinConfidence: in.MinConfidence, IncludeHistory: in.IncludeHistory,
		})
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"count": len(records), "records": records})

	case "forget":
		var in struct {
			Subject   string `json:"subject"`
			Predicate string `json:"predicate"`
			Value     string `json:"value"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return fail(err)
		}
		n, err := a.store.Forget(in.Subject, in.Predicate, in.Value)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"deleted": n})

	default:
		return fail(fmt.Errorf("unknown tool %q", name))
	}
}

// Turn sends one user message and drives the tool loop until Claude produces
// a final reply, printing every tool call and result along the way.
func (a *Agent) Turn(ctx context.Context, userText string) (string, error) {
	a.messages = append(a.messages, anthropic.NewUserMessage(anthropic.NewTextBlock(userText)))

	for {
		resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(a.model),
			MaxTokens: 16000,
			System:    []anthropic.TextBlockParam{{Text: a.system}},
			Messages:  a.messages,
			Tools:     tools(),
		})
		if err != nil {
			// Drop the failed turn from history so the conversation can continue.
			a.messages = a.messages[:len(a.messages)-1]
			return "", err
		}
		a.messages = append(a.messages, resp.ToParam())

		var replies []string
		var results []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				replies = append(replies, v.Text)
			case anthropic.ToolUseBlock:
				raw := []byte(v.JSON.Input.Raw())
				fmt.Printf("%s  %s→ %s(%s)%s\n", dim, cyan, v.Name, compactJSON(raw), reset)
				result, isErr := a.dispatch(v.Name, raw)
				marker := "←"
				if isErr {
					marker = "← error:"
				}
				fmt.Printf("%s  %s %s%s\n", dim, marker, compactJSON([]byte(result)), reset)
				results = append(results, anthropic.NewToolResultBlock(v.ID, result, isErr))
			}
		}

		if resp.StopReason == anthropic.StopReasonRefusal {
			return "", fmt.Errorf("the model declined this request (%s)", resp.StopDetails.Category)
		}
		if resp.StopReason != anthropic.StopReasonToolUse {
			return strings.Join(replies, "\n"), nil
		}
		a.messages = append(a.messages, anthropic.NewUserMessage(results...))
	}
}

// compactJSON renders JSON on one line for the tool trace; non-JSON (like a
// validation error string) passes through untouched.
func compactJSON(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}
