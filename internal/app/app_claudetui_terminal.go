package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/transport"
)

// app_claudetui_terminal.go exposes the take-control surface for a claude-tui
// session's PTY to the frontend. Unlike the app-owned terminal manager
// (app_terminal.go), this PTY lives inside the provider session's private
// terminal.Manager, so these RPCs reach it through sess.ClaudeTUI rather than
// a.terminals. Output is fanned out only while a take-control pane is attached;
// PTY exit is NOT a channel here — it rides the existing session-died rail the
// session already emits on onPTYExit, so the frontend hears about death once.
//
// Every method below steers a provider subprocess + its host PTY, so all
// carry //ao:scope terminal:operate.
//
// An attachment belongs to the CONNECTION that made it. That is what makes the
// take-control lease survivable: a socket that dies mid-take-control releases
// its own claim through transport.ConnState cleanup, where a session-wide
// boolean would have stayed held and refused every Send on the thread until the
// session restarted. It is also what keeps two clients apart — a second pane
// attaching takes nothing from the first, and detaching gives back only its
// own. An in-process caller (a saga, a test) has no ConnState and gets no
// safety net; it owes the explicit Detach it always did.

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

// providerTerminalKey addresses one caller's claim on one thread's PTY. The
// connection is half the key because the claim is per connection; a nil one is
// the honest key for every in-process caller, which shares a single claim per
// thread exactly as the pre-connection code did.
type providerTerminalKey struct {
	conn     *transport.ConnState
	threadID string
}

// providerTerminalAttachments is the App's map from a caller to the claudetui
// attachment it armed. Zero value ready; entries are pointer-sized and are
// dropped by the caller's detach or by its connection's teardown.
type providerTerminalAttachments struct {
	mu       sync.Mutex
	byCaller map[providerTerminalKey]*claudetui.TerminalAttachment
	// armed holds every connection that has a cleanup registered with it.
	// One cleanup per connection for its lifetime, releasing whatever claims
	// it holds when it runs: registering one per attach would leave a pane
	// that remounts across its socket's life holding a closure per remount.
	armed map[*transport.ConnState]struct{}
}

// put stores this caller's attachment, returning the one it displaced so the
// caller can release it. A displaced claim means the same connection attached
// twice to one thread without detaching (a pane remounting across a reconnect),
// and leaving the old one armed would hold its lease forever.
func (p *providerTerminalAttachments) put(key providerTerminalKey, attachment *claudetui.TerminalAttachment) *claudetui.TerminalAttachment {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byCaller == nil {
		p.byCaller = make(map[providerTerminalKey]*claudetui.TerminalAttachment, 1)
	}
	prior := p.byCaller[key]
	p.byCaller[key] = attachment
	return prior
}

// arm reports whether conn still needs a cleanup registered, marking it armed
// when so. Marking and asking are one step so two attaches racing on one
// connection register exactly one cleanup between them.
func (p *providerTerminalAttachments) arm(conn *transport.ConnState) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, done := p.armed[conn]; done {
		return false
	}
	if p.armed == nil {
		p.armed = make(map[*transport.ConnState]struct{}, 1)
	}
	p.armed[conn] = struct{}{}
	return true
}

// get returns this caller's live attachment, or nil when it has none.
func (p *providerTerminalAttachments) get(key providerTerminalKey) *claudetui.TerminalAttachment {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byCaller[key]
}

// take removes this caller's attachment and hands it back, or nil when there
// was none. Removal and retrieval are one step so two racing releases (the
// client's detach and its socket's teardown) cannot both release it.
func (p *providerTerminalAttachments) take(key providerTerminalKey) *claudetui.TerminalAttachment {
	p.mu.Lock()
	defer p.mu.Unlock()
	attachment, ok := p.byCaller[key]
	if !ok {
		return nil
	}
	delete(p.byCaller, key)
	return attachment
}

// takeAll removes every claim conn holds and forgets that it was armed, so a
// connection object reused after its cleanup ran (tests) can arm again.
func (p *providerTerminalAttachments) takeAll(conn *transport.ConnState) []*claudetui.TerminalAttachment {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.armed, conn)
	var taken []*claudetui.TerminalAttachment
	for key, attachment := range p.byCaller {
		if key.conn == conn {
			delete(p.byCaller, key)
			taken = append(taken, attachment)
		}
	}
	return taken
}

// releaseProviderTerminal drops the caller's claim on a thread's PTY: it leaves
// the session's fan-out refcount and gives back the take-control lease if this
// caller held it. Idempotent — the client's own detach and its connection's
// cleanup both run it, in whichever order they happen.
func (a *App) releaseProviderTerminal(key providerTerminalKey) {
	a.providerTerminals.take(key).Release()
}

