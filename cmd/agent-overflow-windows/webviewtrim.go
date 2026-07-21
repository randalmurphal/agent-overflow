//go:build windows

// webviewtrim.go frees the WebView2's memory while the launcher window
// sits minimised. A parked window holds the full rendering stack live —
// measured ~500MB working set across the renderer + GPU processes for a
// 4-pane session — none of it observable while minimised: the SPA fires
// no OS notifications, never mutates the taskbar title, and the
// transport's replay ring + seq-gap refetch reconstruct anything missed
// once the page thaws. Suspension is therefore invisible except for a
// brief catch-up repaint on restore.
package main

import (
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
