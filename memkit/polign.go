package memkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// PolignClient is a minimal HTTP client for a polign_db server, covering just
// the calls this demo needs. Metadata is typed end to end: values are JSON
// strings, numbers, or booleans, and reads opt in to typed responses so a
// number written as a number comes back as one.
type PolignClient struct {
	base string
	key  string
	http *http.Client
}

func NewPolignClient(base string) *PolignClient { return NewPolignClientWithKey(base, "") }

func NewPolignClientWithKey(base, key string) *PolignClient {
	return NewPolignClientWithTransport(base, key, nil)
}

// NewPolignClientWithTransport lets a caller observe or shape the HTTP
// exchange, for example to record write receipts with TokenTransport. A nil
// transport uses the default.
func NewPolignClientWithTransport(base, key string, rt http.RoundTripper) *PolignClient {
	return &PolignClient{
		base: strings.TrimRight(base, "/"),
		key:  key,
		http: NewHTTPClient(rt),
	}
}

// NewHTTPClient builds the client every demo caller uses: a 30-second
// timeout and no redirect following, so a misconfigured endpoint fails
// instead of quietly talking to somewhere else.
func NewHTTPClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// WriteTokens remembers the newest write receipt a caller has seen. A polign
// node returns one on every mutation (X-Polign-Write-Token); handing it back
// on a read (X-Polign-Require-Write-Token) makes that node wait until it has
// replayed the write log past that point. That is what lets a second node on
// the same store answer read-your-writes for a write the first node took.
type WriteTokens struct {
	mu     sync.Mutex
	latest string
}

// Observe records a receipt. Empty receipts are ignored.
func (t *WriteTokens) Observe(token string) {
	if token == "" {
		return
	}
	t.mu.Lock()
	t.latest = token
	t.mu.Unlock()
}

// Latest returns the most recent receipt, or "" when nothing was written yet.
func (t *WriteTokens) Latest() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.latest
}

const (
	writeTokenHeader        = "X-Polign-Write-Token"
	requireWriteTokenHeader = "X-Polign-Require-Write-Token"
)

// TokenTransport records every write receipt a response carries into tokens.
// A nil base uses http.DefaultTransport.
func TokenTransport(tokens *WriteTokens, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return tokenTransport{tokens: tokens, base: base}
}

type tokenTransport struct {
	tokens *WriteTokens
	base   http.RoundTripper
}

func (t tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		t.tokens.Observe(resp.Header.Get(writeTokenHeader))
	}
	return resp, err
}

// StoredVector is one record as the server returns it.
type StoredVector struct {
	ID       string         `json:"id"`
	Values   []float32      `json:"values"`
	Metadata map[string]any `json:"metadata"`
}

// Hit is one search result.
type Hit struct {
	ID       string         `json:"id"`
	Distance float32        `json:"distance"`
	Score    float32        `json:"score"`
	Metadata map[string]any `json:"metadata"`
}

// QueryOptions describes a general collection query. Memory recall uses
// Search below; read-only knowledge collections can additionally opt into the
// object-store path and the collection's lexical index.
type QueryOptions struct {
	Values []float32
	Text   string
	K      int
	Cold   bool
	NProbe int
}

// Put upserts one vector. The collection is created on first use.
func (c *PolignClient) Put(collection, id string, values []float32, metadata map[string]any) error {
	body := map[string]any{"values": values}
	if len(metadata) > 0 {
		body["metadata"] = metadata
	}
	path := fmt.Sprintf("/v1/collections/%s/vectors/%s", seg(collection), seg(id))
	_, err := c.request(http.MethodPut, path, body)
	return err
}

// GetMany fetches records by id with byte-exact values (served from the
// bucket on a cold collection). Unknown ids are omitted, not an error.
func (c *PolignClient) GetMany(collection string, ids []string) ([]StoredVector, error) {
	body := map[string]any{"ids": ids, "typed_metadata": true}
	path := fmt.Sprintf("/v1/collections/%s/vectors:get", seg(collection))
	raw, err := c.request(http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Vectors []StoredVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("polign: parse vectors:get response: %w", err)
	}
	return out.Vectors, nil
}

