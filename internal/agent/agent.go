package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"frostgate/internal/schema"
	"frostgate/internal/tokens"
)

// Caller performs one model turn. ToolChat is non-streaming; ToolChatStream
// streams text deltas via onText and returns the reassembled response.
type Caller interface {
	ToolChat(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error)
	ToolChatStream(ctx context.Context, req *schema.ChatRequest, onText, onReasoning func(string)) (*schema.ChatResponse, error)
}

// Approver decides whether a destructive tool call may run. preview is a short
// one-line description; diff is a fuller (possibly multi-line) preview.
type Approver func(toolName, preview, diff string) bool

// undoEntry records how to revert one applied tool action.
type undoEntry struct {
	desc    string
	restore func() error
}

// UI receives streamed events from the loop so the REPL can render them.
type UI interface {
	AssistantText(s string)              // final model prose (non-stream fallback)
	StreamDelta(s string)                // incremental text token(s)
	ReasoningDelta(s string)             // incremental reasoning/thinking token(s)
	StreamEnd()                          // end of a streamed assistant message
	ToolStart(name, preview string)      // about to run a tool
	ToolResult(name, result string)      // tool output (already truncated)
	Info(s string)                       // status/notice
	Error(s string)                      // error notice
	Thinking(on bool)                    // toggle the "thinking" spinner
	TurnStats(d time.Duration, tok int)  // per-turn timing + token usage
	Todos(items []TodoItem)              // render the agent's task list
}

// TodoItem is one entry in the agent's visible task list.
type TodoItem struct {
	Task   string `json:"task"`
	Status string `json:"status"` // pending | in_progress | done
}

// Mode controls what the agent is allowed to do.
type Mode int

const (
	// ModeBuild grants the full toolset (read + write + shell).
	ModeBuild Mode = iota
	// ModePlan is read-only: the agent may inspect the project and propose a
	// plan, but cannot modify files or run state-changing commands.
	ModePlan
)

func (m Mode) String() string {
	if m == ModePlan {
		return "plan"
	}
	return "build"
}

// Agent holds conversation state and configuration.
type Agent struct {
	Model      string
	caller     Caller
	tools      []Tool
	byName     map[string]Tool
	messages   []schema.Message
	approve    Approver
	ui         UI
	maxSteps   int
	mode       Mode
	basePrompt string // harness prompt without the mode note
	undo       []undoEntry

	// CompactBudget is the estimated-token threshold above which old turns are
	// auto-summarized before a turn. 0 disables auto-compaction.
	CompactBudget int
	// KeepRecent is how many trailing non-system messages compaction preserves
	// verbatim.
	KeepRecent int

	// Effort is the reasoning effort hint ("low".."max"), sent as a passthrough
	// param. Temperature overrides sampling temperature when set.
	Effort      string
	Temperature *float64
	// ShowReasoning surfaces the model's streamed reasoning to the UI.
	ShowReasoning bool
	// goal is a persistent objective injected into the system prompt.
	goal string
}

// SetGoal sets a persistent objective that is kept in the system prompt across
// turns. Empty clears it.
func (a *Agent) SetGoal(goal string) {
	a.goal = strings.TrimSpace(goal)
	a.applySystem()
}

// Goal returns the current persistent goal.
func (a *Agent) Goal() string { return a.goal }

// Undo reverts the most recent applied tool action. Returns its description.
func (a *Agent) Undo() (string, error) {
	if len(a.undo) == 0 {
		return "", fmt.Errorf("nothing to undo")
	}
	e := a.undo[len(a.undo)-1]
	a.undo = a.undo[:len(a.undo)-1]
	if err := e.restore(); err != nil {
		return "", err
	}
	return e.desc, nil
}

// UndoDepth reports how many actions can be undone.
func (a *Agent) UndoDepth() int { return len(a.undo) }

// New builds an Agent. basePrompt is the harness system prompt; the mode note
// is appended automatically and refreshed on SetMode.
func New(model string, caller Caller, tools []Tool, approve Approver, ui UI, basePrompt string) *Agent {
	byName := map[string]Tool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	a := &Agent{
		Model: model, caller: caller, tools: tools, byName: byName,
		approve: approve, ui: ui, maxSteps: 25, mode: ModeBuild, basePrompt: basePrompt,
		CompactBudget: 24000, KeepRecent: 8,
	}
	a.applySystem()
	return a
}

