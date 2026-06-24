//go:build !windows

package agent

// stdinRawMode is a no-op on non-Windows platforms for now: the rich palette
// editor is Windows-only, and other platforms fall back to plain line input.
// (A termios implementation can be added here later.)
func stdinRawMode() (func(), bool) { return nil, false }

// consoleWidth defaults to 100 columns off-Windows.
func consoleWidth() int { return 100 }
