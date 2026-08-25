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
// user input has been idle past its threshold. Returns what happened —
// "requested", "skipped-active-turn", or "skipped-recent" — so the caller
// can log without a second RPC. LocalOnly (internal/transport/internalmethods.go).
func (a *App) RequestWebviewMemoryTrim() (string, error) {
	if a.hasActiveProviderTurn() {
		return "skipped-active-turn", nil
	}
	now := time.Now().UnixNano()
	last := a.webviewTrimLastUnixNano.Load()
	if last != 0 && now-last < int64(webviewTrimMinInterval) {
		return "skipped-recent", nil
	}
	if !a.webviewTrimLastUnixNano.CompareAndSwap(last, now) {
		// A concurrent request won the slot; treat this one as the duplicate.
		return "skipped-recent", nil
	}
	log.Printf("webview trim: idle trim directive emitted")
	a.emit(eventchan.WebviewTrim, webviewTrimDirective{Reason: "idle"})
	return "requested", nil
}

// hasActiveProviderTurn reports whether any live session holds an open turn.
// A streaming turn means the renderer is painting; a forced GC there would
// be a visible hitch, so the trim waits for the next idle report.
func (a *App) hasActiveProviderTurn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, sess := range a.sessions {
		if sess.liveness != nil && sess.liveness.activeTurns.Load() > 0 {
			return true
		}
	}
	return false
}
