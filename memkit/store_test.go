package memkit

import (
	"context"
	"encoding/json"
	"github.com/Polign/recall"
	recallpolign "github.com/Polign/recall/polign"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
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
	mu       sync.Mutex
	recs     map[string]StoredVector // id -> record
	coldList bool
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
			if in, hasIn := op["$in"].([]any); hasIn {
				found := false
				for _, candidate := range in {
					if candidate == got {
						found = true
						break
					}
				}
				if !found {
					return false
				}
				continue
			}
			num, isNum := got.(float64)
			if !isNum {
				return false
			}
			if gte, ok := op["$gte"].(float64); ok && num < gte {
				return false
			}
			if lte, ok := op["$lte"].(float64); ok && num > lte {
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
			out := make([]StoredVector, 0)
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
			if f.coldList {
				w.WriteHeader(http.StatusNotImplemented)
				writeJSON(w, map[string]any{"error": "listing is not supported for a cold-served resource: its records live in segments"})
				return
			}
			filter := parseFilter(r.URL.Query().Get("filter"))
			out := make([]StoredVector, 0)
			for _, rec := range f.recs {
				if f.matches(rec.Metadata, filter) {
					out = append(out, rec)
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
			total := len(out)
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			offset = min(offset, len(out))
			out = out[offset:]
			if limit > 0 && len(out) > limit {
				out = out[:limit]
			}
			writeJSON(w, map[string]any{"vectors": out, "total": total})

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

// testPredicates is a small registry covering every shape the tests need:
// single- and multi-valued predicates, and each value type.
const testPredicates = `{
	"prefers_editor":  {"cardinality": "single", "value_type": "string",  "description": "preferred editor"},
	"works_at":        {"cardinality": "single", "value_type": "string",  "description": "current employer"},
	"lives_in":        {"cardinality": "single", "value_type": "string",  "description": "current city"},
	"daily_step_goal": {"cardinality": "single", "value_type": "number",  "description": "daily step goal"},
	"uses_dark_mode":  {"cardinality": "single", "value_type": "boolean", "description": "dark mode on"},
	"likes":           {"cardinality": "multi",  "value_type": "string",  "description": "something liked"}
}`

func newTestStore(t *testing.T) (*Store, *fakePolign) {
	t.Helper()
	fake := newFakePolign()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	registry, err := LoadRegistry([]byte(testPredicates))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := recallpolign.New(recallpolign.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(backend, "memories", registry, recall.EmbedFunc(func(ctx context.Context, text string) ([]float32, error) { return stubEmbed(text), ctx.Err() }))
	if err != nil {
		t.Fatal(err)
	}
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

func TestIncompleteColdHistoryRefusesReadAndWrite(t *testing.T) {
	store, fake := newTestStore(t)
	if _, err := store.Remember("fact", "user", "lives_in", "Seattle", 0, ""); err != nil {
		t.Fatal(err)
	}
	fake.coldList = true
	if _, err := store.Recall(RecallQuery{Subject: "user", Predicate: "lives_in"}); err == nil {
		t.Fatal("incomplete history must fail")
	}
	if _, err := store.Remember("fact", "user", "lives_in", "Portland", 0, ""); err == nil {
		t.Fatal("write without complete history must fail")
	}
	if len(fake.recs) != 1 {
		t.Fatal("failed write changed storage")
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

func TestForgetAppendsRetraction(t *testing.T) {
	store, fake := newTestStore(t)

	if _, err := store.Remember("preference", "anup", "prefers_editor", "vim", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("preference", "anup", "prefers_editor", "neovim", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("preference", "anup", "likes", "coffee", 0, ""); err != nil {
		t.Fatal(err)
	}
	n, err := store.Forget("anup", "prefers_editor", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("forget should withdraw one current belief, got %d", n)
	}
	// A withdrawal appends an event and preserves the earlier assertions.
	if len(fake.recs) != 4 {
		t.Fatalf("expected three assertions and a retraction, got %d", len(fake.recs))
	}

	// Current exact and search results exclude the withdrawn preference.
	for _, q := range []RecallQuery{
		{Subject: "anup", Predicate: "prefers_editor"},
		{Query: "anup prefers editor neovim", Subject: "anup", Predicate: "prefers_editor"},
	} {
		records, err := store.Recall(q)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 0 {
			t.Fatalf("recall %+v surfaced forgotten records: %+v", q, records)
		}
	}

	// Forgetting again is a no-op, not a double count.
	n, err = store.Forget("anup", "prefers_editor", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second forget should be a no-op, forgot %d", n)
	}

	// Re-remembering the same statement revives it.
	res, err := store.Remember("preference", "anup", "prefers_editor", "vim", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Existing {
		t.Fatalf("reviving a forgotten record should be a fresh write, got %+v", res)
	}
	active, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "prefers_editor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Value != "vim" {
		t.Fatalf("revived record missing: %+v", active)
	}
}

func TestForgetSingleValue(t *testing.T) {
	store, _ := newTestStore(t)

	if _, err := store.Remember("preference", "anup", "likes", "coffee", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("preference", "anup", "likes", "tea", 0, ""); err != nil {
		t.Fatal(err)
	}
	n, err := store.Forget("anup", "likes", "coffee")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("value-scoped forget should hit one record, forgot %d", n)
	}
	records, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "likes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Value != "tea" {
		t.Fatalf("only tea should remain, got %+v", records)
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
	if _, err := LoadRegistry([]byte(`{"ok_name": {"cardinality": "single", "value_type": "date"}}`)); err == nil {
		t.Error("unknown value_type should be rejected")
	}
}

func TestValueTypesEnforced(t *testing.T) {
	store, _ := newTestStore(t)

	// A number predicate rejects a quoted number; the error names the
	// expected type so the model can self-repair.
	_, err := store.Remember("fact", "anup", "daily_step_goal", "8000", 0, "")
	if err == nil || !strings.Contains(err.Error(), "expects a number") {
		t.Fatalf("string into number predicate should be rejected with the expected type, got: %v", err)
	}
	if _, err := store.Remember("fact", "anup", "daily_step_goal", 8000.0, 0, ""); err != nil {
		t.Fatalf("number value rejected: %v", err)
	}

	// A boolean predicate rejects strings and accepts booleans.
	if _, err := store.Remember("preference", "anup", "uses_dark_mode", "yes", 0, ""); err == nil {
		t.Fatal("string into boolean predicate should be rejected")
	}
	if _, err := store.Remember("preference", "anup", "uses_dark_mode", true, 0, ""); err != nil {
		t.Fatalf("boolean value rejected: %v", err)
	}

	// A string predicate rejects a number.
	if _, err := store.Remember("preference", "anup", "prefers_editor", 7.0, 0, ""); err == nil {
		t.Fatal("number into string predicate should be rejected")
	}

	// Typed values round-trip as their types.
	records, err := store.Recall(RecallQuery{Subject: "anup", Predicate: "daily_step_goal"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one step goal, got %+v", records)
	}
	if v, ok := records[0].Value.(float64); !ok || v != 8000 {
		t.Fatalf("number value did not round-trip typed: %#v", records[0].Value)
	}
}

func TestNumberValuesSupersedeAndDedupe(t *testing.T) {
	store, fake := newTestStore(t)

	if _, err := store.Remember("fact", "anup", "daily_step_goal", 8000.0, 0, ""); err != nil {
		t.Fatal(err)
	}
	dup, err := store.Remember("fact", "anup", "daily_step_goal", 8000.0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Existing {
		t.Fatalf("same number twice should be idempotent, got %+v", dup)
	}
	res, err := store.Remember("fact", "anup", "daily_step_goal", 10000.0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Superseded) != 1 || res.Superseded[0].Value.(float64) != 8000 {
		t.Fatalf("new number should supersede the old one, got %+v", res)
	}
	if len(fake.recs) != 2 {
		t.Fatalf("store should hold active + superseded, has %d", len(fake.recs))
	}
}

func TestValueRangeRecall(t *testing.T) {
	store, _ := newTestStore(t)

	if _, err := store.Remember("fact", "anup", "daily_step_goal", 9000.0, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("fact", "mira", "daily_step_goal", 4000.0, 0, ""); err != nil {
		t.Fatal(err)
	}

	lo := 8000.0
	records, err := store.Recall(RecallQuery{Predicate: "daily_step_goal", ValueMin: &lo})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Subject != "anup" {
		t.Fatalf("value_min filter should keep only anup, got %+v", records)
	}

	hi := 5000.0
	records, err = store.Recall(RecallQuery{Predicate: "daily_step_goal", ValueMax: &hi})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Subject != "mira" {
		t.Fatalf("value_max filter should keep only mira, got %+v", records)
	}
}

func TestForgetParsesTypedValues(t *testing.T) {
	store, _ := newTestStore(t)

	if _, err := store.Remember("fact", "anup", "daily_step_goal", 9000.0, 0, ""); err != nil {
		t.Fatal(err)
	}
	n, err := store.Forget("anup", "daily_step_goal", "9000")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("typed forget-by-value should match the number record, forgot %d", n)
	}
	if _, err := store.Forget("anup", "daily_step_goal", "not a number"); err == nil {
		t.Fatal("unparseable value for a number predicate should be rejected")
	}
}

func TestReassertionHasNewEventAndPreservesOldRecord(t *testing.T) {
	store, fake := newTestStore(t)
	first, err := store.Remember("preference", "user", "prefers_editor", "vim", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	original := fake.recs[first.Stored.ID]
	if _, err := store.Forget("user", "prefers_editor", "vim"); err != nil {
		t.Fatal(err)
	}
	next, err := store.Remember("preference", "user", "prefers_editor", "vim", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Stored.ID == first.Stored.ID {
		t.Fatal("reassertion overwrote the earlier event")
	}
	if !reflect.DeepEqual(original, fake.recs[first.Stored.ID]) {
		t.Fatal("old assertion was changed")
	}
	history, err := store.Recall(RecallQuery{Subject: "user", Predicate: "prefers_editor", IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].Status != "historical" || history[1].Status != "withdrawal" || history[2].Status != "active" {
		t.Fatalf("history: %+v", history)
	}
}

func TestLegacyRowsAreRejectedWithoutBeingRewritten(t *testing.T) {
	store, fake := newTestStore(t)
	old := StoredVector{ID: "old", Values: []float32{1}, Metadata: map[string]any{"subject": "user", "predicate": "prefers_editor", "status": "superseded", "value": "vim"}}
	fake.recs[old.ID] = old
	if err := store.Check(t.Context()); err == nil || !strings.Contains(err.Error(), "legacy memkit") {
		t.Fatalf("legacy startup: %v", err)
	}
	if _, err := store.Remember("preference", "user", "prefers_editor", "neovim", 1, ""); err == nil {
		t.Fatal("legacy history allowed a write")
	}
	if len(fake.recs) != 1 || !reflect.DeepEqual(fake.recs[old.ID], old) {
		t.Fatal("legacy collection changed")
	}
}

func TestHistoryUsesRecallAsOfAndKeepsWithdrawals(t *testing.T) {
	store, _ := newTestStore(t)
	first, err := store.Remember("preference", "user", "prefers_editor", "vim", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("preference", "user", "prefers_editor", "neovim", 1, ""); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, first.Stored.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.Recall(RecallQuery{Subject: "user", Predicate: "prefers_editor", AsOf: at})
	if err != nil || len(rows) != 1 || rows[0].Value != "vim" {
		t.Fatalf("as-of: %+v, %v", rows, err)
	}
	if _, err := store.Forget("user", "prefers_editor", ""); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Recall(RecallQuery{Subject: "user", Predicate: "prefers_editor", IncludeHistory: true})
	if err != nil || len(rows) != 3 || rows[2].Status != "withdrawal" {
		t.Fatalf("withdrawal: %+v, %v", rows, err)
	}
	if _, err := store.Recall(RecallQuery{IncludeHistory: true, Limit: 1}); err == nil {
		t.Fatal("inspector silently truncated history")
	}
}
