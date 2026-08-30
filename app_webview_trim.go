// app_webview_trim.go is the backend half of the renderer memory trim.
//
// Blink only runs its memory-reducing GC — the one that DECOMMITS Oilpan
// pages instead of pooling them — when the page is hidden or the OS reports
// memory pressure. An always-visible desktop app never gets either signal,
// so the renderer parks at its high-water mark forever (measured 2026-08-25:
// 5 idle hours flat at ~293MB with ~20MB live; one forced GC returned 54MB
// instantly). Simulated pressure notifications were A/B'd and return
// nothing in WebView2; the forced GC is the only working lever, and its
// whole cost is one ~58ms main-thread stall (soak rig, streaming load).
// That price shapes the policy: the trim fires in input-quiet windows
// BETWEEN turns — ~10s after the last turn completes, at most once per
// webviewTrimMinInterval — so an active session returns to floor after
// every turn instead of ratcheting until the user walks away. The frontend
// reports input quiet over RequestWebviewMemoryTrim; this side gates on
// provider activity and forwards the webview:trim directive to the process
// that owns the webview (the Windows launcher, over the notification
// bridge), which fires CDP HeapProfiler.collectGarbage through WebView2's
// DevTools bridge.
//
// The gates live HERE, not in the frontend, because only this process knows
// whether a turn is open: a GC pause (tens of ms) is invisible on an idle
// window but a jank lever mid-stream. The directive channel is ephemeral
// and loopback-only (internal/transport/event_channels.go), and the RPC is
// LocalOnly — a remote client's idleness says nothing about the desktop
// session.
//
// Platform reach: the directive only does something where a launcher-owned
// webview subscribes to it — the Windows/WSL split today. The native
// desktop builds (in-process webview) and --connect clients emit into
// silence; wiring an in-process trim for those is a follow-up.
package main

import (
	"log"
	"time"

	"agent-overflow/internal/eventchan"
)

// webviewTrimMinInterval is the backend-side floor between two accepted trim
// requests. The frontend paces itself far slower; this guard exists so a
// misbehaving or duplicated client cannot turn the directive into a GC
// firehose.
const webviewTrimMinInterval = 4 * time.Minute

// webviewTrimDirective is the webview:trim payload. Reason is for the
// launcher's log line, nothing else.
type webviewTrimDirective struct {
	Reason string `json:"reason"`
}

// RequestWebviewMemoryTrim asks the webview's owning process to run a
// memory-reducing GC in the renderer. Called by the embedded frontend when
// user input has been idle past its threshold; inputSinceLastTrim is the
// caller's half of the activity gate — whether any user input landed after
// the last trim this caller saw accepted. Returns what happened —
// "requested", "skipped-active-turn", "skipped-recent", or
// "skipped-no-activity" — so the caller can log without a second RPC.
// LocalOnly (internal/transport/internalmethods.go).
func (a *App) RequestWebviewMemoryTrim(inputSinceLastTrim bool) (string, error) {
	if a.hasActiveProviderTurn() {
		return "skipped-active-turn", nil
	}
	now := time.Now().UnixNano()
	last := a.webviewTrimLastUnixNano.Load()
	if last != 0 && now-last < int64(webviewTrimMinInterval) {
		return "skipped-recent", nil
	}
	// The renderer only re-accumulates trimmable garbage when it WORKS:
	// user input (the caller's fact) or a provider turn (this side's,
	// stamped at turn lifecycle events — recordActivity). With neither
	// since the last accepted trim, the renderer is already at floor and a
	// forced GC is a ~50ms stall that reclaims nothing; an idle overnight
	// window used to fire one every floor interval, 717 in a sitting
	// (2026-08-26, the 165Hz frame-drop attribution). First trim since
	// boot (last == 0) always passes — boot render churn is real work.
	if last != 0 && !inputSinceLastTrim && a.turnActivityUnixNano.Load() <= last {
		return "skipped-no-activity", nil
	}
	if !a.webviewTrimLastUnixNano.CompareAndSwap(last, now) {
		// A concurrent request won the slot; treat this one as the duplicate.
		return "skipped-recent", nil
	}
	log.Printf("webview trim: idle trim directive emitted")
	a.emit(eventchan.WebviewTrim, webviewTrimDirective{Reason: "idle"})
	return "requested", nil
}

// webviewTrimRecentWireWindow is the "wire just spoke" horizon of the
// active-turn gate. It exists for the streams no turn-state predicate
// covers: a backgrounded subagent's sidechain keeps painting after the
// parent round soft-closes, and every wire event bumps the session's
// lastActivity stamp. Sized under the frontend's 10s input-quiet
// threshold so the between-turns fire path (~10s after the last turn
// completes) stays open.
const webviewTrimRecentWireWindow = 5 * time.Second

// hasActiveProviderTurn reports whether any live session is mid-turn or
// still streaming. A streaming turn means the renderer is painting; a
// forced GC there would be a visible hitch, so the trim waits for the
// next idle report. Three arms, most precise first:
//
//  1. triage's open logical-turn / wire-round state — the wire-driven,
//     provider-agnostic truth. Load-bearing for Claude: its sessions
//     never emit EventTurnStart (app_provider_events.go), so arm 2's
//     counter is permanently zero for them, and until this arm existed
//     every idle trim could land mid-Claude-turn as a 60-130ms stall
//     (found live 2026-08-27: six replay trims, every one with items
//     streaming within ±5s).
//  2. the per-session activeTurns counter (Codex's EventTurnStart /
//     EventTurnComplete pair).
//  3. wire activity within webviewTrimRecentWireWindow — covers
//     post-round sidechain streaming and any future provider whose turn
//     events are asymmetric, so the gate no longer depends on per-
//     provider event symmetry to fail safe.
func (a *App) hasActiveProviderTurn() bool {
	if a.triage.AnyInFlightTurnOrRound() {
		return true
	}
	cutoff := time.Now().Add(-webviewTrimRecentWireWindow).UnixNano()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, sess := range a.sessions {
		if sess.liveness == nil {
			continue
		}
		if sess.liveness.activeTurns.Load() > 0 {
			return true
		}
		if sess.liveness.lastActivityUnixNano.Load() > cutoff {
			return true
		}
	}
	return false
}
