//go:build windows

package agent

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput = 0x0001
	enableLineInput      = 0x0002
	enableEchoInput      = 0x0004
	enableVTInput        = 0x0200
	enableVTOutput       = 0x0004 // ENABLE_VIRTUAL_TERMINAL_PROCESSING (stdout)
)

// stdinRawMode puts the console into raw mode for character-by-character input
// and returns a restore func. ok is false when stdin is not a console (e.g.
// piped input), in which case the caller falls back to cooked line reading.
func stdinRawMode() (func(), bool) {
	inFd := os.Stdin.Fd()
	var orig uint32
	r, _, _ := procGetConsoleMode.Call(inFd, uintptr(unsafe.Pointer(&orig)))
	if r == 0 {
		return nil, false // not a console
	}
	raw := orig &^ uint32(enableLineInput|enableEchoInput|enableProcessedInput)
	raw |= enableVTInput
	if rr, _, _ := procSetConsoleMode.Call(inFd, uintptr(raw)); rr == 0 {
		return nil, false
	}

	// Ensure stdout interprets our ANSI sequences.
	outFd := os.Stdout.Fd()
	var outOrig uint32
	procGetConsoleMode.Call(outFd, uintptr(unsafe.Pointer(&outOrig)))
	procSetConsoleMode.Call(outFd, uintptr(outOrig|enableVTOutput))

	return func() { procSetConsoleMode.Call(inFd, uintptr(orig)) }, true
}

// consoleWidth returns the console width in columns, or 100 if unknown.
func consoleWidth() int {
	type coord struct{ x, y int16 }
	type smallRect struct{ left, top, right, bottom int16 }
	type info struct {
		size       coord
		cursor     coord
		attrs      uint16
		window     smallRect
		maxWinSize coord
	}
	var ci info
	r, _, _ := procGetConsoleScreenBufferInfo.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&ci)))
	if r == 0 {
		return 100
	}
	w := int(ci.window.right - ci.window.left + 1)
	if w < 40 {
		return 100
	}
	return w
}
