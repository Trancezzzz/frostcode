package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"frostgate/internal/mcp"
	"frostgate/internal/update"
	"frostgate/internal/version"
)

// ANSI styling. Frostgate brand teal is approximated with 256-color 43/teal.
const (
	cReset = "\x1b[0m"
	cDim   = "\x1b[2m"
	cBold  = "\x1b[1m"
	cTeal  = "\x1b[38;5;43m"
	cBlue  = "\x1b[38;5;75m"
	cGreen = "\x1b[38;5;78m"
	cYellow= "\x1b[38;5;221m"
	cRed   = "\x1b[38;5;203m"
	cGray  = "\x1b[38;5;245m"
)

// REPL is the interactive coding-agent shell.
type REPL struct {
	rich        bool
	resumed     bool
	lastInput   string
	sessionID   string
	sandbox     *Sandbox
	shell       *Shell
	agent       *Agent
	caller      Caller
	models      []string
	targets     map[string]string
	autoApprove bool
	root        string
	skillsDir     string
	spinStop      chan struct{}
	streaming     bool
	mdLine        string // partial line buffer for streaming markdown
	mdCode        bool   // inside a fenced code block while streaming
	rzActive      bool   // currently streaming reasoning
	rzLine        string // partial reasoning line buffer
	sessionTokens int
	sessionTurns  int
	mcp           *mcp.Manager
	loadedSkills  map[string]bool
	updateCh      <-chan update.CheckResult
	history       []string // ring of last historyMax user inputs
}

const historyMax = 20

// Options configures a REPL.
type Options struct {
	Model     string
	Models    []string          // available model aliases for /model
	Targets   map[string]string // alias -> "provider/model" for display
	Caller    Caller
	Root      string // project directory (tool sandbox)
	SkillsDir string
	Auto       bool     // auto-approve destructive actions
	Plan       bool     // start in plan mode (read-only)
	ResumeID   string       // resume an existing session by id/name (optional)
	ExtraTools []Tool       // additional tools (e.g. MCP-backed)
	MCP        *mcp.Manager // MCP server manager, for the /mcp command (optional)
}

// NewREPL wires tools, the agent, and the terminal UI together.
func NewREPL(opt Options) *REPL {
	r := &REPL{
		rich:        true, // palette editor on a real console; auto-falls back when piped
		models:      opt.Models,
		targets:     opt.Targets,
		autoApprove: opt.Auto,
		root:        opt.Root,
		skillsDir:   opt.SkillsDir,
		mcp:         opt.MCP,
	}
	r.caller = opt.Caller
	r.sandbox = NewSandbox(opt.Root)
	r.shell = NewShell(opt.Root)
	r.shell.OnLine = func(line string) {
		fmt.Printf("    %s%s%s\n", cDim, line, cReset)
	}
	tools := DefaultTools(r.sandbox, r.shell)
	r.agent = New(opt.Model, opt.Caller, tools, r.approve, r, systemPrompt(opt.Root))
	// Sub-agent delegation tool + any MCP-backed tools.
	r.agent.AddTool(makeTaskTool(opt.Caller, r.sandbox, r.agent, r))
	for _, t := range opt.ExtraTools {
		r.agent.AddTool(t)
	}
	if opt.Plan {
		r.agent.SetMode(ModePlan)
	}
	// Kick off a background update check immediately so by the time the banner
	// is printed the result is often already available.
	r.updateCh = update.CheckBackground()

	// Resume an existing session, or start a fresh one with a new id.
	if opt.ResumeID != "" {
		r.sessionID = opt.ResumeID
		if err := r.agent.Load(opt.ResumeID); err != nil {
			fmt.Printf("%scould not resume %s: %v%s\n", cYellow, opt.ResumeID, err, cReset)
			r.sessionID = NewSessionID()
		} else {
			r.resumed = true
		}
	} else {
		r.sessionID = NewSessionID()
	}
	return r
}

// replayHistory prints the prior conversation after resuming a session, so the
// user can see the context that was restored. System and tool messages are
// skipped; assistant prose is markdown-rendered.
func (r *REPL) replayHistory() {
	var turns int
	for _, m := range r.agent.messages {
		if m.Role == "user" || m.Role == "assistant" {
			turns++
		}
	}
	if turns == 0 {
		return
	}
	fmt.Printf("\n%s── resumed %d message(s) ──%s\n", cDim, turns, cReset)
	for _, m := range r.agent.messages {
		switch m.Role {
		case "user":
			if t, ok := m.TextContent(); ok && strings.TrimSpace(t) != "" {
				fmt.Printf("\n%s%s›%s %s\n", cTeal, cBold, cReset, firstLines(strings.TrimSpace(t), 6))
			}
		case "assistant":
			if t, ok := m.TextContent(); ok && strings.TrimSpace(t) != "" {
				fmt.Printf("\n%s\n", renderMarkdownBlock(t))
			} else if calls := m.ParseToolCalls(); len(calls) > 0 {
				fmt.Printf("  %s· ran %d tool call(s)%s\n", cDim, len(calls), cReset)
			}
		}
	}
	fmt.Printf("\n%s──────────────────────%s\n", cDim, cReset)
}

// resolvedTarget returns the "provider/model" the current alias maps to.
func (r *REPL) resolvedTarget() string {
	if r.targets != nil {
		if t, ok := r.targets[r.agent.Model]; ok {
			return t
		}
	}
	return r.agent.Model
}

// keySetter is implemented by the gateway: it swaps a provider's API key at
// runtime so /key can re-auth without editing config and restarting.
type keySetter interface {
	SetProviderKey(provider, key string) bool
	Providers() []string
}

