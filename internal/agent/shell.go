package agent

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Shell keeps a single long-lived bash session so that working directory and
// environment changes (cd, export, ...) persist across bash tool calls — unlike
// spawning a fresh process per command. Commands run sequentially.
type Shell struct {
	dir     string
	isCmd   bool          // true when running Windows cmd.exe instead of bash
	timeout time.Duration // per-command timeout; 0 uses defaultShellTimeout

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	alive  bool
}

const defaultShellTimeout = 30 * time.Second

func (s *Shell) effectiveTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return defaultShellTimeout
}

// NewShell returns a lazily-started shell rooted at dir.
func NewShell(dir string) *Shell { return &Shell{dir: dir} }

const shellSentinel = "__FROST_DONE__"

// ensure starts the shell process if it isn't running.
func (s *Shell) ensure() error {
	if s.alive {
		return nil
	}
	s.isCmd = false
	shell, err := exec.LookPath("bash")
	if err != nil {
		// Prefer Git Bash on Windows before falling back to cmd.exe.
		for _, candidate := range []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		} {
			if _, e2 := exec.LookPath(candidate); e2 == nil {
				shell = candidate
				break
			}
		}
		if shell == "" && runtime.GOOS == "windows" {
			shell, err = exec.LookPath("cmd")
			if err == nil {
				s.isCmd = true
			}
		}
		if shell == "" {
			return fmt.Errorf("no shell found (install Git for Windows for best results)")
		}
	}
	var cmd *exec.Cmd
	if s.isCmd {
		cmd = exec.Command(shell, "/q") // /q suppresses echo
	} else {
		cmd = exec.Command(shell, "--norc", "--noprofile")
	}
	cmd.Dir = s.dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd, s.stdin, s.stdout, s.alive = cmd, stdin, bufio.NewReader(stdout), true
	return nil
}

// Run executes command in the persistent session and returns its combined
// output. cd/export persist for subsequent calls.
func (s *Shell) Run(command string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("empty command")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(); err != nil {
		return "", err
	}
	// Run the command with stderr merged, then emit a sentinel + exit code.
	// cmd.exe uses different syntax for exit-code capture.
	var line string
	if s.isCmd {
		line = fmt.Sprintf("%s\r\necho %s%%ERRORLEVEL%%\r\n", command, shellSentinel)
	} else {
		line = fmt.Sprintf("{ %s ; } 2>&1 ; printf '\\n%s%%d\\n' \"$?\"\n", command, shellSentinel)
	}
	if _, err := io.WriteString(s.stdin, line); err != nil {
		s.kill()
		return "", err
	}

	type result struct {
		out  string
		code string
	}
	ch := make(chan result, 1)
	go func() {
		var b strings.Builder
		for {
			ln, err := s.stdout.ReadString('\n')
			if i := strings.Index(ln, shellSentinel); i >= 0 {
				code := strings.TrimSpace(ln[i+len(shellSentinel):])
				b.WriteString(ln[:i])
				ch <- result{out: b.String(), code: code}
				return
			}
			b.WriteString(ln)
			if err != nil {
				ch <- result{out: b.String(), code: "?"}
				return
			}
		}
	}()

	tmo := timeout
	if tmo <= 0 {
		tmo = s.effectiveTimeout()
	}
	select {
	case res := <-ch:
		out := strings.TrimRight(res.out, "\n")
		if res.code != "0" && res.code != "" && res.code != "?" {
			out += fmt.Sprintf("\n[exit %s]", res.code)
		}
		return truncate(out, 16000), nil
	case <-time.After(tmo):
		s.kill() // command is stuck; reset the session
		return fmt.Sprintf("[timed out after %s; shell session reset — use /timeout to raise the limit]", tmo), nil
	}
}

// kill terminates the session; it will be restarted on the next Run.
func (s *Shell) kill() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.alive = false
}

// Close shuts down the shell session.
func (s *Shell) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alive && s.stdin != nil {
		_, _ = io.WriteString(s.stdin, "exit\n")
		_ = s.stdin.Close()
	}
	s.kill()
}
