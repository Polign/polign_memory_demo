package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// Model is a loaded static embedding model: model2vec inference in pure Go.
// A sentence embedding is the L2-normalized mean of the text's WordPiece
// token embeddings, so free-text queries need no inference runtime, no cgo,
// and no API key. The token embedding matrix (minishlab/potion-base-8M) ships
// in the demo dataset artifact built by prepare/prepare.py, and the same
// embedder encodes both the corpus and the queries, so the two can never
// disagree on tokenization.
type Model struct {
	dim          int
	vocab        map[string]int
	vecs         []float32 // vocab_size x dim, row-major
	unkID        int
	maxWordChars int
	normalize    bool
}

// modelMeta mirrors model.json in the dataset artifact.
type modelMeta struct {
	Format               int    `json:"format"`
	Source               string `json:"source"`
	Dim                  int    `json:"dim"`
	VocabSize            int    `json:"vocab_size"`
	UnkToken             string `json:"unk_token"`
	MaxInputCharsPerWord int    `json:"max_input_chars_per_word"`
	Normalize            bool   `json:"normalize"`
}

// LoadModel reads model.json, vocab.txt, and embeddings.f32 from dir.
func LoadModel(dir string) (*Model, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "model.json"))
	if err != nil {
		return nil, fmt.Errorf("demo model: %w", err)
	}
	var meta modelMeta
	if uerr := json.Unmarshal(raw, &meta); uerr != nil {
		return nil, fmt.Errorf("demo model: parse model.json: %w", uerr)
	}
	if meta.Format != 1 {
		return nil, fmt.Errorf("demo model: unsupported format %d (update polign?)", meta.Format)
	}

	vf, err := os.Open(filepath.Join(dir, "vocab.txt"))
	if err != nil {
		return nil, fmt.Errorf("demo model: %w", err)
	}
	defer vf.Close()
	vocab := make(map[string]int, meta.VocabSize)
	sc := bufio.NewScanner(vf)
	for sc.Scan() {
		vocab[sc.Text()] = len(vocab)
	}
	if serr := sc.Err(); serr != nil {
		return nil, fmt.Errorf("demo model: read vocab.txt: %w", serr)
	}
	if len(vocab) != meta.VocabSize {
		return nil, fmt.Errorf("demo model: vocab.txt has %d tokens, model.json says %d", len(vocab), meta.VocabSize)
	}
	unkID, ok := vocab[meta.UnkToken]
	if !ok {
		return nil, fmt.Errorf("demo model: unk token %q not in vocabulary", meta.UnkToken)
	}

	emb, err := os.ReadFile(filepath.Join(dir, "embeddings.f32"))
	if err != nil {
		return nil, fmt.Errorf("demo model: %w", err)
	}
	if want := meta.VocabSize * meta.Dim * 4; len(emb) != want {
		return nil, fmt.Errorf("demo model: embeddings.f32 is %d bytes, want %d", len(emb), want)
	}
	vecs := make([]float32, meta.VocabSize*meta.Dim)
	for i := range vecs {
		vecs[i] = math.Float32frombits(binary.LittleEndian.Uint32(emb[i*4:]))
	}

	maxWord := meta.MaxInputCharsPerWord
	if maxWord <= 0 {
		maxWord = 100
	}
	return &Model{
		dim:          meta.Dim,
		vocab:        vocab,
		vecs:         vecs,
		unkID:        unkID,
		maxWordChars: maxWord,
		normalize:    meta.Normalize,
	}, nil
}

// Dim returns the embedding dimensionality.
func (m *Model) Dim() int { return m.dim }

// Embed encodes text as the mean of its known token embeddings (unknown
// tokens are dropped, matching model2vec), L2-normalized. Text with no known
// tokens embeds to the zero vector.
func (m *Model) Embed(text string) []float32 {
	out := make([]float32, m.dim)
	known := 0
	for _, id := range m.tokenize(text) {
		if id == m.unkID {
			continue
		}
		row := m.vecs[id*m.dim : (id+1)*m.dim]
		for i, v := range row {
			out[i] += v
		}
		known++
	}
	if known == 0 {
		return out
	}
	var sq float64
	for i := range out {
		out[i] /= float32(known)
		sq += float64(out[i]) * float64(out[i])
	}
	if m.normalize && sq > 0 {
		inv := float32(1 / math.Sqrt(sq))
		for i := range out {
			out[i] *= inv
		}
	}
	return out
}