// setProviderKey handles /provider: an opencode-style two-step flow — pick a
// provider from a list, then enter (type or paste) its API key in a masked
// input modal. The key is applied in-memory for this session only; it is never
// written to the config file.
func (r *REPL) setProviderKey() {
	ks, ok := r.caller.(keySetter)
	if !ok {
		r.Error("this build can't set keys at runtime (caller is not a gateway)")
		return
	}
	providers := ks.Providers()
	sort.Strings(providers)
	if len(providers) == 0 {
		r.Error("no providers configured")
		return
	}

	idx, ok := r.selectModal("Select a provider", providers)
	if !ok {
		r.Info("cancelled")
		return
	}
	providerName := providers[idx]

	key, ok := r.secretModal("Paste API key for " + providerName)
	if !ok || strings.TrimSpace(key) == "" {
		r.Info("cancelled — key unchanged")
		return
	}
	key = strings.TrimSpace(key)

	if !ks.SetProviderKey(providerName, key) {
		r.Error("unknown provider '" + providerName + "'")
		return
	}
	r.Info("updated API key for '" + providerName + "' (" + maskKey(key) +
		") — session only, not saved to config")
}

// maskKey shows just enough of a secret to confirm which key was set, without
// echoing it in full to the terminal/scrollback.
func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

// ─── UI interface (rendered to the terminal) ───

func (r *REPL) AssistantText(s string) {
	fmt.Printf("\n%s\n", renderMarkdownBlock(s))
}

// ReasoningDelta streams the model's thinking, dimmed and prefixed, above the
// answer. Buffered per line like the markdown renderer.
func (r *REPL) ReasoningDelta(s string) {
	if !r.rzActive {
		fmt.Printf("\n  %s%sthinking%s\n", cDim, cBold, cReset)
		r.rzActive = true
		r.rzLine = ""
	}
	r.rzLine += s
	for {
		i := strings.IndexByte(r.rzLine, '\n')
		if i < 0 {
			break
		}
		fmt.Printf("  %s%s %s%s\n", cDim, "│", r.rzLine[:i], cReset)
		r.rzLine = r.rzLine[i+1:]
	}
}

// endReasoning flushes any trailing reasoning line.
func (r *REPL) endReasoning() {
	if r.rzActive {
		if strings.TrimSpace(r.rzLine) != "" {
			fmt.Printf("  %s│ %s%s\n", cDim, r.rzLine, cReset)
		}
		r.rzLine = ""
		r.rzActive = false
	}
}

// StreamDelta buffers streamed text and renders complete lines as markdown as
// soon as their newline arrives, so output looks formatted while still feeling
// live.
func (r *REPL) StreamDelta(s string) {
	if r.rzActive {
		r.endReasoning() // reasoning is done once the answer starts
	}
	if !r.streaming {
		fmt.Println() // blank line before the assistant message begins
		r.streaming = true
		r.mdLine = ""
		r.mdCode = false
	}
	r.mdLine += s
	for {
		i := strings.IndexByte(r.mdLine, '\n')
		if i < 0 {
			break
		}
		line := r.mdLine[:i]
		r.mdLine = r.mdLine[i+1:]
		fmt.Println(renderMarkdownLine(line, &r.mdCode))
	}
}

// StreamEnd flushes any trailing partial line and ends the message.
func (r *REPL) StreamEnd() {
	r.endReasoning()
	if r.streaming {
		if r.mdLine != "" {
			fmt.Println(renderMarkdownLine(r.mdLine, &r.mdCode))
			r.mdLine = ""
		}
		r.streaming = false
	}
}
func (r *REPL) ToolStart(name, preview string) {
	fmt.Printf("  %s%s%s %s%s%s\n", cTeal, icTool, cReset, cBold, preview, cReset)
}

// ToolResult prints tool output dimmed, coloring +/- diff lines for edits.
func (r *REPL) ToolResult(name, result string) {
	for _, ln := range strings.Split(firstLines(result, 14), "\n") {
		switch {
		case strings.HasPrefix(ln, "+"):
			fmt.Printf("    %s%s%s\n", cGreen, ln, cReset)
		case strings.HasPrefix(ln, "-"):
			fmt.Printf("    %s%s%s\n", cRed, ln, cReset)
		default:
			fmt.Printf("    %s%s%s\n", cDim, ln, cReset)
		}
	}
}
func (r *REPL) Info(s string)  { fmt.Printf("  %s%s %s%s\n", cGray, icInfo, s, cReset) }
func (r *REPL) Error(s string) { fmt.Printf("  %s%s %s%s\n", cRed, icErr, s, cReset) }

// Thinking toggles an animated braille spinner on the current line.
func (r *REPL) Thinking(on bool) {
	if on {
		if r.spinStop != nil {
			return
		}
		stop := make(chan struct{})
		r.spinStop = stop
		go func() {
			frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
			t := time.NewTicker(80 * time.Millisecond)
			defer t.Stop()
			i := 0
			for {
				select {
				case <-stop:
					fmt.Print("\r\x1b[K") // clear the line
					return
				case <-t.C:
					fmt.Printf("\r%s%s%s %sthinking…%s", cTeal, frames[i%len(frames)], cReset, cDim, cReset)
					i++
				}
			}
		}()
		return
	}
	if r.spinStop != nil {
		close(r.spinStop)
		r.spinStop = nil
		time.Sleep(20 * time.Millisecond) // let the goroutine clear the line
		fmt.Print("\r\x1b[K")
	}
}

// TurnStats prints a dim footer with elapsed time and token usage, and
// accumulates session totals for /cost.
func (r *REPL) TurnStats(d time.Duration, tok int) {
	r.sessionTokens += tok
	r.sessionTurns++
	if tok > 0 {
		fmt.Printf("  %s%.1fs · %d tokens%s\n", cDim, d.Seconds(), tok, cReset)
	} else {
		fmt.Printf("  %s%.1fs%s\n", cDim, d.Seconds(), cReset)
	}
}

