package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Predicate is one registry entry. Cardinality decides what a second value
// for the same subject means: "single" makes the new value supersede the old
// one, "multi" makes it an additional fact.
type Predicate struct {
	Cardinality string `json:"cardinality"`
	Description string `json:"description"`
}

// Registry is the closed set of predicates the store accepts. A write with an
// unregistered predicate is rejected, so the agent cannot invent near-duplicate
// predicates ("editor_preference" vs "prefers_editor") and split one fact
// across two names.
type Registry map[string]Predicate

func LoadRegistry(raw []byte) (Registry, error) {
	var r Registry
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("predicate registry: %w", err)
	}
	for name, p := range r {
		if !predicateName.MatchString(name) {
			return nil, fmt.Errorf("predicate registry: %q is not snake_case", name)
		}
		if p.Cardinality != "single" && p.Cardinality != "multi" {
			return nil, fmt.Errorf("predicate registry: %q has cardinality %q, want single or multi", name, p.Cardinality)
		}
	}
	return r, nil
}

// Names returns the registered predicates sorted, for error messages and the
// system prompt.
func (r Registry) Names() []string {
	out := make([]string, 0, len(r))
	for name := range r {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// PromptTable renders the registry for the agent's system prompt.
func (r Registry) PromptTable() string {
	var b strings.Builder
	for _, name := range r.Names() {
		p := r[name]
		fmt.Fprintf(&b, "- %s (%s-valued): %s\n", name, p.Cardinality, p.Description)
	}
	return b.String()
}

// Record is one memory: a typed, durable statement about a subject.
type Record struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Subject      string  `json:"subject"`
	Predicate    string  `json:"predicate"`
	Value        string  `json:"value"`
	Confidence   float64 `json:"confidence"`
	Source       string  `json:"source"`
	Status       string  `json:"status"`
	SupersededBy string  `json:"superseded_by,omitempty"`
	ObservedAt   string  `json:"observed_at"`
}

// RememberResult is what a write reports back to the agent: the record that
// now holds, whether it already existed, and anything it superseded.
type RememberResult struct {
	Stored     Record   `json:"stored"`
	Existing   bool     `json:"already_known,omitempty"`
	Superseded []Record `json:"superseded,omitempty"`
}

// Store enforces the typed memory model over a polign_db collection. It is
// the layer between the agent's tools and the database: writes are validated
// against the registry, supersession follows from predicate cardinality (never
// from model judgment), and every record's vector is derived from its own text
// so semantic recall searches the same records exact recall filters.
type Store struct {
	db         *PolignClient
	collection string
	registry   Registry
	embed      func(string) []float32
	now        func() time.Time
}

func NewStore(db *PolignClient, collection string, registry Registry, embed func(string) []float32) *Store {
	return &Store{db: db, collection: collection, registry: registry, embed: embed, now: time.Now}
}

var (
	predicateName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	validSources  = map[string]bool{"user_stated": true, "agent_inferred": true, "tool_result": true}
)

// Remember validates and writes one memory. For a single-valued predicate an
// active record with a different value is flipped to superseded and linked to
// the new record; the same value is idempotent. For a multi-valued predicate
// the exact same (subject, predicate, value) is idempotent and a new value is
// simply an additional record.
func (s *Store) Remember(kind, subject, predicate, value string, confidence float64, source string) (RememberResult, error) {
	var zero RememberResult

	if kind != "fact" && kind != "preference" {
		return zero, fmt.Errorf(`kind must be "fact" or "preference", got %q`, kind)
	}
	subject = strings.ToLower(strings.TrimSpace(subject))
	if subject == "" {
		return zero, fmt.Errorf("subject must not be empty")
	}
	predicate = strings.TrimSpace(predicate)
	if !predicateName.MatchString(predicate) {
		return zero, fmt.Errorf("predicate must be snake_case (e.g. prefers_editor), got %q", predicate)
	}
	spec, ok := s.registry[predicate]
	if !ok {
		return zero, fmt.Errorf("predicate %q is not in the registry; registered predicates are: %s", predicate, strings.Join(s.registry.Names(), ", "))
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return zero, fmt.Errorf("value must not be empty")
	}
	if confidence == 0 {
		confidence = 1.0
	}
	if confidence < 0 || confidence > 1 {
		return zero, fmt.Errorf("confidence must be in [0, 1], got %g", confidence)
	}
	if source == "" {
		source = "user_stated"
	}
	if !validSources[source] {
		return zero, fmt.Errorf(`source must be "user_stated", "agent_inferred", or "tool_result", got %q`, source)
	}

	rec := Record{
		ID:         recordID(subject, predicate, value),
		Kind:       kind,
		Subject:    subject,
		Predicate:  predicate,
		Value:      value,
		Confidence: confidence,
		Source:     source,
		Status:     "active",
		ObservedAt: s.now().UTC().Format(time.RFC3339),
	}

	// The active records this write competes with: same value for both
	// cardinalities (idempotency), any value for single-valued (supersession).
	active, err := s.activeRecords(subject, predicate)
	if err != nil {
		return zero, err
	}

	var superseded []Record
	for _, existing := range active {
		if existing.ID == rec.ID {
			return RememberResult{Stored: existing, Existing: true}, nil
		}
		if spec.Cardinality == "single" {
			existing.Status = "superseded"
			existing.SupersededBy = rec.ID
			superseded = append(superseded, existing)
		}
	}

	if err := s.put(rec); err != nil {
		return zero, err
	}
	for _, old := range superseded {
		if err := s.rewrite(old); err != nil {
			return zero, fmt.Errorf("stored %s but failed to supersede %s: %w", rec.ID, old.ID, err)
		}
	}
	return RememberResult{Stored: rec, Superseded: superseded}, nil
}

// RecallQuery is one read. With Query set the read is semantic (the query text
// is embedded and searched); without it the read is an exact filtered listing.
// Both modes apply the same structural filters over the same records.
type RecallQuery struct {
	Query          string
	Subject        string
	Predicate      string
	Kind           string
	MinConfidence  float64
	IncludeHistory bool
	Limit          int
}

func (s *Store) Recall(q RecallQuery) ([]Record, error) {
	// Forgotten records never come back; include_history only adds the
	// superseded ones.
	filter := map[string]any{"status": "active"}
	if q.IncludeHistory {
		filter["status"] = map[string]any{"$in": []string{"active", "superseded"}}
	}
	if q.Subject != "" {
		filter["subject"] = strings.ToLower(strings.TrimSpace(q.Subject))
	}
	if q.Predicate != "" {
		filter["predicate"] = strings.TrimSpace(q.Predicate)
	}
	if q.Kind != "" {
		filter["kind"] = q.Kind
	}
	if q.MinConfidence > 0 {
		filter["confidence"] = map[string]any{"$gte": q.MinConfidence}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}

	if q.Query != "" {
		hits, err := s.db.Search(s.collection, s.embed(q.Query), limit, filter)
		if err != nil {
			return nil, err
		}
		out := make([]Record, 0, len(hits))
		for _, h := range hits {
			out = append(out, recordFromMetadata(h.ID, h.Metadata))
		}
		return out, nil
	}

	vectors, _, err := s.db.List(s.collection, filter, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(vectors))
	for _, v := range vectors {
		out = append(out, recordFromMetadata(v.ID, v.Metadata))
	}
	return out, nil
}

// Forget tombstones matching records: their status flips to "deleted", which
// every recall mode excludes. With value set only that exact record goes;
// without it every record for (subject, predicate) goes, superseded history
// included. A tombstone is a write, not a delete, on purpose: writes are
// read-your-writes through the server's tail overlay, while a hard delete of
// a record already persisted to a bucket segment can ack and still be served
// from the segment until compaction. Re-remembering the same statement
// revives the record (same content-derived id, upserted back to active).
func (s *Store) Forget(subject, predicate, value string) (int, error) {
	subject = strings.ToLower(strings.TrimSpace(subject))
	predicate = strings.TrimSpace(predicate)
	if subject == "" || predicate == "" {
		return 0, fmt.Errorf("forget needs a subject and a predicate")
	}
	filter := map[string]any{"subject": subject, "predicate": predicate}
	if value = strings.TrimSpace(value); value != "" {
		filter["value"] = value
	}
	vectors, _, err := s.db.List(s.collection, filter, 100)
	if err != nil {
		return 0, err
	}
	forgotten := 0
	for _, v := range vectors {
		rec := recordFromMetadata(v.ID, v.Metadata)
		if rec.Status == "deleted" {
			continue
		}
		rec.Status = "deleted"
		if err := s.rewrite(rec); err != nil {
			return forgotten, err
		}
		forgotten++
	}
	return forgotten, nil
}

// activeRecords lists the active records for (subject, predicate).
func (s *Store) activeRecords(subject, predicate string) ([]Record, error) {
	vectors, _, err := s.db.List(s.collection, map[string]any{
		"subject":   subject,
		"predicate": predicate,
		"status":    "active",
	}, 100)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(vectors))
	for _, v := range vectors {
		out = append(out, recordFromMetadata(v.ID, v.Metadata))
	}
	return out, nil
}

