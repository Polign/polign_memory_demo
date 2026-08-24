package main

import (
	"context"
	"encoding/json"
	"fmt"
)

const (
	dim   = "\x1b[2m"
	cyan  = "\x1b[36m"
	reset = "\x1b[0m"
)

// Agent is one provider's conversation loop over the shared toolbox. Both
// implementations are written out by hand on purpose: every tool call and
// result is printed, so the audience sees the whole path (model, typed tool,
// validation, database) instead of a chatbot that magically remembers.
type Agent interface {
	// Turn sends one user message and drives the tool loop until the model
	// produces a final reply.
	Turn(ctx context.Context, userText string) (string, error)
	// Reset clears the conversation. The memory store is untouched.
	Reset()
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

// toolSpec is one tool in provider-neutral form; each agent renders it into
// its SDK's tool type. The JSON schemas are the first layer of enforcement;
// the store's validation is the second.
type toolSpec struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

func toolSpecs() []toolSpec {
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

	return []toolSpec{
		{
			Name:        "remember_fact",
			Description: "Store one durable fact as a typed record. Single-valued predicates supersede any previous value; the result reports what was replaced.",
			Properties:  rememberProps,
			Required:    rememberRequired,
		},
		{
			Name:        "remember_preference",
			Description: "Store one durable preference as a typed record. Same semantics as remember_fact.",
			Properties:  rememberProps,
			Required:    rememberRequired,
		},
		{
			Name:        "recall",
			Description: "Query the memory store. Pass query for an open-ended semantic search, or structural filters (subject, predicate, kind, min_confidence) for an exact lookup; both can be combined. Only active records return unless include_history is true.",
			Properties: map[string]any{
				"query":           map[string]any{"type": "string", "description": "Free-text semantic query (e.g. \"dev environment setup\")"},
				"subject":         map[string]any{"type": "string"},
				"predicate":       map[string]any{"type": "string"},
				"kind":            map[string]any{"type": "string", "enum": []string{"fact", "preference"}},
				"min_confidence":  map[string]any{"type": "number"},
				"include_history": map[string]any{"type": "boolean", "description": "Also return superseded records, with what replaced them"},
			},
		},
		{
			Name:        "forget",
			Description: "Permanently delete records for a subject and predicate. With value set, only that record; without it, every record including history.",
			Properties: map[string]any{
				"subject":   map[string]any{"type": "string"},
				"predicate": map[string]any{"type": "string"},
				"value":     map[string]any{"type": "string"},
			},
			Required: []string{"subject", "predicate"},
		},
	}
}

// toolbox runs tool calls against the store and prints the trace both agents
// share.
type toolbox struct {
	store *Store
}

// run executes one tool call, printing it and its result. Errors return as
// (message, true) so the model can self-repair against the store's validation.
func (tb *toolbox) run(name string, input []byte) (string, bool) {
	fmt.Printf("%s  %s→ %s(%s)%s\n", dim, cyan, name, compactJSON(input), reset)
	result, isErr := tb.dispatch(name, input)
	marker := "←"
	if isErr {
		marker = "← error:"
	}
	fmt.Printf("%s  %s %s%s\n", dim, marker, compactJSON([]byte(result)), reset)
	return result, isErr
}

func (tb *toolbox) dispatch(name string, input []byte) (string, bool) {
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
		res, err := tb.store.Remember(kind, in.Subject, in.Predicate, in.Value, in.Confidence, in.Source)
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
		records, err := tb.store.Recall(RecallQuery{
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
		n, err := tb.store.Forget(in.Subject, in.Predicate, in.Value)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"deleted": n})

	default:
		return fail(fmt.Errorf("unknown tool %q", name))
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