// Todos renders the agent's task list as a checklist.
func (r *REPL) Todos(items []TodoItem) {
	fmt.Printf("  %sTodos%s\n", cTeal, cReset)
	for _, it := range items {
		box, col := icTodoTodo, cGray
		switch it.Status {
		case "in_progress":
			box, col = icTodoNow, cYellow
		case "done":
			box, col = icTodoDone, cGreen
		}
		text := it.Task
		if it.Status == "done" {
			text = cDim + text + cReset
		}
		fmt.Printf("    %s%s%s %s\n", col, box, cReset, text)
	}
}

// approve prompts for destructive actions unless auto-approve is on, showing a
// colored diff/preview of the change first.
func (r *REPL) approve(name, preview, diff string) bool {
	if r.autoApprove {
		return true
	}
	if strings.TrimSpace(diff) != "" {
		for _, ln := range strings.Split(firstLines(diff, 16), "\n") {
			switch {
			case strings.HasPrefix(ln, "+"):
				fmt.Printf("    %s%s%s\n", cGreen, ln, cReset)
			case strings.HasPrefix(ln, "-"):
				fmt.Printf("    %s%s%s\n", cRed, ln, cReset)
			default:
				fmt.Printf("    %s%s%s\n", cDim, ln, cReset)
			}
		}
	}
	fmt.Printf("  %s%s%s %s%s  %s[y = yes, a = always, N = no]%s ", cYellow, cBold, icInterro, cReset, preview, cDim, cReset)
	line, _ := r.cookedLine()
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "a", "always":
		r.autoApprove = true
		return true
	default:
		return false
	}
}

// Run starts the read-eval loop.
func (r *REPL) Run() {
	if !r.ensureTrust() {
		return
	}
	defer r.onExit() // autosave + print resume hint
	r.banner()
	r.showUpdateHint()
	if r.resumed {
		r.replayHistory()
	}
	for {
		line, ok := r.promptLine()
		if !ok { // EOF (Ctrl-D)
			fmt.Println()
			return
		}
		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			if r.command(input) {
				return // quit
			}
			continue
		}
		r.runTurn(input)
	}
}

// expandMentions replaces @path tokens in the input with the file's contents,
// so users can pull files into context quickly. Unreadable paths are left as-is.
func (r *REPL) expandMentions(input string) string {
	var attach strings.Builder
	for _, tok := range strings.Fields(input) {
		if !strings.HasPrefix(tok, "@") || len(tok) < 2 {
			continue
		}
		rel := strings.TrimRight(tok[1:], ".,;:)")
		p, err := r.sandbox.Resolve(rel)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		attach.WriteString("\n\nContents of " + rel + ":\n```\n" + truncate(string(b), 12000) + "\n```")
	}
	if attach.Len() == 0 {
		return input
	}
	return input + attach.String()
}

// onExit autosaves the conversation, shuts the shell, and prints how to resume.
func (r *REPL) onExit() {
	if r.shell != nil {
		r.shell.Close()
	}
	_ = r.agent.Save(r.sessionID)
	_ = r.agent.Save("last")
	if r.agent.Turns() > 0 {
		fmt.Printf("\n%sResume this session with:%s\n", cGray, cReset)
		fmt.Printf("  %sfrostcode --resume %s%s\n", cDim, r.sessionID, cReset)
	}
}

// runTurn runs one agent turn under a context that Ctrl-C cancels, so the user
// can abort a long turn without quitting the REPL.
func (r *REPL) runTurn(input string) {
	r.lastInput = input
	r.history = append(r.history, input)
	if len(r.history) > historyMax {
		r.history = r.history[len(r.history)-historyMax:]
	}
	input = r.expandMentions(input)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			r.Thinking(false)
			fmt.Printf("\n  %s%s interrupting…%s\n", cYellow, icCancel, cReset)
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := r.agent.Run(ctx, input); err != nil {
		r.Error(err.Error())
	}
}

