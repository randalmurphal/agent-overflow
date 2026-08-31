package app

import (
	"encoding/base64"
	"fmt"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/terminal"
)

// app_claudetui_terminal.go exposes the take-control surface for a claude-tui
// session's PTY to the frontend. Unlike the app-owned terminal manager
// (app_terminal.go), this PTY lives inside the provider session's private
// terminal.Manager, so these RPCs reach it through sess.ClaudeTUI rather than
// a.terminals. Output is fanned out only while a take-control pane is attached;
// PTY exit is NOT a channel here — it rides the existing session-died rail the
// session already emits on onPTYExit, so the frontend hears about death once.
//
// Every method below steers a provider subprocess + its host PTY, so all are
// classified LocalOnly in internal/transport/internalmethods.go.

// ProviderTerminalHandle is what attaching to a provider PTY returns: the
// terminal id (for routing output + replay) plus its current summary (winsize)
// so the frontend can size an xterm to the last-drawn frame before writing the
// replay buffer.
type ProviderTerminalHandle struct {
	TerminalID string                  `json:"terminalID"`
	ThreadID   string                  `json:"threadID"`
	Summary    terminal.SessionSummary `json:"summary"`
}

// ProviderTerminalOutputEvent is the payload of the `provider:terminal_output`
// event: raw PTY bytes from a claude-tui session's terminal. Data is
// base64-encoded because terminal output carries control sequences that are not
// valid UTF-8 and would round-trip lossily through JSON (same rationale as
// TerminalOutputEvent).
type ProviderTerminalOutputEvent struct {
	TerminalID string `json:"terminalID"`
	ThreadID   string `json:"threadID"`
	Sequence   uint64 `json:"sequence"`
	Data       string `json:"data"`
}

// ProviderTerminalAttach arms raw-output fan-out for the take-control pane and
// returns the live terminal handle. The sink emits `provider:terminal_output`
// for every chunk; the terminal ring keeps buffering for replay regardless, so
// nothing is lost between attach and the frontend's first replay fetch (the
// frontend dedupes overlap by the replay watermark).
func (a *App) ProviderTerminalAttach(threadID string) (ProviderTerminalHandle, error) {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return ProviderTerminalHandle{}, err
	}
	summary, err := sess.AttachTerminal(func(terminalID string, seq uint64, data []byte) {
		a.emit(eventchan.ProviderTerminalOutput, ProviderTerminalOutputEvent{
			TerminalID: terminalID,
			ThreadID:   threadID,
			Sequence:   seq,
			Data:       base64.StdEncoding.EncodeToString(data),
		})
	})
	if err != nil {
		return ProviderTerminalHandle{}, err
	}
	return ProviderTerminalHandle{
		TerminalID: summary.TerminalID,
		ThreadID:   threadID,
		Summary:    summary,
	}, nil
}

// ProviderTerminalDetach stops output fan-out and releases the take-control
// lease for the session's terminal. Called when a take-control pane closes.
func (a *App) ProviderTerminalDetach(threadID string) error {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return err
	}
	sess.DetachTerminal()
	return nil
}

// ProviderTerminalReplay returns the base64 replay buffer plus the output
// sequence watermark so a freshly mounted take-control xterm renders the
// provider's last frame. Mirrors GetTerminalReplay for the provider PTY.
func (a *App) ProviderTerminalReplay(threadID string) (TerminalReplay, error) {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return TerminalReplay{}, err
	}
	snapshot, err := sess.TerminalReplaySnapshot()
	if err != nil {
		return TerminalReplay{}, err
	}
	return TerminalReplay{
		Data:            base64.StdEncoding.EncodeToString(snapshot.Data),
		FromSequence:    snapshot.FromSequence,
		ThroughSequence: snapshot.ThroughSequence,
	}, nil
}

// ProviderTerminalInput delivers base64-encoded human keystrokes to the PTY.
// The session refuses input unless the take-control lease is held, so a
// read-only attach cannot inject keystrokes even if a caller tries.
func (a *App) ProviderTerminalInput(threadID string, dataB64 string) error {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return fmt.Errorf("provider terminal: decode input payload: %w", err)
	}
	return sess.WriteInput(raw)
}

// ProviderTerminalResize forwards a winsize change to the PTY so the TUI
// repaints at the take-control pane's width.
func (a *App) ProviderTerminalResize(threadID string, rows uint16, cols uint16) error {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return err
	}
	return sess.ResizeTerminal(rows, cols)
}

// ProviderTerminalRefresh forces the TUI to repaint a glitched frame via a
// winsize nudge (the visible grid is unchanged).
func (a *App) ProviderTerminalRefresh(threadID string) error {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return err
	}
	return sess.RefreshTerminal()
}

// ProviderTerminalSetControl acquires (control=true) or releases the human
// take-control input lease. While a human holds it, AO's programmatic Send is
// refused so the two input drivers never interleave.
func (a *App) ProviderTerminalSetControl(threadID string, control bool) error {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return err
	}
	sess.SetTakeControl(control)
	return nil
}

// claudetuiSession resolves the live claude-tui session for a thread, erroring
// if the app is shutting down, no session exists, or the thread is a different
// provider. Mirrors the StopClaudeTask resolution shape.
func (a *App) claudetuiSession(threadID string) (*claudetui.Session, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return nil, fmt.Errorf("provider terminal: no active session for thread %s", threadID)
	}
	if sess.ClaudeTUI == nil {
		return nil, fmt.Errorf("provider terminal: thread %s is not a claude-tui session", threadID)
	}
	return sess.ClaudeTUI, nil
}
