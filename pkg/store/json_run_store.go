package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// persist writes the records atomically via a tmp-file + rename pattern.
// This ensures that a crash during write never leaves a partial/corrupt file:
// either the old file remains intact or the new file is fully written.
//
// Improvement over os.WriteFile direct: eliminates partial-write corruption risk.
// LIMITATION: os.Rename is atomic on POSIX (same filesystem). Not guaranteed on
// cross-device or network filesystems. Windows: atomic rename requires ReplaceFile.
// For PoC this is sufficient. Production: use WAL or a proper database.
func (s *JsonRunStore) persist() error {
	data, err := json.Marshal(s.records)
	if err != nil {
		return err
	}

	// Write to temp file in the same directory (ensures same filesystem for rename)
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".runstore-*.tmp")
	if err != nil {
		return fmt.Errorf("persist tmp create: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("persist tmp write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("persist tmp close: %w", err)
	}

	// Atomic rename: on POSIX, this is guaranteed atomic
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("persist rename: %w", err)
	}
	return nil
}

// Path returns the file path this store is backed by.
// Useful for opening a second store instance (e.g., restart simulation in tests).
func (s *JsonRunStore) Path() string {
	return s.path
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