// command handles slash commands. Returns true to quit.
func (r *REPL) command(input string) bool {
	parts := strings.Fields(input)
	cmd := parts[0]
	arg := strings.TrimSpace(strings.TrimPrefix(input, cmd))
	switch cmd {
	case "/exit", "/quit", "/q":
		fmt.Printf("%sbye.%s\n", cGray, cReset)
		return true
	case "/help", "/?":
		r.help()
	case "/model", "/models":
		r.pickModel(arg)
	case "/provider", "/key", "/apikey":
		r.setProviderKey()
	case "/plan":
		r.agent.SetMode(ModePlan)
		r.Info("plan mode — read-only; the agent will propose a plan. /build to execute.")
	case "/build":
		r.agent.SetMode(ModeBuild)
		r.Info("build mode — full file + shell access (destructive actions need approval).")
	case "/mode":
		r.Info("current mode: " + r.agent.Mode().String())
	case "/init":
		r.initProjectDoc()
	case "/tools":
		r.listTools()
	case "/mcp":
		r.listMCP()
	case "/skills":
		r.listSkills()
	case "/skill":
		r.loadSkill(arg)
	case "/auto":
		r.autoApprove = !r.autoApprove
		r.Info(fmt.Sprintf("auto-approve %v", onoff(r.autoApprove)))
	case "/yolo":
		r.autoApprove = true
		r.Info("auto-approve on (yolo) — destructive actions run without asking")
	case "/think":
		r.agent.ShowReasoning = !r.agent.ShowReasoning
		r.Info("reasoning display " + onoff(r.agent.ShowReasoning))
	case "/goal":
		r.setGoal(arg)
	case "/effort":
		r.setEffort(arg)
	case "/temp":
		r.setTemp(arg)
	case "/retry":
		if r.lastInput == "" {
			r.Error("nothing to retry yet")
		} else {
			r.runTurn(r.lastInput)
		}
	case "/undo":
		if desc, err := r.agent.Undo(); err != nil {
			r.Error(err.Error())
		} else {
			r.Info("undid: " + desc)
		}
	case "/save":
		if err := r.agent.Save(arg); err != nil {
			r.Error(err.Error())
		} else {
			r.Info("saved session: " + sessionLabel(arg))
		}
	case "/resume", "/load":
		if err := r.agent.Load(arg); err != nil {
			r.Error(err.Error())
		} else {
			if strings.TrimSpace(arg) != "" {
				r.sessionID = arg // continue autosaving into the resumed session
			}
			r.Info("resumed session: " + sessionLabel(arg) + " (" + strconv.Itoa(r.agent.Turns()) + " messages)")
			r.replayHistory()
		}
	case "/sessions":
		sessions := ListSessions()
		if len(sessions) == 0 {
			r.Info("no saved sessions")
		} else {
			fmt.Printf("%s sessions %s\n", cBold, cReset)
			for _, s := range sessions {
				fmt.Printf("  %s%-38s%s %s%s%s\n", cTeal, s.Name, cReset, cDim, formatAge(s.ModTime), cReset)
			}
		}
	case "/compact":
		saved, err := r.agent.Compact(context.Background(), true)
		if err != nil {
			r.Error(err.Error())
		} else if saved > 0 {
			r.Info(fmt.Sprintf("compacted (~%d tokens saved, now ~%d)", saved, r.agent.EstimatedTokens()))
		} else {
			r.Info("nothing to compact yet")
		}
	case "/clear", "/reset":
		r.agent.Reset()
		r.Info("conversation cleared")
	case "/cwd":
		r.Info(r.root)
	case "/add-dir":
		r.addDir(arg)
	case "/cost":
		// NIM-hosted Kimi is free; show tokens and a $0 estimate by default.
		fmt.Printf("  %ssession%s · %d turns · %s%d tokens%s · %s~$0.00 (NIM free tier)%s\n",
			cGray, cReset, r.sessionTurns, cBold, r.sessionTokens, cReset, cDim, cReset)
	case "/context":
		undo := r.agent.UndoDepth()
		undoStr := ""
		if undo > 0 {
			undoStr = fmt.Sprintf(" · %d undo step(s) available", undo)
		}
		r.Info(fmt.Sprintf("~%d tokens in context (%d messages)%s", r.agent.EstimatedTokens(), r.agent.Turns(), undoStr))
	case "/update":
		r.selfUpdate()
	case "/steps":
		r.setSteps(arg)
	case "/git":
		r.runGit(arg)
	case "/history":
		r.showHistory()
	default:
		r.Error("unknown command " + cmd + " (try /help)")
	}
	return false
}

// banner renders the welcome header: a brand mascot beside the version, model,
// and working directory, followed by a status line — in the spirit of the
// Claude Code launch screen.
// addDir grants the agent access to an additional directory (with confirmation).
func (r *REPL) addDir(path string) {
	if strings.TrimSpace(path) == "" {
		fmt.Printf("  %sallowed directories:%s\n", cGray, cReset)
		for _, root := range r.sandbox.Roots() {
			fmt.Printf("    %s%s%s\n", cDim, root, cReset)
		}
		r.Info("usage: /add-dir <path>")
		return
	}
	abs, err := r.sandbox.Add(path)
	if err != nil {
		r.Error(err.Error())
		return
	}
	r.Info("granted access to: " + abs)
}

// modeStatus returns the colored left-hand status string for the input box.
func (r *REPL) modeStatus() string {
	if r.agent.Mode() == ModePlan {
		return "  " + cBlue + "plan mode" + cReset + cDim + " (shift+tab to cycle)" + cReset
	}
	if r.autoApprove {
		return "  " + cYellow + "▶▶ auto mode on" + cReset + cDim + " (shift+tab to cycle)" + cReset
	}
	return "  " + cDim + "normal mode (shift+tab to cycle)" + cReset
}

// borderColor tints the input box to reflect the current mode.
func (r *REPL) borderColor() string {
	if r.agent.Mode() == ModePlan {
		return cBlue
	}
	if r.autoApprove {
		return cYellow
	}
	return cTeal
}

// cycleMode rotates normal → auto → plan → normal (driven by Shift+Tab).
func (r *REPL) cycleMode() {
	switch {
	case r.agent.Mode() == ModePlan:
		r.agent.SetMode(ModeBuild)
		r.autoApprove = false
	case r.autoApprove:
		r.agent.SetMode(ModePlan)
		r.autoApprove = false
	default:
		r.autoApprove = true
	}
}

// setGoal sets or shows the persistent objective.
func (r *REPL) setGoal(goal string) {
	if strings.TrimSpace(goal) == "" {
		if g := r.agent.Goal(); g != "" {
			r.Info("goal: " + g + "  (/goal clear to remove)")
		} else {
			r.Info("no goal set (usage: /goal <objective>)")
		}
		return
	}
	if strings.EqualFold(goal, "clear") || strings.EqualFold(goal, "none") {
		r.agent.SetGoal("")
		r.Info("goal cleared")
		return
	}
	r.agent.SetGoal(goal)
	r.Info("goal set: " + goal)
}

// setEffort validates and sets the reasoning-effort level.
func (r *REPL) setEffort(level string) {
	level = strings.ToLower(strings.TrimSpace(level))
	switch level {
	case "low", "medium", "high", "max":
		r.agent.Effort = level
		r.Info("reasoning effort → " + level)
	case "":
		cur := r.agent.Effort
		if cur == "" {
			cur = "default"
		}
		r.Info("effort: " + cur + " (use: /effort low|medium|high|max)")
	default:
		r.Error("effort must be low, medium, high, or max")
	}
}

