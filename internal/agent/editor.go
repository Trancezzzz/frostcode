package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmdSpec describes a slash command for the palette and /help.
type cmdSpec struct {
	name string
	args string
	desc string
}

// paletteCommands drives the slash-command palette and the /help listing.
var paletteCommands = []cmdSpec{
	{"/help", "", "Show all commands"},
	{"/model", "[name]", "List models or switch the active model"},
	{"/provider", "", "Pick a provider and set its API key"},
	{"/effort", "<low|medium|high|max>", "Set the model's reasoning effort"},
	{"/think", "", "Toggle showing the model's reasoning"},
	{"/goal", "<objective>", "Set a persistent goal (or 'clear')"},
	{"/temp", "<0-2>", "Set sampling temperature"},
	{"/plan", "", "Plan mode: read-only, propose a plan"},
	{"/build", "", "Build mode: full file + shell access"},
	{"/mode", "", "Show the current mode"},
	{"/tools", "", "List the tools the agent can use"},
	{"/mcp", "", "Manage MCP servers: status, tools, reconnect"},
	{"/skills", "", "List available skills"},
	{"/skill", "<name>", "Load a skill's instructions into context"},
	{"/init", "", "Scaffold a FROSTCODE.md project context file"},
	{"/auto", "", "Toggle auto-approve for file/shell actions"},
	{"/yolo", "", "Enable auto-approve (no prompts)"},
	{"/undo", "", "Revert the last file change"},
	{"/save", "[name]", "Save the current conversation"},
	{"/resume", "[name]", "Resume a saved conversation"},
	{"/sessions", "", "List saved conversations"},
	{"/compact", "", "Summarize old turns to free context"},
	{"/cost", "", "Show session token usage / cost"},
	{"/context", "", "Show estimated tokens in context"},
	{"/update", "", "Check for and install the latest release"},
	{"/git", "<args>", "Run a git command in the project root (e.g. /git status)"},
	{"/history", "", "Show recent inputs from this session"},
	{"/retry", "", "Re-run your last message"},
	{"/clear", "", "Reset the conversation"},
	{"/cwd", "", "Show the working directory"},
	{"/add-dir", "<path>", "Grant the agent access to another directory"},
	{"/exit", "", "Quit Frostcode"},
}

// cookedLine reads one line directly from stdin (no bufio), used for sub-prompts
// and as the fallback when a rich console editor isn't available (e.g. piped
// input). Returns ok=false on EOF.
func (r *REPL) cookedLine() (string, bool) {
	var sb []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			switch buf[0] {
			case '\n':
				return string(sb), true
			case '\r':
				// ignore; \n will terminate
			default:
				sb = append(sb, buf[0])
			}
		}
		if err != nil {
			if len(sb) > 0 {
				return string(sb), true
			}
			return "", false
		}
	}
}

// promptLine reads the main prompt: the rich palette editor on a real console,
// or a plain cooked line otherwise.
func (r *REPL) promptLine() (string, bool) {
	if r.rich {
		return r.richLine()
	}
	fmt.Printf("\n%s%s›%s ", cTeal, cBold, cReset)
	return r.cookedLine()
}

// filterCommands returns palette entries whose name starts with the typed token
// (which begins with '/' and has no space yet).
func filterCommands(token string) []cmdSpec {
	out := make([]cmdSpec, 0, len(paletteCommands))
	lt := strings.ToLower(token)
	for _, c := range paletteCommands {
		if strings.HasPrefix(c.name, lt) {
			out = append(out, c)
		}
	}
	return out
}

