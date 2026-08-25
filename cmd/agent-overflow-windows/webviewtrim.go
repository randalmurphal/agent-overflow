//go:build windows

// webviewtrim.go frees the WebView2's memory on the two signals Blink
// never generates for itself in an always-visible desktop app.
//
// Minimise: suspend the whole webview after 30s parked. A parked window
// holds the full rendering stack live — measured ~500MB working set
// across the renderer + GPU processes for a 4-pane session — none of it
// observable while minimised: the SPA fires no OS notifications, never
// mutates the taskbar title, and the transport's replay ring + seq-gap
// refetch reconstruct anything missed once the page thaws. Suspension is
// therefore invisible except for a brief catch-up repaint on restore.
//
// Input idle: force one memory-reducing GC in the renderer while the
// window stays VISIBLE. Blink pools freed Oilpan pages and only
// decommits them on a memory-reducing GC, which it triggers on page-hide
// or OS memory pressure — neither ever fires here, so the renderer parks
// at its high-water mark for hours (measured 2026-08-25: 5 idle hours
// flat at ~293MB with ~20MB live; one forced GC returned 54MB). The WSL
// backend emits webview:trim once the frontend reports input idle and no
// provider turn is open; trimRendererMemory answers it with CDP
// HeapProfiler.collectGarbage over WebView2's DevTools bridge.
package main

import (
	"log"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// suspendAfterMinimiseDelay is how long the window must stay minimised
// before the WebView2 suspends. Long enough that alt-tab round trips
// never pay the resume repaint; short enough that a parked window frees
// its memory promptly.
const suspendAfterMinimiseDelay = 30 * time.Second

// trimWebviewMemoryOnMinimise arms a suspension timer on minimise and
// resumes on un-minimise. The stop/fire race is settled on the main
// thread: SuspendWebview's Windows implementation re-checks the
// minimised state before hiding anything, so a timer that fires after
// an un-minimise cannot blank a visible window, and ResumeWebview is a
// no-op when nothing was suspended.
func trimWebviewMemoryOnMinimise(w *application.WebviewWindow) {
	var mu sync.Mutex
	var pending *time.Timer

	w.OnWindowEvent(events.Windows.WindowMinimise, func(*application.WindowEvent) {
		mu.Lock()
		defer mu.Unlock()
		if pending != nil {
			pending.Stop()
		}
		pending = time.AfterFunc(suspendAfterMinimiseDelay, func() {
			w.SuspendWebview()
		})
	})

	// WindowUnMinimise fires exactly once per un-minimise on both exit
	// paths (restore-to-normal and restore-to-maximised). WindowRestore
	// is deliberately not used: it re-fires on every WM_SIZE during a
	// live drag-resize.
	w.OnWindowEvent(events.Windows.WindowUnMinimise, func(*application.WindowEvent) {
		mu.Lock()
		if pending != nil {
			pending.Stop()
			pending = nil
		}
		mu.Unlock()
		w.ResumeWebview()
	})
}

// trimRendererMemory answers one webview:trim directive: a
// HeapProfiler.collectGarbage CDP call, which runs V8's low-memory GC and
// Blink's memory-reducing Oilpan GC — the only decommit trigger a visible
// page can get. The pause is tens of milliseconds; the backend only emits
// while user input is idle and no provider turn streams, so nobody sees it.
//
// Fire-and-forget by design: every failure mode (window gone, webview
// suspended, CDP error) costs one skipped trim and a log line, and the
// backend re-emits on the next idle report. errorCode is the raw COM
// HRESULT because that is what WebView2 hands the completion.
func trimRendererMemory(w *application.WebviewWindow, reason string) {
	if w == nil {
		log.Printf("webview trim: no window yet; dropping %q directive", reason)
		return
	}
	// enable → collectGarbage → disable, chained through the completions
	// (which land on the main thread; CallDevToolsProtocol is safe to call
	// from one — dispatchOnMainThread runs inline there). The enable/disable
	// bracket mirrors the sequence the collectGarbage behaviour was verified
	// under; the domain holds no tracking state between them.
	started := time.Now()
	step := func(method string, next func()) {
		err := w.CallDevToolsProtocol(method, "", func(errorCode uintptr, _ string) {
			if errorCode != 0 {
				log.Printf("webview trim: %s completed with HRESULT %#x (reason %q)", method, errorCode, reason)
				return
			}
			if next != nil {
				next()
			}
		})
		if err != nil {
			log.Printf("webview trim: dispatch %s: %v (reason %q)", method, err, reason)
		}
	}
	step("HeapProfiler.enable", func() {
		step("HeapProfiler.collectGarbage", func() {
			log.Printf("webview trim: renderer GC done in %s (reason %q)", time.Since(started).Round(time.Millisecond), reason)
			step("HeapProfiler.disable", nil)
		})
	})
}
