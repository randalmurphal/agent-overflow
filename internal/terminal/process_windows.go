//go:build windows

// Windows stub for the terminal package. The Windows binary
// (cmd/agent-overflow-windows) is a launcher around wsl.exe — it never
// constructs a terminal Manager and therefore never calls into this
// stub. It exists purely to satisfy the GOOS=windows ./... cross-
// compile so the wsllauncher and the cmd/agent-overflow-windows main
// can be built on Windows.
//
// Every function returns a not-supported error or zero value so a
// regression that accidentally wires the terminal manager into the
// Windows launcher fails loudly at runtime instead of silently no-op'ing.
package terminal

import (
	"errors"
	"syscall"
	"time"
)

// errWindows is the sentinel returned by every method on the Windows
// stub. The Windows binary never reaches these — if it does, it's a
// wiring bug worth surfacing.
var errWindows = errors.New("terminal: not supported on Windows (use the WSL-side Linux backend)")

const (
	defaultRows uint16 = 24
	defaultCols uint16 = 80
	killGrace          = 500 * time.Millisecond
)

// ProcessConfig mirrors the POSIX type so callers can still construct
// the value at compile time.
type ProcessConfig struct {
	Shell string
	Args  []string
	Cwd   string
	Env   []string
	Rows  uint16
	Cols  uint16
}

// ExitStatus mirrors the POSIX type. The Signal field has no meaning
// on Windows but stays in the shape so cross-platform call sites
// don't need build tags of their own.
type ExitStatus struct {
	Code   int
	Signal syscall.Signal
	Reason string
}

// Process is a placeholder; every method errors.
type Process struct{}

// Start always errors on Windows.
func Start(cfg ProcessConfig) (*Process, error) {
	_ = cfg
	return nil, errWindows
}

// Output returns a closed channel so callers don't block.
func (p *Process) Output() <-chan []byte {
	ch := make(chan []byte)
	close(ch)
	return ch
}

// Done returns a closed channel so callers don't block.
func (p *Process) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// ExitStatus returns a "not supported" sentinel.
func (p *Process) ExitStatus() ExitStatus {
	return ExitStatus{Code: -1, Reason: "windows-stub"}
}

// PID returns 0 because the stub never starts a real child.
func (p *Process) PID() int { return 0 }

// Write errors out — no PTY exists to write to.
func (p *Process) Write(data []byte) error {
	_ = data
	return errWindows
}

// Resize errors out — no PTY exists to resize.
func (p *Process) Resize(rows, cols uint16) error {
	_ = rows
	_ = cols
	return errWindows
}

// Refresh errors out — no PTY exists to nudge.
func (p *Process) Refresh(rows, cols uint16) error {
	_ = rows
	_ = cols
	return errWindows
}

// Kill is a no-op so Manager.Shutdown stays idempotent.
func (p *Process) Kill() error { return nil }

// Close is a no-op so Manager.Shutdown stays idempotent.
func (p *Process) Close() error { return nil }
