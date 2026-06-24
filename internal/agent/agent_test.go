package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"frostgate/internal/schema"
)

// --- tool tests ---

func TestWriteEditReadDelete(t *testing.T) {
	root := t.TempDir()
	tools := map[string]Tool{}
	for _, tl := range DefaultTools(NewSandbox(root), nil) {
		tools[tl.Name] = tl
	}

	// write
	if _, err := tools["write_file"].Run(map[string]any{"path": "a/b.txt", "content": "hello world"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a", "b.txt")); string(b) != "hello world" {
		t.Fatalf("file content wrong: %q", b)
	}
	// edit
	if _, err := tools["edit_file"].Run(map[string]any{"path": "a/b.txt", "old_string": "world", "new_string": "frostcode"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	out, err := tools["read_file"].Run(map[string]any{"path": "a/b.txt"})
	if err != nil || !strings.Contains(out, "hello frostcode") {
		t.Fatalf("read after edit: %q err=%v", out, err)
	}
	// delete
	if _, err := tools["delete_file"].Run(map[string]any{"path": "a/b.txt"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("file should be gone")
	}
}

func TestNewTools(t *testing.T) {
	root := t.TempDir()
	tools := map[string]Tool{}
	for _, tl := range DefaultTools(NewSandbox(root), nil) {
		tools[tl.Name] = tl
	}

	// make_dir + write + tree
	if _, err := tools["make_dir"].Run(map[string]any{"path": "src/pkg"}); err != nil {
		t.Fatalf("make_dir: %v", err)
	}
	if _, err := tools["write_file"].Run(map[string]any{"path": "src/pkg/a.go", "content": "package pkg\n"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	tree, err := tools["tree"].Run(map[string]any{"depth": 3})
	if err != nil || !strings.Contains(tree, "src/") || !strings.Contains(tree, "a.go") {
		t.Fatalf("tree missing entries: %q err=%v", tree, err)
	}

	// move_file
	if _, err := tools["move_file"].Run(map[string]any{"from": "src/pkg/a.go", "to": "src/pkg/b.go"}); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src/pkg/b.go")); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}

	// multi_edit
	_, _ = tools["write_file"].Run(map[string]any{"path": "c.txt", "content": "one two three"})
	if _, err := tools["multi_edit"].Run(map[string]any{
		"path": "c.txt",
		"edits": []any{
			map[string]any{"old_string": "one", "new_string": "1"},
			map[string]any{"old_string": "three", "new_string": "3"},
		},
	}); err != nil {
		t.Fatalf("multi_edit: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "c.txt"))
	if string(b) != "1 two 3" {
		t.Fatalf("multi_edit result wrong: %q", b)
	}
}

func TestEditReturnsDiff(t *testing.T) {
	root := t.TempDir()
	tools := map[string]Tool{}
	for _, tl := range DefaultTools(NewSandbox(root), nil) {
		tools[tl.Name] = tl
	}
	_, _ = tools["write_file"].Run(map[string]any{"path": "f.txt", "content": "alpha"})
	out, err := tools["edit_file"].Run(map[string]any{"path": "f.txt", "old_string": "alpha", "new_string": "beta"})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(out, "- alpha") || !strings.Contains(out, "+ beta") {
		t.Fatalf("expected diff in output, got %q", out)
	}
}

func TestUndoRestoresAndDeletes(t *testing.T) {
	root := t.TempDir()
	ui := &captureUI{}
	allow := func(string, string, string) bool { return true }
	a := New("test", &fakeCaller{}, DefaultTools(NewSandbox(root), nil), allow, ui, "sys")
	tools := map[string]Tool{}
	for _, tl := range a.tools {
		tools[tl.Name] = tl
	}

	// Simulate the agent capturing undo + applying an edit on an existing file.
	_, _ = tools["write_file"].Run(map[string]any{"path": "f.txt", "content": "original"})
	editArgs := map[string]any{"path": "f.txt", "old_string": "original", "new_string": "changed"}
	restore, err := tools["edit_file"].Capture(editArgs)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	a.undo = append(a.undo, undoEntry{desc: "edit f.txt", restore: restore})
	_, _ = tools["edit_file"].Run(editArgs)

	if b, _ := os.ReadFile(filepath.Join(root, "f.txt")); string(b) != "changed" {
		t.Fatalf("edit not applied: %q", b)
	}
	if _, err := a.Undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "f.txt")); string(b) != "original" {
		t.Fatalf("undo did not restore content: %q", b)
	}

	// New-file write: undo should delete it.
	newArgs := map[string]any{"path": "new.txt", "content": "x"}
	restore, _ = tools["write_file"].Capture(newArgs)
	a.undo = append(a.undo, undoEntry{desc: "write new.txt", restore: restore})
	_, _ = tools["write_file"].Run(newArgs)
	if _, err := a.Undo(); err != nil {
		t.Fatalf("undo new: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("undo should have deleted new file")
	}
}

// summarizeCaller returns a fixed summary, used to drive compaction.
type summarizeCaller struct{}

func (s *summarizeCaller) ToolChat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	m := schema.Message{Role: "assistant"}
	m.SetText("SUMMARY: user built X; edited a.go; tests pass.")
	return &schema.ChatResponse{Choices: []schema.Choice{{Message: m}}}, nil
}
func (s *summarizeCaller) ToolChatStream(ctx context.Context, req *schema.ChatRequest, onText, onReasoning func(string)) (*schema.ChatResponse, error) {
	return s.ToolChat(ctx, req)
}

func TestCompactionReducesTokensAndKeepsRecent(t *testing.T) {
	ui := &captureUI{}
	a := New("test", &summarizeCaller{}, nil, nil, ui, "system prompt")
	a.CompactBudget = 1 // force the budget check to trigger
	a.KeepRecent = 2
	// Build a long history: 1 system (index 0) + many user/assistant turns.
	big := strings.Repeat("word ", 200)
	for i := 0; i < 10; i++ {
		a.messages = append(a.messages, userMsg("old turn "+big))
		am := schema.Message{Role: "assistant"}
		am.SetText("old answer " + big)
		a.messages = append(a.messages, am)
	}
	a.messages = append(a.messages, userMsg("most recent question"))
	before := a.EstimatedTokens()

	saved, err := a.Compact(context.Background(), false)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if saved <= 0 || a.EstimatedTokens() >= before {
		t.Fatalf("compaction did not reduce tokens: before=%d after=%d saved=%d", before, a.EstimatedTokens(), saved)
	}
	// System prompt preserved, summary present, recent turn kept verbatim.
	if a.messages[0].Role != "system" {
		t.Fatalf("system prompt lost")
	}
	last, _ := a.messages[len(a.messages)-1].TextContent()
	if last != "most recent question" {
		t.Fatalf("recent turn not preserved: %q", last)
	}
	joined := ""
	for _, m := range a.messages {
		if tx, ok := m.TextContent(); ok {
			joined += tx
		}
	}
	if !strings.Contains(joined, "SUMMARY:") {
		t.Fatalf("summary note missing")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())          // unix
	t.Setenv("USERPROFILE", t.TempDir())   // windows
	ui := &captureUI{}
	a := New("kimi", &fakeCaller{}, nil, nil, ui, "sys")
	a.messages = append(a.messages, userMsg("hello"))
	am := schema.Message{Role: "assistant"}
	am.SetText("hi there")
	a.messages = append(a.messages, am)

	if err := a.Save("unit-test-sess"); err != nil {
		t.Fatalf("save: %v", err)
	}
	b := New("other", &fakeCaller{}, nil, nil, ui, "sys")
	if err := b.Load("unit-test-sess"); err != nil {
		t.Fatalf("load: %v", err)
	}
	if b.Model != "kimi" {
		t.Fatalf("model not restored: %q", b.Model)
	}
	if len(b.messages) != len(a.messages) {
		t.Fatalf("message count mismatch: %d vs %d", len(b.messages), len(a.messages))
	}
	last, _ := b.messages[len(b.messages)-1].TextContent()
	if last != "hi there" {
		t.Fatalf("last message not restored: %q", last)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written.
func captureStdout(fn func()) string {
	old := os.Stdout
	rd, wr, _ := os.Pipe()
	os.Stdout = wr
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	_ = wr.Close()
	os.Stdout = old
	return <-done
}

func TestReplayHistoryPrintsPriorTurns(t *testing.T) {
	root := t.TempDir()
	ui := &captureUI{}
	r := &REPL{agent: New("kimi", &fakeCaller{}, DefaultTools(NewSandbox(root), nil), nil, ui, "sys"), sandbox: NewSandbox(root)}
	r.agent.messages = append(r.agent.messages, userMsg("what is 2+2?"))
	am := schema.Message{Role: "assistant"}
	am.SetText("It is **4**.")
	r.agent.messages = append(r.agent.messages, am)

	// Capture stdout while replaying.
	out := captureStdout(func() { r.replayHistory() })
	if !strings.Contains(out, "what is 2+2?") {
		t.Fatalf("replay missing prior user turn: %q", out)
	}
	if !strings.Contains(out, "4") || !strings.Contains(out, "resumed") {
		t.Fatalf("replay missing assistant turn / header: %q", out)
	}
}

func TestPersistentShellKeepsState(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	sh := NewShell(root)
	defer sh.Close()

	// Set a variable, then read it back in a later command → proves the
	// session persists env between calls.
	if _, err := sh.Run("export FROST_TEST=hello", 20*time.Second); err != nil {
		t.Fatalf("run1: %v", err)
	}
	out, err := sh.Run("echo $FROST_TEST", 20*time.Second)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("env did not persist across calls: %q", out)
	}
}

func TestSandboxConfinesAndGrants(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	sb := NewSandbox(root)

	// Relative paths resolve under the primary root.
	if _, err := sb.Resolve("sub/file.txt"); err != nil {
		t.Fatalf("in-root path rejected: %v", err)
	}
	// Parent escape is blocked.
	if _, err := sb.Resolve("../../etc/passwd"); err == nil {
		t.Fatalf("expected parent-escape rejection")
	}
	// An absolute path outside any root is blocked...
	if _, err := sb.Resolve(filepath.Join(other, "x")); err == nil {
		t.Fatalf("expected outside-root rejection")
	}
	// ...until that directory is granted.
	if _, err := sb.Add(other); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := sb.Resolve(filepath.Join(other, "x")); err != nil {
		t.Fatalf("granted dir still rejected: %v", err)
	}
}

func TestTaskToolIsReadOnly(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(root)
	ui := &captureUI{}
	parent := New("test", &fakeCaller{}, DefaultTools(sb, nil), func(string, string, string) bool { return true }, ui, "sys")

	task := makeTaskTool(&fakeCaller{}, sb, parent, ui)
	if task.Destructive {
		t.Fatalf("task tool should be non-destructive")
	}
	out, err := task.Run(map[string]any{"description": "explore", "prompt": "make a file out.txt"})
	if err != nil {
		t.Fatalf("task run: %v", err)
	}
	// fakeCaller tries write_file then answers; the sub-agent is read-only so
	// the file must NOT exist, but we still get the final text back.
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("sub-agent must not create files (read-only)")
	}
	if !strings.Contains(out, "out.txt") {
		t.Fatalf("expected sub-agent findings, got %q", out)
	}
}

func TestParseTodos(t *testing.T) {
	args := map[string]any{"items": []any{
		map[string]any{"task": "write code", "status": "done"},
		map[string]any{"task": "run tests", "status": "in_progress"},
		map[string]any{"task": "", "status": "pending"}, // dropped (no task)
	}}
	got := parseTodos(args)
	if len(got) != 2 {
		t.Fatalf("expected 2 todos, got %d", len(got))
	}
	if got[0].Status != "done" || got[1].Status != "in_progress" {
		t.Fatalf("statuses wrong: %+v", got)
	}
}

func TestPreviewReturnsDiff(t *testing.T) {
	root := t.TempDir()
	var edit Tool
	for _, tl := range DefaultTools(NewSandbox(root), nil) {
		if tl.Name == "edit_file" {
			edit = tl
		}
	}
	if edit.Preview == nil {
		t.Fatalf("edit_file should have a Preview")
	}
	d := edit.Preview(map[string]any{"path": "x", "old_string": "foo", "new_string": "bar"})
	if !strings.Contains(d, "- foo") || !strings.Contains(d, "+ bar") {
		t.Fatalf("preview diff wrong: %q", d)
	}
}

func TestEditAmbiguousRejected(t *testing.T) {
	root := t.TempDir()
	tools := map[string]Tool{}
	for _, tl := range DefaultTools(NewSandbox(root), nil) {
		tools[tl.Name] = tl
	}
	_, _ = tools["write_file"].Run(map[string]any{"path": "f.txt", "content": "x x x"})
	if _, err := tools["edit_file"].Run(map[string]any{"path": "f.txt", "old_string": "x", "new_string": "y"}); err == nil {
		t.Fatalf("expected ambiguity error without replace_all")
	}
	if _, err := tools["edit_file"].Run(map[string]any{"path": "f.txt", "old_string": "x", "new_string": "y", "replace_all": true}); err != nil {
		t.Fatalf("replace_all should succeed: %v", err)
	}
}

func TestPathEscapeBlocked(t *testing.T) {
	root := t.TempDir()
	tools := map[string]Tool{}
	for _, tl := range DefaultTools(NewSandbox(root), nil) {
		tools[tl.Name] = tl
	}
	if _, err := tools["read_file"].Run(map[string]any{"path": "../../etc/passwd"}); err == nil {
		t.Fatalf("expected path-escape rejection")
	}
}

// --- agentic loop test with a fake tool-calling model ---

// fakeCaller emits one write_file tool call, then a final answer once it sees
// the tool result.
type fakeCaller struct{ called bool }

func (f *fakeCaller) ToolChat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	sawTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	msg := schema.Message{Role: "assistant"}
	if !sawTool {
		args, _ := json.Marshal(map[string]string{"path": "out.txt", "content": "generated"})
		tc, _ := json.Marshal([]map[string]any{{
			"id": "c1", "type": "function",
			"function": map[string]string{"name": "write_file", "arguments": string(args)},
		}})
		msg.ToolCalls = tc
	} else {
		msg.SetText("Created out.txt as requested.")
	}
	return &schema.ChatResponse{Choices: []schema.Choice{{Message: msg}}}, nil
}

// ToolChatStream reuses ToolChat (no real streaming) and emits the text once,
// exercising the streaming code path in the loop.
func (f *fakeCaller) ToolChatStream(ctx context.Context, req *schema.ChatRequest, onText, onReasoning func(string)) (*schema.ChatResponse, error) {
	resp, err := f.ToolChat(ctx, req)
	if err == nil && len(resp.Choices) > 0 {
		if txt, ok := resp.Choices[0].Message.TextContent(); ok && txt != "" {
			onText(txt)
		}
	}
	return resp, err
}

// captureUI records loop events.
type captureUI struct {
	text, tools []string
	stream      string
	todos       []TodoItem
}

func (c *captureUI) AssistantText(s string)              { c.text = append(c.text, s) }
func (c *captureUI) StreamDelta(s string)                { c.stream += s }
func (c *captureUI) ReasoningDelta(s string)             {}
func (c *captureUI) StreamEnd()                          { if c.stream != "" { c.text = append(c.text, c.stream); c.stream = "" } }
func (c *captureUI) ToolStart(n, p string)               { c.tools = append(c.tools, n) }
func (c *captureUI) ToolResult(n, r string)              {}
func (c *captureUI) Info(s string)                       {}
func (c *captureUI) Error(s string)                      {}
func (c *captureUI) Thinking(on bool)                    {}
func (c *captureUI) TurnStats(d time.Duration, tok int)  {}
func (c *captureUI) Todos(items []TodoItem)              { c.todos = items }

func TestAgentLoopExecutesToolThenAnswers(t *testing.T) {
	root := t.TempDir()
	ui := &captureUI{}
	autoApprove := func(name, preview, diff string) bool { return true }
	a := New("test", &fakeCaller{}, DefaultTools(NewSandbox(root), nil), autoApprove, ui, "sys")

	if err := a.Run(context.Background(), "make a file out.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The tool must have actually created the file.
	if b, err := os.ReadFile(filepath.Join(root, "out.txt")); err != nil || string(b) != "generated" {
		t.Fatalf("tool did not create file: %q err=%v", b, err)
	}
	if len(ui.tools) != 1 || ui.tools[0] != "write_file" {
		t.Fatalf("expected write_file tool call, got %v", ui.tools)
	}
	if len(ui.text) != 1 || !strings.Contains(ui.text[0], "out.txt") {
		t.Fatalf("expected final answer, got %v", ui.text)
	}
}

func TestPlanModeBlocksWrites(t *testing.T) {
	root := t.TempDir()
	ui := &captureUI{}
	a := New("test", &fakeCaller{}, DefaultTools(NewSandbox(root), nil), func(string, string, string) bool { return true }, ui, "sys")
	a.SetMode(ModePlan)

	// In plan mode the write tool isn't advertised...
	for _, tl := range parseAdvertised(a) {
		if tl == "write_file" || tl == "bash" {
			t.Fatalf("plan mode must not advertise destructive tool %q", tl)
		}
	}
	// ...and even a forced call is blocked, so no file is created.
	_ = a.Run(context.Background(), "make a file")
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("plan mode must not create files")
	}
}

// parseAdvertised extracts tool names from the agent's active tools JSON.
func parseAdvertised(a *Agent) []string {
	var tools []OpenAITool
	_ = json.Unmarshal(a.toolsJSON(), &tools)
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Function.Name)
	}
	return out
}

func TestApprovalDenialBlocksTool(t *testing.T) {
	root := t.TempDir()
	ui := &captureUI{}
	deny := func(name, preview, diff string) bool { return false }
	a := New("test", &fakeCaller{}, DefaultTools(NewSandbox(root), nil), deny, ui, "sys")

	_ = a.Run(context.Background(), "make a file")
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied tool must not create the file")
	}
}
