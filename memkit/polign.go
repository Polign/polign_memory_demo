package memkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PolignClient is a minimal HTTP client for a polign_db server, covering just
// the calls this demo needs. Metadata is typed end to end: values are JSON
// strings, numbers, or booleans, and reads opt in to typed responses so a
// number written as a number comes back as one.
type PolignClient struct {
	base string
	http *http.Client
}

func NewPolignClient(base string) *PolignClient {
	return &PolignClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 30 * time.Second},
	}
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
