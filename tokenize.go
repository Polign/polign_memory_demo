package main

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// tokenize splits text into WordPiece token ids exactly the way the reference
// tokenizers library does for a BertNormalizer(clean_text, handle_chinese_chars,
// lowercase) + BertPreTokenizer + WordPiece pipeline. Golden tests against
// model2vec output (testdata/golden.json) pin this port to the reference.
func (m *Model) tokenize(text string) []int {
	words := preTokenize(normalizeBert(text))
	// Most words map to one or two pieces; reserve accordingly.
	ids := make([]int, 0, 2*len(words))
	for _, w := range words {
		ids = m.wordPiece(ids, w)
	}
	return ids
}

// wordPiece appends the greedy longest-match pieces of one word to ids. A word
// longer than maxWordChars, or with any unmatchable remainder, becomes a single
// unknown token.
func (m *Model) wordPiece(ids []int, word string) []int {
	runes := []rune(word)
	if len(runes) > m.maxWordChars {
		return append(ids, m.unkID)
	}
	start := 0
	n := len(ids)
	for start < len(runes) {
		end := len(runes)
		id := -1
		for end > start {
			piece := string(runes[start:end])
			if start > 0 {
				piece = "##" + piece
			}
			if got, ok := m.vocab[piece]; ok {
				id = got
				break
			}
			end--
		}
		if id < 0 {
			return append(ids[:n], m.unkID)
		}
		ids = append(ids, id)
		start = end
	}
	return ids
}

// normalizeBert applies BertNormalizer: drop control characters, collapse all
// whitespace to plain spaces, pad CJK ideographs with spaces, strip combining
// accents (NFD, drop Mn), and lowercase.
func normalizeBert(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == 0 || r == 0xFFFD || isControl(r):
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case isCJK(r):
			b.WriteRune(' ')
			b.WriteRune(r)
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	var out strings.Builder
	out.Grow(b.Len())
	for _, r := range norm.NFD.String(b.String()) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		out.WriteRune(unicode.ToLower(r))
	}
	return out.String()
}

// preTokenize splits on whitespace and isolates each punctuation character as
// its own word (BertPreTokenizer).
func preTokenize(text string) []string {
	var words []string
	var w strings.Builder
	flush := func() {
		if w.Len() > 0 {
			words = append(words, w.String())
			w.Reset()
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isPunct(r):
			flush()
			words = append(words, string(r))
		default:
			w.WriteRune(r)
		}
	}
	flush()
	return words
}

// isControl reports whether r is a control character in the BERT sense:
// category C, except tab/newline/carriage-return which count as whitespace.
func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.Is(unicode.C, r)
}

// isPunct matches BERT punctuation: all non-alphanumeric ASCII (so symbols
// like $ and ` split words too) plus Unicode category P.
func isPunct(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

// isCJK reports whether r is a CJK ideograph (the ranges BertNormalizer pads
// with spaces so each ideograph tokenizes on its own).
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x20000 && r <= 0x2A6DF,
		r >= 0x2A700 && r <= 0x2B73F,
		r >= 0x2B740 && r <= 0x2B81F,
		r >= 0x2B920 && r <= 0x2CEAF,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0x2F800 && r <= 0x2FA1F:
		return true
	}
	return false
}
