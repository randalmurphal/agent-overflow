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
//
// Attaching is REFCOUNTED and the lease is per attachment. Two panes on one
// session is an ordinary state — a second window, a phone beside the desktop,
// a transport reconnect that has not finished tearing the old socket down —
// and the app mints one attachment per connection, so nothing a second viewer
// does can strip the first viewer's output or its keyboard. Everything a
// client armed is released through its own *TerminalAttachment, which is what
// lets a dead WebSocket give the lease back on the client's behalf.

// TerminalAttachment is ONE client's claim on the session's PTY mirror:
// membership in the fan-out refcount, plus the option to hold the
// single-holder input lease.
//
// Every method is safe on an attachment that has already been released,
// because two things release it and they cannot order themselves against each
// other: the client's own detach, and its connection's teardown.
type TerminalAttachment struct {
	s *Session
}

// AttachTerminal arms raw-output fan-out to sink and returns this client's
// attachment plus the live terminal's current summary (terminalID + winsize)
// so the frontend can size an xterm to the PTY's last-drawn frame before
// writing the replay. The terminal ring buffers output regardless of attach
// state, so a session nobody is attached to loses nothing; sink only starts
// the live tee.
//
// A second attach ADDS a claim rather than replacing the first. Release the
// returned attachment to give the claim back.
func (s *Session) AttachTerminal(sink func(terminalID string, sequence uint64, data []byte)) (*TerminalAttachment, terminal.SessionSummary, error) {
	if sink == nil {
		return nil, terminal.SessionSummary{}, fmt.Errorf("claudetui: attach requires an output sink")
	}
	s.mu.Lock()
	term, terminalID := s.term, s.terminalID
	s.mu.Unlock()
	if term == nil || terminalID == "" {
		return nil, terminal.SessionSummary{}, fmt.Errorf("claudetui: no live terminal to attach")
	}
	// Summary first: a failure here must leave no claim behind, and arming the
	// tee before the caller has a handle to release is how one would.
	summary, err := term.Summary(terminalID)
	if err != nil {
		return nil, terminal.SessionSummary{}, err
	}
	return s.addAttachment(sink), summary, nil
}

// addAttachment joins the fan-out set, installing the live tee when this is
// the first claim. Split out of AttachTerminal so the refcount and lease rules
// are exercisable without a live PTY.
func (s *Session) addAttachment(sink func(terminalID string, sequence uint64, data []byte)) *TerminalAttachment {
	attachment := &TerminalAttachment{s: s}
	s.mu.Lock()
	if s.attachments == nil {
		s.attachments = make(map[*TerminalAttachment]struct{}, 1)
	}
	s.attachments[attachment] = struct{}{}
	if s.sink == nil {
		s.sink = sink
	}
	s.mu.Unlock()
	return attachment
}

// Release drops this claim: it leaves the fan-out refcount and gives the input
// lease back if this attachment held it. The live tee is torn down only when
// the last claim goes, so one pane closing never blinds another. Idempotent
// and nil-safe.
func (a *TerminalAttachment) Release() {
	if a == nil {
		return
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, live := s.attachments[a]; !live {
		return
	}
	delete(s.attachments, a)
	if s.controlHolder == a {
		s.controlHolder = nil
	}
	if len(s.attachments) == 0 {
		s.sink = nil
	}
}

// SetControl acquires (hold=true) or releases this attachment's human input
// lease. While a lease is held, WriteInput is accepted from its holder and
// Send (AO's programmatic turn) is refused, so the two input drivers never
// interleave keystrokes into the TUI composer.
//
// Acquiring while ANOTHER attachment holds the lease is refused rather than
// taken: seizing it would put two humans in one composer, which is the same
// interleaving the lease exists to prevent. Releasing when this attachment is
// not the holder is a no-op and not an error, because a pane unmounting and
// its socket dying both run it.
func (a *TerminalAttachment) SetControl(hold bool) error {
	if a == nil {
		return fmt.Errorf("claudetui: take-control needs an attached terminal")
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, live := s.attachments[a]; !live {
		if !hold {
			return nil
		}
		return fmt.Errorf("claudetui: this client is no longer attached to the terminal")
	}
	if !hold {
		if s.controlHolder == a {
			s.controlHolder = nil
		}
		return nil
	}
	if s.controlHolder != nil && s.controlHolder != a {
		return fmt.Errorf("claudetui: another client holds take-control of this terminal")
	}
	s.controlHolder = a
	return nil
}

// HoldsControl reports whether THIS attachment holds the input lease.
func (a *TerminalAttachment) HoldsControl() bool {
	if a == nil {
		return false
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlHolder == a
}

// WriteInput delivers a human keystroke to the PTY. Refused unless THIS
// attachment holds the take-control lease, so neither a read-only attach nor a
// second pane watching over the holder's shoulder can inject input.
func (a *TerminalAttachment) WriteInput(data []byte) error {
	if a == nil {
		return fmt.Errorf("claudetui: take-control not held; acquire control before sending input")
	}
	s := a.s
	s.mu.Lock()
	held := s.controlHolder == a
	s.mu.Unlock()
	if !held {
		return fmt.Errorf("claudetui: take-control not held; acquire control before sending input")
	}
	return s.writePTY(data)
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

// HasTakeControl reports whether ANY attachment currently holds the input
// lease. Session-wide, because that is the question Send asks.
func (s *Session) HasTakeControl() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlHolder != nil
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
