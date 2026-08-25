package main

import (
	"encoding/base64"
	"fmt"
	"strings"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/terminal"
)

// TerminalOpenOptions is the caller-supplied shape for opening a terminal.
// Rows/Cols are optional; zero values fall back to the package defaults.
// The shell is resolved server-side when Shell is empty.
type TerminalOpenOptions struct {
	Cwd   string `json:"cwd"`
	Shell string `json:"shell,omitempty"`
	Rows  uint16 `json:"rows,omitempty"`
	Cols  uint16 `json:"cols,omitempty"`
}

// TerminalHandle is what a new terminal returns to the caller.
type TerminalHandle struct {
	TerminalID string                  `json:"terminalID"`
	ThreadID   string                  `json:"threadID"`
	Summary    terminal.SessionSummary `json:"summary"`
}

// TerminalOutputEvent is the payload of the `terminal:output` event.
type TerminalOutputEvent struct {
	TerminalID string `json:"terminalID"`
	ThreadID   string `json:"threadID"`
	Sequence   uint64 `json:"sequence"`
	// Data is always base64-encoded because the underlying bytes can include
	// invalid UTF-8 sequences that would otherwise round-trip lossily
	// through JSON.
	Data string `json:"data"`
}

// TerminalExitEvent is the payload of the `terminal:exit` event.
type TerminalExitEvent struct {
	TerminalID string `json:"terminalID"`
	ThreadID   string `json:"threadID"`
	Code       int    `json:"code"`
	Reason     string `json:"reason"`
}

// TerminalReplay is the base64-encoded replay buffer plus the last output
// sequence included in that replay snapshot.
type TerminalReplay struct {
	Data            string `json:"data"`
	FromSequence    uint64 `json:"fromSequence"`
	ThroughSequence uint64 `json:"throughSequence"`
}

// OpenTerminal starts a new PTY-backed terminal session bound to the given
// thread.
func (a *App) OpenTerminal(threadID string, opts TerminalOpenOptions) (TerminalHandle, error) {
	if a.terminals == nil {
		return TerminalHandle{}, fmt.Errorf("terminal manager not initialized")
	}
	summary, err := a.terminals.Open(threadID, terminal.SessionOptions{
		Shell: opts.Shell,
		Cwd:   opts.Cwd,
		Rows:  opts.Rows,
		Cols:  opts.Cols,
	})
	if err != nil {
		return TerminalHandle{}, err
	}
	return TerminalHandle{
		TerminalID: summary.TerminalID,
		ThreadID:   summary.ThreadID,
		Summary:    summary,
	}, nil
}

// WriteTerminal writes base64-encoded data to the given terminal. The
// payload is base64 rather than a raw string so that non-UTF-8 byte
// sequences (control codes, mouse events, binary heredocs) round-trip
// losslessly from the frontend.
func (a *App) WriteTerminal(terminalID string, dataB64 string) error {
	if a.terminals == nil {
		return fmt.Errorf("terminal manager not initialized")
	}
	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return fmt.Errorf("terminal: decode write payload: %w", err)
	}
	return a.terminals.Write(terminalID, raw)
}

// ResizeTerminal forwards a winsize change to the PTY.
func (a *App) ResizeTerminal(terminalID string, rows uint16, cols uint16) error {
	if a.terminals == nil {
		return fmt.Errorf("terminal manager not initialized")
	}
	return a.terminals.Resize(terminalID, rows, cols)
}

// RefreshTerminal forces the terminal's child process to repaint by briefly
// nudging the PTY winsize and restoring it — the programmatic form of the manual
// resize users do to clear a glitched provider TUI frame (e.g. Claude Code's Ink
// renderer after a reflow desync). The visible grid size is unchanged.
func (a *App) RefreshTerminal(terminalID string) error {
	if a.terminals == nil {
		return fmt.Errorf("terminal manager not initialized")
	}
	return a.terminals.Refresh(terminalID)
}

// CloseTerminal kills the terminal's process group and removes the session.
func (a *App) CloseTerminal(terminalID string) error {
	if a.terminals == nil {
		return fmt.Errorf("terminal manager not initialized")
	}
	return a.terminals.Close(terminalID)
}

// ListTerminals returns a summary per active terminal for the given thread.
func (a *App) ListTerminals(threadID string) ([]terminal.SessionSummary, error) {
	if a.terminals == nil {
		return nil, fmt.Errorf("terminal manager not initialized")
	}
	return a.terminals.List(threadID), nil
}