// richLine is the raw-mode line editor with a live slash-command palette.
func (r *REPL) richLine() (string, bool) {
	restore, ok := stdinRawMode()
	if !ok {
		fmt.Printf("\n%s%s›%s ", cTeal, cBold, cReset)
		return r.cookedLine()
	}
	defer restore()

	width := consoleWidth()
	if width < 40 {
		width = 40
	}
	field := width - 6 // text area between "│ > " and " │"
	const maxRows = 9

	buf := []rune{}
	cursor := 0
	scroll := 0 // horizontal scroll offset into buf
	sel := 0
	primed := false
	drawn := false

	paletteFor := func() []cmdSpec {
		s := string(buf)
		if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, " \t") {
			return nil
		}
		return filterCommands(s)
	}

	render := func() {
		items := paletteFor()
		if sel >= len(items) {
			sel = len(items) - 1
		}
		if sel < 0 {
			sel = 0
		}
		// Keep the cursor within the visible text field.
		if cursor < scroll {
			scroll = cursor
		}
		if cursor-scroll >= field {
			scroll = cursor - field + 1
		}
		if scroll < 0 {
			scroll = 0
		}
		end := scroll + field
		if end > len(buf) {
			end = len(buf)
		}
		visible := string(buf[scroll:end])
		pad := field - (end - scroll)
		if pad < 0 {
			pad = 0
		}

		var b strings.Builder
		if drawn {
			b.WriteString("\r\x1b[1A\x1b[J") // up to top border, clear region
		} else {
			b.WriteString("\n") // breathing room above the box
			drawn = true
		}
		// Bordered input box, tinted by the current mode.
		bc := r.borderColor()
		b.WriteString(bc + "╭" + strings.Repeat("─", width-2) + "╮" + cReset + "\n")
		b.WriteString(bc + "│" + cReset + " " + bc + cBold + ">" + cReset + " ")
		b.WriteString(visible + strings.Repeat(" ", pad))
		b.WriteString(" " + bc + "│" + cReset + "\n")
		b.WriteString(bc + "╰" + strings.Repeat("─", width-2) + "╯" + cReset)

		// Below the box: palette when active, else the mode status line.
		var below []string
		rows := len(items)
		if rows > maxRows {
			rows = maxRows
		}
		for i := 0; i < rows; i++ {
			it := items[i]
			name := it.name
			if it.args != "" {
				name += " " + it.args
			}
			name = padRight(name, 26)
			desc := it.desc
			if mx := width - 30; mx > 8 && len(desc) > mx {
				desc = desc[:mx-1] + "…"
			}
			if i == sel {
				below = append(below, cTeal+cBold+icSel+name+cReset+" "+desc)
			} else {
				below = append(below, " "+cGray+name+cReset+cDim+" "+desc+cReset)
			}
		}
		if len(below) == 0 {
			if primed {
				below = append(below, "  "+cDim+"Press Ctrl-C again to exit"+cReset)
			} else {
				below = append(below, statusLine(r.modeStatus(), r.agent.Model, width))
			}
		}
		for _, ln := range below {
			b.WriteString("\n" + ln)
		}
		// Move cursor back to the input line at the right column.
		// Region rows below the input line = 1 (bottom border) + len(below).
		fmt.Fprintf(&b, "\x1b[%dA", 1+len(below))
		fmt.Fprintf(&b, "\r\x1b[%dC", 4+(cursor-scroll)) // 4 = "│ > "
		fmt.Print(b.String())
	}

	finish := func(line string) {
		fmt.Print("\r\x1b[1A\x1b[J") // clear the box region
		fmt.Printf("%s%s›%s %s\n", cTeal, cBold, cReset, line)
	}

	render()
	rb := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(rb)
		if err != nil || n == 0 {
			finish(string(buf))
			return string(buf), len(buf) > 0
		}
		c := rb[0]
		if c != 0x03 {
			primed = false
		}
		switch {
		case c == 0x1b: // escape sequence
			var s2 [2]byte
			os.Stdin.Read(s2[:1])
			if s2[0] == '[' {
				os.Stdin.Read(s2[1:2])
				items := paletteFor()
				switch s2[1] {
				case 'A':
					if len(items) > 0 && sel > 0 {
						sel--
					}
				case 'B':
					if len(items) > 0 && sel < len(items)-1 {
						sel++
					}
				case 'C':
					if cursor < len(buf) {
						cursor++
					}
				case 'D':
					if cursor > 0 {
						cursor--
					}
				case 'Z': // Shift+Tab: cycle permission/plan mode
					r.cycleMode()
				}
			}
		case c == '\r' || c == '\n':
			items := paletteFor()
			if len(items) > 0 {
				line := items[sel].name
				finish(line)
				return line, true
			}
			finish(string(buf))
			return string(buf), true
		case c == '\t':
			items := paletteFor()
			if len(items) > 0 {
				buf = []rune(items[sel].name + " ")
				cursor = len(buf)
			}
		case c == 0x7f || c == 0x08:
			if cursor > 0 {
				buf = append(buf[:cursor-1], buf[cursor:]...)
				cursor--
			}
		case c == 0x03: // Ctrl-C
			if len(buf) > 0 {
				buf = buf[:0]
				cursor = 0
				sel = 0
				primed = false
			} else if primed {
				fmt.Print("\r\x1b[1A\x1b[J")
				return "", false
			} else {
				primed = true
			}
		case c == 0x04: // Ctrl-D
			if len(buf) == 0 {
				finish("")
				return "", false
			}
		case c >= 0x20:
			buf = append(buf, 0)
			copy(buf[cursor+1:], buf[cursor:])
			buf[cursor] = rune(c)
			cursor++
		}
		render()
	}
}

