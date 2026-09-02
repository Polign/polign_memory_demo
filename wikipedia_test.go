package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Polign/polign_memory_demo/memkit"
)

func TestWikipediaSearchUsesSeparateColdCollection(t *testing.T) {
	var path string
	var body map[string]any
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":[{"id":"wiki-1","score":4.2,"metadata":{"title":"Bee","url":"https://en.wikipedia.org/wiki/Bee","text":"Bees are insects."}}]}`))
	}))
	defer node.Close()

	wiki := newWikipediaSearch(memkit.NewPolignClient(node.URL), "wikipedia_bge", "", 384, 8)
	results, err := wiki.Search("what are bees", 3)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/collections/wikipedia_bge/query" {
		t.Fatalf("path = %q", path)
	}
	if body["text"] != "what are bees" || body["cold"] != true || body["k"] != float64(3) {
		t.Fatalf("query body = %#v", body)
	}
	if _, ok := body["values"]; ok {
		t.Fatalf("lexical query unexpectedly included an embedding: %#v", body)
	}
	if len(results) != 1 || results[0].Title != "Bee" || results[0].URL == "" || results[0].Text != "Bees are insects." {
		t.Fatalf("results = %#v", results)
	}
}

func TestWikipediaSearchUsesBGEEmbeddingForSemanticQuery(t *testing.T) {
	var embeddedText string
	embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		embeddedText = in["text"]
		_, _ = w.Write([]byte(`{"values":[0.1,0.2,0.3]}`))
	}))
	defer embed.Close()

	var body map[string]any
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"hits":[]}`))
	}))
	defer node.Close()

	wiki := newWikipediaSearch(memkit.NewPolignClient(node.URL), "wikipedia_bge", embed.URL, 3, 8)
	if _, err := wiki.Search("who invented the telephone", 5); err != nil {
		t.Fatal(err)
	}
	if embeddedText != "who invented the telephone" {
		t.Fatalf("embedder got %q", embeddedText)
	}
	if !reflect.DeepEqual(body["values"], []any{0.1, 0.2, 0.3}) || body["nprobe"] != float64(8) {
		t.Fatalf("semantic query body = %#v", body)
	}
	if _, ok := body["text"]; ok {
		t.Fatalf("semantic query unexpectedly included a lexical leg: %#v", body)
	}
}

type stubWikipedia struct {
	query string
	limit int
}

func (s *stubWikipedia) Collection() string { return "wikipedia_bge" }

func (s *stubWikipedia) Search(query string, limit int) ([]WikipediaResult, error) {
	s.query, s.limit = query, limit
	return []WikipediaResult{{Title: "Go", URL: "https://en.wikipedia.org/wiki/Go_(programming_language)", Text: "Go is a programming language."}}, nil
}

func TestWikipediaToolDispatchDoesNotUseMemoryRecall(t *testing.T) {
	wiki := &stubWikipedia{}
	tb := &toolbox{wikipedia: wiki}
	result, isErr := tb.dispatch("search_wikipedia", []byte(`{"query":"Go language","limit":4}`))
	if isErr {
		t.Fatal(result)
	}
	if wiki.query != "Go language" || wiki.limit != 4 {
		t.Fatalf("search = %q, %d", wiki.query, wiki.limit)
	}
	if !strings.Contains(result, "en.wikipedia.org") || !strings.Contains(result, `"passages"`) {
		t.Fatalf("tool result = %s", result)
	}
}

func TestWikipediaSearchDefaultsToThreePassages(t *testing.T) {
	var body map[string]any
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"hits":[]}`))
	}))
	defer node.Close()

	wiki := newWikipediaSearch(memkit.NewPolignClient(node.URL), "wikipedia_bge", "", 384, 8)
	if _, err := wiki.Search("default limit", 0); err != nil {
		t.Fatal(err)
	}
	if body["k"] != float64(3) {
		t.Fatalf("default k = %#v, want 3", body["k"])
	}
	if _, err := wiki.Search("capped limit", 10); err != nil {
		t.Fatal(err)
	}
	if body["k"] != float64(3) {
		t.Fatalf("capped k = %#v, want 3", body["k"])
	}
}

func TestWikipediaRetrievalSourceRequiresSuccessfulToolCall(t *testing.T) {
	tb := &toolbox{wikipedia: &stubWikipedia{}}
	if got := tb.retrievalSource("search_wikipedia", false); got != "wikipedia_bge" {
		t.Fatalf("source = %q", got)
	}
	if got := tb.retrievalSource("search_wikipedia", true); got != "" {
		t.Fatalf("failed search source = %q, want empty", got)
	}
	if got := tb.retrievalSource("recall", false); got != "" {
		t.Fatalf("recall source = %q, want empty", got)
	}
}

func TestWikipediaPromptKeepsKnowledgeSeparateFromMemory(t *testing.T) {
	registry, err := memkit.LoadRegistry([]byte(`{"likes":{"cardinality":"multi","value_type":"string","description":"things liked"}}`))
	if err != nil {
		t.Fatal(err)
	}
	prompt := systemPrompt(registry, true)
	for _, want := range []string{"search_wikipedia", "article URL", "never write Wikipedia", "passages into memory"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