// put writes a record with a freshly embedded vector.
func (s *Store) put(rec Record) error {
	return s.db.Put(s.collection, rec.ID, s.embed(rec.text()), rec.metadata())
}

// rewrite re-puts an existing record with updated metadata, preserving its
// exact stored vector (GetMany is the byte-exact read).
func (s *Store) rewrite(rec Record) error {
	stored, err := s.db.GetMany(s.collection, []string{rec.ID})
	if err != nil {
		return err
	}
	if len(stored) == 0 {
		return fmt.Errorf("record %s vanished during rewrite", rec.ID)
	}
	return s.db.Put(s.collection, rec.ID, stored[0].Values, rec.metadata())
}

// text is the natural-language rendering a record is embedded from.
func (rec Record) text() string {
	return rec.Subject + " " + strings.ReplaceAll(rec.Predicate, "_", " ") + " " + rec.Value
}

// metadata flattens a record to typed metadata. Confidence stays a JSON
// number so range filters compare numerically; observed_at is an RFC3339
// string, the documented convention for timestamps.
func (rec Record) metadata() map[string]any {
	return map[string]any{
		"kind":          rec.Kind,
		"subject":       rec.Subject,
		"predicate":     rec.Predicate,
		"value":         rec.Value,
		"confidence":    rec.Confidence,
		"source":        rec.Source,
		"status":        rec.Status,
		"superseded_by": rec.SupersededBy,
		"observed_at":   rec.ObservedAt,
	}
}

func recordFromMetadata(id string, m map[string]any) Record {
	str := func(k string) string { v, _ := m[k].(string); return v }
	conf, _ := m["confidence"].(float64)
	return Record{
		ID:           id,
		Kind:         str("kind"),
		Subject:      str("subject"),
		Predicate:    str("predicate"),
		Value:        str("value"),
		Confidence:   conf,
		Source:       str("source"),
		Status:       str("status"),
		SupersededBy: str("superseded_by"),
		ObservedAt:   str("observed_at"),
	}
}

// recordID derives a record's id from its content, so re-remembering the
// exact same statement is an idempotent upsert by construction.
func recordID(subject, predicate, value string) string {
	h := sha256.Sum256([]byte(subject + "\x00" + predicate + "\x00" + strings.ToLower(value)))
	return fmt.Sprintf("m-%x", h[:6])
}
