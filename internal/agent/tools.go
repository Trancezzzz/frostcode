// Package agent implements a coding-agent CLI on top of the Frostgate gateway:
// local tools (file ops, shell, search), an agentic tool loop, a terminal REPL,
// mode note injection, and persistent goal tracking. The model reaches the tools
// via OpenAI function-calling.
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Tool is one capability exposed to the model.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema for the function parameters
	Destructive bool            // requires user approval unless auto-approve is on
	Run         func(args map[string]any) (string, error)

	// Preview returns a human-readable diff/description of what Run would do,
	// computed WITHOUT applying it. Shown in the approval prompt. Optional.
	Preview func(args map[string]any) string
	// Capture snapshots prior state and returns a restore closure for /undo,
	// called just before Run. Optional.
	Capture func(args map[string]any) (func() error, error)
}

// OpenAITool is the function-tool wire shape sent to the model.
type OpenAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// asOpenAI converts a Tool to the wire format.
func (t Tool) asOpenAI() OpenAITool {
	var o OpenAITool
	o.Type = "function"
	o.Function.Name = t.Name
	o.Function.Description = t.Description
	o.Function.Parameters = t.Schema
	return o
}

// DefaultTools returns the coding toolset rooted at root (the project dir).
// All path arguments are resolved relative to root and confined to it.
// DefaultTools builds the toolset confined to sb. If sh is non-nil, the bash
// tool uses that persistent shell session (cd/env persist); otherwise each
// command runs in a fresh process.
func DefaultTools(sb *Sandbox, sh *Shell) []Tool {
	abs := sb.Resolve

	// captureFile snapshots a file's current state; the returned closure
	// restores it (rewriting prior bytes with original permissions, or deleting a file that is new).
	captureFile := func(rel string) (func() error, error) {
		p, err := abs(rel)
		if err != nil {
			return nil, err
		}
		info, statErr := os.Stat(p)
		prior, readErr := os.ReadFile(p)
		existed := readErr == nil
		var origMode os.FileMode = 0o644
		if statErr == nil {
			origMode = info.Mode()
		}
		return func() error {
			if !existed {
				return os.Remove(p)
			}
			return os.WriteFile(p, prior, origMode)
		}, nil
	}

	tools := []Tool{
		{
			Name:        "read_file",
			Description: "Read a UTF-8 text file and return its contents with 1-based line numbers. Use before editing.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"path":{"type":"string","description":"file path relative to the project root"},
				"offset":{"type":"integer","description":"1-based start line (optional)"},
				"limit":{"type":"integer","description":"max lines to read (optional)"}},
				"required":["path"]}`),
			Run: func(a map[string]any) (string, error) {
				p, err := abs(str(a, "path"))
				if err != nil {
					return "", err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return "", err
				}
				lines := strings.Split(string(b), "\n")
				start, limit := intOr(a, "offset", 1), intOr(a, "limit", len(lines))
				if start < 1 {
					start = 1
				}
				var sb strings.Builder
				for i := start - 1; i < len(lines) && i < start-1+limit; i++ {
					fmt.Fprintf(&sb, "%6d\t%s\n", i+1, lines[i])
				}
				return sb.String(), nil
			},
		},
		{
			Name:        "write_file",
			Description: "Create or overwrite a file with the given content (creates parent directories).",
			Destructive: true,
			Schema: json.RawMessage(`{"type":"object","properties":{
				"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			Run: func(a map[string]any) (string, error) {
				p, err := abs(str(a, "path"))
				if err != nil {
					return "", err
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return "", err
				}
				content := str(a, "content")
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("wrote %s (%d bytes)", str(a, "path"), len(content)), nil
			},
		},
		{
			Name:        "edit_file",
			Description: "Replace an exact substring in a file. old_string must occur exactly once unless replace_all is true.",
			Destructive: true,
			Schema: json.RawMessage(`{"type":"object","properties":{
				"path":{"type":"string"},
				"old_string":{"type":"string"},
				"new_string":{"type":"string"},
				"replace_all":{"type":"boolean"}},
				"required":["path","old_string","new_string"]}`),
			Run: func(a map[string]any) (string, error) {
				p, err := abs(str(a, "path"))
				if err != nil {
					return "", err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return "", err
				}
				body := string(b)
				old, neu := str(a, "old_string"), str(a, "new_string")
				n := strings.Count(body, old)
				if n == 0 {
					return "", fmt.Errorf("old_string not found in %s", str(a, "path"))
				}
				if n > 1 && !boolOr(a, "replace_all", false) {
					return "", fmt.Errorf("old_string occurs %d times; set replace_all or add more context", n)
				}
				out := strings.ReplaceAll(body, old, neu)
				if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("edited %s (%d replacement(s))\n%s", str(a, "path"), n, diffBlock(old, neu)), nil
			},
		},
		{
			Name:        "delete_file",
			Description: "Delete a file or empty directory.",
			Destructive: true,
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			Run: func(a map[string]any) (string, error) {
				p, err := abs(str(a, "path"))
				if err != nil {
					return "", err
				}
				if err := os.Remove(p); err != nil {
					return "", err
				}
				return "deleted " + str(a, "path"), nil
			},
		},
		{
			Name:        "list_dir",
			Description: "List the entries of a directory (default: project root).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			Run: func(a map[string]any) (string, error) {
				rel := str(a, "path")
				if rel == "" {
					rel = "."
				}
				p, err := abs(rel)
				if err != nil {
					return "", err
				}
				ents, err := os.ReadDir(p)
				if err != nil {
					return "", err
				}
				var sb strings.Builder
				for _, e := range ents {
					name := e.Name()
					if e.IsDir() {
						name += "/"
					}
					sb.WriteString(name + "\n")
				}
				if sb.Len() == 0 {
					return "(empty)", nil
				}
				return sb.String(), nil
			},
		},
		{
			Name:        "glob",
			Description: "Find files matching a glob pattern (e.g. **/*.go). Returns matching paths.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`),
			Run: func(a map[string]any) (string, error) {
				return globFiles(sb.Primary(), str(a, "pattern"))
			},
		},
		{
			Name:        "grep",
			Description: "Search file contents for a query string. Set regex=true to use a regular expression (Go syntax). Returns matching path:line: text.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"query":{"type":"string"},
				"path":{"type":"string","description":"subdir to search (optional)"},
				"regex":{"type":"boolean","description":"treat query as a Go regular expression (default false)"}},
				"required":["query"]}`),
			Run: func(a map[string]any) (string, error) {
				base := sb.Primary()
				if s := str(a, "path"); s != "" {
					p, err := abs(s)
					if err != nil {
						return "", err
					}
					base = p
				}
				return grepFiles(sb.Primary(), base, str(a, "query"), boolOr(a, "regex", false))
			},
		},
		{
			Name:        "multi_edit",
			Description: "Apply several exact-substring edits to one file in order, atomically. edits is a list of {old_string,new_string,replace_all?}.",
			Destructive: true,
			Schema: json.RawMessage(`{"type":"object","properties":{
				"path":{"type":"string"},
				"edits":{"type":"array","items":{"type":"object","properties":{
					"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},
					"required":["old_string","new_string"]}}},
				"required":["path","edits"]}`),
			Run: func(a map[string]any) (string, error) {
				p, err := abs(str(a, "path"))
				if err != nil {
					return "", err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return "", err
				}
				body := string(b)
				edits, _ := a["edits"].([]any)
				if len(edits) == 0 {
					return "", fmt.Errorf("no edits provided")
				}
				applied := 0
				for i, raw := range edits {
					e, _ := raw.(map[string]any)
					old, neu := str(e, "old_string"), str(e, "new_string")
					cnt := strings.Count(body, old)
					if cnt == 0 {
						return "", fmt.Errorf("edit %d: old_string not found", i+1)
					}
					if cnt > 1 && !boolOr(e, "replace_all", false) {
						return "", fmt.Errorf("edit %d: old_string occurs %d times; set replace_all", i+1, cnt)
					}
					body = strings.ReplaceAll(body, old, neu)
					applied++
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("applied %d edit(s) to %s", applied, str(a, "path")), nil
			},
		},
		{
			Name:        "make_dir",
			Description: "Create a directory (and any parents).",
			Destructive: true,
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			Run: func(a map[string]any) (string, error) {
				p, err := abs(str(a, "path"))
				if err != nil {
					return "", err
				}
				if err := os.MkdirAll(p, 0o755); err != nil {
					return "", err
				}
				return "created " + str(a, "path") + "/", nil
			},
		},
		{
			Name:        "move_file",
			Description: "Move or rename a file or directory within the project.",
			Destructive: true,
			Schema: json.RawMessage(`{"type":"object","properties":{
				"from":{"type":"string"},"to":{"type":"string"}},"required":["from","to"]}`),
			Run: func(a map[string]any) (string, error) {
				from, err := abs(str(a, "from"))
				if err != nil {
					return "", err
				}
				to, err := abs(str(a, "to"))
				if err != nil {
					return "", err
				}
				if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
					return "", err
				}
				if err := os.Rename(from, to); err != nil {
					return "", err
				}
				return fmt.Sprintf("moved %s → %s", str(a, "from"), str(a, "to")), nil
			},
		},
		{
			Name:        "tree",
			Description: "Show a directory tree (default: project root) to a depth. Great for getting your bearings.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"path":{"type":"string"},"depth":{"type":"integer","description":"max depth (default 2)"}},"properties_note":"all optional"}`),
			Run: func(a map[string]any) (string, error) {
				rel := str(a, "path")
				if rel == "" {
					rel = "."
				}
				base, err := abs(rel)
				if err != nil {
					return "", err
				}
				return treeView(base, intOr(a, "depth", 2))
			},
		},
		{
			Name:        "fetch_url",
			Description: "HTTP GET a URL and return the response body as text (truncated). Use to read docs or API references.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
			Run: func(a map[string]any) (string, error) {
				return fetchURL(str(a, "url"))
			},
		},
		{
			Name: "todo_write",
			Description: "Maintain a visible task list for the current job. Call it to set/update todos as you work. " +
				"status is one of: pending, in_progress, done. Keep exactly one item in_progress at a time.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"items":{"type":"array","items":{"type":"object","properties":{
					"task":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","done"]}},
					"required":["task","status"]}}},
				"required":["items"]}`),
			Run: func(a map[string]any) (string, error) {
				items, _ := a["items"].([]any)
				if len(items) == 0 {
					return "", fmt.Errorf("items is required")
				}
				return fmt.Sprintf("todo list updated (%d items)", len(items)), nil
			},
		},
		{
			Name:        "bash",
			Description: "Run a shell command in the project root and return combined stdout/stderr. Use for builds, tests, git, etc.",
			Destructive: true,
			Schema: json.RawMessage(`{"type":"object","properties":{
				"command":{"type":"string"}},"required":["command"]}`),
			Run: func(a map[string]any) (string, error) {
				if sh != nil {
					return sh.Run(str(a, "command"), 120*time.Second)
				}
				return runShell(sb.Primary(), str(a, "command"))
			},
		},
	}

	// Attach diff previews and undo capture to file-mutating tools by name.
	readBody := func(rel string) string {
		p, err := abs(rel)
		if err != nil {
			return ""
		}
		b, _ := os.ReadFile(p)
		return string(b)
	}
	for i := range tools {
		t := &tools[i]
		switch t.Name {
		case "write_file":
			t.Capture = func(a map[string]any) (func() error, error) { return captureFile(str(a, "path")) }
			t.Preview = func(a map[string]any) string {
				path, content := str(a, "path"), str(a, "content")
				if old := readBody(path); old != "" {
					return diffBlock(old, content)
				}
				return "new file " + path + "\n" + diffBlock("", content)
			}
		case "edit_file":
			t.Capture = func(a map[string]any) (func() error, error) { return captureFile(str(a, "path")) }
			t.Preview = func(a map[string]any) string { return diffBlock(str(a, "old_string"), str(a, "new_string")) }
		case "multi_edit":
			t.Capture = func(a map[string]any) (func() error, error) { return captureFile(str(a, "path")) }
			t.Preview = func(a map[string]any) string {
				edits, _ := a["edits"].([]any)
				var b strings.Builder
				for _, raw := range edits {
					e, _ := raw.(map[string]any)
					b.WriteString(diffBlock(str(e, "old_string"), str(e, "new_string")) + "\n")
				}
				return strings.TrimRight(b.String(), "\n")
			}
		case "delete_file":
			t.Capture = func(a map[string]any) (func() error, error) { return captureFile(str(a, "path")) }
			t.Preview = func(a map[string]any) string { return "delete " + str(a, "path") }
		case "move_file":
			t.Capture = func(a map[string]any) (func() error, error) {
				from, err := abs(str(a, "from"))
				if err != nil {
					return nil, err
				}
				to, err := abs(str(a, "to"))
				if err != nil {
					return nil, err
				}
				return func() error { return os.Rename(to, from) }, nil
			}
			t.Preview = func(a map[string]any) string { return str(a, "from") + " → " + str(a, "to") }
		case "make_dir":
			t.Capture = func(a map[string]any) (func() error, error) {
				p, err := abs(str(a, "path"))
				if err != nil {
					return nil, err
				}
				_, statErr := os.Stat(p)
				existed := statErr == nil
				return func() error {
					if existed {
						return nil // already there before; nothing to undo
					}
					return os.RemoveAll(p)
				}, nil
			}
			t.Preview = func(a map[string]any) string { return "create dir " + str(a, "path") + "/" }
		}
	}
	return tools
}

// runShell executes a command via bash (preferred) or cmd, with a timeout.
func runShell(root, command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("empty command")
	}
	var cmd *exec.Cmd
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd = exec.Command(bash, "-lc", command)
	} else if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	cmd.Dir = root
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		return string(out) + "\n[timed out after 120s]", nil
	}
	res := string(out)
	if runErr != nil {
		res += "\n[exit error: " + runErr.Error() + "]"
	}
	return truncate(res, 16000), nil
}

// globFiles walks root collecting paths matching pattern (supports **).
func globFiles(root, pattern string) (string, error) {
	var matches []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if skipDir(path) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if matchGlob(pattern, rel) {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return "(no matches)", nil
	}
	return strings.Join(matches, "\n"), nil
}

// matchGlob supports a leading "**/" and standard filepath.Match on the rest.
func matchGlob(pattern, name string) bool {
	if strings.HasPrefix(pattern, "**/") {
		suffix := pattern[3:]
		base := name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			base = name[i+1:]
		}
		ok, _ := filepath.Match(suffix, base)
		if ok {
			return true
		}
	}
	ok, _ := filepath.Match(pattern, name)
	return ok
}

// grepFiles searches under base for query, displaying paths relative to displayRoot.
// When useRegex is true the query is compiled as a Go regular expression.
// Results are capped at 200 matches; a notice is appended when the cap is hit.
func grepFiles(displayRoot, base, query string, useRegex bool) (string, error) {
	if query == "" {
		return "", fmt.Errorf("empty query")
	}
	var re *regexp.Regexp
	if useRegex {
		var err error
		re, err = regexp.Compile(query)
		if err != nil {
			return "", fmt.Errorf("invalid regex: %w", err)
		}
	}
	match := func(line string) bool {
		if re != nil {
			return re.MatchString(line)
		}
		return strings.Contains(line, query)
	}
	var hits []string
	truncated := false
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || skipDir(path) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		ln := 0
		for sc.Scan() {
			ln++
			if match(sc.Text()) {
				rel, _ := filepath.Rel(displayRoot, path)
				hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), ln, strings.TrimSpace(sc.Text())))
				if len(hits) >= 200 {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return "(no matches)", nil
	}
	result := strings.Join(hits, "\n")
	if truncated {
		result += "\n…[truncated: showing first 200 matches — narrow your search or use a subdirectory]"
	}
	return result, nil
}

// diffBlock renders a compact -old/+new diff (capped) for edit previews.
func diffBlock(old, neu string) string {
	var b strings.Builder
	cap := func(s string) []string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		if len(lines) > 8 {
			lines = append(lines[:8], "…")
		}
		return lines
	}
	for _, l := range cap(old) {
		b.WriteString("- " + l + "\n")
	}
	for _, l := range cap(neu) {
		b.WriteString("+ " + l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// treeView renders an indented directory tree up to depth.
func treeView(base string, depth int) (string, error) {
	if depth < 1 {
		depth = 1
	}
	var b strings.Builder
	var walk func(dir, prefix string, d int) error
	walk = func(dir, prefix string, d int) error {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		// dirs first, then files, each alphabetical.
		sort.Slice(ents, func(i, j int) bool {
			if ents[i].IsDir() != ents[j].IsDir() {
				return ents[i].IsDir()
			}
			return ents[i].Name() < ents[j].Name()
		})
		for i, e := range ents {
			if skipDir(filepath.Join(dir, e.Name())) && e.IsDir() {
				continue
			}
			branch := "├─ "
			nextPrefix := prefix + "│  "
			if i == len(ents)-1 {
				branch = "└─ "
				nextPrefix = prefix + "   "
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(prefix + branch + name + "\n")
			if e.IsDir() && d < depth {
				_ = walk(filepath.Join(dir, e.Name()), nextPrefix, d+1)
			}
		}
		return nil
	}
	if err := walk(base, "", 1); err != nil {
		return "", err
	}
	if b.Len() == 0 {
		return "(empty)", nil
	}
	return truncate(b.String(), maxToolResultChars), nil
}

// fetchURL performs a GET and returns the body text, capped at 1 MiB.
// A notice is appended when the body was cut short.
func fetchURL(url string) (string, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("url must start with http:// or https://")
	}
	const cap = 1 << 20 // 1 MiB
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, cap+1))
	truncated := len(body) > cap
	if truncated {
		body = body[:cap]
	}
	result := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, truncate(string(body), 16000))
	if truncated {
		result += "\n…[truncated: response exceeded 1 MiB — only the first 1 MiB is shown]"
	}
	return result, nil
}

// skipDir filters noisy directories from search/glob.
func skipDir(path string) bool {
	p := filepath.ToSlash(path)
	for _, seg := range []string{"/.git/", "/node_modules/", "/.frostcode/", "/vendor/"} {
		if strings.Contains(p, seg) {
			return true
		}
	}
	return false
}

// --- small arg helpers ---

func str(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}
func intOr(a map[string]any, k string, def int) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}
func boolOr(a map[string]any, k string, def bool) bool {
	if v, ok := a[k].(bool); ok {
		return v
	}
	return def
}
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
