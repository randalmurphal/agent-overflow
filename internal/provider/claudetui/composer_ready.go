package claudetui

import (
	"bytes"
	"context"
	"time"

	"agent-overflow/internal/stringsx"
)

// composer_ready.go owns the cold-start composer-ready gate: deciding when a
// freshly-launched (or worktree-switched) TUI has mounted its interactive
// composer, so the FIRST Send's keystrokes land as input instead of being
// swallowed while claude is still booting. The keystroke contract Send drives
// once ready lives in session_send.go.
//
// Why a gate at all: before the Ink composer mounts, claude is not draining
// stdin. A paste+submit written then is read in a single chunk and the submit CR
// is swallowed with the paste — the turn types into the composer but never sends.
// The user hit this cold (opening message) and again on a worktree switch, whose
// session restart relaunches claude mid-conversation.
//
// Why the bottom-bar marker (not output timing): the earlier gate guessed
// readiness from the PTY stream — ">=512 bytes AND >=400ms idle". That guess is
// robust to claude's own boot bursts (it always emits <512 bytes, then a
// cwd-work gap, then banner+composer in one post-gap burst, so the byte
// threshold stays coupled to the composer paint), which is why a clean harness
// could never reproduce the swallow. But it is NOT robust to the real runtime:
// the PTY pump delivers output on its own goroutine, so under load the gate's
// idle clock can read false-idle while claude is still painting, and a worktree
// switch's cross-cwd resume reshapes the boot stream. The composer's bottom
// status bar rendering is instead a DETERMINISTIC "composer mounted, stdin
// draining" signal: in spike/claude-mitm/probe_marker_discriminate.py it
// submitted 5/5 at ~0.64s on a cold boot, where send-immediately (0/5) and the
// bracketed-paste-enable escape (0/4, fires before stdin drains) both swallowed,
// and the old timing gate only reached ready at ~1.31s. It is also
// content-agnostic, so an image-only first send is gated identically — unlike
// echo-gating the paste, which has no verbatim echo once the composer collapses
// a multi-line paste to a "[Pasted text]" chip.
//
// Binary behavior: the marker strings are claude TUI chrome and must be
// re-probed on a version bump (spike/claude-mitm/). The bounded timeout is the
// safety net if they ever drift — the send proceeds anyway (a re-sendable miss
// beats a hang), exactly as the old timing gate's timeout did.

const (
	// composerReadyPoll is the gate's check cadence; composerReadyTimeout is the
	// bounded fallback so a never-mounting composer (or a drifted marker) degrades
	// to a re-sendable miss instead of a hang.
	composerReadyPoll    = 40 * time.Millisecond
	composerReadyTimeout = 8 * time.Second
	// maxComposerScanBytes bounds the raw boot output retained for marker
	// scanning. The bottom bar re-renders on every status tick, so it is always in
	// the most-recent output; keeping a tail this size reliably contains a
	// freshly-mounted bar without growing unbounded on a pathological never-ready
	// boot (the timeout closes that case).
	maxComposerScanBytes = 1 << 16
)

// composerBarMarkers are stable, chrome-specific substrings of the composer's
// bottom status bar AFTER normalizeForMarker (de-ANSI, space-stripped,
// lowercased): "shift+tab to cycle" is the mode-cycle hint and "bypass
// permissions on" is the full-access mode indicator this provider always
// launches into (it is full-access only), so the live bar always renders both.
//
// ALL of them must be present for the gate to open (see noteComposerOutput),
// not any one. The bar carries both together, but replayed transcript content
// during a cross-cwd resume can mention ONE in prose ("press shift+tab to
// cycle ..."); requiring the whole bar makes such prose far less likely to trip
// the gate before the composer mounts — which on a worktree switch would swallow
// the auto-sent first message, the exact failure this gate exists to prevent.
// The cost: a claude version bump that renames ONE phrase degrades detection to
// the bounded timeout (the send still proceeds, just later) rather than riding
// on the surviving phrase — but that drift is a known re-probe event, not a
// silent runtime miss. A match means claude is parked reading input. Re-probe on
// version bump (spike/claude-mitm/).
var composerBarMarkers = [][]byte{
	[]byte("shift+tabtocycle"),
	[]byte("bypasspermissionson"),
}

// noteComposerOutput feeds raw PTY output to the readiness gate until the
// composer bar is seen (or the session latches ready). It accumulates a bounded
// tail and flips composerMarkerSeen once the whole bar (every marker) normalizes
// out of it. The caller (onPTYOutput) holds s.mu. Cheap and bounded: it runs only
// during boot, stops on the first match, and releases the scratch buffer then.
func (s *Session) noteComposerOutput(data []byte) {
	if s.composerMarkerSeen || s.composerReady {
		return
	}
	s.composerScanBuf = append(s.composerScanBuf, data...)
	if len(s.composerScanBuf) > maxComposerScanBytes {
		// Keep the most-recent tail; the bar is always in recent output.
		s.composerScanBuf = s.composerScanBuf[len(s.composerScanBuf)-maxComposerScanBytes:]
	}
	norm := normalizeForMarker(s.composerScanBuf)
	for _, m := range composerBarMarkers {
		if !bytes.Contains(norm, m) {
			return // not the whole bar yet — keep scanning
		}
	}
	s.composerMarkerSeen = true
	s.composerScanBuf = nil // done scanning; release the scratch
}

// awaitComposerReady blocks the first Send until the composer bar has rendered
// (composer mounted, stdin draining) so the submit CR is read as its own keypress
// instead of being swallowed with the paste. The result latches under mu: only
// the first cold send pays the wait; every later send returns at once. On the
// bounded timeout it proceeds anyway (a re-sendable miss beats a hang) — the
// fallback that fires for production callers, which pass context.Background; the
// ctx branch is for tests / a future cancellable caller. It logs the outcome once
// per session: which signal released the send and how long it took.
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
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		s.mu.Lock()
		seen := s.composerMarkerSeen
		timedOut := time.Now().After(deadline)
		if seen || timedOut {
			s.composerReady = true
			s.composerScanBuf = nil
			s.mu.Unlock()
			if seen {
				s.logf("claudetui: composer ready (bar marker) after %s", time.Since(start).Round(time.Millisecond))
			} else {
				s.logf("claudetui: composer-ready gate timed out after %s; sending anyway (bar marker not seen)", timeout)
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

// normalizeForMarker reduces raw PTY bytes to the form composerBarMarkers are
// written in: ANSI/OSC escapes stripped, only printable non-space ASCII kept,
// lowercased. The TUI lays out the status bar with cursor-movement escapes and
// padding, so its words are not contiguous in the raw stream; this normalization
// (mirroring spike/claude-mitm/aoprobe.py's _norm) lets a plain substring match
// find a stable bar marker. High/multibyte bytes (e.g. the bar's glyphs) drop
// out, which is fine — the markers are pure ASCII.
func normalizeForMarker(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); {
		c := raw[i]
		if c == 0x1b { // ESC: skip the whole escape sequence
			i = stringsx.SkipANSIEscape(raw, i)
			continue
		}
		if c > 0x20 && c < 0x7f { // printable ASCII, excluding space
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			out = append(out, c)
		}
		i++
	}
	return out
}
