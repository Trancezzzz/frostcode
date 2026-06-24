package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Sandbox confines file tools to one or more allowed root directories. All
// tool paths resolve through it; anything outside the allowed roots is
// rejected. Additional roots can be granted at runtime (e.g. via /add-dir).
type Sandbox struct {
	mu    sync.RWMutex
	roots []string // absolute, cleaned
}

// NewSandbox creates a sandbox rooted at root (its primary directory).
func NewSandbox(root string) *Sandbox {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	return &Sandbox{roots: []string{abs}}
}

// Primary returns the main project directory (used as the shell working dir and
// the base for relative paths).
func (s *Sandbox) Primary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.roots[0]
}

// Roots returns a copy of the allowed roots.
func (s *Sandbox) Roots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.roots))
	copy(out, s.roots)
	return out
}

// Add grants an additional allowed root.
func (s *Sandbox) Add(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("not a directory: %s", root)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.roots {
		if r == abs {
			return abs, nil // already allowed
		}
	}
	s.roots = append(s.roots, abs)
	return abs, nil
}

// Resolve turns a tool path into an absolute path confined to an allowed root.
// Relative paths are taken against the primary root; absolute paths are allowed
// only if they fall within some granted root.
func (s *Sandbox) Resolve(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("empty path")
	}
	var cand string
	if filepath.IsAbs(rel) {
		cand = filepath.Clean(rel)
	} else {
		cand = filepath.Join(s.Primary(), filepath.Clean(rel))
	}
	candAbs, err := filepath.Abs(cand)
	if err != nil {
		return "", err
	}
	for _, root := range s.Roots() {
		if candAbs == root || strings.HasPrefix(candAbs, root+string(os.PathSeparator)) {
			return candAbs, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed directories (use /add-dir to grant access)", rel)
}
