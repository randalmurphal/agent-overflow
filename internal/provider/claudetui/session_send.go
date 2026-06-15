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
	// claudePasteCompletionWindow mirrors Claude's PASTE_COMPLETION_TIMEOUT_MS
	// (src/hooks/usePasteHandler.ts): the input-quiet gap after which the Ink
	// composer treats a bracketed paste as complete. A second paste arriving
	// inside this window merges into the first's chunk buffer — the two payloads
	// concatenate, so an image PATH fuses with the text and neither parses as
	// intended. Source is 2.1.88; cross-checked against the installed 2.1.170 in
	// spike/claude-mitm/probe_hook_attach.py.
	claudePasteCompletionWindow = 100 * time.Millisecond
	// pasteSettle is the gap Send leaves between two CONSECUTIVE bracketed pastes
	// (the image paste and the text paste of one send), sized above the completion
	// window with margin for binary drift and scheduling jitter so each lands as
	// its own composer block. composerSettle stays correct everywhere a
	// paste-merge is impossible: clear→firstpaste (the clear is Ctrl-U keystrokes,
	// not a paste) and lastpaste→submit (the submit CR is deferred by the paste
	// handler's pastePendingRef, not merged into the paste).
	pasteSettle = claudePasteCompletionWindow + 100*time.Millisecond
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
// pressing Enter. Image attachments are pasted as their absolute on-disk file
// PATHS: the real TUI's paste handler reads each path into an image content
// block (Strategy A, LIVE in spike/claude-mitm/probe_hook_attach.py), so an
// image send needs no stdin image protocol the interactive provider lacks. The
// app layer resolves each attachment to a Path for this provider — see
// resolveSendMessageAttachments.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	s.mu.Lock()
	held := s.controlHeld
	s.mu.Unlock()
	if held {
		return fmt.Errorf("claudetui: a human holds take-control of the terminal; release control to send")
	}
	imagePaths, err := attachmentPaths(opts.Attachments)
	if err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" && len(imagePaths) == 0 {
		return fmt.Errorf("claudetui: user message requires text or image content")
	}
	// Cold-start guard: don't write until the freshly-launched TUI is parked
	// reading input, or the submit CR is swallowed with the paste and the turn
	// never sends (see composerReady*). Latched, so only the first send waits.
	if err := s.awaitComposerReady(ctx); err != nil {
		return err
	}
	// Drive the composer one block at a time, settling between blocks so the Ink
	// paste handler ingests each cleanly: a short beat after the line-kill clear,
	// the longer pasteSettle between any two consecutive bracketed pastes so they
	// don't merge (the message body replays as interleaved text/image pastes),
	// then a short beat before the submit CR. buildSendSteps owns the ordering and
	// the per-gap durations; Send just writes and waits.
	for _, step := range buildSendSteps(content, imagePaths) {
		if err := s.writePTY(step.data); err != nil {
			return err
		}
		if err := s.settle(ctx, step.settle); err != nil {
			return err
		}
	}
	// Record the send so the reconstructor confirms it with a user{isReplay} echo
	// on the next main request, consuming triage's pending-send FIFO. Direct sends
	// carry an app-minted UserMessageUUID; queued sends (flush path) supply none,
	// so mint a stable id here — persistDeferredUserText needs a non-empty
	// provider_item_id or the queued row never lands. Pushed only after the writes
	// succeed so a write failure (which the caller turns into a pending-send clear)
	// leaves the echo FIFO aligned with the pending-send FIFO. The echo carries the
	// TEXT only; the image rides the optimistically-persisted user row (its
	// attachment meta), which the echo confirms by id.
	echoUUID := opts.UserMessageUUID
	if echoUUID == "" {
		echoUUID = uuid.NewString()
	}
	s.queueUserEcho(content, echoUUID)
	return nil
}

// sendStep is one PTY write Send issues, paired with the settle that must follow
// it. settle is 0 for the final submit, which has nothing after it.
type sendStep struct {
	data   []byte
	settle time.Duration
}

