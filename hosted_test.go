package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Polign/polign_memory_demo/memkit"
	"github.com/Polign/recall"
)

// fakeNode is an in-memory polign_db that scopes records by bearer key, the
// way a real node scopes them by the key's namespace. It hands out a write
// token per mutation and records the freshness token reads carry.
type fakeNode struct {
	mu       sync.Mutex
	rows     map[string]map[string]fakeRow // key -> id -> row
	writes   int
	barriers []string
	srv      *httptest.Server
}

type fakeRow struct {
	ID       string         `json:"id"`
	Values   []float32      `json:"values"`
	Metadata map[string]any `json:"metadata"`
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	n := &fakeNode{rows: map[string]map[string]fakeRow{}}
	n.srv = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNode) seed(key, id string, metadata map[string]any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rows[key] == nil {
		n.rows[key] = map[string]fakeRow{}
	}
	n.rows[key][id] = fakeRow{ID: id, Values: []float32{1}, Metadata: metadata}
}

func (n *fakeNode) count(key string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.rows[key])
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		_, _ = w.Write([]byte("ok"))
		return
	}
	key := bearerToken(r)
	if key == "" {
		http.Error(w, `{"error":"key required"}`, http.StatusUnauthorized)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rows[key] == nil {
		n.rows[key] = map[string]fakeRow{}
	}
	rows := n.rows[key]
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/watermark"):
		http.Error(w, `{"error":"not here"}`, http.StatusNotImplemented)
	case r.Method == http.MethodPut && strings.Contains(path, "/vectors/"):
		var row fakeRow
		_ = json.NewDecoder(r.Body).Decode(&row)
		row.ID = path[strings.LastIndex(path, "/")+1:]
		rows[row.ID] = row
		n.writes++
		w.Header().Set("X-Polign-Write-Token", strconv.Itoa(n.writes))
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.Method == http.MethodGet && strings.Contains(path, "/vectors/"):
		n.barriers = append(n.barriers, r.Header.Get("X-Polign-Require-Write-Token"))
		id := path[strings.LastIndex(path, "/")+1:]
		row, ok := rows[id]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(row)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/vectors"):
		var filter map[string]any
		if f := r.URL.Query().Get("filter"); f != "" {
			_ = json.Unmarshal([]byte(f), &filter)
		}
		var out []fakeRow
		for _, row := range rows {
			match := true
			for k, v := range filter {
				if fmt.Sprint(row.Metadata[k]) != fmt.Sprint(v) {
					match = false
				}
			}
			if match {
				out = append(out, row)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		total := len(out)
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if offset > len(out) {
			offset = len(out)
		}
		out = out[offset:]
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		if out == nil {
			out = []fakeRow{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vectors": out, "total": total})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/vectors:delete"):
		var req struct {
			IDs []string `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var gone []string
		for _, id := range req.IDs {
			if _, ok := rows[id]; ok {
				delete(rows, id)
				gone = append(gone, id)
			}
		}
		n.writes++
		w.Header().Set("X-Polign-Write-Token", strconv.Itoa(n.writes))
		_ = json.NewEncoder(w).Encode(map[string]any{"ids": gone})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/query"):
		var req struct {
			K int `json:"k"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		hits := []map[string]any{}
		for _, row := range rows {
			if len(hits) < req.K {
				hits = append(hits, map[string]any{"id": row.ID, "score": 1, "metadata": row.Metadata})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": hits})
	default:
		http.Error(w, `{"error":"unexpected route `+r.Method+` `+path+`"}`, http.StatusNotFound)
	}
}

// hostedStubAgent stands in for a model loop: it reports one tool call
// through the sink and answers with an echo.
type hostedStubAgent struct {
	sink   func(TraceEvent)
	resets int
	turns  []string
}

func (a *hostedStubAgent) Turn(_ context.Context, text string) (AgentReply, error) {
	a.turns = append(a.turns, text)
	if a.sink != nil {
		a.sink(TraceEvent{Kind: "call", Tool: "recall", Payload: `{"subject":"user"}`})
		a.sink(TraceEvent{Kind: "result", Tool: "recall", Payload: `{"count":0}`})
	}
	return AgentReply{Text: "echo: " + text}, nil
}

func (a *hostedStubAgent) Reset()                          { a.resets++ }
func (a *hostedStubAgent) setTraceSink(s func(TraceEvent)) { a.sink = s }

type fakeMinter struct {
	mu   sync.Mutex
	made []string
}

func (m *fakeMinter) Mint(_ context.Context, namespace, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.made = append(m.made, namespace)
	return "plgn_key_for_" + namespace, nil
}

type hostedFixture struct {
	server *hostedServer
	nodes  []*fakeNode
	minter *fakeMinter
	http   *httptest.Server
}

func newHostedFixture(t *testing.T, turnsPerHour int) *hostedFixture {
	t.Helper()
	registry, err := memkit.LoadRegistry(defaultPredicates)
	if err != nil {
		t.Fatal(err)
	}
	minter := &fakeMinter{}
	keys, err := loadKeyring(filepath.Join(t.TempDir(), "keys.json"), minter)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []*fakeNode{newFakeNode(t), newFakeNode(t)}
	server, err := newHostedServer(hostedConfig{
		Origins:    []string{"https://polign.com"},
		Nodes:      []string{nodes[0].srv.URL, nodes[1].srv.URL},
		Collection: "memories",
		Registry:   registry,
		Embedder:   recall.LexicalEmbedder{},
		NewAgent:   func(*memkit.Store, wikipediaSource) Agent { return &hostedStubAgent{} },
		Label:      "claude",
		Model:      "test-model",
		Verify: func(_ context.Context, token string) (identity, error) {
			switch token {
			case "alice-token":
				return identity{Subject: "alice-sub", Email: "alice@example.com"}, nil
			case "bob-token":
				return identity{Subject: "bob-sub", Email: "bob@example.com"}, nil
			}
			return identity{}, fmt.Errorf("%w: nope", errUnauthorized)
		},
		Keys:         keys,
		TurnsPerHour: turnsPerHour,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &hostedFixture{server: server, nodes: nodes, minter: minter, http: httptest.NewServer(server.handler())}
	t.Cleanup(f.http.Close)
	return f
}

func (f *hostedFixture) call(t *testing.T, token, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Origin", "https://polign.com")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, sb.String()
}

func TestHostedRequiresSignIn(t *testing.T) {
	f := newHostedFixture(t, 30)
	for _, token := range []string{"", "forged"} {
		resp, _ := f.call(t, token, http.MethodGet, "/api/session", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, resp.StatusCode)
		}
	}
	resp, _ := f.call(t, "", http.MethodOptions, "/api/chat", "")
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "https://polign.com" {
		t.Fatalf("preflight = %d %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestHostedSessionsGetTheirOwnNamespaceAndKey(t *testing.T) {
	f := newHostedFixture(t, 30)
	var alice, bob sessionResponse
	for token, into := range map[string]*sessionResponse{"alice-token": &alice, "bob-token": &bob} {
		resp, body := f.call(t, token, http.MethodGet, "/api/session", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("session status = %d: %s", resp.StatusCode, body)
		}
		if err := json.Unmarshal([]byte(body), into); err != nil {
			t.Fatal(err)
		}
	}
	if alice.Namespace == bob.Namespace || !strings.HasPrefix(alice.Namespace, "memdemo:") || alice.Email != "alice@example.com" {
		t.Fatalf("namespaces not distinct: %+v %+v", alice, bob)
	}
	if len(alice.Agents) != 2 || alice.Agents[0].ID != "a" || alice.Agents[1].ID != "b" || alice.TurnsLeft != 30 {
		t.Fatalf("agents = %+v turns_left = %d", alice.Agents, alice.TurnsLeft)
	}
	sort.Strings(f.minter.made)
	if len(f.minter.made) != 2 || f.minter.made[0] == f.minter.made[1] {
		t.Fatalf("minted keys = %v, want one per namespace", f.minter.made)
	}
	// A second request reuses the tenant instead of minting again.
	f.call(t, "alice-token", http.MethodGet, "/api/session", "")
	if len(f.minter.made) != 2 {
		t.Fatalf("re-minted on a repeat visit: %v", f.minter.made)
	}
}

func TestHostedChatStreamsTraceThenReply(t *testing.T) {
	f := newHostedFixture(t, 30)
	resp, body := f.call(t, "alice-token", http.MethodPost, "/api/chat", `{"agent":"b","message":"I use Vim"}`)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status = %d type = %q body = %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	for _, want := range []string{
		"event: call\ndata: {\"kind\":\"call\",\"tool\":\"recall\"",
		"event: result\n",
		"event: reply\ndata: {\"label\":\"claude\",\"retrieved_from\":null,\"text\":\"echo: I use Vim\"}",
		"event: done\ndata: {\"turns_left\":29}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
	tenant := f.server.tenants[f.server.namespaceFor("alice-sub")]
	if a, b := tenant.seat("a").agent.(*hostedStubAgent), tenant.seat("b").agent.(*hostedStubAgent); len(a.turns) != 0 || len(b.turns) != 1 {
		t.Fatalf("turns landed on the wrong seat: a=%v b=%v", a.turns, b.turns)
	}
	resp, body = f.call(t, "alice-token", http.MethodPost, "/api/chat", `{"agent":"c","message":"hi"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown agent: status = %d body = %s", resp.StatusCode, body)
	}
	resp, _ = f.call(t, "alice-token", http.MethodPost, "/api/chat", `{"agent":"a","message":""}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty message: status = %d", resp.StatusCode)
	}
}

func TestHostedTurnBudget(t *testing.T) {
	f := newHostedFixture(t, 1)
	if resp, _ := f.call(t, "alice-token", http.MethodPost, "/api/chat", `{"agent":"a","message":"one"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("first turn = %d", resp.StatusCode)
	}
	resp, body := f.call(t, "alice-token", http.MethodPost, "/api/chat", `{"agent":"b","message":"two"}`)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "turns for this hour") {
		t.Fatalf("second turn = %d %s", resp.StatusCode, body)
	}
	// Another visitor has a budget of their own.
	if resp, _ := f.call(t, "bob-token", http.MethodPost, "/api/chat", `{"agent":"a","message":"one"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("bob's first turn = %d", resp.StatusCode)
	}
}

func TestHostedSeatCatchesUpWithTheOtherNodesWrites(t *testing.T) {
	f := newHostedFixture(t, 30)
	f.call(t, "alice-token", http.MethodGet, "/api/session", "")
	tenant := f.server.tenants[f.server.namespaceFor("alice-sub")]
	// Agent A remembers something through node A.
	if _, err := tenant.seat("a").store.Remember("preference", "user", "prefers_editor", "vim", 0, "user_stated"); err != nil {
		t.Fatal(err)
	}
	if got := tenant.tokens.Latest(); got != "1" {
		t.Fatalf("write token = %q, want the node's receipt", got)
	}
	// A turn on agent B first makes node B wait for that write.
	_, body := f.call(t, "alice-token", http.MethodPost, "/api/chat", `{"agent":"b","message":"which editor?"}`)
	if !strings.Contains(body, "event: sync\n") {
		t.Fatalf("no sync event:\n%s", body)
	}
	if got := f.nodes[1].barriers; len(got) != 1 || got[0] != "1" {
		t.Fatalf("node B barrier tokens = %v, want [1]", got)
	}
	if got := f.nodes[0].barriers; len(got) != 0 {
		t.Fatalf("node A saw a barrier it should not have: %v", got)
	}
}

func TestHostedMemoriesAndResets(t *testing.T) {
	f := newHostedFixture(t, 30)
	f.call(t, "alice-token", http.MethodGet, "/api/session", "")
	f.call(t, "bob-token", http.MethodGet, "/api/session", "")
	aliceKey := "plgn_key_for_" + f.server.namespaceFor("alice-sub")
	bobKey := "plgn_key_for_" + f.server.namespaceFor("bob-sub")
	seed := func(key, id, value, at string) {
		f.nodes[0].seed(key, id, map[string]any{"kind": "preference", "subject": "user", "predicate": "prefers_editor", "value": value, "confidence": 1, "source": "user_stated", "observed_at": at})
	}
	seed(aliceKey, "m-old000000000", "vim", "2026-08-23T10:00:00Z")
	seed(aliceKey, "m-new000000000", "neovim", "2026-08-23T11:00:00Z")
	seed(bobKey, "m-bob000000000", "emacs", "2026-08-23T11:00:00Z")

	resp, body := f.call(t, "alice-token", http.MethodGet, "/api/memories", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("memories = %d %s", resp.StatusCode, body)
	}
	var view struct {
		Namespace string          `json:"namespace"`
		Records   []memkit.Record `json:"records"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Records) != 2 || view.Records[0].Value != "vim" || view.Records[0].Status != "historical" || view.Records[1].Status != "active" {
		t.Fatalf("records = %+v", view.Records)
	}

	// Reset the conversation on one seat only.
	if resp, _ := f.call(t, "alice-token", http.MethodPost, "/api/reset", `{"agent":"a"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %d", resp.StatusCode)
	}
	tenant := f.server.tenants[f.server.namespaceFor("alice-sub")]
	if tenant.seat("a").agent.(*hostedStubAgent).resets != 1 || tenant.seat("b").agent.(*hostedStubAgent).resets != 0 {
		t.Fatal("conversation reset touched the wrong seat")
	}

	// Reset memories: Alice's records go, Bob's stay, and the seats are rebuilt.
	before := tenant.seat("a").agent
	resp, body = f.call(t, "alice-token", http.MethodPost, "/api/reset-memories", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"deleted":2`) {
		t.Fatalf("reset-memories = %d %s", resp.StatusCode, body)
	}
	if f.nodes[0].count(aliceKey) != 0 || f.nodes[0].count(bobKey) != 1 {
		t.Fatalf("alice=%d bob=%d records after reset", f.nodes[0].count(aliceKey), f.nodes[0].count(bobKey))
	}
	if tenant.seat("a").agent == before {
		t.Fatal("seat kept its old agent and store after a memory reset")
	}
	_, body = f.call(t, "alice-token", http.MethodGet, "/api/memories", "")
	if !strings.Contains(body, `"records":[]`) {
		t.Fatalf("memories after reset: %s", body)
	}
}

func TestHostedEvictsIdleTenants(t *testing.T) {
	f := newHostedFixture(t, 30)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	f.server.cfg.Now = func() time.Time { return now }
	f.call(t, "alice-token", http.MethodGet, "/api/session", "")
	if f.server.evictIdle() != 0 {
		t.Fatal("evicted a live tenant")
	}
	now = now.Add(31 * time.Minute)
	if f.server.evictIdle() != 1 || len(f.server.tenants) != 0 {
		t.Fatal("idle tenant survived")
	}
}