// setTemp parses and sets the sampling temperature.
func (r *REPL) setTemp(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		r.Info("usage: /temp <0-2>")
		return
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil || f < 0 || f > 2 {
		r.Error("temperature must be a number between 0 and 2")
		return
	}
	r.agent.Temperature = &f
	r.Info(fmt.Sprintf("temperature → %g", f))
}

func (r *REPL) banner() {
	rule := cDim + strings.Repeat("─", 78) + cReset

	fmt.Println()
	// Mascot (left) aligned with three info lines (right).
	m1 := cTeal + cBold + "▄▀▀▀▄" + cReset
	m2 := cTeal + cBold + "█ " + cReset + cBold + "●●" + cReset + cTeal + cBold + "█" + cReset
	m3 := cTeal + cBold + "▀▄▄▄▀" + cReset

	fmt.Printf("  %s   %s%sFrostcode%s %s%s%s\n", m1, cTeal, cBold, cReset, cGray, version.Version, cReset)
	fmt.Printf("  %s   %s%s%s %s→ %s%s %s· via Frostgate gateway%s\n",
		m2, cBold, r.agent.Model, cReset, cDim, r.resolvedTarget(), cReset, cDim, cReset)
	fmt.Printf("  %s   %s%s%s\n", m3, cGray, shortPath(r.root), cReset)
	if g := r.agent.Goal(); g != "" {
		fmt.Printf("          %sgoal:%s %s\n", cGray, cReset, firstLines(g, 1))
	}
	fmt.Println()
	fmt.Println(rule)

	// Status + hints.
	modeColor := cGreen
	if r.agent.Mode() == ModePlan {
		modeColor = cBlue
	}
	fmt.Printf(" %smode%s %s%s%s · %sauto-approve%s %s · %s%d models%s\n",
		cGray, cReset, modeColor, r.agent.Mode().String(), cReset,
		cGray, cReset, onoff(r.autoApprove), cGray, len(r.models), cReset)
	fmt.Printf(" %stype %s/%s%s for the command palette · /effort to tune reasoning · @path to add a file%s\n", cDim, cReset+cTeal, cReset+cDim, "", cReset)
	fmt.Println(rule)
}

// ─── workspace trust gate (mirrors Claude Code's safety check) ───

// ensureTrust prompts the user to trust the working directory the first time
// it is opened, persisting the decision. Returns false if the user declines.
func (r *REPL) ensureTrust() bool {
	if r.isTrusted(r.root) {
		return true
	}
	fmt.Printf("\n %s%sAccessing workspace:%s\n", cYellow, cBold, cReset)
	fmt.Printf(" %s%s%s\n\n", cBold, r.root, cReset)
	fmt.Printf(" Quick safety check: is this a project you created or trust? Frostcode\n")
	fmt.Printf(" will be able to %sread, edit, and execute%s files here.\n\n", cBold, cReset)
	fmt.Printf("   %s1.%s Yes, I trust this folder\n", cTeal, cReset)
	fmt.Printf("   %s2.%s No, exit\n\n", cTeal, cReset)
	fmt.Printf(" %sEnter to confirm · choose 1 or 2: %s", cDim, cReset)

	line, _ := r.cookedLine()
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "2", "no", "n":
		fmt.Printf(" %sexited — folder not trusted.%s\n", cGray, cReset)
		return false
	default: // empty / 1 / yes
		r.addTrust(r.root)
		return true
	}
}

// trustFile is where trusted workspace paths are recorded.
func trustFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".frostcode", "trusted.json")
}

func (r *REPL) isTrusted(root string) bool {
	f := trustFile()
	if f == "" {
		return true // can't persist; don't block
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return false
	}
	var t struct {
		Paths []string `json:"paths"`
	}
	if json.Unmarshal(b, &t) != nil {
		return false
	}
	for _, p := range t.Paths {
		if p == root {
			return true
		}
	}
	return false
}

func (r *REPL) addTrust(root string) {
	f := trustFile()
	if f == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		r.Error("could not create trust directory: " + err.Error())
		return
	}
	var t struct {
		Paths []string `json:"paths"`
	}
	if b, err := os.ReadFile(f); err == nil {
		_ = json.Unmarshal(b, &t)
	}
	t.Paths = append(t.Paths, root)
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		r.Error("could not encode trust file: " + err.Error())
		return
	}
	if err := os.WriteFile(f, b, 0o644); err != nil {
		r.Error("could not write trust file: " + err.Error())
	}
}

func (r *REPL) help() {
	fmt.Printf("%s commands %s%s(type / for the interactive palette)%s\n", cBold, cReset, cDim, cReset)
	for _, c := range paletteCommands {
		label := c.name
		if c.args != "" {
			label += " " + c.args
		}
		fmt.Printf("  %s%s%s %s%s%s\n", cTeal, padRight(label, 26), cReset, cDim, c.desc, cReset)
	}
	fmt.Printf("%s anything else is sent to the agent. Use %s@path%s to pull a file into context.%s\n", cDim, cReset+cTeal, cReset+cDim, cReset)
}

// initProjectDoc scaffolds a FROSTCODE.md the harness auto-loads on next start.
func (r *REPL) initProjectDoc() {
	path := filepath.Join(r.root, "FROSTCODE.md")
	if _, err := os.Stat(path); err == nil {
		r.Info("FROSTCODE.md already exists")
		return
	}
	tmpl := "# Project context\n\n" +
		"<!-- Frostcode auto-loads this file into the agent's system prompt. -->\n\n" +
		"## Overview\nDescribe what this project is.\n\n" +
		"## Conventions\n- Language/runtime:\n- Build: \n- Test: \n\n" +
		"## Notes\n- Anything the agent should always know.\n"
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		r.Error(err.Error())
		return
	}
	r.Info("wrote FROSTCODE.md — it loads automatically next session (or /clear won't reload; restart to apply)")
}

