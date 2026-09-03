package uiwindow

import (
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"agent-overflow/internal/pagehost"
)

// DeliverPageTicket wires a window to hand each document it loads a
// fresh one-time page ticket, so the page URL the window navigates to
// carries no credential at all (internal/pagehost).
//
// The trigger is WindowRuntimeReady, which Wails emits when the loaded
// document announces itself to its host. That announcement is what makes
// the delivery work at all: WebviewWindow.ExecJS QUEUES until a document
// has announced, and this app's SPA replaces @wailsio/runtime with its
// own transport shim, so nothing announces unless the page does it
// itself. It does — frontend/src/lib/transport/pageHost.ts, but only
// when the URL marks it webview-hosted — and re-announces on a bounded
// cadence until its ticket arrives, which is what recovers the one race
// this cannot otherwise cover: a document that finished loading before
// the caller subscribed.
//
// mint is called per announcement rather than once per navigation, so a
// document that reloaded itself (devtools, the SPA reloading in place)
// gets a live ticket instead of the spent one its predecessor used.
// Re-delivering a spent ticket is harmless either way — such a page
// already holds the cookie, and Credential.Exchange authenticates before
// it looks at a ticket — so no state has to track which document is
// which.
//
// The returned function unsubscribes. Callers that keep the window for
// the process lifetime can drop it.
func DeliverPageTicket(window *application.WebviewWindow, mint func() (string, error)) func() {
	if window == nil || mint == nil {
		return func() {}
	}
	return window.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
		ticket, err := mint()
		if err != nil {
			log.Printf("uiwindow: mint page ticket: %v", err)
			return
		}
		script, err := pagehost.DeliveryScript(ticket)
		if err != nil {
			log.Printf("uiwindow: render page-ticket delivery: %v", err)
			return
		}
		window.ExecJS(script)
	})
}
