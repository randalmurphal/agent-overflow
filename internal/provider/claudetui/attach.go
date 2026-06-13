package claudetui

import (
	"fmt"

	"agent-overflow/internal/terminal"
)

// attach.go is the take-control seam: a read-only mirror of the interactive
// claude PTY (output fan-out + replay) plus a single-holder input lease that
// lets a human drive the real terminal. The PTY itself lives in the session's
// private terminal.Manager (session.go); these methods are what the app's
// ProviderTerminal* RPCs reach through. The package never emits UI events or
// resolves answers from the PTY — it only hands raw bytes to the app, which
// owns the frontend fan-out. See docs/architecture/claude-tui-provider.md
// §Attach & take-control.

// AttachTerminal arms raw-output fan-out to sink and returns the live terminal's
// current summary (terminalID + winsize) so the frontend can size an xterm to
// the PTY's last-drawn frame before writing the replay. The terminal ring
// buffers output regardless of attach state, so a detached session loses
// nothing; sink only starts the live tee. Idempotent — a second attach replaces
// the sink, which is exactly what a transport reconnect needs.
func (s *Session) AttachTerminal(sink func(terminalID string, sequence uint64, data []byte)) (terminal.SessionSummary, error) {
	s.mu.Lock()
	term, terminalID := s.term, s.terminalID
	s.sink = sink
	s.mu.Unlock()
	if term == nil || terminalID == "" {
		return terminal.SessionSummary{}, fmt.Errorf("claudetui: no live terminal to attach")
	}
	return term.Summary(terminalID)
}

// DetachTerminal stops output fan-out and releases any take-control lease. The
// PTY keeps running; the human just stopped watching and driving it.
func (s *Session) DetachTerminal() {
	s.mu.Lock()
	s.sink = nil
	s.controlHeld = false
	s.mu.Unlock()
}

// TerminalReplaySnapshot returns the PTY's replay buffer plus the output
// sequence watermark so a freshly mounted xterm renders the provider's last
// frame before live output resumes. The watermark lets the frontend drop any
// live chunk already covered by the replay.
func (s *Session) TerminalReplaySnapshot() (terminal.ReplaySnapshot, error) {
	s.mu.Lock()
	term, terminalID := s.term, s.terminalID
	s.mu.Unlock()
	if term == nil || terminalID == "" {
		return terminal.ReplaySnapshot{}, fmt.Errorf("claudetui: no live terminal to replay")
	}
	return term.ReplaySnapshot(terminalID)
}

// SetTakeControl acquires (hold=true) or releases the human input lease. While
// held, WriteInput is accepted and Send (AO's programmatic turn) is refused, so
// the two input drivers never interleave keystrokes into the TUI composer.
func (s *Session) SetTakeControl(hold bool) {
	s.mu.Lock()
	s.controlHeld = hold
	s.mu.Unlock()
}

// HasTakeControl reports whether a human currently holds the input lease.
func (s *Session) HasTakeControl() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlHeld
}

// WriteInput delivers a human keystroke to the PTY. Refused unless the
// take-control lease is held, so a read-only attach can never inject input.
func (s *Session) WriteInput(data []byte) error {
	s.mu.Lock()
	held := s.controlHeld
	s.mu.Unlock()
	if !held {
		return fmt.Errorf("claudetui: take-control not held; acquire control before sending input")
	}
	return s.writePTY(data)
}

// ResizeTerminal forwards a winsize change to the PTY. When take-control sizes
// the xterm to its pane, this resizes the real terminal so the TUI repaints at
// that width. Reconstruction is wire+hook sourced, so PTY width never affects
// the normalized event stream.
func (s *Session) ResizeTerminal(rows, cols uint16) error {
	s.mu.Lock()
	term, terminalID := s.term, s.terminalID
	s.mu.Unlock()
	if term == nil || terminalID == "" {
		return fmt.Errorf("claudetui: no live terminal to resize")
	}
	return term.Resize(terminalID, rows, cols)
}

// RefreshTerminal nudges the PTY winsize to force the TUI to repaint a glitched
// frame — the take-control equivalent of the drawer's refresh affordance, and
// what the design routes a glitched Ink frame to instead of parsing the TUI.
func (s *Session) RefreshTerminal() error {
	s.mu.Lock()
	term, terminalID := s.term, s.terminalID
	s.mu.Unlock()
	if term == nil || terminalID == "" {
		return fmt.Errorf("claudetui: no live terminal to refresh")
	}
	return term.Refresh(terminalID)
}

// TerminalID returns the live terminal's id (empty before launch / after close).
func (s *Session) TerminalID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalID
}