// EstimatedTokens reports the approximate prompt size of the conversation.
func (a *Agent) EstimatedTokens() int { return tokens.EstimateMessages(a.messages) }

// Compact summarizes older turns into a single system note, preserving the
// leading system prompt and the last KeepRecent messages. It returns the number
// of estimated tokens saved (0 if nothing was compacted). force ignores the
// budget check.
func (a *Agent) Compact(ctx context.Context, force bool) (int, error) {
	before := tokens.EstimateMessages(a.messages)
	if !force && (a.CompactBudget <= 0 || before <= a.CompactBudget) {
		return 0, nil
	}
	// Split: system messages (kept), older middle (summarized), recent tail.
	var sys, rest []schema.Message
	for _, m := range a.messages {
		if m.Role == "system" {
			sys = append(sys, m)
		} else {
			rest = append(rest, m)
		}
	}
	keep := a.KeepRecent
	if keep < 2 {
		keep = 2
	}
	if len(rest) <= keep {
		return 0, nil // nothing old enough to compact
	}
	old, recent := rest[:len(rest)-keep], rest[len(rest)-keep:]

	// Ask the model to summarize the old turns.
	var sb strings.Builder
	sb.WriteString("Summarize the earlier part of this coding session so it can replace the raw " +
		"messages without losing anything important. Preserve: the user's goals, decisions made, " +
		"files created/edited and why, key code facts, and open TODOs. Output only the summary.\n\n")
	for _, m := range old {
		if t, ok := m.TextContent(); ok && strings.TrimSpace(t) != "" {
			sb.WriteString(m.Role + ": " + t + "\n")
		}
		for _, tc := range m.ParseToolCalls() {
			sb.WriteString("assistant tool_call: " + tc.Function.Name + " " + tc.Function.Arguments + "\n")
		}
	}
	req := &schema.ChatRequest{Model: a.Model, Messages: []schema.Message{userMsg(sb.String())}}
	resp, err := a.caller.ToolChat(ctx, req)
	if err != nil {
		return 0, err
	}
	summary := ""
	if len(resp.Choices) > 0 {
		summary, _ = resp.Choices[0].Message.TextContent()
	}
	if strings.TrimSpace(summary) == "" {
		return 0, fmt.Errorf("empty summary")
	}

	// Rebuild: system prompt(s) + summary note + recent tail.
	note := sysMsg("Summary of earlier conversation (compacted to save context):\n" + summary)
	rebuilt := make([]schema.Message, 0, len(sys)+1+len(recent))
	rebuilt = append(rebuilt, sys...)
	rebuilt = append(rebuilt, note)
	rebuilt = append(rebuilt, recent...)
	a.messages = rebuilt
	a.undo = nil // file undo stack is unrelated, but message indices shifted

	saved := before - tokens.EstimateMessages(a.messages)
	if saved < 0 {
		saved = 0
	}
	return saved, nil
}

// AddTool registers an additional tool (e.g. the task tool or MCP tools).
func (a *Agent) AddTool(t Tool) {
	a.tools = append(a.tools, t)
	a.byName[t.Name] = t
}

// ReplaceMCPTools swaps the agent's MCP-backed tools (those tagged "[MCP]" in
// their description) for a fresh set, leaving built-in and task tools intact.
// Used after an MCP server reconnect to reflect added/removed tools.
func (a *Agent) ReplaceMCPTools(tools []Tool) {
	kept := a.tools[:0:0]
	for _, t := range a.tools {
		if strings.HasPrefix(t.Description, "[MCP]") {
			delete(a.byName, t.Name)
			continue
		}
		kept = append(kept, t)
	}
	a.tools = kept
	for _, t := range tools {
		a.tools = append(a.tools, t)
		a.byName[t.Name] = t
	}
}

// Mode returns the current mode.
func (a *Agent) Mode() Mode { return a.mode }

// SetMode switches mode and refreshes the system prompt + active toolset.
func (a *Agent) SetMode(m Mode) { a.mode = m; a.applySystem() }

// applySystem (re)writes the leading system message from the base prompt and
// the current mode note.
func (a *Agent) applySystem() {
	full := a.basePrompt
	if full != "" {
		full += "\n\n"
	}
	full += modeNote(a.mode)
	if a.goal != "" {
		full += "\n\n# Current goal\n" + a.goal + "\nKeep working toward this goal until it is met."
	}
	sm := sysMsg(full)
	if len(a.messages) > 0 && a.messages[0].Role == "system" {
		a.messages[0] = sm
	} else {
		a.messages = append([]schema.Message{sm}, a.messages...)
	}
}

