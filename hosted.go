// Hosted mode serves the memory demo to many visitors at once behind the
// polign.com sign-in. Every visitor gets a namespace of their own, enforced
// by the polign_db nodes rather than by this process: the key minted for a
// namespace cannot read or write outside it. Two agents share that namespace
// through two separate cold-first nodes on one bucket prefix, which is the
// point of the demo: memory that lives in the store, not in a process.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Polign/polign_memory_demo/memkit"
	"github.com/Polign/recall"
	recallpolign "github.com/Polign/recall/polign"
)

const (
	hostedMaxMessageBytes = 2000
	// hostedMaxConversationTurns bounds the context a single conversation can
	// grow to before it is cleared; memories are unaffected.
	hostedMaxConversationTurns = 30
	hostedTurnTimeout          = 2 * time.Minute
	hostedSlotWait             = 20 * time.Second
	hostedInspectorLimit       = 1000
)

// hostedConfig is everything the hosted server needs, with the pieces that
// touch a network (identity, keys, agents) injectable so tests run offline.
type hostedConfig struct {
	Origins    []string
	Nodes      []string
	Collection string
	Registry   memkit.Registry
	Embedder   recall.Embedder
	Wikipedia  wikipediaSource
	NewAgent   func(store *memkit.Store, wikipedia wikipediaSource) Agent
	Label      string
	Model      string
	Verify     func(ctx context.Context, token string) (identity, error)
	Keys       interface {
		Key(ctx context.Context, namespace, note string) (string, error)
	}
	NamespacePrefix string
	TurnsPerHour    int
	MaxConcurrent   int
	IdleTTL         time.Duration
	Now             func() time.Time
}

// hostedSeat is one agent of a tenant: its own conversation, its own Recall
// client, and its own polign node. Both seats share the namespace and key.
type hostedSeat struct {
	id     string
	node   string
	mu     sync.Mutex
	agent  Agent
	store  *memkit.Store
	client *memkit.PolignClient
	turns  int
}

// hostedTenant is one signed-in visitor.
type hostedTenant struct {
	namespace string
	email     string
	key       string
	tokens    memkit.WriteTokens
	seats     []*hostedSeat

	mu       sync.Mutex
	turns    []time.Time
	lastUsed time.Time
}

type hostedServer struct {
	cfg     hostedConfig
	mu      sync.Mutex
	tenants map[string]*hostedTenant
	slots   chan struct{}
}