// MoveThreadTerminals rekeys live terminal sessions from a placeholder thread
// id to the materialized thread id without restarting their PTYs.
func (a *App) MoveThreadTerminals(fromThreadID, toThreadID string) ([]terminal.SessionSummary, error) {
	if a.terminals == nil {
		return nil, fmt.Errorf("terminal manager not initialized")
	}
	if !isDraftPlaceholderThreadID(fromThreadID) {
		return nil, fmt.Errorf("terminal: source thread must be a draft placeholder")
	}
	if a.store == nil {
		return nil, fmt.Errorf("terminal: store unavailable")
	}
	if _, err := a.store.GetThread(toThreadID); err != nil {
		return nil, fmt.Errorf("terminal: resolve target thread %s: %w", toThreadID, err)
	}
	return a.terminals.MoveThread(fromThreadID, toThreadID)
}

// CloseThreadTerminals kills every live terminal session bound to a thread-like
// key. Placeholder draft ids use this to tear down ephemeral drawer terminals
// when their workspace context changes or the placeholder is replaced.
func (a *App) CloseThreadTerminals(threadID string) error {
	if a.terminals == nil {
		return fmt.Errorf("terminal manager not initialized")
	}
	if !isDraftPlaceholderThreadID(threadID) {
		return fmt.Errorf("terminal: thread must be a draft placeholder")
	}
	return a.terminals.CloseThread(threadID)
}

func isDraftPlaceholderThreadID(threadID string) bool {
	return strings.HasPrefix(strings.TrimSpace(threadID), "draft:")
}

// RestartTerminal kills the given terminal and spawns a fresh replacement
// with the same configuration.
func (a *App) RestartTerminal(terminalID string) (TerminalHandle, error) {
	if a.terminals == nil {
		return TerminalHandle{}, fmt.Errorf("terminal manager not initialized")
	}
	summary, err := a.terminals.Restart(terminalID)
	if err != nil {
		return TerminalHandle{}, err
	}
	return TerminalHandle{
		TerminalID: summary.TerminalID,
		ThreadID:   summary.ThreadID,
		Summary:    summary,
	}, nil
}

// GetTerminalReplay returns the base64-encoded replay buffer plus a sequence
// watermark. base64 keeps binary safety for non-UTF-8 bytes emitted by the
// shell.
func (a *App) GetTerminalReplay(terminalID string) (TerminalReplay, error) {
	if a.terminals == nil {
		return TerminalReplay{}, fmt.Errorf("terminal manager not initialized")
	}
	snapshot, err := a.terminals.ReplaySnapshot(terminalID)
	if err != nil {
		return TerminalReplay{}, err
	}
	return TerminalReplay{
		Data:            base64.StdEncoding.EncodeToString(snapshot.Data),
		FromSequence:    snapshot.FromSequence,
		ThroughSequence: snapshot.ThroughSequence,
	}, nil
}

// terminalOutputCallback emits a `terminal:output` event to the frontend.
func (a *App) terminalOutputCallback(threadID, terminalID string, sequence uint64, data []byte) {
	a.emit(eventchan.TerminalOutput, TerminalOutputEvent{
		TerminalID: terminalID,
		ThreadID:   threadID,
		Sequence:   sequence,
		Data:       base64.StdEncoding.EncodeToString(data),
	})
}

// terminalExitCallback emits a `terminal:exit` event to the frontend.
//
// Suppressed during shutdown: Manager.Shutdown SIGTERMs every PTY, which fires
// each session's exit callback. The frontend treats a real terminal exit as
// "this terminal is gone" and drops the thread from the sidebar (ctrl+D / last
// tab close). Terminal threads must instead PERSIST across restart, so we must
// not let the shutdown-time mass-kill reach the frontend as exits. shuttingDown
// is CAS'd true at the very top of Shutdown, before terminals close, so every
// shutdown-induced exit observes it.
func (a *App) terminalExitCallback(threadID, terminalID string, status terminal.ExitStatus) {
	if a.shuttingDown.Load() {
		return
	}
	a.emit(eventchan.TerminalExit, TerminalExitEvent{
		TerminalID: terminalID,
		ThreadID:   threadID,
		Code:       status.Code,
		Reason:     status.Reason,
	})
}
