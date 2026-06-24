package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"frostgate/internal/schema"
)

// NewSessionID returns a random UUID-like session identifier.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "session"
	}
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// sessionData is the persisted form of a conversation.
type sessionData struct {
	Model    string           `json:"model"`
	Mode     string           `json:"mode"`
	Messages []schema.Message `json:"messages"`
}

// sessionsDir is ~/.frostcode/sessions.
func sessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".frostcode/sessions"
	}
	return filepath.Join(home, ".frostcode", "sessions")
}

func sessionPath(name string) string {
	if name == "" {
		name = "last"
	}
	// sanitize to a safe filename
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, name)
	return filepath.Join(sessionsDir(), name+".json")
}

// Save writes the agent's conversation to a named session.
func (a *Agent) Save(name string) error {
	if err := os.MkdirAll(sessionsDir(), 0o755); err != nil {
		return err
	}
	data := sessionData{Model: a.Model, Mode: a.mode.String(), Messages: a.messages}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := sessionPath(name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, sessionPath(name))
}

// Load restores a named session into the agent (replacing current messages).
func (a *Agent) Load(name string) error {
	b, err := os.ReadFile(sessionPath(name))
	if err != nil {
		return err
	}
	var data sessionData
	if err := json.Unmarshal(b, &data); err != nil {
		return err
	}
	if data.Model != "" {
		a.Model = data.Model
	}
	if data.Mode == "plan" {
		a.mode = ModePlan
	} else {
		a.mode = ModeBuild
	}
	a.messages = data.Messages
	// Ensure the system prompt reflects the (possibly new) mode.
	if len(a.messages) == 0 || a.messages[0].Role != "system" {
		a.applySystem()
	}
	a.undo = nil
	return nil
}

// ListSessions returns the saved session names, newest-looking first.
func ListSessions() []string {
	ents, err := os.ReadDir(sessionsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}