func newHostedServer(cfg hostedConfig) (*hostedServer, error) {
	if len(cfg.Nodes) < 1 {
		return nil, fmt.Errorf("hosted mode needs at least one polign node")
	}
	if cfg.Verify == nil || cfg.Keys == nil || cfg.NewAgent == nil {
		return nil, fmt.Errorf("hosted mode needs a token verifier, a key source, and an agent factory")
	}
	if cfg.TurnsPerHour <= 0 {
		cfg.TurnsPerHour = 30
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 30 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NamespacePrefix == "" {
		cfg.NamespacePrefix = "memdemo:"
	}
	return &hostedServer{cfg: cfg, tenants: map[string]*hostedTenant{}, slots: make(chan struct{}, cfg.MaxConcurrent)}, nil
}

// namespaceFor derives a namespace from the account subject. Hashing keeps
// the identifier out of the store's object keys while staying stable per
// account, and the result uses only characters polign_db namespaces allow.
func (s *hostedServer) namespaceFor(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return s.cfg.NamespacePrefix + hex.EncodeToString(sum[:])[:24]
}

func (s *hostedServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/session", s.withTenant(s.session))
	mux.HandleFunc("GET /api/memories", s.withTenant(s.memories))
	mux.HandleFunc("POST /api/chat", s.withTenant(s.chat))
	mux.HandleFunc("POST /api/reset", s.withTenant(s.resetConversation))
	mux.HandleFunc("POST /api/reset-memories", s.withTenant(s.resetMemories))
	return s.cors(mux)
}

// cors answers browser preflights and marks responses for the site origins.
// There are no cookies here, only bearer tokens, so an origin outside the
// list simply cannot read the answer.
func (s *hostedServer) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := false
		for _, o := range s.cfg.Origins {
			if strings.EqualFold(o, origin) {
				allowed = true
			}
		}
		w.Header().Add("Vary", "Origin")
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *hostedServer) health(w http.ResponseWriter, r *http.Request) {
	for _, node := range s.cfg.Nodes {
		if !memkit.NewPolignClient(node).Healthy() {
			http.Error(w, "polign node unavailable: "+node, http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// withTenant authenticates the request and resolves (or builds) the caller's
// tenant before the handler runs.
func (s *hostedServer) withTenant(next func(http.ResponseWriter, *http.Request, *hostedTenant)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeWebJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in to use the demo"})
			return
		}
		who, err := s.cfg.Verify(r.Context(), token)
		if err != nil {
			if errors.Is(err, errUnauthorized) {
				writeWebJSON(w, http.StatusUnauthorized, map[string]string{"error": "your sign-in expired; sign in again"})
			} else {
				log.Printf("hosted: verify token: %v", err)
				writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": "sign-in could not be checked right now"})
			}
			return
		}
		tenant, err := s.tenant(r.Context(), who)
		if err != nil {
			log.Printf("hosted: tenant for %s: %v", s.namespaceFor(who.Subject), err)
			writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": "the memory nodes are not reachable right now"})
			return
		}
		tenant.mu.Lock()
		tenant.lastUsed = s.cfg.Now()
		tenant.mu.Unlock()
		next(w, r, tenant)
	}
}

// tenant returns the visitor's tenant, building it on first sight: one key
// for the namespace, then one seat per node.
func (s *hostedServer) tenant(ctx context.Context, who identity) (*hostedTenant, error) {
	ns := s.namespaceFor(who.Subject)
	s.mu.Lock()
	t := s.tenants[ns]
	s.mu.Unlock()
	if t != nil {
		return t, nil
	}
	key, err := s.cfg.Keys.Key(ctx, ns, "memory demo visitor")
	if err != nil {
		return nil, err
	}
	t = &hostedTenant{namespace: ns, email: who.Email, key: key, lastUsed: s.cfg.Now()}
	for i, node := range s.cfg.Nodes {
		seat, err := s.buildSeat(ctx, t, i, node)
		if err != nil {
			return nil, err
		}
		t.seats = append(t.seats, seat)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.tenants[ns]; existing != nil {
		return existing, nil
	}
	s.tenants[ns] = t
	return t, nil
}

func (s *hostedServer) buildSeat(ctx context.Context, t *hostedTenant, index int, node string) (*hostedSeat, error) {
	transport := memkit.TokenTransport(&t.tokens, nil)
	backend, err := recallpolign.New(recallpolign.Config{BaseURL: node, APIKey: t.key, HTTPClient: memkit.NewHTTPClient(transport)})
	if err != nil {
		return nil, err
	}
	store, err := memkit.NewStore(backend, s.cfg.Collection, s.cfg.Registry, s.cfg.Embedder)
	if err != nil {
		return nil, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := store.Check(checkCtx); err != nil {
		return nil, fmt.Errorf("%s: %w", node, err)
	}
	seat := &hostedSeat{
		id:     string(rune('a' + index)),
		node:   node,
		store:  store,
		client: memkit.NewPolignClientWithTransport(node, t.key, transport),
		agent:  s.cfg.NewAgent(store, s.cfg.Wikipedia),
	}
	return seat, nil
}

func (t *hostedTenant) seat(id string) *hostedSeat {
	for _, seat := range t.seats {
		if seat.id == id {
			return seat
		}
	}
	return nil
}

// allowTurn enforces the per-visitor hourly budget.
func (s *hostedServer) allowTurn(t *hostedTenant) (remaining int, ok bool) {
	now := s.cfg.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.turns[:0]
	for _, at := range t.turns {
		if now.Sub(at) < time.Hour {
			kept = append(kept, at)
		}
	}
	t.turns = kept
	if len(t.turns) >= s.cfg.TurnsPerHour {
		return 0, false
	}
	t.turns = append(t.turns, now)
	return s.cfg.TurnsPerHour - len(t.turns), true
}

func (s *hostedServer) remainingTurns(t *hostedTenant) int {
	now := s.cfg.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, at := range t.turns {
		if now.Sub(at) < time.Hour {
			n++
		}
	}
	return max(s.cfg.TurnsPerHour-n, 0)
}

type sessionResponse struct {
	Email        string     `json:"email"`
	Namespace    string     `json:"namespace"`
	Collection   string     `json:"collection"`
	Model        string     `json:"model"`
	Label        string     `json:"label"`
	Agents       []seatInfo `json:"agents"`
	TurnsPerHour int        `json:"turns_per_hour"`
	TurnsLeft    int        `json:"turns_left"`
	Wikipedia    string     `json:"wikipedia,omitempty"`
}

type seatInfo struct {
	ID   string `json:"id"`
	Node string `json:"node"`
}

func nodeLabel(node string) string {
	if u, err := url.Parse(node); err == nil && u.Host != "" {
		return u.Host
	}
	return node
}

func (s *hostedServer) session(w http.ResponseWriter, _ *http.Request, t *hostedTenant) {
	resp := sessionResponse{Email: t.email, Namespace: t.namespace, Collection: s.cfg.Collection, Model: s.cfg.Model, Label: s.cfg.Label, TurnsPerHour: s.cfg.TurnsPerHour, TurnsLeft: s.remainingTurns(t)}
	for _, seat := range t.seats {
		resp.Agents = append(resp.Agents, seatInfo{ID: seat.id, Node: nodeLabel(seat.node)})
	}
	if s.cfg.Wikipedia != nil {
		resp.Wikipedia = s.cfg.Wikipedia.Collection()
	}
	writeWebJSON(w, http.StatusOK, resp)
}

// memories renders the namespace's event log the way the inspector does,
// read through the first node after it has caught up with every write this
// tenant has made on any node.
func (s *hostedServer) memories(w http.ResponseWriter, r *http.Request, t *hostedTenant) {
	seat := t.seats[0]
	if err := seat.client.Barrier(s.cfg.Collection, t.tokens.Latest()); err != nil {
		writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": "memory node did not catch up: " + err.Error()})
		return
	}
	records, err := seat.store.RecallContext(r.Context(), memkit.RecallQuery{IncludeHistory: true, Limit: hostedInspectorLimit})
	if err != nil {
		writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		if a.Predicate != b.Predicate {
			return a.Predicate < b.Predicate
		}
		return a.ObservedAt < b.ObservedAt
	})
	if records == nil {
		records = []memkit.Record{}
	}
	writeWebJSON(w, http.StatusOK, map[string]any{"namespace": t.namespace, "collection": s.cfg.Collection, "node": nodeLabel(seat.node), "records": records})
}

type chatRequest struct {
	Agent   string `json:"agent"`
	Message string `json:"message"`
}

func decodeStrict(r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

// sseWriter streams server-sent events. Everything about one turn goes down
// the same response: the trace as it happens, then the reply.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSE(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: flusher}
}

func (s *sseWriter) send(event string, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = []byte(`{}`)
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, raw)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *hostedServer) chat(w http.ResponseWriter, r *http.Request, t *hostedTenant) {
	var req chatRequest
	if err := decodeStrict(r, &req); err != nil {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" || len(req.Message) > hostedMaxMessageBytes {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("message must contain 1 to %d bytes", hostedMaxMessageBytes)})
		return
	}
	seat := t.seat(req.Agent)
	if seat == nil {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown agent"})
		return
	}
	if !seat.mu.TryLock() {
		writeWebJSON(w, http.StatusConflict, map[string]string{"error": "this agent is still answering"})
		return
	}
	defer seat.mu.Unlock()
	remaining, ok := s.allowTurn(t)
	if !ok {
		writeWebJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("you have used your %d turns for this hour; come back later", s.cfg.TurnsPerHour)})
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-time.After(hostedSlotWait):
		writeWebJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the demo is busy; try again in a moment"})
		return
	case <-r.Context().Done():
		return
	}

	sse := newSSE(w)
	if seat.turns >= hostedMaxConversationTurns {
		seat.agent.Reset()
		seat.turns = 0
		sse.send("note", map[string]string{"text": "conversation cleared after " + fmt.Sprint(hostedMaxConversationTurns) + " turns; memories are untouched"})
	}
	// Catch this seat's node up with writes the other seat made, so a recall
	// here sees a memory the other agent just stored.
	if token := t.tokens.Latest(); token != "" {
		if err := seat.client.Barrier(s.cfg.Collection, token); err != nil {
			sse.send("note", map[string]string{"text": "node " + nodeLabel(seat.node) + " could not confirm it caught up with the write log: " + err.Error()})
		} else {
			sse.send("sync", map[string]string{"node": nodeLabel(seat.node)})
		}
	}
	if sinkable, ok := seat.agent.(interface{ setTraceSink(func(TraceEvent)) }); ok {
		sinkable.setTraceSink(func(ev TraceEvent) { sse.send(ev.Kind, ev) })
		defer sinkable.setTraceSink(nil)
	}
	ctx, cancel := context.WithTimeout(r.Context(), hostedTurnTimeout)
	defer cancel()
	reply, err := seat.agent.Turn(ctx, req.Message)
	if err != nil {
		log.Printf("hosted: %s seat %s: turn failed: %v", t.namespace, seat.id, err)
		sse.send("failed", map[string]string{"message": "the model request failed; try again"})
		sse.send("done", map[string]int{"turns_left": remaining})
		return
	}
	seat.turns++
	sse.send("reply", map[string]any{"label": s.cfg.Label, "text": reply.Text, "retrieved_from": reply.RetrievedFrom})
	sse.send("done", map[string]int{"turns_left": remaining})
}