// activeTools returns the tools advertised to the model for the current mode.
// Plan mode exposes only non-destructive (read-only) tools.
func (a *Agent) activeTools() []Tool {
	if a.mode != ModePlan {
		return a.tools
	}
	out := make([]Tool, 0, len(a.tools))
	for _, t := range a.tools {
		if !t.Destructive {
			out = append(out, t)
		}
	}
	return out
}

// modeNote is the per-mode instruction appended to the system prompt.
func modeNote(m Mode) string {
	if m == ModePlan {
		return "MODE: PLAN (read-only). You may use read_file, list_dir, glob, and grep to investigate, " +
			"but you must NOT modify files or run state-changing commands. After investigating, present a " +
			"concise, numbered implementation plan and tell the user to run /build to execute it."
	}
	return "MODE: BUILD. You may create, edit, and delete files and run shell commands (the user approves " +
		"destructive actions) to complete the task. Inspect before changing; verify with builds/tests when relevant."
}

// Reset clears the conversation, keeping the system prompt.
func (a *Agent) Reset() {
	if len(a.messages) > 0 && a.messages[0].Role == "system" {
		a.messages = a.messages[:1]
	} else {
		a.messages = nil
	}
}

// AddSystem appends a system message (used by /skill to inject instructions).
func (a *Agent) AddSystem(s string) { a.messages = append(a.messages, sysMsg(s)) }

// Turns returns the number of non-system messages (for the status line).
func (a *Agent) Turns() int {
	n := 0
	for _, m := range a.messages {
		if m.Role != "system" {
			n++
		}
	}
	return n
}

// toolsJSON serializes the active tool catalog for the request.
func (a *Agent) toolsJSON() json.RawMessage {
	active := a.activeTools()
	arr := make([]OpenAITool, 0, len(active))
	for _, t := range active {
		arr = append(arr, t.asOpenAI())
	}
	b, _ := json.Marshal(arr)
	return b
}

// Run processes one user input through the agentic loop until the model stops
// requesting tools or the step budget is exhausted.
func (a *Agent) Run(ctx context.Context, userInput string) error {
	a.messages = append(a.messages, userMsg(userInput))
	start := time.Now()
	tokenCount := 0

	// Auto-compact older turns when the context grows past the budget.
	if saved, err := a.Compact(ctx, false); err == nil && saved > 0 {
		a.ui.Info(fmt.Sprintf("compacted context (~%d tokens saved)", saved))
	}

	for step := 0; step < a.maxSteps; step++ {
		req := &schema.ChatRequest{
			Model:    a.Model,
			Messages: a.messages,
			Tools:    a.toolsJSON(),
		}
		if a.Temperature != nil {
			req.Temperature = a.Temperature
		}
		if a.Effort != "" {
			b, _ := json.Marshal(a.Effort)
			req.Passthrough = map[string]json.RawMessage{"reasoning_effort": b}
		}
		a.ui.Thinking(true)
		streamed := false
		stop := func() {
			if !streamed {
				a.ui.Thinking(false) // first token: drop the spinner
				streamed = true
			}
		}
		onText := func(delta string) {
			stop()
			a.ui.StreamDelta(delta)
		}
		onReasoning := func(delta string) {
			if a.ShowReasoning {
				stop()
				a.ui.ReasoningDelta(delta)
			}
		}
		resp, err := a.caller.ToolChatStream(ctx, req, onText, onReasoning)
		a.ui.Thinking(false)
		if err != nil {
			if streamed {
				a.ui.StreamEnd()
			}
			// Context cancellation (Ctrl-C) is a clean stop, not an error.
			if ctx.Err() != nil {
				a.ui.Info("interrupted")
				return nil
			}
			return err
		}
		tokenCount += resp.Usage.TotalTokens
		if len(resp.Choices) == 0 {
			a.ui.Error("model returned no choices")
			return nil
		}
		msg := resp.Choices[0].Message
		a.messages = append(a.messages, msg)

		calls := msg.ParseToolCalls()
		if len(calls) == 0 {
			if streamed {
				a.ui.StreamEnd()
			} else if txt, ok := msg.TextContent(); ok && strings.TrimSpace(txt) != "" {
				a.ui.AssistantText(txt)
			}
			a.ui.TurnStats(time.Since(start), tokenCount)
			return nil // model produced a final answer
		}
		if streamed {
			a.ui.StreamEnd() // close the streamed text before tool output
		}

		// Execute each requested tool, appending a tool result message.
		for _, c := range calls {
			a.execToolCall(c)
		}
	}
	a.ui.TurnStats(time.Since(start), tokenCount)
	a.ui.Error(fmt.Sprintf("stopped after %d steps (step budget reached)", a.maxSteps))
	return nil
}

