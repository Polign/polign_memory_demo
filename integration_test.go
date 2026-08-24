package main

import (
	"os"
	"testing"
	"time"
)

// Integration tests against a real polign_db server, opt-in via
// POLIGN_MEMORY_DEMO_URL (e.g. http://127.0.0.1:24100). They use the stub
// embedder: the contract under test is store <-> server, not the model.
//
// The durability script drives them in two phases:
//
//	POLIGN_MEMORY_DEMO_URL=... go test -run TestIntegrationWrite
//	(kill the server, restart it from the same -store)
//	POLIGN_MEMORY_DEMO_URL=... go test -run TestIntegrationRecallAfterRestart
func itStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("POLIGN_MEMORY_DEMO_URL")
	if url == "" {
		t.Skip("set POLIGN_MEMORY_DEMO_URL to run integration tests")
	}
	db := NewPolignClient(url)
	if !db.Healthy() {
		t.Fatalf("no polign_db server at %s", url)
	}
	registry, err := LoadRegistry(defaultPredicates)
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(db, "it-memories", registry, stubEmbed)
}

func TestIntegrationWrite(t *testing.T) {
	store := itStore(t)

	// Clean slate so the test is rerunnable.
	if _, err := store.Forget("anup", "prefers_editor", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Forget("anup", "likes", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Forget("anup", "daily_step_goal", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Remember("preference", "anup", "prefers_editor", "vim", 0, ""); err != nil {
		t.Fatal(err)
	}
	res, err := store.Remember("preference", "anup", "prefers_editor", "neovim", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Superseded) != 1 || res.Superseded[0].Value != "vim" {
		t.Fatalf("expected vim superseded, got %+v", res)
	}
	if _, err := store.Remember("preference", "anup", "likes", "coffee", 0.9, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("fact", "anup", "daily_step_goal", 9000.0, 0, ""); err != nil {
		t.Fatal(err)
	}

	// Read-your-writes: the record is visible immediately after the ack.
	assertMemoryState(t, store)
}

func TestIntegrationRecallAfterRestart(t *testing.T) {
	store := itStore(t)
	// The server may still be priming from the bucket right after boot.
	deadline := time.Now().Add(15 * time.Second)
	for {
		records, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "prefers_editor"})
		if err == nil && len(records) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("memories not visible after restart: records=%v err=%v", records, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	assertMemoryState(t, store)
}

// assertMemoryState checks the invariants both phases share: one active
// editor preference (neovim), a superseded vim linked to it, the multi-valued
// like intact, and semantic recall returning only active records.
func assertMemoryState(t *testing.T, store *Store) {
	t.Helper()

	active, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "prefers_editor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Value != "neovim" || active[0].Status != "active" {
		t.Fatalf("active editor preference wrong: %+v", active)
	}

	history, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "prefers_editor", IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history should hold vim and neovim, got %+v", history)
	}
	for _, rec := range history {
		if rec.Value == "vim" && (rec.Status != "superseded" || rec.SupersededBy != active[0].ID) {
			t.Fatalf("vim record not linked to its replacement: %+v", rec)
		}
	}

	likes, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "likes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(likes) != 1 || likes[0].Value != "coffee" || likes[0].Confidence != 0.9 {
		t.Fatalf("multi-valued record wrong (typed confidence must survive): %+v", likes)
	}

	// Typed values survive as their types, and range filters compare
	// numerically in the database.
	goals, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "daily_step_goal"})
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 {
		t.Fatalf("expected one step goal, got %+v", goals)
	}
	if v, ok := goals[0].Value.(float64); !ok || v != 9000 {
		t.Fatalf("number value did not round-trip typed through the server: %#v", goals[0].Value)
	}
	lo, hi := 8000.0, 10000.0
	inRange, err := store.Recall(RecallQuery{Predicate: "daily_step_goal", ValueMin: &lo, ValueMax: &hi})
	if err != nil {
		t.Fatal(err)
	}
	if len(inRange) != 1 {
		t.Fatalf("numeric range filter should match the goal, got %+v", inRange)
	}
	tooHigh := 9500.0
	outOfRange, err := store.Recall(RecallQuery{Predicate: "daily_step_goal", ValueMin: &tooHigh})
	if err != nil {
		t.Fatal(err)
	}
	if len(outOfRange) != 0 {
		t.Fatalf("range filter matched below its bound: %+v", outOfRange)
	}

	semantic, err := store.Recall(RecallQuery{Query: "anup prefers editor neovim"})
	if err != nil {
		t.Fatal(err)
	}
	if len(semantic) == 0 {
		t.Fatal("semantic recall returned nothing")
	}
	for _, rec := range semantic {
		if rec.Status != "active" {
			t.Fatalf("semantic recall leaked a superseded record: %+v", rec)
		}
	}
}