// releaseProviderTerminalsFor is the connection's cleanup: every claim the
// dead socket still holds, on every thread, goes back at once.
func (a *App) releaseProviderTerminalsFor(conn *transport.ConnState) {
	for _, attachment := range a.providerTerminals.takeAll(conn) {
		attachment.Release()
	}
}

// ProviderTerminalAttach arms raw-output fan-out for the take-control pane and
// returns the live terminal handle. The sink emits `provider:terminal_output`
// for every chunk; the terminal ring keeps buffering for replay regardless, so
// nothing is lost between attach and the frontend's first replay fetch (the
// frontend dedupes overlap by the replay watermark).
//
// The claim is released when this connection drops, so a client that dies
// mid-take-control does not leave the input lease held. The frontend SHOULD
// still call ProviderTerminalDetach on unmount; the connection-tied cleanup is
// the safety net for unclean disconnects.
//
//ao:scope terminal:operate
func (a *App) ProviderTerminalAttach(ctx context.Context, threadID string) (ProviderTerminalHandle, error) {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return ProviderTerminalHandle{}, err
	}
	attachment, summary, err := sess.AttachTerminal(func(terminalID string, seq uint64, data []byte) {
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
	state := transport.ConnStateFromContext(ctx)
	key := providerTerminalKey{conn: state, threadID: threadID}
	a.providerTerminals.put(key, attachment).Release()
	if state != nil && a.providerTerminals.arm(state) {
		// A false return means the connection is already tearing down — the
		// safety net it would have provided is gone, so release now rather
		// than hold a lease until the session dies.
		if !state.RegisterCleanup(func() { a.releaseProviderTerminalsFor(state) }) {
			a.releaseProviderTerminalsFor(state)
			return ProviderTerminalHandle{}, fmt.Errorf("provider terminal: connection closing")
		}
	}
	return ProviderTerminalHandle{
		TerminalID: summary.TerminalID,
		ThreadID:   threadID,
		Summary:    summary,
	}, nil
}

// ProviderTerminalDetach stops output fan-out for this caller and releases the
// take-control lease if it held it. Called when a take-control pane closes.
// Idempotent, and never an error: the connection-cleanup safety net may have
// released the same claim first, and a caller that already has nothing to give
// back has nothing to be told.
//
//ao:scope terminal:operate
func (a *App) ProviderTerminalDetach(ctx context.Context, threadID string) error {
	a.releaseProviderTerminal(providerTerminalKey{
		conn:     transport.ConnStateFromContext(ctx),
		threadID: threadID,
	})
	return nil
}

// ProviderTerminalReplay returns the base64 replay buffer plus the output
// sequence watermark so a freshly mounted take-control xterm renders the
// provider's last frame. Mirrors GetTerminalReplay for the provider PTY.
//
//ao:scope terminal:operate
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
// The session refuses input unless THIS caller's attachment holds the
// take-control lease, so neither a read-only attach nor a second pane watching
// over the holder's shoulder can inject keystrokes.
//
//ao:scope terminal:operate
func (a *App) ProviderTerminalInput(ctx context.Context, threadID string, dataB64 string) error {
	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return fmt.Errorf("provider terminal: decode input payload: %w", err)
	}
	return a.providerTerminals.get(providerTerminalKey{
		conn:     transport.ConnStateFromContext(ctx),
		threadID: threadID,
	}).WriteInput(raw)
}

// ProviderTerminalResize forwards a winsize change to the PTY so the TUI
// repaints at the take-control pane's width.
//
//ao:scope terminal:operate
func (a *App) ProviderTerminalResize(threadID string, rows uint16, cols uint16) error {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return err
	}
	return sess.ResizeTerminal(rows, cols)
}

// ProviderTerminalRefresh forces the TUI to repaint a glitched frame via a
// winsize nudge (the visible grid is unchanged).
//
//ao:scope terminal:operate
func (a *App) ProviderTerminalRefresh(threadID string) error {
	sess, err := a.claudetuiSession(threadID)
	if err != nil {
		return err
	}
	return sess.RefreshTerminal()
}

// ProviderTerminalSetControl acquires (control=true) or releases the human
// take-control input lease for THIS caller's attachment. While a human holds
// it, AO's programmatic Send is refused so the two input drivers never
// interleave, and another client's acquire is refused rather than taking the
// keyboard away.
//
// Releasing without an attachment is a no-op: a pane unmounting after its
// session died has nothing left to give back, and reporting that as a failure
// would leave the UI showing a lease nobody holds.
//
//ao:scope terminal:operate
func (a *App) ProviderTerminalSetControl(ctx context.Context, threadID string, control bool) error {
	attachment := a.providerTerminals.get(providerTerminalKey{
		conn:     transport.ConnStateFromContext(ctx),
		threadID: threadID,
	})
	if attachment == nil {
		if !control {
			return nil
		}
		return fmt.Errorf("provider terminal: attach to thread %s before taking control", threadID)
	}
	return attachment.SetControl(control)
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
