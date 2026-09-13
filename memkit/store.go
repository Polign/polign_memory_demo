// Package memkit adapts Recall's Go client to the demo's tools and inspector.
// Recall owns validation, event storage, corrections, retractions, and folding.
package memkit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Polign/recall"
)

type Predicate = recall.Predicate
type Registry = recall.Registry

func LoadRegistry(raw []byte) (Registry, error) { return recall.LoadRegistry(raw) }

// Record is a presentation of a Recall belief or event, never stored metadata.
// Historical assertions and withdrawals remain visible in the inspector.
type Record struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Subject      string  `json:"subject"`
	Predicate    string  `json:"predicate"`
	Value        any     `json:"value"`
	Confidence   float64 `json:"confidence"`
	Source       string  `json:"source"`
	Status       string  `json:"status"`
	SupersededBy string  `json:"superseded_by,omitempty"`
	ObservedAt   string  `json:"observed_at"`
	Retraction   bool    `json:"retraction,omitempty"`
}

type RememberResult struct {
	Stored     Record   `json:"stored"`
	Existing   bool     `json:"already_known,omitempty"`
	Superseded []Record `json:"superseded,omitempty"`
}

type Store struct {
	client     *recall.Client
	backend    recall.Backend
	collection string
}

func NewStore(backend recall.Backend, collection string, registry Registry, embedder recall.Embedder) (*Store, error) {
	guarded := memoryBackend{backend}
	client, err := recall.NewClient(recall.Config{Backend: guarded, Collection: collection, Registry: registry, Embedder: embedder, Materialize: true})
	if err != nil {
		return nil, err
	}
	return &Store{client: client, backend: guarded, collection: collection}, nil
}

// Check verifies listing support and catches an old demo collection at startup.
func (s *Store) Check(ctx context.Context) error {
	_, _, err := s.backend.List(ctx, s.collection, nil, 1)
	if err != nil {
		return fmt.Errorf("Recall needs a dedicated event collection and a Polign server with complete listings (v0.6.4+): %w", err)
	}
	return nil
}

func (s *Store) Registry() Registry { return s.client.Registry() }

// Remember preserves the old demo helper's zero-means-default convention.
// Tool callers use RememberContext so an explicit confidence of zero survives.
func (s *Store) Remember(kind, subject, predicate string, value any, confidence float64, source string) (RememberResult, error) {
	var conf *float64
	if confidence != 0 {
		conf = &confidence
	}
	return s.RememberContext(context.Background(), recall.RememberRequest{Kind: kind, Subject: subject, Predicate: predicate, Value: value, Confidence: conf, Source: source})
}

func (s *Store) RememberContext(ctx context.Context, q recall.RememberRequest) (RememberResult, error) {
	result, err := s.client.Remember(ctx, q)
	if err != nil {
		return RememberResult{}, err
	}
	out := RememberResult{Stored: beliefRecord(result.Stored), Existing: result.Existing}
	for _, b := range result.Superseded {
		rec := beliefRecord(b)
		rec.Status, rec.SupersededBy = "superseded", out.Stored.ID
		out.Superseded = append(out.Superseded, rec)
	}
	return out, nil
}

type RecallQuery struct {
	Query          string
	Subject        string
	Predicate      string
	Kind           string
	MinConfidence  float64
	IncludeHistory bool
	Limit          int
	ValueMin       *float64
	ValueMax       *float64
	AsOf           time.Time
}

func (s *Store) Recall(q RecallQuery) ([]Record, error) {
	return s.RecallContext(context.Background(), q)
}

func (s *Store) RecallContext(ctx context.Context, q RecallQuery) ([]Record, error) {
	if q.IncludeHistory {
		return s.historyRecords(ctx, q)
	}
	beliefs, err := s.client.Recall(ctx, recall.Query{Text: q.Query, Subject: q.Subject, Predicate: q.Predicate, Kind: q.Kind, MinConfidence: q.MinConfidence, Limit: q.Limit, ValueMin: q.ValueMin, ValueMax: q.ValueMax, AsOf: q.AsOf})
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(beliefs))
	for _, b := range beliefs {
		out = append(out, beliefRecord(b))
	}
	return out, nil
}