// buildSendSteps builds the ordered PTY writes (and the gap after each) that
// drive one user turn into the TUI composer:
//  1. composer-clear: composerClearKeystrokes Ctrl-U line-kills so a prompt the
//     TUI restored on a prior Esc-revert can't fuse with this send.
//  2. the message body, replayed IN ORDER as a paste per segment. The composer
//     embeds an "[Image #i]" marker at each image's drop point;
//     splitContentByImageMarkers turns the content back into text runs and images
//     at their original positions. Each text run is one bracketed paste; each
//     image is its own bracketed paste of the absolute PATH at that spot, which
//     Claude's paste handler reads into an image block and labels inline (so the
//     image lands where the user put it, not front-loaded). Text and image pastes
//     stay SEPARATE — a path shares its paste with no text, because Claude routes
//     a paste's non-image remainder through a space-split that would mangle text
//     containing " /". Empty text runs are skipped.
//  3. submit: the CR.
//
// Every body segment is a bracketed paste, so each interior boundary is the
// merge-sensitive paste→paste case and takes the longer pasteSettle; the
// clear→first-paste and last-paste→submit boundaries can't merge and take the
// short composerSettle, and submit (nothing follows) takes 0.
func buildSendSteps(content string, imagePaths []string) []sendStep {
	type block struct {
		data    []byte
		isPaste bool
	}
	blocks := []block{{data: []byte(strings.Repeat(composerClearKey, composerClearKeystrokes))}}
	for _, part := range splitContentByImageMarkers(content, len(imagePaths)) {
		if part.imageIndex >= 0 {
			blocks = append(blocks, block{data: bracketedPaste(imagePaths[part.imageIndex]), isPaste: true})
			continue
		}
		if part.text != "" {
			blocks = append(blocks, block{data: bracketedPaste(part.text), isPaste: true})
		}
	}
	blocks = append(blocks, block{data: []byte(submitKey)})

	steps := make([]sendStep, len(blocks))
	for i, b := range blocks {
		var settle time.Duration
		switch {
		case i == len(blocks)-1:
			settle = 0 // submit: nothing follows
		case b.isPaste && blocks[i+1].isPaste:
			settle = pasteSettle // two bracketed pastes back-to-back must not merge
		default:
			settle = composerSettle
		}
		steps[i] = sendStep{data: b.data, settle: settle}
	}
	return steps
}

// bracketedPaste wraps content in bracketed-paste markers so it lands in the
// composer as one block, stripping any stray paste marker first so the content
// can't reframe the paste. Both markers are dangerous, not just the terminator: a
// stray END (bracketedPasteEnd) closes the paste early and the tail reads as raw
// keystrokes, and a stray START (bracketedPasteStart) is matched ahead of the
// in-paste literal branch by Claude's tokenizer and RESETS the paste buffer —
// silently dropping everything pasted before it (claude-code's parse-keypress.ts:
// the PASTE_START token sets pasteBuffer=""). Removing both keeps the frame intact.
func bracketedPaste(content string) []byte {
	safe := strings.NewReplacer(bracketedPasteStart, "", bracketedPasteEnd, "").Replace(content)
	return []byte(bracketedPasteStart + safe + bracketedPasteEnd)
}

// attachmentPaths extracts the absolute file paths claude-tui pastes into the
// composer to ingest images. The app layer resolves each attachment to a Path
// for this provider; a missing one is a wiring bug we fail loudly on rather than
// silently dropping the image from the user's turn.
func attachmentPaths(attachments []provider.ImageAttachment) ([]string, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	paths := make([]string, 0, len(attachments))
	for _, att := range attachments {
		if att.Path == "" {
			return nil, fmt.Errorf("claudetui: image attachment %q has no on-disk path", att.ID)
		}
		// A control byte in a path would corrupt its bracketed paste: a \n splits
		// one path into two in Claude's paste parser, and a paste terminator or
		// ESC could break out of the paste into raw keystrokes. The store never
		// produces such a path (a uuid + whitelisted extension under an
		// app-controlled root), so this is defense-in-depth: fail loudly if a
		// future path scheme ever yields one rather than paste it.
		if i := strings.IndexFunc(att.Path, isPasteUnsafeRune); i >= 0 {
			return nil, fmt.Errorf("claudetui: image attachment %q path contains a control byte at offset %d; refusing to paste", att.ID, i)
		}
		paths = append(paths, att.Path)
	}
	return paths, nil
}

// isPasteUnsafeRune reports whether r is a C0 control or DEL — bytes that have no
// place in a filesystem path and would corrupt the bracketed-paste framing (a
// newline splits a path, ESC/terminator can break out of the paste). High bytes
// (UTF-8 multibyte, e.g. a non-ASCII home dir) are intentionally left alone.
func isPasteUnsafeRune(r rune) bool {
	return r < 0x20 || r == 0x7f
}

// settle gives the Ink TUI a beat between PTY writes, honoring ctx cancellation
// so a Stop mid-send returns promptly. d picks the gap: composerSettle (the short
// ingest beat) or pasteSettle (the longer gap that keeps two bracketed pastes
// from merging). A non-positive d is a no-op — the final submit has no trailing
// gap.
func (s *Session) settle(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
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
