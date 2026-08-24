package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Polign/polign_memory_demo/memkit"
)

// inspectorFake serves just the List route with a canned supersession pair:
// vim (superseded) replaced by neovim (active).
func inspectorFake(t *testing.T) *memkit.Store {
	t.Helper()
	vectors := `{"vectors":[
		{"id":"m-old000000000","values":[1],"metadata":{"kind":"preference","subject":"user","predicate":"prefers_editor","value":"vim","confidence":1,"source":"user_stated","status":"superseded","superseded_by":"m-new000000000","observed_at":"2026-08-23T10:00:00Z"}},
		{"id":"m-new000000000","values":[1],"metadata":{"kind":"preference","subject":"user","predicate":"prefers_editor","value":"neovim","confidence":1,"source":"user_stated","status":"active","superseded_by":"","observed_at":"2026-08-23T11:00:00Z"}},
		{"id":"m-goal00000000","values":[1],"metadata":{"kind":"fact","subject":"user","predicate":"daily_step_goal","value":9000,"confidence":1,"source":"user_stated","status":"active","superseded_by":"","observed_at":"2026-08-23T10:30:00Z"}}
	],"total":3}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/vectors") {
			http.Error(w, `{"error":"unexpected route"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vectors))
	}))
	t.Cleanup(srv.Close)

	registry, err := memkit.LoadRegistry(defaultPredicates)
	if err != nil {
		t.Fatal(err)
	}
	embed := func(string) []float32 { return []float32{1} }
	return memkit.NewStore(memkit.NewPolignClient(srv.URL), "memories", registry, embed)
}

func TestInspectorRendersSupersession(t *testing.T) {
	store := inspectorFake(t)
	rec := httptest.NewRecorder()
	inspectorHandler(store, "memories").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"vim", "neovim", "9000",
		`class="superseded"`,
		`href="#m-new000000000"`,
		`id="m-new000000000"`,
		"3 records",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestInspectorRejectsOtherPaths(t *testing.T) {
	store := inspectorFake(t)
	rec := httptest.NewRecorder()
	inspectorHandler(store, "memories").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/delete-everything", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