// execToolCall parses arguments, gates destructive actions, runs the tool, and
// appends the result message.
func (a *Agent) execToolCall(c schema.ToolCall) {
	name := c.Function.Name
	tool, ok := a.byName[name]
	args := parseArgs(c.Function.Arguments)
	preview := previewOf(name, args)

	if !ok {
		a.appendToolResult(c.ID, "error: unknown tool "+name)
		a.ui.Error("model called unknown tool " + name)
		return
	}

	// Plan mode is read-only: refuse destructive tools even if the model
	// tries to call one that wasn't advertised.
	if a.mode == ModePlan && tool.Destructive {
		a.appendToolResult(c.ID, "blocked: "+name+" is not allowed in plan mode (read-only)")
		a.ui.Info("blocked in plan mode: " + name)
		return
	}

	a.ui.ToolStart(name, preview)

	if tool.Destructive && a.approve != nil {
		diff := ""
		if tool.Preview != nil {
			diff = tool.Preview(args)
		}
		if !a.approve(name, preview, diff) {
			a.appendToolResult(c.ID, "user denied this action")
			a.ui.Info("denied: " + name)
			return
		}
	}

	// Capture undo state just before applying (best-effort).
	if tool.Capture != nil {
		if restore, err := tool.Capture(args); err == nil && restore != nil {
			a.undo = append(a.undo, undoEntry{desc: preview, restore: restore})
		}
	}

	result, err := tool.Run(args)
	if err != nil {
		msg := "error: " + err.Error()
		a.appendToolResult(c.ID, msg)
		a.ui.ToolResult(name, msg)
		return
	}
	a.appendToolResult(c.ID, result)

	// todo_write renders as a checklist rather than raw tool output.
	if name == "todo_write" {
		a.ui.Todos(parseTodos(args))
		return
	}
	a.ui.ToolResult(name, result)
}

// parseTodos extracts TodoItems from a todo_write call's arguments.
func parseTodos(args map[string]any) []TodoItem {
	raw, _ := args["items"].([]any)
	out := make([]TodoItem, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		task, _ := m["task"].(string)
		status, _ := m["status"].(string)
		if task == "" {
			continue
		}
		if status == "" {
			status = "pending"
		}
		out = append(out, TodoItem{Task: task, Status: status})
	}
	return out
}

// maxToolResultChars caps how much of a tool's output is kept in the
// conversation, so large file reads / command output don't blow the context
// budget. The user still sees more in the terminal.
const maxToolResultChars = 8000

func (a *Agent) appendToolResult(id, content string) {
	if len(content) > maxToolResultChars {
		content = content[:maxToolResultChars] +
			fmt.Sprintf("\n…[truncated %d chars; read a smaller range with offset/limit if you need more]", len(content)-maxToolResultChars)
	}
	m := schema.Message{Role: "tool", ToolCallID: id}
	m.SetText(content)
	a.messages = append(a.messages, m)
}

// parseArgs decodes the JSON-encoded argument string into a map.
func parseArgs(s string) map[string]any {
	m := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// previewOf builds a one-line human description of a tool call.
func previewOf(name string, args map[string]any) string {
	switch name {
	case "bash":
		return "$ " + str(args, "command")
	case "write_file":
		return "write " + str(args, "path")
	case "edit_file":
		return "edit " + str(args, "path")
	case "delete_file":
		return "delete " + str(args, "path")
	case "read_file", "list_dir":
		return name + " " + str(args, "path")
	case "glob":
		return "glob " + str(args, "pattern")
	case "grep":
		return "grep " + str(args, "query")
	}
	return name
}

func sysMsg(s string) schema.Message  { m := schema.Message{Role: "system"}; m.SetText(s); return m }
func userMsg(s string) schema.Message { m := schema.Message{Role: "user"}; m.SetText(s); return m }
