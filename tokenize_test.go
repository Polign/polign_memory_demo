package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The golden file pins this tokenizer port to the reference model2vec
// pipeline. Regenerate it from the real downloaded model with:
//
//	POLIGN_MEMORY_DEMO_MODEL_DIR="$HOME/Library/Caches/polign-memory-demo" \
//	  go test -run TestGoldenAgainstRealModel -update
var updateGolden = flag.Bool("update", false, "rewrite testdata/golden.json from the real model")

const goldenPath = "testdata/golden.json"

// goldenInputs is the source of truth for what the golden file covers; the
// -update pass tokenizes exactly these.
var goldenInputs = []string{
	"user prefers editor neovim",
	"user daily step goal 9000",
	"I use Vim as my editor.",
	"Café au lait",
	"naïve résumé",
	`he said: "hello, world!"`,
	"漢字 and kana",
	"supercalifragilisticexpialidocious antidisestablishmentarianism",
	"polign_db is a vector database",
	"MIXED Case text",
	"$5.99 or 8,000 steps",
	"emoji 🙂 in text",
	"tabs\tand\nnewlines",
	"it's read-your-writes",
}

type goldenCase struct {
	Text   string   `json:"text"`
	Tokens []string `json:"tokens"`
}

func TestNormalizeBert(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Café", "cafe"},
		{"naïve RÉSUMÉ", "naive resume"},
		{"hello\tworld\nagain", "hello world again"},
		{"ab", "ab"},
		{"abc漢def", "abc 漢 def"},
		{"MiXeD", "mixed"},
	}
	for _, c := range cases {
		if got := normalizeBert(c.in); got != c.want {
			t.Errorf("normalizeBert(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPreTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"hello, world!", []string{"hello", ",", "world", "!"}},
		{"it's $5", []string{"it", "'", "s", "$", "5"}},
		{"read-your-writes", []string{"read", "-", "your", "-", "writes"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
	}
	for _, c := range cases {
		if got := preTokenize(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("preTokenize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsPunct(t *testing.T) {
	for _, r := range "$`!,." {
		if !isPunct(r) {
			t.Errorf("isPunct(%q) = false, want true", r)
		}
	}
	if !isPunct('。') {
		t.Error("isPunct('。') = false, want true")
	}
	for _, r := range "a5 " {
		if isPunct(r) {
			t.Errorf("isPunct(%q) = true, want false", r)
		}
	}
}

func TestWordPiece(t *testing.T) {
	m := &Model{
		vocab:        map[string]int{"[UNK]": 0, "un": 1, "##aff": 2, "##able": 3, "hello": 4},
		unkID:        0,
		maxWordChars: 100,
	}
	cases := []struct {
		word string
		want []int
	}{
		{"unaffable", []int{1, 2, 3}},
		{"hello", []int{4}},
		{"xyz", []int{0}},
		{"unxyz", []int{0}}, // matchable prefix, unmatchable remainder: one unk
	}
	for _, c := range cases {
		if got := m.wordPiece(nil, c.word); !reflect.DeepEqual(got, c.want) {
			t.Errorf("wordPiece(%q) = %v, want %v", c.word, got, c.want)
		}
	}
	m.maxWordChars = 5
	if got := m.wordPiece(nil, "unaffable"); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("over-long word = %v, want [0]", got)
	}
}

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("golden file is empty")
	}
	return cases
}

// TestGoldenTokenize runs offline on every test invocation. It rebuilds a
// model whose vocabulary is exactly the pieces the goldens expect (plus
// [UNK]); a greedy longest-match tokenizer over a subset vocabulary that
// contains every expected piece must reproduce the reference segmentation,
// so this pins the whole normalize, pretokenize, wordpiece pipeline without
// the model download.
func TestGoldenTokenize(t *testing.T) {
	cases := loadGolden(t)
	vocab := map[string]int{"[UNK]": 0}
	for _, c := range cases {
		for _, tok := range c.Tokens {
			if _, ok := vocab[tok]; !ok {
				vocab[tok] = len(vocab)
			}
		}
	}
	m := &Model{vocab: vocab, unkID: 0, maxWordChars: 100}
	pieces := make(map[int]string, len(vocab))
	for tok, id := range vocab {
		pieces[id] = tok
	}
	for _, c := range cases {
		var got []string
		for _, id := range m.tokenize(c.Text) {
			got = append(got, pieces[id])
		}
		if !reflect.DeepEqual(got, c.Tokens) {
			t.Errorf("tokenize(%q) = %q, want %q", c.Text, got, c.Tokens)
		}
	}
}

// TestGoldenAgainstRealModel is the true pin against the shipped vocabulary.
// It needs the downloaded model, so it is opt-in; with -update it rewrites
// the golden file instead of checking it.
func TestGoldenAgainstRealModel(t *testing.T) {
	dir := os.Getenv("POLIGN_MEMORY_DEMO_MODEL_DIR")
	if dir == "" {
		t.Skip("set POLIGN_MEMORY_DEMO_MODEL_DIR to run against the real model")
	}
	m, err := LoadModel(dir)
	if err != nil {
		t.Fatal(err)
	}
	pieces := make(map[int]string, len(m.vocab))
	for tok, id := range m.vocab {
		pieces[id] = tok
	}
	tokenizeToPieces := func(text string) []string {
		var out []string
		for _, id := range m.tokenize(text) {
			out = append(out, pieces[id])
		}
		return out
	}

	if *updateGolden {
		cases := make([]goldenCase, len(goldenInputs))
		for i, text := range goldenInputs {
			cases[i] = goldenCase{Text: text, Tokens: tokenizeToPieces(text)}
		}
		raw, err := json.MarshalIndent(cases, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s with %d cases", goldenPath, len(cases))
		return
	}

	for _, c := range loadGolden(t) {
		if got := tokenizeToPieces(c.Text); !reflect.DeepEqual(got, c.Tokens) {
			t.Errorf("tokenize(%q) = %q, want %q", c.Text, got, c.Tokens)
		}
	}
}
