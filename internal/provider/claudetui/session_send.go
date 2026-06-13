package claudetui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/provider"
)

// session_send.go is the send half of the Session: driving a user turn into the
// real TUI composer by writing keystrokes to the PTY. It owns the composer/send
// keystroke contract (bracketed paste, composer-clear, submit) and the cold-start
// readiness gate that keeps the first turn from being swallowed. The rest of the
// Session (lifecycle, parser feed, PTY callbacks) lives in session.go.

const (
	// bracketedPaste* frame composer input so multi-line content lands as one
	// block instead of submitting on each embedded newline. The TUI enables
	// bracketed-paste mode; we wrap the payload in the same markers a real
	// terminal paste uses.
	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
	// composerSettle gives the TUI a beat to ingest the pasted block before the
	// submit key, so the Enter isn't swallowed by the paste handler. The spike
	// found the Ink composer needs a short gap here.
	composerSettle = 60 * time.Millisecond
	// submitKey is the raw byte the TUI reads as "send" (interruptKey, its "abort
	// this turn" twin, lives with Interrupt in session.go).
	submitKey = "\r"
	// composerClearKey is Ctrl-U — a readline line-kill. Send presses it
	// composerClearKeystrokes times before pasting to empty any prompt the TUI
	// restored to the composer on a prior think-only Esc-revert (the TUI's native
	// revert puts the just-sent prompt back; LIVE in
	// spike/claude-mitm/probe_hook_escrevert.py). Without this, a re-send after a
	// revert FUSES the restored leftover with the new paste. Ctrl-U clears one
	// composer line, a multi-line paste collapses to a single placeholder chip, so
	// the composer is never more than a few lines and 16 covers it with margin;
	// excess Ctrl-U on an already-empty composer is a no-op (LIVE-characterized in
	// spike/claude-mitm/probe_composer_clear.py on 2.1.170), so it's harmless in
	// the common already-empty case.
	composerClearKey        = "\x15"
	composerClearKeystrokes = 16
	// composerReady* gate the FIRST Send until the freshly-launched TUI has
	// reached steady-state input reading. Before that, claude hasn't begun
	// draining stdin, so our submit CR is read in the same chunk as the paste and
	// swallowed — the turn types into the composer but never sends (the user hit
	// this on the opening message; the second message, sent warm, worked).
	// Readiness = the init output burst has landed (>= MinBytes) AND the PTY
	// stream has since gone idle (>= Quiet), i.e. claude is parked reading.
	// LIVE-validated in spike/claude-mitm/probe_cold_submit.py: send-immediately
	// 0/3 submitted, idle-gate 3/3, and a two-message run reproduced the exact
	// "first sticks, second sends" report. Latched once; warm sends skip the wait.
	// Poll is the gate's check cadence; Timeout is a bounded fallback so a
	// pathological never-idle stream degrades to a re-sendable miss, never a hang.
	composerReadyMinBytes = 512
	composerReadyQuiet    = 400 * time.Millisecond
	composerReadyPoll     = 40 * time.Millisecond
	composerReadyTimeout  = 8 * time.Second
)

// Send delivers a user turn by pasting the content into the TUI composer and
// pressing Enter. The interactive provider has no stdin image path, so image
// attachments are rejected rather than silently dropped.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	s.mu.Lock()
	held := s.controlHeld
	s.mu.Unlock()
	if held {
		return fmt.Errorf("claudetui: a human holds take-control of the terminal; release control to send")
	}
	if len(opts.Attachments) > 0 {
		return fmt.Errorf("claudetui: image attachments are not supported on the interactive provider")
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("claudetui: user message requires text content")
	}
	// Cold-start guard: don't write until the freshly-launched TUI is parked
	// reading input, or the submit CR is swallowed with the paste and the turn
	// never sends (see composerReady*). Latched, so only the first send waits.
	if err := s.awaitComposerReady(ctx); err != nil {
		return err
	}
	// The composer-clear empties any prompt the TUI restored on a prior Esc-revert
	// (see composerClearKey); a settle between each block gives the Ink composer a
	// beat so the paste isn't interleaved with the line-kill and the Enter isn't
	// swallowed by the paste handler.
	clear, paste, submit := sendKeystrokes(content)
	if err := s.writePTY(clear); err != nil {
		return err
	}
	if err := s.settleComposer(ctx); err != nil {
		return err
	}
	if err := s.writePTY(paste); err != nil {
		return err
	}
	if err := s.settleComposer(ctx); err != nil {
		return err
	}
	if err := s.writePTY(submit); err != nil {
		return err
	}
	// Record the send so the reconstructor confirms it with a user{isReplay} echo
	// on the next main request, consuming triage's pending-send FIFO. Direct sends
	// carry an app-minted UserMessageUUID; queued sends (flush path) supply none,
	// so mint a stable id here — persistDeferredUserText needs a non-empty
	// provider_item_id or the queued row never lands. Pushed only after submit
	// succeeds so a write failure (which the caller turns into a pending-send
	// clear) leaves the echo FIFO aligned with the pending-send FIFO.
	echoUUID := opts.UserMessageUUID
	if echoUUID == "" {
		echoUUID = uuid.NewString()
	}
	s.queueUserEcho(content, echoUUID)
	return nil
}

