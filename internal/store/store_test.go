package store

import (
	"path/filepath"
	"testing"
)

func TestFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f := NewFile(path)

	// First load on a missing file yields an empty snapshot, no error.
	s, err := f.Load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if len(s.Spend) != 0 {
		t.Fatalf("expected empty snapshot, got %v", s.Spend)
	}

	s.Spend["team-a"] = 1234
	s.Spend["oidc:user-9"] = 50
	if err := f.Save(s); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A fresh File at the same path must read back the saved spend.
	got, err := NewFile(path).Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Spend["team-a"] != 1234 || got.Spend["oidc:user-9"] != 50 {
		t.Fatalf("roundtrip mismatch: %v", got.Spend)
	}
}

func TestMemoryStore(t *testing.T) {
	m := NewMemory()
	_ = m.Save(Snapshot{Spend: map[string]int64{"x": 7}})
	s, _ := m.Load()
	if s.Spend["x"] != 7 {
		t.Fatalf("memory store lost value")
	}
	// Mutating the returned snapshot must not affect stored state (clone).
	s.Spend["x"] = 99
	s2, _ := m.Load()
	if s2.Spend["x"] != 7 {
		t.Fatalf("memory store not isolated from caller mutation")
	}
}
