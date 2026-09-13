package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/Polign/polign_memory_demo/memkit"
	"github.com/Polign/recall"
)

// Only the storage contract is faked; tool validation and memory semantics run
// through the real Recall client. Provider APIs are not needed for these tests.
type toolBackend struct {
	rows map[string]recall.StoredVector
}

func (b *toolBackend) Put(ctx context.Context, _, id string, values []float32, metadata map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.rows[id] = recall.StoredVector{ID: id, Values: values, Metadata: metadata}
	return nil
}
func (b *toolBackend) List(ctx context.Context, _ string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	rows := []recall.StoredVector{}
	for _, row := range b.rows {
		match := true
		for key, value := range filter {
			if !reflect.DeepEqual(row.Metadata[key], value) {
				match = false
				break
			}
		}
		if match {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	total := len(rows)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total, nil
}
func (*toolBackend) Search(context.Context, string, []float32, int, map[string]any) ([]recall.Hit, error) {
	return nil, fmt.Errorf("search not used in these tool tests")
}

func TestRecallToolsShareMemoryWithNativeClient(t *testing.T) {
	backend := &toolBackend{rows: map[string]recall.StoredVector{}}
	registry, err := memkit.LoadRegistry(defaultPredicates)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memkit.NewStore(backend, "test", registry, recall.LexicalEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	tb := toolbox{store: store}
	call := func(name, input string) string {
		t.Helper()
		out, failed := tb.dispatch(name, []byte(input))
		if failed {
			t.Fatalf("%s: %s", name, out)
		}
		return out
	}
	call("remember_preference", `{"subject":"user","predicate":"prefers_editor","value":"vim","confidence":0}`)
	native, err := recall.NewClient(recall.Config{Backend: backend, Collection: "test", Registry: registry, Embedder: recall.LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	beliefs, err := native.Recall(t.Context(), recall.Query{Subject: "user", Predicate: "prefers_editor"})
	if err != nil || len(beliefs) != 1 || beliefs[0].Value != "vim" || beliefs[0].Confidence != 0 {
		t.Fatalf("native read: %+v, %v", beliefs, err)
	}
	if _, err := native.Remember(t.Context(), recall.RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "neovim"}); err != nil {
		t.Fatal(err)
	}
	var current struct {
		Records []memkit.Record `json:"records"`
	}
	if err := json.Unmarshal([]byte(call("recall", `{"subject":"user","predicate":"prefers_editor"}`)), &current); err != nil {
		t.Fatal(err)
	}
	if len(current.Records) != 1 || current.Records[0].Value != "neovim" {
		t.Fatalf("tool read: %+v", current)
	}
	call("forget", `{"subject":"user","predicate":"prefers_editor","all":true}`)
	var history struct {
		Events []recall.Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(call("memory_history", `{"subject":"user","predicate":"prefers_editor"}`)), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Events) != 3 || !history.Events[2].Retraction {
		t.Fatalf("history: %+v", history)
	}
	call("remember_fact", `{"subject":"user","predicate":"uses_dark_mode","value":false}`)
	call("forget", `{"subject":"user","predicate":"uses_dark_mode","value":false}`)
	call("remember_fact", `{"subject":"user","predicate":"daily_step_goal","value":0}`)
	call("forget", `{"subject":"user","predicate":"daily_step_goal","value":0}`)
	for _, input := range []string{
		`{"subject":"user","predicate":"prefers_editor"}`,
		`{"subject":"user","predicate":"prefers_editor","value":"neovim","all":true}`,
	} {
		if _, failed := tb.dispatch("forget", []byte(input)); !failed {
			t.Fatalf("ambiguous forget succeeded: %s", input)
		}
	}
	before := len(backend.rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, failed := tb.dispatchContext(ctx, "remember_fact", []byte(`{"subject":"user","predicate":"daily_step_goal","value":5000}`)); !failed || len(backend.rows) != before {
		t.Fatal("canceled tool wrote memory")
	}
}