// sendKeystrokes builds the ordered keystroke blocks Send writes to drive a user
// turn into the TUI composer: clear the composer, paste the content, submit.
//   - clear is composerClearKeystrokes line-kills so a prompt the TUI restored to
//     the composer on a prior Esc-revert can't fuse with this paste.
//   - paste wraps the content in bracketed-paste markers, stripping any stray
//     terminator first so user content can't close the paste early and have its
//     tail interpreted as raw keystrokes.
func sendKeystrokes(content string) (clear, paste, submit []byte) {
	clear = []byte(strings.Repeat(composerClearKey, composerClearKeystrokes))
	safe := strings.ReplaceAll(content, bracketedPasteEnd, "")
	paste = []byte(bracketedPasteStart + safe + bracketedPasteEnd)
	submit = []byte(submitKey)
	return clear, paste, submit
}

// settleComposer gives the Ink TUI a beat to ingest a block of input (a clear or
// a paste) before the next keystroke, so a following Enter isn't swallowed by
// the paste handler and a paste isn't interleaved with the preceding line-kill.
// Honors ctx cancellation so a Stop mid-send returns promptly.
func (s *Session) settleComposer(ctx context.Context) error {
	select {
	case <-time.After(composerSettle):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ptyReadyForSend reports whether the launched TUI has reached steady-state input
// reading: its init output burst has landed (bytes) and the PTY stream has since
// been idle long enough (quiet) that claude is parked reading rather than still
// painting startup. See composerReady* for why the first send must wait for this.
// On a brand-new session lastPTYAt is the zero Time, so the caller's quiet is
// effectively infinite; the bytes >= MinBytes half is what holds the gate shut
// until the init burst actually lands.
func ptyReadyForSend(bytes int, quiet time.Duration) bool {
	return bytes >= composerReadyMinBytes && quiet >= composerReadyQuiet
}

// awaitComposerReady blocks until the just-launched TUI is parked reading input,
// so the first Send's submit CR is read as a distinct keypress instead of being
// swallowed with the paste (the cold-start bug). The result is latched under mu:
// only the first cold send pays the wait; every later send returns immediately.
// On the bounded timeout it proceeds anyway — a swallowed re-sendable submit beats
// hanging the send — and that timeout is the live safety net, since production
// callers pass context.Background(); the ctx branch is for tests / a future
// cancellable caller.
func (s *Session) awaitComposerReady(ctx context.Context) error {
	s.mu.Lock()
	ready := s.composerReady
	s.mu.Unlock()
	if ready {
		return nil
	}
	poll, timeout := s.readyPoll, s.readyTimeout
	if poll <= 0 {
		poll = composerReadyPoll
	}
	if timeout <= 0 {
		timeout = composerReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		ok := ptyReadyForSend(s.ptyBytes, time.Since(s.lastPTYAt))
		timedOut := time.Now().After(deadline)
		if ok || timedOut {
			s.composerReady = true
			s.mu.Unlock()
			if timedOut && !ok {
				s.logf("claudetui: composer-ready gate timed out after %s; sending anyway", timeout)
			}
			return nil
		}
		s.mu.Unlock()
		select {
		case <-time.After(poll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