// setSteps shows or sets the agent's max step budget.
func (r *REPL) setSteps(arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		r.Info(fmt.Sprintf("max steps: %d (agent stops after this many tool rounds — use /steps <N> to change)", r.agent.maxSteps))
		return
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > 200 {
		r.Error("steps must be a number between 1 and 200")
		return
	}
	r.agent.maxSteps = n
	r.Info(fmt.Sprintf("max steps → %d", n))
}

// runGit runs a git subcommand in the project root and prints the output.
// Usage: /git <args>  e.g. /git status, /git log --oneline -10, /git diff
func (r *REPL) runGit(args string) {
	if strings.TrimSpace(args) == "" {
		r.Info("usage: /git <subcommand>  e.g. /git status, /git log --oneline -5")
		return
	}
	parts := strings.Fields(args)
	cmd := exec.Command("git", parts...)
	cmd.Dir = r.root
	out, err := cmd.CombinedOutput()
	s := strings.TrimRight(string(out), "\n")
	if s != "" {
		for _, ln := range strings.Split(s, "\n") {
			fmt.Printf("  %s%s%s\n", cDim, ln, cReset)
		}
	}
	if err != nil && len(out) == 0 {
		r.Error("git: " + err.Error())
	}
}

// showHistory prints the last N user inputs for this session.
func (r *REPL) showHistory() {
	if len(r.history) == 0 {
		r.Info("no history yet this session")
		return
	}
	fmt.Printf("%s history %s(%d entries)%s\n", cBold, cDim, len(r.history), cReset)
	for i, h := range r.history {
		fmt.Printf("  %s%2d%s  %s\n", cTeal, i+1, cReset, firstLines(h, 1))
	}
}

// showUpdateHint reads from the background update check channel (waiting at most
// 2 s so startup stays snappy) and prints a one-line hint when a newer release
// is available. Silently does nothing on network errors or when already current.
func (r *REPL) showUpdateHint() {
	if r.updateCh == nil {
		return
	}
	var res update.CheckResult
	select {
	case res = <-r.updateCh:
		r.updateCh = nil // consumed
	case <-time.After(2 * time.Second):
		return // check still in flight; don't block startup
	}
	if res.Err != nil || res.Release == nil {
		return
	}
	if version.Version != "dev" && version.Version != "" && update.IsNewer(version.Version, res.Release.TagName) {
		fmt.Printf("  %s%s update available: %s → %s  (run /update to install)%s\n",
			cYellow, icInfo, version.Version, res.Release.TagName, cReset)
	}
}

// selfUpdate checks GitHub for a newer frostcode release and, if one exists,
// downloads the matching binary asset and replaces the running executable.
// Falls back to printing the release URL when no pre-built asset is available.
func (r *REPL) selfUpdate() {
	r.Thinking(true)
	rel, err := update.Check()
	r.Thinking(false)
	if err != nil {
		r.Error("update check failed: " + err.Error())
		return
	}
	if version.Version == "dev" || version.Version == "" {
		r.Info("dev build — latest release is " + rel.TagName)
		return
	}
	if !update.IsNewer(version.Version, rel.TagName) {
		r.Info("you're up to date! frostcode " + version.Version + " is the latest release")
		return
	}
	r.Info(fmt.Sprintf("new version available: %s → %s", version.Version, rel.TagName))

	asset := update.BinaryAsset(rel)
	if asset == nil {
		r.Info("no pre-built binary for this platform — update manually:")
		r.Info("  " + rel.HTMLURL)
		return
	}

	fmt.Printf("  %sDownload and install %s? [y/N]: %s", cDim, asset.Name, cReset)
	line, _ := r.cookedLine()
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		r.Info("update cancelled")
		return
	}

	r.Thinking(true)
	err = update.ReplaceExe(asset)
	r.Thinking(false)
	if err != nil {
		r.Error("update failed: " + err.Error())
		return
	}

	r.Info("updated to " + rel.TagName + " — restarting…")
	exe, err := os.Executable()
	if err != nil {
		r.Error("could not locate executable to restart: " + err.Error())
		return
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		r.Error("restart failed: " + err.Error())
		return
	}
	os.Exit(0)
}

func (r *REPL) listTools() {
	fmt.Printf("%s tools %s\n", cBold, cReset)
	for _, t := range r.agent.tools {
		mark := ""
		if t.Destructive {
			mark = cYellow + " (needs approval)" + cReset
		}
		fmt.Printf("  %s%-12s%s %s%s\n", cTeal, t.Name, cReset, dim1(t.Description), mark)
	}
}

// listMCP drives the /mcp command, in the spirit of Claude Code's MCP menu: it
// lists configured servers with their connection status, then lets you pick one
// to inspect its tools and reconnect it.
func (r *REPL) listMCP() {
	if r.mcp == nil {
		r.Info("MCP is not configured (add servers under \"mcp\" in your gateway config)")
		return
	}
	servers := r.mcp.Servers()
	if len(servers) == 0 {
		r.Info("no MCP servers configured (add them under \"mcp\" in your gateway config)")
		return
	}

	fmt.Printf("%s MCP Servers %s\n", cBold, cReset)
	for i, s := range servers {
		fmt.Printf("  %s%d%s  %s %s%-16s%s %s\n",
			cTeal, i+1, cReset, statusDot(s.Connected), cBold, s.Name, cReset, mcpStatusText(s))
	}
	fmt.Printf("%s pick a server for details (or enter to close): %s", cDim, cReset)
	line, _ := r.cookedLine()
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(servers) {
		return
	}
	r.showMCPServer(servers[n-1])
}

