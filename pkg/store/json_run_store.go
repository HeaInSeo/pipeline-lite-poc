package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// JsonRunStore is a durable-lite RunStore backed by a single JSON file.
//
// NOT production-grade: no WAL, no atomic rename guarantee on all OSes,
// no concurrent multi-process access. Purpose: demonstrate that a durable
// store survives process restart while InMemoryRunStore does not.
//
// ASSUMPTION: production replaces this with PostgreSQL or equivalent.
type JsonRunStore struct {
	mu      sync.Mutex
	path    string
	records map[string]RunRecord
}

var _ RunStore = (*JsonRunStore)(nil)

// NewJsonRunStore opens or creates the JSON store at path.
// If path does not exist, it starts empty (no error).
func NewJsonRunStore(path string) (*JsonRunStore, error) {
	s := &JsonRunStore{path: path, records: make(map[string]RunRecord)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *JsonRunStore) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil // fresh start
	}
	if err != nil {
		return fmt.Errorf("json store load %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil // empty file = fresh start
	}
	return json.Unmarshal(data, &s.records)
}

func (s *JsonRunStore) persist() error {
	data, err := json.Marshal(s.records)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

func (s *JsonRunStore) Enqueue(_ context.Context, rec RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[rec.RunID]; ok {
		return ErrAlreadyExists
	}
	now := time.Now()
	rec.CreatedAt = now
	rec.UpdatedAt = now
	s.records[rec.RunID] = rec
	return s.persist()
}

func (s *JsonRunStore) Get(_ context.Context, runID string) (RunRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[runID]
	return r, ok, nil
}

func (s *JsonRunStore) UpdateState(_ context.Context, runID string, from, to RunState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[runID]
	if !ok {
		return ErrNotFound
	}
	if r.State != from {
		return fmt.Errorf("state mismatch: expected %s, got %s", from, r.State)
	}
	if err := ValidateTransition(from, to); err != nil {
		return err
	}
	r.State = to
	r.UpdatedAt = time.Now()
	s.records[runID] = r
	return s.persist()
}

func (s *JsonRunStore) ListByState(_ context.Context, state RunState) ([]RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RunRecord
	for _, r := range s.records {
		if r.State == state {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *JsonRunStore) Delete(_ context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[runID]; !ok {
		return ErrNotFound
	}
	delete(s.records, runID)
	return s.persist()
}
