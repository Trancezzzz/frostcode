// Package store provides durable gateway state. It is deliberately small: a
// Snapshot of the data that must survive restarts (today: per-identity token
// spend), behind a Store interface with in-memory and JSON-file backends.
//
// A shared File path is also how co-located cluster nodes share budgets; a
// production deployment would implement Store against Redis instead.
package store

import (
	"encoding/json"
	"os"
	"sync"
)

// Snapshot is the serializable gateway state.
type Snapshot struct {
	// Spend maps a stable identity name (virtual-key name, or "oidc:<sub>")
	// to cumulative total tokens spent.
	Spend map[string]int64 `json:"spend"`
}

// newSnapshot returns an initialized Snapshot.
func newSnapshot() Snapshot { return Snapshot{Spend: map[string]int64{}} }

// Store loads and saves a Snapshot.
type Store interface {
	Load() (Snapshot, error)
	Save(Snapshot) error
}

// Memory is a non-durable Store used when persistence is disabled or in tests.
type Memory struct {
	mu   sync.Mutex
	snap Snapshot
}

// NewMemory builds an empty in-memory store.
func NewMemory() *Memory { return &Memory{snap: newSnapshot()} }

func (m *Memory) Load() (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.snap), nil
}

func (m *Memory) Save(s Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snap = clone(s)
	return nil
}

// File is a JSON-file backed Store. Saves are atomic (write temp + rename) so
// a crash mid-write can't corrupt the state file.
type File struct {
	path string
	mu   sync.Mutex
}

// NewFile builds a file store at path.
func NewFile(path string) *File { return &File{path: path} }

func (f *File) Load() (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return newSnapshot(), nil // first run
		}
		return newSnapshot(), err
	}
	s := newSnapshot()
	if err := json.Unmarshal(b, &s); err != nil {
		return newSnapshot(), err
	}
	if s.Spend == nil {
		s.Spend = map[string]int64{}
	}
	return s, nil
}

func (f *File) Save(s Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

func clone(s Snapshot) Snapshot {
	out := newSnapshot()
	for k, v := range s.Spend {
		out.Spend[k] = v
	}
	return out
}