// List returns records ordered by id, restricted to filter (the server's
// filter dict language: plain equality plus $gte/$in/... operators).
func (c *PolignClient) List(collection string, filter map[string]any, limit int) ([]StoredVector, int, error) {
	path := fmt.Sprintf("/v1/collections/%s/vectors?limit=%d&offset=0&typed=true", seg(collection), limit)
	if len(filter) > 0 {
		fj, err := json.Marshal(filter)
		if err != nil {
			return nil, 0, err
		}
		path += "&filter=" + url.QueryEscape(string(fj))
	}
	raw, err := c.request(http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		Vectors []StoredVector `json:"vectors"`
		Total   int            `json:"total"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, fmt.Errorf("polign: parse list response: %w", err)
	}
	return out.Vectors, out.Total, nil
}

// Search runs a nearest-neighbour query restricted to filter.
func (c *PolignClient) Search(collection string, values []float32, k int, filter map[string]any) ([]Hit, error) {
	body := map[string]any{"values": values, "k": k, "typed_metadata": true}
	if len(filter) > 0 {
		body["filter"] = filter
	}
	path := fmt.Sprintf("/v1/collections/%s/query", seg(collection))
	raw, err := c.request(http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Hits []Hit `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("polign: parse query response: %w", err)
	}
	return out.Hits, nil
}

// Query runs a vector, lexical, or hybrid query. It is intentionally separate
// from Search so the typed memory store cannot accidentally broaden its
// filtered recall into an unrelated knowledge collection.
func (c *PolignClient) Query(collection string, opts QueryOptions) ([]Hit, error) {
	body := map[string]any{"k": opts.K}
	if len(opts.Values) > 0 {
		body["values"] = opts.Values
	}
	if opts.Text != "" {
		body["text"] = opts.Text
	}
	if opts.Cold {
		body["cold"] = true
	}
	if opts.NProbe > 0 {
		body["nprobe"] = opts.NProbe
	}
	path := fmt.Sprintf("/v1/collections/%s/query", seg(collection))
	raw, err := c.request(http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Hits []Hit `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("polign: parse query response: %w", err)
	}
	return out.Hits, nil
}

// Delete removes one record. A missing record reports false, not an error.
func (c *PolignClient) Delete(collection, id string) (bool, error) {
	path := fmt.Sprintf("/v1/collections/%s/vectors/%s", seg(collection), seg(id))
	raw, err := c.request(http.MethodDelete, path, nil)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return false, nil
		}
		return false, err
	}
	var out struct {
		Deleted bool `json:"deleted"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("polign: parse delete response: %w", err)
	}
	return out.Deleted, nil
}

// DeleteMany removes records by id in one call and returns how many ids the
// server acknowledged. Ids the caller cannot see (another namespace's, or
// already gone) are simply absent from the answer.
func (c *PolignClient) DeleteMany(collection string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	path := fmt.Sprintf("/v1/collections/%s/vectors:delete", seg(collection))
	raw, err := c.request(http.MethodPost, path, map[string]any{"ids": ids})
	if err != nil {
		return 0, err
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("polign: parse vectors:delete response: %w", err)
	}
	return len(out.IDs), nil
}

// Barrier blocks until this node has replayed the write log past token, so a
// following list or search on the same node sees that write. It is a point
// read of an id that does not exist: the server applies the freshness wait
// before the lookup, and the resulting 404 is the expected answer. An empty
// token returns immediately.
func (c *PolignClient) Barrier(collection, token string) error {
	if token == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, c.base+fmt.Sprintf("/v1/collections/%s/vectors/%s", seg(collection), seg("__barrier__")), nil)
	if err != nil {
		return err
	}
	req.Header.Set(requireWriteTokenHeader, token)
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("polign: barrier: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("polign: barrier: HTTP %d", resp.StatusCode)
}

// Healthy reports whether the server answers /healthz.
func (c *PolignClient) Healthy() bool {
	resp, err := c.http.Get(c.base + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (c *PolignClient) request(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("polign: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("polign: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, fmt.Errorf("polign: %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	return raw, nil
}

func seg(s string) string { return url.PathEscape(s) }