func (s *hostedServer) resetConversation(w http.ResponseWriter, r *http.Request, t *hostedTenant) {
	var req struct {
		Agent string `json:"agent"`
	}
	if err := decodeStrict(r, &req); err != nil {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	seat := t.seat(req.Agent)
	if seat == nil {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown agent"})
		return
	}
	seat.mu.Lock()
	seat.agent.Reset()
	seat.turns = 0
	seat.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// resetMemories deletes every record the namespace can see, then rebuilds
// both seats so no client-side cache or conversation remembers them. The
// namespaced key is what keeps this from touching anyone else's records.
func (s *hostedServer) resetMemories(w http.ResponseWriter, r *http.Request, t *hostedTenant) {
	for _, seat := range t.seats {
		seat.mu.Lock()
		defer seat.mu.Unlock()
	}
	first := t.seats[0]
	deleted := 0
	for round := 0; round < 20; round++ {
		rows, _, err := first.client.List(s.cfg.Collection, nil, hostedInspectorLimit)
		if err != nil {
			writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if len(rows) == 0 {
			break
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		n, err := first.client.DeleteMany(s.cfg.Collection, ids)
		if err != nil {
			writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		deleted += n
		if n == 0 {
			break
		}
	}
	for i, seat := range t.seats {
		fresh, err := s.buildSeat(r.Context(), t, i, seat.node)
		if err != nil {
			writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		seat.agent, seat.store, seat.client, seat.turns = fresh.agent, fresh.store, fresh.client, 0
	}
	writeWebJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}

// evictIdle drops tenants nobody has used for a while. Their conversations
// go with them; their memories are in the bucket and come back on sign-in.
func (s *hostedServer) evictIdle() int {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for ns, t := range s.tenants {
		t.mu.Lock()
		idle := now.Sub(t.lastUsed) > s.cfg.IdleTTL
		t.mu.Unlock()
		if idle {
			delete(s.tenants, ns)
			n++
		}
	}
	return n
}

func (s *hostedServer) evictLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := s.evictIdle(); n > 0 {
				log.Printf("hosted: evicted %d idle visitor(s)", n)
			}
		}
	}
}

// serveHosted runs the hosted server until the listener fails.
func serveHosted(addr string, s *hostedServer) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.evictLoop(ctx)
	server := &http.Server{
		Addr:              addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      hostedTurnTimeout + 30*time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("hosted server: %w", err)
}