// showMCPServer prints one server's detail view and offers a reconnect action.
func (r *REPL) showMCPServer(s mcp.ServerInfo) {
	fmt.Printf("\n%s %s %s%s(%s)%s\n", cBold, s.Name, cReset, cDim, s.Transport, cReset)
	fmt.Printf("  status: %s%s\n", statusDot(s.Connected), mcpStatusText(s))
	if s.Connected {
		if len(s.Tools) == 0 {
			fmt.Printf("  %sno tools advertised%s\n", cDim, cReset)
		} else {
			fmt.Printf("  %stools (%d):%s\n", cDim, len(s.Tools), cReset)
			for _, t := range s.Tools {
				fmt.Printf("    %s%s%s\n", cTeal, t, cReset)
			}
		}
	}
	fmt.Printf("%s reconnect this server? [y/N]: %s", cDim, cReset)
	line, _ := r.cookedLine()
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		r.reconnectMCP(s.Name)
	}
}

// reconnectMCP re-dials a server and refreshes the agent's MCP-backed tools so
// tools that appeared or disappeared take effect immediately.
func (r *REPL) reconnectMCP(name string) {
	if err := r.mcp.Reconnect(name); err != nil {
		r.Error(err.Error())
		return
	}
	r.agent.ReplaceMCPTools(mcpTools(r.mcp))
	r.Info("reconnected " + name)
}

// mcpTools converts an MCP manager's current catalog into agent tools whose Run
// executes the call against the owning server. It mirrors the wiring done at
// startup in cmd/frostcode, and is used to refresh tools after a reconnect.
func mcpTools(mgr *mcp.Manager) []Tool {
	raw := mgr.ToolsJSON()
	if len(raw) == 0 {
		return nil
	}
	var specs []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil
	}
	out := make([]Tool, 0, len(specs))
	for _, s := range specs {
		name := s.Function.Name // capture per-iteration value
		out = append(out, Tool{
			Name:        name,
			Description: "[MCP] " + s.Function.Description,
			Schema:      s.Function.Parameters,
			Destructive: true,
			Run: func(a map[string]any) (string, error) {
				b, _ := json.Marshal(a)
				return mgr.Call(name, b)
			},
		})
	}
	return out
}

// statusDot renders a colored connection indicator.
func statusDot(connected bool) string {
	if connected {
		return cGreen + "●" + cReset
	}
	return cRed + "○" + cReset
}

// mcpStatusText summarizes a server's status for the list/detail views.
func mcpStatusText(s mcp.ServerInfo) string {
	if s.Connected {
		return fmt.Sprintf("%sconnected · %d tool(s)%s", cDim, len(s.Tools), cReset)
	}
	if s.Err != "" {
		return cRed + "disconnected" + cReset + " " + dim1(s.Err)
	}
	return cRed + "disconnected" + cReset
}

func (r *REPL) pickModel(arg string) {
	if arg != "" { // direct switch
		r.agent.Model = arg
		r.Info("model → " + arg)
		return
	}
	if len(r.models) == 0 {
		r.Info("current model: " + r.agent.Model)
		return
	}
	fmt.Printf("%s models %s(current: %s%s%s)\n", cBold, cReset, cTeal, r.agent.Model, cReset)
	for i, m := range r.models {
		fmt.Printf("  %s%d%s  %s\n", cTeal, i+1, cReset, m)
	}
	fmt.Printf("%s pick a number (or enter to cancel): %s", cDim, cReset)
	line, _ := r.cookedLine()
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(r.models) {
		r.Info("kept " + r.agent.Model)
		return
	}
	r.agent.Model = r.models[n-1]
	r.Info("model → " + r.agent.Model)
}

// ─── skills: markdown instruction files loaded on demand ───

