package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Polign/polign_memory_demo/memkit"
)

// WikipediaResult is the grounded passage exposed to the agent. The URL is
// kept alongside the text so answers can cite the article they came from.
type WikipediaResult struct {
	Title    string  `json:"title"`
	URL      string  `json:"url"`
	Text     string  `json:"text"`
	Distance float32 `json:"distance,omitempty"`
	Score    float32 `json:"score,omitempty"`
}

type wikipediaSource interface {
	Search(query string, limit int) ([]WikipediaResult, error)
}

// wikipediaSearch is a read-only view of a distinct Polign collection. It
// never receives memory writes. When an embedder is configured, queries use
// BGE semantic retrieval; otherwise the collection's BM25 index is used.
type wikipediaSearch struct {
	db         *memkit.PolignClient
	collection string
	embedder   *remoteEmbedder
	nprobe     int
}

func newWikipediaSearch(db *memkit.PolignClient, collection, embedAddr string, embedDim, nprobe int) *wikipediaSearch {
	var embedder *remoteEmbedder
	if strings.TrimSpace(embedAddr) != "" {
		embedder = newRemoteEmbedder(embedAddr, embedDim)
	}
	return &wikipediaSearch{db: db, collection: collection, embedder: embedder, nprobe: nprobe}
}

func (w *wikipediaSearch) Search(query string, limit int) ([]WikipediaResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("wikipedia query must not be empty")
	}
	if limit <= 0 || limit > 10 {
		limit = 5
	}
	opts := memkit.QueryOptions{K: limit, Cold: true}
	if w.embedder != nil {
		values, err := w.embedder.Embed(query)
		if err != nil {
			return nil, err
		}
		opts.Values = values
		opts.NProbe = w.nprobe
	} else {
		opts.Text = query
	}
	hits, err := w.db.Query(w.collection, opts)
	if err != nil {
		return nil, err
	}
	out := make([]WikipediaResult, 0, len(hits))
	for _, hit := range hits {
		out = append(out, WikipediaResult{
			Title:    metadataString(hit.Metadata, "title"),
			URL:      metadataString(hit.Metadata, "url"),
			Text:     metadataString(hit.Metadata, "text"),
			Distance: hit.Distance,
			Score:    hit.Score,
		})
	}
	return out, nil
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}

// remoteEmbedder speaks the same small HTTP contract as polign_demo's BGE
// sidecar. The sidecar owns BGE's asymmetric query prefix; callers send the
// user's raw question.
type remoteEmbedder struct {
	url    string
	dim    int
	client *http.Client
}

func newRemoteEmbedder(addr string, dim int) *remoteEmbedder {
	return &remoteEmbedder{
		url:    strings.TrimRight(addr, "/") + "/embed",
		dim:    dim,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *remoteEmbedder) Embed(text string) ([]float32, error) {
	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Post(r.url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("wikipedia embedder: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Values []float32 `json:"values"`
		Error  string    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("wikipedia embedder: decode status %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wikipedia embedder: status %d: %s", resp.StatusCode, out.Error)
	}
	if len(out.Values) != r.dim {
		return nil, fmt.Errorf("wikipedia embedder returned dim %d; collection requires %d", len(out.Values), r.dim)
	}
	return out.Values, nil
}