// The inspector derives its labels from an audited Recall replay. It does not
// implement correction rules or update old rows to maintain their status.
func (s *Store) historyRecords(ctx context.Context, q RecallQuery) ([]Record, error) {
	if q.Query != "" || q.Kind != "" || q.MinConfidence != 0 || q.ValueMin != nil || q.ValueMax != nil {
		return nil, fmt.Errorf("history accepts subject, predicate, as_of and limit; use current recall for search and value filters")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	if limit > recall.MaxExport {
		return nil, fmt.Errorf("history limit exceeds %d", recall.MaxExport)
	}
	bundle, err := s.client.ExportAudit(ctx, recall.AuditRequest{Scope: recall.AuditScope{Subject: q.Subject, Predicate: q.Predicate}, AsOf: q.AsOf})
	if err != nil {
		return nil, err
	}
	if len(bundle.Events) > limit {
		return nil, fmt.Errorf("history contains %d events; narrow subject/predicate or raise limit (maximum %d)", len(bundle.Events), recall.MaxExport)
	}
	beliefs, err := bundle.Replay()
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, b := range beliefs {
		active[b.EventID] = true
	}
	out := make([]Record, 0, len(bundle.Events))
	for _, e := range bundle.Events {
		status := "historical"
		switch {
		case e.ObservedAt.After(bundle.AsOf):
			status = "pending"
		case e.Retraction:
			status = "withdrawal"
		case active[e.ID]:
			status = "active"
		}
		out = append(out, Record{ID: e.ID, Kind: e.Kind, Subject: e.Subject, Predicate: e.Predicate, Value: e.Value, Confidence: e.Confidence, Source: e.Source, Status: status, ObservedAt: e.ObservedAt.UTC().Format(time.RFC3339Nano), Retraction: e.Retraction})
	}
	return out, nil
}

func (s *Store) History(ctx context.Context, subject, predicate string) ([]recall.Event, error) {
	return s.client.History(ctx, subject, predicate)
}

func (s *Store) ForgetContext(ctx context.Context, q recall.ForgetRequest) (int, error) {
	return s.client.Forget(ctx, q)
}

// Forget adapts the old string-based helper. Model tools use typed values and
// explicit All through ForgetContext, so false and zero cannot clear a pair.
func (s *Store) Forget(subject, predicate, value string) (int, error) {
	q := recall.ForgetRequest{Subject: subject, Predicate: predicate, All: strings.TrimSpace(value) == ""}
	if !q.All {
		q.Value = value
		var err error
		switch s.Registry()[predicate].ValueType {
		case "number":
			q.Value, err = strconv.ParseFloat(value, 64)
		case "boolean":
			q.Value, err = strconv.ParseBool(value)
		}
		if err != nil {
			return 0, err
		}
	}
	return s.ForgetContext(context.Background(), q)
}

func beliefRecord(b recall.Belief) Record {
	return Record{ID: b.EventID, Kind: b.Kind, Subject: b.Subject, Predicate: b.Predicate, Value: b.Value, Confidence: b.Confidence, Source: b.Source, Status: "active", ObservedAt: b.ObservedAt.UTC().Format(time.RFC3339Nano)}
}

// Legacy memkit rows were mutated in place. Treating every old row as an
// assertion would revive superseded/deleted facts. Refuse them rather than
// guessing a history or mixing embedding formats; the old collection stays put.
type memoryBackend struct{ recall.Backend }

func legacyRecord(metadata map[string]any) error {
	if _, exists := metadata["status"]; exists {
		return fmt.Errorf("legacy memkit records found; use a new Recall collection (default recall_demo_lexical_v1); old memories are not automatically migrated")
	}
	return nil
}

func (b memoryBackend) List(ctx context.Context, collection string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	rows, total, err := b.Backend.List(ctx, collection, filter, limit)
	if err != nil {
		return nil, 0, err
	}
	for _, row := range rows {
		if err := legacyRecord(row.Metadata); err != nil {
			return nil, 0, err
		}
	}
	return rows, total, nil
}

func (b memoryBackend) Search(ctx context.Context, collection string, values []float32, k int, filter map[string]any) ([]recall.Hit, error) {
	hits, err := b.Backend.Search(ctx, collection, values, k, filter)
	if err != nil {
		return nil, err
	}
	for _, hit := range hits {
		if err := legacyRecord(hit.Metadata); err != nil {
			return nil, err
		}
	}
	return hits, nil
}

func (b memoryBackend) Watermark(ctx context.Context, collection string) (string, error) {
	if backend, ok := b.Backend.(recall.WatermarkBackend); ok {
		return backend.Watermark(ctx, collection)
	}
	return "", recall.ErrWatermarkUnsupported
}