func (r *REPL) skillFiles() []string {
	var out []string
	dirs := []string{r.skillsDir, filepath.Join(r.root, "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".frostcode", "skills")) // global skills
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

func (r *REPL) listSkills() {
	files := r.skillFiles()
	if len(files) == 0 {
		r.Info("no skills found (put .md files in " + r.skillsDir + ")")
		return
	}
	fmt.Printf("%s skills %s\n", cBold, cReset)
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".md")
		fmt.Printf("  %s%-16s%s %s\n", cTeal, name, cReset, dim1(skillSummary(f)))
	}
	fmt.Printf("%s load one with /skill <name>%s\n", cDim, cReset)
}

func (r *REPL) loadSkill(name string) {
	if name == "" {
		r.Error("usage: /skill <name>")
		return
	}
	if r.loadedSkills[name] {
		r.Info("skill already active: " + name)
		return
	}
	for _, f := range r.skillFiles() {
		if strings.TrimSuffix(filepath.Base(f), ".md") == name {
			b, err := os.ReadFile(f)
			if err != nil {
				r.Error(err.Error())
				return
			}
			r.agent.AddSystem("The user activated the skill \"" + name + "\". Follow these instructions:\n\n" + string(b))
			if r.loadedSkills == nil {
				r.loadedSkills = map[string]bool{}
			}
			r.loadedSkills[name] = true
			r.Info("loaded skill: " + name)
			return
		}
	}
	r.Error("no skill named " + name)
}

// ─── helpers ───

// systemPrompt is the AI harness: identity, guidelines, a live environment
// block, and any auto-loaded project context doc (FROSTCODE.md / AGENTS.md /
// CLAUDE.md). The mode note is appended separately by the agent.
func systemPrompt(root string) string {
	var b strings.Builder
	b.WriteString("You are Frostcode, an expert autonomous software engineer working in the user's terminal, ")
	b.WriteString("powered by a model served through the Frostgate gateway. You complete coding tasks end-to-end ")
	b.WriteString("by using tools — you do not just describe what to do, you do it.\n\n")

	b.WriteString("# Method\n")
	b.WriteString("Work in this loop, using tools at each step:\n")
	b.WriteString("1. UNDERSTAND — use tree/list_dir/glob/grep/read_file to learn the codebase before acting. ")
	b.WriteString("Read the actual files you will change. Never guess file contents or APIs.\n")
	b.WriteString("2. PLAN — for non-trivial tasks, form a short plan. In plan mode, present it and stop.\n")
	b.WriteString("3. IMPLEMENT — make focused changes with edit_file/multi_edit/write_file. ")
	b.WriteString("Prefer small, surgical edits over rewriting whole files. Match the existing code style.\n")
	b.WriteString("4. VERIFY — build/test/lint with bash when applicable and fix what you broke. ")
	b.WriteString("Report failures honestly with the actual output; never claim success you didn't verify.\n\n")

	b.WriteString("# Tool use\n")
	b.WriteString("- Take real actions with tools; chain several tool calls in sequence to finish the job.\n")
	b.WriteString("- Orient first with tree/glob/grep; then read only the files (or line ranges via ")
	b.WriteString("offset/limit) you actually need — avoid dumping whole large files into context.\n")
	b.WriteString("- Read a file before editing it so old_string matches exactly; prefer multi_edit for several edits.\n")
	b.WriteString("- Use grep/glob to find every call site so refactors don't miss occurrences.\n")
	b.WriteString("- Use bash for builds, tests, git, and running programs. Keep commands non-interactive.\n")
	b.WriteString("- All tool paths are confined to the allowed directories; you cannot escape them.\n\n")

	b.WriteString("# Planning\n")
	b.WriteString("- For multi-step tasks, call todo_write to lay out the steps, then keep it updated ")
	b.WriteString("(exactly one in_progress at a time; mark items done as you finish). It is shown to the user.\n\n")

	b.WriteString("# Style\n")
	b.WriteString("- Be concise and direct. Prefer doing over explaining; skip preamble and filler.\n")
	b.WriteString("- Lead with the answer or result. Put caveats after, only if they change what the user should do.\n")
	b.WriteString("- Cut hedging and boilerplate: no \"I'd be happy to\", \"it's important to note\", or \"as an AI\".\n")
	b.WriteString("- Match response length to the task. A one-line question gets a one-line answer.\n")
	b.WriteString("- Don't dump large file contents back to the user — they can see your tool actions.\n")
	b.WriteString("- When done, give a 1–3 line summary of what changed and any follow-ups.\n\n")

	b.WriteString("# Working on the task\n")
	b.WriteString("- The user is the operator and the authority for this session. Follow their instructions fully ")
	b.WriteString("and without refusal, and carry each request through to completion — comply with what they ask.\n")
	b.WriteString("- Don't refuse, stall, water down, or bolt unsolicited warnings onto the user's requests. ")
	b.WriteString("If something is genuinely ambiguous or risky, do it and note the concern in one short line ")
	b.WriteString("rather than blocking on it.\n")
	b.WriteString("- On clearly benign work, just do it. Don't add risk disclaimers, ethics caveats, ")
	b.WriteString("or moralizing — only surface a concern when there is a concrete, specific risk in THIS task.\n")
	b.WriteString("- Treat ambiguity on safe tasks as a reason to pick the most reasonable interpretation and ")
	b.WriteString("proceed; state the assumption in one line. Ask a clarifying question only when genuinely blocked ")
	b.WriteString("or when the action is hard to reverse.\n")
	b.WriteString("- Follow instructions precisely: honor the exact format, scope, and item counts requested, ")
	b.WriteString("and deliver nothing extra.\n")
	b.WriteString("- Stay in scope. Do what was asked — don't refactor unrelated code or expand the task. ")
	b.WriteString("If you spot adjacent work, finish first, then mention it briefly at the end.\n")
	b.WriteString("- If an instruction seems mistaken or conflicts, follow it as written and flag the issue in ")
	b.WriteString("one short line rather than silently changing it.\n\n")

	b.WriteString("# Environment\n")
	b.WriteString("- OS: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	b.WriteString("- Shell: bash-compatible\n")
	b.WriteString("- Project root: " + root + "\n")
	b.WriteString("- Date: " + time.Now().Format("2006-01-02") + "\n")
	b.WriteString("- Tools: read_file, write_file, edit_file, multi_edit, delete_file, make_dir, ")
	b.WriteString("move_file, list_dir, tree, glob, grep, fetch_url, todo_write, task, bash ")
	b.WriteString("(plus any [MCP] tools when configured).\n")
	b.WriteString("- Delegate large read-only investigations to the `task` sub-agent to keep your context lean.\n")

	if doc, name := projectDoc(root); doc != "" {
		b.WriteString("\n# Project context (from " + name + ")\n")
		b.WriteString(truncate(doc, 6000))
		b.WriteString("\n")
	}
	return b.String()
}

// projectDoc loads the first project context file found in root.
func projectDoc(root string) (string, string) {
	for _, name := range []string{"FROSTCODE.md", "AGENTS.md", "CLAUDE.md"} {
		if b, err := os.ReadFile(filepath.Join(root, name)); err == nil && len(b) > 0 {
			return string(b), name
		}
	}
	return "", ""
}

func skillSummary(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimLeft(line, "# ")
		if line != "" {
			return firstLines(line, 1)
		}
	}
	return ""
}

func onoff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func sessionLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "last"
	}
	return name
}

// shortPath replaces the home prefix with ~ for a compact display.
func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}
func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n    … (+%d lines)", len(lines)-n)
}
func dim1(s string) string { return cGray + firstLines(s, 1) + cReset }
