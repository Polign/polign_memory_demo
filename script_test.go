package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// stubAgent records what the repl dispatches to it.
type stubAgent struct {
	turns  []string
	resets int
}

func (a *stubAgent) Turn(_ context.Context, text string) (string, error) {
	a.turns = append(a.turns, text)
	return "ok", nil
}

func (a *stubAgent) Reset() { a.resets++ }

func TestLoadScriptSkipsBlanksAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script.txt")
	content := "# a comment\n\nfirst line\n  second line  \n\n# another\nthird line\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := loadScript(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first line", "second line", "third line"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
}

func TestLoadScriptRejectsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, []byte("# only a comment\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadScript(path); err == nil {
		t.Fatal("expected an error for a script with no lines")
	}
}

func TestReplReplaysScriptInOrder(t *testing.T) {
	agent := &stubAgent{}
	next := scriptSource([]string{"one", "two", "three"}, 0, 0)
	if err := repl(agent, "claude", next); err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two", "three"}
	if !reflect.DeepEqual(agent.turns, want) {
		t.Fatalf("turns = %q, want %q", agent.turns, want)
	}
}

func TestReplHandlesSlashCommands(t *testing.T) {
	agent := &stubAgent{}
	next := scriptSource([]string{"/reset", "hello", "/quit", "never sent"}, 0, 0)
	if err := repl(agent, "claude", next); err != nil {
		t.Fatal(err)
	}
	if agent.resets != 1 {
		t.Fatalf("resets = %d, want 1", agent.resets)
	}
	if want := []string{"hello"}; !reflect.DeepEqual(agent.turns, want) {
		t.Fatalf("turns = %q, want %q", agent.turns, want)
	}
}