// selectModal shows a vertical list and lets the user pick one entry with the
// arrow keys (Enter confirms, Esc/Ctrl-C cancels). It returns the selected index
// and ok=true, or ok=false if cancelled. Without a raw console it falls back to a
// numbered prompt read from stdin.
func (r *REPL) selectModal(title string, items []string) (int, bool) {
	if len(items) == 0 {
		return 0, false
	}

	restore, ok := stdinRawMode()
	if !ok {
		fmt.Printf("\n%s%s%s\n", cBold, title, cReset)
		for i, it := range items {
			fmt.Printf("  %s%2d%s  %s\n", cGray, i+1, cReset, it)
		}
		fmt.Printf("%s%s›%s ", cTeal, cBold, cReset)
		line, ok := r.cookedLine()
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(items) {
			return 0, false
		}
		return n - 1, true
	}
	defer restore()

	sel := 0
	drawn := false
	render := func() {
		var b strings.Builder
		if drawn {
			fmt.Fprintf(&b, "\r\x1b[%dA\x1b[J", len(items)+1)
		} else {
			b.WriteString("\n")
			drawn = true
		}
		b.WriteString(cBold + title + cReset + "\n")
		for i, it := range items {
			if i == sel {
				b.WriteString(cTeal + cBold + icSel + it + cReset)
			} else {
				b.WriteString(" " + cGray + it + cReset)
			}
			if i < len(items)-1 {
				b.WriteString("\n")
			}
		}
		fmt.Print(b.String())
	}
	clear := func() { fmt.Printf("\r\x1b[%dA\x1b[J", len(items)+1) }

	render()
	rb := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(rb)
		if err != nil || n == 0 {
			clear()
			return 0, false
		}
		switch rb[0] {
		case 0x1b: // escape sequence or bare Esc
			var s2 [2]byte
			if n, _ := os.Stdin.Read(s2[:1]); n == 0 {
				clear()
				return 0, false
			}
			if s2[0] == '[' {
				os.Stdin.Read(s2[1:2])
				switch s2[1] {
				case 'A':
					if sel > 0 {
						sel--
					}
				case 'B':
					if sel < len(items)-1 {
						sel++
					}
				}
			} else {
				clear()
				return 0, false
			}
		case '\r', '\n':
			clear()
			return sel, true
		case 0x03, 0x04: // Ctrl-C / Ctrl-D
			clear()
			return 0, false
		}
		render()
	}
}

// secretModal reads a secret on one line, echoing a mask instead of the typed
// characters. Enter confirms, Esc/Ctrl-C cancels. Without a raw console it falls
// back to an unmasked cooked line (the best available).
func (r *REPL) secretModal(title string) (string, bool) {
	restore, ok := stdinRawMode()
	if !ok {
		fmt.Printf("\n%s%s%s\n%s%s›%s ", cBold, title, cReset, cTeal, cBold, cReset)
		return r.cookedLine()
	}
	defer restore()

	buf := []rune{}
	drawn := false
	render := func() {
		var b strings.Builder
		if drawn {
			b.WriteString("\r\x1b[1A\x1b[J")
		} else {
			b.WriteString("\n")
			drawn = true
		}
		b.WriteString(cBold + title + cReset + "\n")
		b.WriteString(cTeal + cBold + "›" + cReset + " " + strings.Repeat("•", len(buf)))
		fmt.Print(b.String())
	}
	clear := func() { fmt.Print("\r\x1b[1A\x1b[J") }

	render()
	rb := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(rb)
		if err != nil || n == 0 {
			clear()
			return string(buf), len(buf) > 0
		}
		c := rb[0]
		switch {
		case c == 0x1b: // bare Esc cancels; swallow any sequence
			var s2 [2]byte
			if n, _ := os.Stdin.Read(s2[:1]); n > 0 && s2[0] == '[' {
				os.Stdin.Read(s2[1:2])
				render()
				continue
			}
			clear()
			return "", false
		case c == '\r' || c == '\n':
			clear()
			return string(buf), true
		case c == 0x7f || c == 0x08:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
			}
		case c == 0x03: // Ctrl-C
			clear()
			return "", false
		case c == 0x04: // Ctrl-D
			clear()
			return string(buf), len(buf) > 0
		case c >= 0x20:
			buf = append(buf, rune(c))
		}
		render()
	}
}

// statusLine renders the mode indicator (left) and model (right) padded to w.
func statusLine(left, model string, w int) string {
	right := cDim + model + cReset
	gap := w - visibleLen(left) - visibleLen(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// visibleLen counts printable runes, skipping ANSI escape sequences.
func visibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

// padRight pads s with spaces to width w (no truncation shorter).
func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
