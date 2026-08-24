package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePolign implements just enough of the polign_db HTTP surface for the
// store: upsert, byte-exact batch get, filtered list, filtered vector query,
// delete. Filters support scalar equality and {"$gte": n}, the subset the
// store uses.
type fakePolign struct {
	mu   sync.Mutex
	recs map[string]StoredVector // id -> record
}

func newFakePolign() *fakePolign {
	return &fakePolign{recs: map[string]StoredVector{}}
}

func (f *fakePolign) matches(meta map[string]any, filter map[string]any) bool {
	for key, want := range filter {
		got, ok := meta[key]
		if !ok {
			return false
		}
		if op, isOp := want.(map[string]any); isOp {
			gte, hasGte := op["$gte"].(float64)
			num, isNum := got.(float64)
			if !hasGte || !isNum || num < gte {
				return false
			}
			continue
		}
		if got != want {
			return false
		}
	}
	return true
}

func (f *fakePolign) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		switch {
		case path == "/healthz":
			w.WriteHeader(http.StatusOK)

		case strings.HasSuffix(path, "/vectors:get") && r.Method == http.MethodPost:
			var in struct {
				IDs []string `json:"ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			var out []StoredVector
			for _, id := range in.IDs {
				if rec, ok := f.recs[id]; ok {
					out = append(out, rec)
				}
			}
			writeJSON(w, map[string]any{"vectors": out})

		case strings.Contains(path, "/vectors/") && r.Method == http.MethodPut:
			id := unescape(path[strings.LastIndex(path, "/")+1:])
			var in struct {
				Values   []float32      `json:"values"`
				Metadata map[string]any `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.recs[id] = StoredVector{ID: id, Values: in.Values, Metadata: in.Metadata}
			writeJSON(w, map[string]any{"id": id})

		case strings.Contains(path, "/vectors/") && r.Method == http.MethodDelete:
			id := unescape(path[strings.LastIndex(path, "/")+1:])
			_, ok := f.recs[id]
			delete(f.recs, id)
			writeJSON(w, map[string]any{"deleted": ok})

		case strings.HasSuffix(path, "/vectors") && r.Method == http.MethodGet:
			filter := parseFilter(r.URL.Query().Get("filter"))
			var out []StoredVector
			for _, rec := range f.recs {
				if f.matches(rec.Metadata, filter) {
					out = append(out, rec)
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
			writeJSON(w, map[string]any{"vectors": out, "total": len(out)})

		case strings.HasSuffix(path, "/query") && r.Method == http.MethodPost:
			var in struct {
				Values []float32      `json:"values"`
				K      int            `json:"k"`
				Filter map[string]any `json:"filter"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			var hits []Hit
			for _, rec := range f.recs {
				if !f.matches(rec.Metadata, in.Filter) {
					continue
				}
				hits = append(hits, Hit{ID: rec.ID, Distance: 1 - dot(in.Values, rec.Values), Metadata: rec.Metadata})
			}
			sort.Slice(hits, func(i, j int) bool { return hits[i].Distance < hits[j].Distance })
			if in.K > 0 && len(hits) > in.K {
				hits = hits[:in.K]
			}
			writeJSON(w, map[string]any{"hits": hits})

		default:
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": "not found: " + r.Method + " " + path})
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func parseFilter(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var f map[string]any
	_ = json.Unmarshal([]byte(raw), &f)
	return f
}

func unescape(s string) string {
	out, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return out
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range min(len(a), len(b)) {
		s += a[i] * b[i]
	}
	return s
}

// stubEmbed maps distinct texts to distinct near-orthogonal unit vectors, so
// semantic recall in tests ranks an exact text match first.
func stubEmbed(text string) []float32 {
	v := make([]float32, 8)
	h := 0
	for _, r := range text {
		h = h*31 + int(r)
	}
	for i := range v {
		h = h*1103515245 + 12345
		v[i] = float32((h>>16)%1000) / 1000
	}
	var sq float32
	for _, x := range v {
		sq += x * x
	}
	for i := range v {
		v[i] /= sqrt32(sq)
	}
	return v
}

func sqrt32(x float32) float32 {
	g := x
	for range 20 {
		g = (g + x/g) / 2
	}
	return g
}

func newTestStore(t *testing.T) (*Store, *fakePolign) {
	t.Helper()
	fake := newFakePolign()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	registry, err := LoadRegistry(defaultPredicates)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(NewPolignClient(srv.URL), "memories", registry, stubEmbed)
	tick := 0
	store.now = func() time.Time { tick++; return time.Unix(int64(1700000000+tick), 0) }
	return store, fake
}

func TestSingleValuedSupersedes(t *testing.T) {
	store, _ := newTestStore(t)

	first, err := store.Remember("preference", "Anup", "prefers_editor", "vim", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Existing || len(first.Superseded) != 0 {
		t.Fatalf("first write should be fresh, got %+v", first)
	}
	if first.Stored.Subject != "anup" {
		t.Fatalf("subject not normalized: %q", first.Stored.Subject)
	}
	if first.Stored.Confidence != 1.0 || first.Stored.Source != "user_stated" {
		t.Fatalf("defaults not applied: %+v", first.Stored)
	}

	second, err := store.Remember("preference", "anup", "prefers_editor", "neovim", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Superseded) != 1 || second.Superseded[0].Value != "vim" {
		t.Fatalf("expected vim superseded, got %+v", second.Superseded)
	}
	if second.Superseded[0].SupersededBy != second.Stored.ID {
		t.Fatalf("supersession link missing: %+v", second.Superseded[0])
	}

	active, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "prefers_editor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Value != "neovim" {
		t.Fatalf("active recall should be exactly neovim, got %+v", active)
	}

	history, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "prefers_editor", IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history should hold both records, got %+v", history)
	}
}

func TestSingleValuedIdempotent(t *testing.T) {
	store, fake := newTestStore(t)

	if _, err := store.Remember("preference", "anup", "prefers_editor", "neovim", 0, ""); err != nil {
		t.Fatal(err)
	}
	res, err := store.Remember("preference", "anup", "prefers_editor", "Neovim", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Existing {
		t.Fatalf("case-insensitive re-remember should be idempotent, got %+v", res)
	}
	if len(fake.recs) != 1 {
		t.Fatalf("store should hold one record, has %d", len(fake.recs))
	}
}

func TestMultiValuedAccumulatesAndDedupes(t *testing.T) {
	store, fake := newTestStore(t)

	if _, err := store.Remember("preference", "anup", "likes", "coffee", 0, ""); err != nil {
		t.Fatal(err)
	}
	res, err := store.Remember("preference", "anup", "likes", "tea", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Existing || len(res.Superseded) != 0 {
		t.Fatalf("multi-valued predicates must not supersede, got %+v", res)
	}
	dup, err := store.Remember("preference", "anup", "likes", "coffee", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Existing {
		t.Fatalf("duplicate multi-valued write should be idempotent, got %+v", dup)
	}
	if len(fake.recs) != 2 {
		t.Fatalf("store should hold two records, has %d", len(fake.recs))
	}
}

func TestUnknownPredicateRejected(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.Remember("preference", "anup", "editor_preference", "neovim", 0, "")
	if err == nil {
		t.Fatal("unregistered predicate must be rejected")
	}
	if !strings.Contains(err.Error(), "prefers_editor") {
		t.Fatalf("rejection should list the registry so the model can self-repair, got: %v", err)
	}
}

func TestValidation(t *testing.T) {
	store, _ := newTestStore(t)
	cases := []struct {
		name string
		call func() error
	}{
		{"bad kind", func() error {
			_, err := store.Remember("event", "anup", "likes", "x", 0, "")
			return err
		}},
		{"empty subject", func() error {
			_, err := store.Remember("fact", " ", "likes", "x", 0, "")
			return err
		}},
		{"empty value", func() error {
			_, err := store.Remember("fact", "anup", "likes", "", 0, "")
			return err
		}},
		{"bad confidence", func() error {
			_, err := store.Remember("fact", "anup", "likes", "x", 1.5, "")
			return err
		}},
		{"bad source", func() error {
			_, err := store.Remember("fact", "anup", "likes", "x", 0, "guessed")
			return err
		}},
		{"bad predicate shape", func() error {
			_, err := store.Remember("fact", "anup", "Prefers Editor", "x", 0, "")
			return err
		}},
	}
	for _, tc := range cases {
		if tc.call() == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
	}
}

func TestSemanticRecallFiltersSuperseded(t *testing.T) {
	store, _ := newTestStore(t)

	if _, err := store.Remember("preference", "anup", "prefers_editor", "vim", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("preference", "anup", "prefers_editor", "neovim", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("fact", "anup", "works_at", "polign", 0, ""); err != nil {
		t.Fatal(err)
	}

	records, err := store.Recall(RecallQuery{Query: "anup prefers editor neovim"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("semantic recall returned nothing")
	}
	for _, rec := range records {
		if rec.Status != "active" {
			t.Fatalf("semantic recall leaked a superseded record: %+v", rec)
		}
	}
	if records[0].Value != "neovim" {
		t.Fatalf("closest record should be the editor preference, got %+v", records[0])
	}
}

func TestForget(t *testing.T) {
	store, fake := newTestStore(t)

	if _, err := store.Remember("preference", "anup", "prefers_editor", "vim", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("preference", "anup", "prefers_editor", "neovim", 0, ""); err != nil {
		t.Fatal(err)
	}
	n, err := store.Forget("anup", "prefers_editor", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("forget should delete the history too, deleted %d", n)
	}
	if len(fake.recs) != 0 {
		t.Fatalf("store should be empty, has %d", len(fake.recs))
	}
}

func TestConfidenceRangeFilter(t *testing.T) {
	store, _ := newTestStore(t)

	if _, err := store.Remember("fact", "anup", "lives_in", "san francisco", 0.6, "agent_inferred"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("fact", "anup", "works_at", "polign", 1.0, ""); err != nil {
		t.Fatal(err)
	}
	records, err := store.Recall(RecallQuery{Subject: "anup", MinConfidence: 0.8})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Predicate != "works_at" {
		t.Fatalf("min_confidence filter failed: %+v", records)
	}
}

func TestRegistryRejectsBadEntries(t *testing.T) {
	if _, err := LoadRegistry([]byte(`{"BadName": {"cardinality": "single"}}`)); err == nil {
		t.Error("non-snake-case predicate name should be rejected")
	}
	if _, err := LoadRegistry([]byte(`{"ok_name": {"cardinality": "sometimes"}}`)); err == nil {
		t.Error("unknown cardinality should be rejected")
	}
}

func TestRecordIDStableAndValueCaseInsensitive(t *testing.T) {
	a := recordID("anup", "prefers_editor", "Neovim")
	b := recordID("anup", "prefers_editor", "neovim")
	if a != b {
		t.Error("record id should ignore value case")
	}
	if recordID("anup", "prefers_editor", "vim") == a {
		t.Error("different values must give different ids")
	}
	if fmt.Sprintf("%s", a)[:2] != "m-" {
		t.Errorf("unexpected id shape %q", a)
	}
}
