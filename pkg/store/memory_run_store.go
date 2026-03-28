package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// InMemoryRunStore holds all records in a Go map.
// It is fast and simple but loses all data when the process restarts.
// Use this to prove hypothesis: "memory store loses queued runs on restart."
type InMemoryRunStore struct {
	mu      sync.RWMutex
	records map[string]RunRecord
}

var _ RunStore = (*InMemoryRunStore)(nil)

func NewInMemoryRunStore() *InMemoryRunStore {
	return &InMemoryRunStore{records: make(map[string]RunRecord)}
}

func (s *InMemoryRunStore) Enqueue(_ context.Context, rec RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[rec.RunID]; ok {
		return ErrAlreadyExists
	}
	now := time.Now()
	rec.CreatedAt = now
	rec.UpdatedAt = now
	s.records[rec.RunID] = rec
	return nil
}

func (s *InMemoryRunStore) Get(_ context.Context, runID string) (RunRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[runID]
	return r, ok, nil
}

func (s *InMemoryRunStore) UpdateState(_ context.Context, runID string, from, to RunState) error {
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
	return nil
}

func (s *InMemoryRunStore) ListByState(_ context.Context, state RunState) ([]RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []RunRecord
	for _, r := range s.records {
		if r.State == state {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *InMemoryRunStore) Delete(_ context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[runID]; !ok {
		return ErrNotFound
	}
	delete(s.records, runID)
	return nil
}
