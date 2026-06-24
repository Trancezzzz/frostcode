package agent

import "strings"

// cCode colors inline code and code blocks.
const cCode = "\x1b[38;5;114m"

// renderMarkdownLine converts one line of markdown to ANSI. inCode tracks
// whether we're inside a fenced code block across lines.
func renderMarkdownLine(line string, inCode *bool) string {
	trimmed := strings.TrimSpace(line)

	// Fenced code blocks: ``` toggles; render the fence as a subtle rule.
	if strings.HasPrefix(trimmed, "```") {
		*inCode = !*inCode
		lang := strings.TrimPrefix(trimmed, "```")
		if *inCode && lang != "" {
			return "  " + cDim + "┄┄ " + lang + " ┄┄" + cReset
		}
		return "  " + cDim + "┄┄┄┄┄┄┄┄" + cReset
	}
	if *inCode {
		return cCode + "  " + line + cReset
	}

	// Headers.
	if h := strings.TrimLeft(line, "#"); h != line {
		level := len(line) - len(h)
		if level >= 1 && level <= 6 && strings.HasPrefix(h, " ") {
			return cBold + cTeal + strings.TrimSpace(h) + cReset
		}
	}
	// Bullets.
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		return indent + cTeal + "•" + cReset + " " + applyInline(strings.TrimSpace(trimmed[2:]))
	}
	// Blockquote.
	if strings.HasPrefix(trimmed, "> ") {
		return cDim + "▏ " + applyInline(strings.TrimSpace(trimmed[2:])) + cReset
	}
	return applyInline(line)
}

// applyInline renders `code` spans and **bold** within a line.
func applyInline(s string) string {
	// Inline code first (odd-indexed segments between backticks).
	parts := strings.Split(s, "`")
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 {
			b.WriteString(cCode + p + cReset)
		} else {
			b.WriteString(boldify(p))
		}
	}
	return b.String()
}

// boldify replaces **text** with bold ANSI.
func boldify(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "**")
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.Index(s[i+2:], "**")
		if j < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		b.WriteString(cBold + s[i+2:i+2+j] + cReset)
		s = s[i+2+j+2:]
	}
	return b.String()
}

// renderMarkdownBlock renders a full multi-line string (non-streaming path).
func renderMarkdownBlock(text string) string {
	inCode := false
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = renderMarkdownLine(ln, &inCode)
	}
	return strings.Join(lines, "\n")
}
