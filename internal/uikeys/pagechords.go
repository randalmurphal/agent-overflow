package uikeys

import (
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The window binds Ctrl/Cmd+R to reloading the app before the SPA sees the
// key. While the SPA's browser pane is its focused pane the same chord means
// "reload the page", as it does inside the native page (the engine claims it
// there and the SPA replays it). PageChords makes the window hand the chord to
// the SPA exactly then:
//
//   - The window advertises the capability to each document it loads
//     (CapabilityScript, on the runtime-ready event) and forgets the previous
//     document's state.
//   - The SPA posts FocusMessage(true|false) over the webview's raw message
//     channel as its browser pane gains and loses focus.
//   - While the last report says focused, Ctrl/Cmd+R runs DispatchScript in
//     the page instead of reloading. A SPA that finds its browser pane no
//     longer focused posts ReloadAppMessage, and the window reloads as the
//     binding would have. Anything else reloads at once, as before.
//
// Ctrl/Cmd+Shift+R always reloads the app, so a page that stopped answering
// can still be reloaded with the browser pane focused.
type PageChords struct {
	browserFocused atomic.Bool
	reload         func(application.Window)
}

// NewPageChords returns a window's chord routing, browser pane unfocused.
func NewPageChords() *PageChords { return &PageChords{} }

const (
	// MessagePrefix starts every message the SPA posts for PageChords.
	MessagePrefix = "ao:shell:"
	// CapabilityGlobal is the window property the SPA checks before posting.
	CapabilityGlobal = "__aoShellChords"
	// ReadyEvent is dispatched on the SPA's window once the capability is
	// set, so a SPA that started first reports its state.
	ReadyEvent = "ao-shell-chords"
	// HandlerGlobal is the SPA's function DispatchScript calls.
	HandlerGlobal = "__aoShellChord"

	focusMessage     = MessagePrefix + "browser-pane-focus:"
	ReloadAppMessage = MessagePrefix + "reload-app"
)

// CapabilityScript tells a document the window routes chords to it.
const CapabilityScript = "window." + CapabilityGlobal + " = 1; window.dispatchEvent(new Event('" + ReadyEvent + "'));"

// DispatchScript hands the reload chord to the SPA. A document that has no
// handler yet (still loading) asks the window to reload the app, which is
// what the chord means there.
const DispatchScript = `(function () {
  var h = window.` + HandlerGlobal + `;
  if (typeof h === 'function') { h('reload'); return; }
  var m = '` + ReloadAppMessage + `';
  if (window.chrome && window.chrome.webview && window.chrome.webview.postMessage) { window.chrome.webview.postMessage(m); return; }
  if (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.external) { window.webkit.messageHandlers.external.postMessage(m); }
})();`

// FocusMessage is the message the SPA posts as its browser pane gains or
// loses focus.
func FocusMessage(focused bool) string {
	if focused {
		return focusMessage + "1"
	}
	return focusMessage + "0"
}

// Bindings returns bindings with its Ctrl/Cmd+R routed through c. The
// original reload is kept for when the SPA does not take the chord.
func (c *PageChords) Bindings(bindings map[string]func(application.Window)) map[string]func(application.Window) {
	reload := bindings["CmdOrCtrl+r"]
	if reload == nil {
		return bindings
	}
	c.reload = reload
	bindings["CmdOrCtrl+r"] = func(window application.Window) {
		if c.browserFocused.Load() {
			window.ExecJS(DispatchScript)
			return
		}
		reload(window)
	}
	return bindings
}

// DocumentLoaded forgets the previous document's focus and advertises the
// capability to the new one. Call it on each runtime-ready event.
func (c *PageChords) DocumentLoaded(window application.Window) {
	c.browserFocused.Store(false)
	window.ExecJS(CapabilityScript)
}

// HandleMessage applies one raw message from the window's own SPA and
// reports whether it was a PageChords message. The caller checks the sender
// (TrustedPage) first.
func (c *PageChords) HandleMessage(window application.Window, message string) bool {
	if !strings.HasPrefix(message, MessagePrefix) {
		return false
	}
	switch message {
	case FocusMessage(true):
		c.browserFocused.Store(true)
	case FocusMessage(false):
		c.browserFocused.Store(false)
	case ReloadAppMessage:
		c.browserFocused.Store(false)
		if c.reload != nil {
			c.reload(window)
		}
	}
	return true
}

// BrowserFocused reports the SPA's last focus report.
func (c *PageChords) BrowserFocused() bool { return c.browserFocused.Load() }

// TrustedPage reports whether a raw message came from a document on the
// page URL's origin (scheme and host:port), and its top-level document when
// the platform names it. The picker, an error page and any other origin are
// refused.
func TrustedPage(origin *application.OriginInfo, pageURL string) bool {
	if origin == nil {
		return false
	}
	want, ok := originOf(pageURL)
	if !ok {
		return false
	}
	got, ok := originOf(origin.Origin)
	if !ok || got != want {
		return false
	}
	if origin.TopOrigin == "" {
		return true
	}
	top, ok := originOf(origin.TopOrigin)
	return ok && top == want
}

func originOf(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host), true
}
