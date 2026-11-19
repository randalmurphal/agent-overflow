package uikeys

import (
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The window binds Ctrl/Cmd+R to reloading the app before the SPA sees the
// key. While the SPA's browser pane is its focused pane and the SPA's
// effective keybindings run a command for that chord there (by default
// "reload the page", as inside the native page, where the engine claims the
// chord and the SPA replays it), the window hands the chord to the SPA
// instead. PageChords makes the window do that exactly then:
//
//   - The window advertises the capability to each document it loads
//     (CapabilityScript, on the runtime-ready event) under a number of its
//     own, and forgets the previous document's state.
//   - The SPA posts RouteMessage(document, seq, true|false) over the
//     webview's raw message channel as that answer changes, numbering its
//     reports from 1. The SPA owns the effective bindings and their `when`
//     contexts, so the decision is its own. The window runs the raw message
//     handler on a goroutine per message, so reports can arrive out of
//     order: only the current document's newest report counts.
//   - While the last report says route, Ctrl/Cmd+R runs DispatchScript in the
//     page instead of reloading; the SPA replays it through its keybinding
//     dispatcher. A chord nothing runs comes back as ReloadAppMessage, and
//     the window reloads as the binding would have. Anything else reloads at
//     once, as before.
//
// Ctrl/Cmd+Shift+R always reloads the app, so a page that stopped answering
// can still be reloaded with the browser pane focused.
type PageChords struct {
	reload func(application.Window)

	mu sync.Mutex
	// document numbers the document the window loaded last; seq is the
	// number of its last report applied, route that report's answer.
	document, seq uint64
	route         bool
}

// NewPageChords returns a window's chord routing, not routing.
func NewPageChords() *PageChords { return &PageChords{} }

const (
	// MessagePrefix starts every message the SPA posts for PageChords.
	MessagePrefix = "ao:shell:"
	// CapabilityGlobal is the window property the SPA checks before posting:
	// the document's number, which every message it posts names.
	CapabilityGlobal = "__aoShellChords"
	// ReadyEvent is dispatched on the SPA's window once the capability is
	// set, so a SPA that started first reports its state.
	ReadyEvent = "ao-shell-chords"
	// HandlerGlobal is the SPA's function DispatchScript calls.
	HandlerGlobal = "__aoShellChord"

	routeMessage = MessagePrefix + "route-reload:"
	// reloadAppMessage, with the document's number, asks the window to
	// reload the app: the chord it handed over ran nothing.
	reloadAppMessage = MessagePrefix + "reload-app:"
)

// CapabilityScript tells a document the window routes chords to it, as the
// window's document number document.
func CapabilityScript(document uint64) string {
	return "window." + CapabilityGlobal + " = " + strconv.FormatUint(document, 10) +
		"; window.dispatchEvent(new Event('" + ReadyEvent + "'));"
}

// DispatchScript hands the reload chord to the SPA. A document that has no
// handler yet (still loading) asks the window to reload the app, which is
// what the chord means there.
const DispatchScript = `(function () {
  var h = window.` + HandlerGlobal + `;
  if (typeof h === 'function') { h('reload'); return; }
  var m = '` + reloadAppMessage + `' + window.` + CapabilityGlobal + `;
  if (window.chrome && window.chrome.webview && window.chrome.webview.postMessage) { window.chrome.webview.postMessage(m); return; }
  if (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.external) { window.webkit.messageHandlers.external.postMessage(m); }
})();`

// RouteMessage is the message the SPA posts as it starts or stops taking
// the reload chord: its seq-th report in the window's document document.
func RouteMessage(document, seq uint64, route bool) string {
	answer := "0"
	if route {
		answer = "1"
	}
	return routeMessage + strconv.FormatUint(document, 10) + ":" + strconv.FormatUint(seq, 10) + ":" + answer
}

// ReloadAppMessage is the message document document posts when a chord the
// window handed over ran nothing there.
func ReloadAppMessage(document uint64) string {
	return reloadAppMessage + strconv.FormatUint(document, 10)
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
		if c.Routing() {
			window.ExecJS(DispatchScript)
			return
		}
		reload(window)
	}
	return bindings
}

// DocumentLoaded forgets the previous document's report and advertises the
// capability to the new one under the next document number, so a report
// the previous document posted counts for nothing. Call it on each
// runtime-ready event.
func (c *PageChords) DocumentLoaded(window application.Window) {
	c.mu.Lock()
	c.document++
	c.seq, c.route = 0, false
	document := c.document
	c.mu.Unlock()
	window.ExecJS(CapabilityScript(document))
}

// HandleMessage applies one raw message from the window's own SPA and
// reports whether it was a PageChords message. The caller checks the sender
// (TrustedPage) first. A report of another document, or older than the
// last one applied, changes nothing; so does a reload request of another
// document.
func (c *PageChords) HandleMessage(window application.Window, message string) bool {
	rest, ok := strings.CutPrefix(message, MessagePrefix)
	if !ok {
		return false
	}
	if report, ok := strings.CutPrefix(rest, "route-reload:"); ok {
		document, seq, route, ok := parseRouteReport(report)
		if ok {
			c.mu.Lock()
			if document == c.document && seq > c.seq {
				c.seq, c.route = seq, route
			}
			c.mu.Unlock()
		}
		return true
	}
	if number, ok := strings.CutPrefix(rest, "reload-app:"); ok {
		document, err := strconv.ParseUint(number, 10, 64)
		c.mu.Lock()
		current := err == nil && document == c.document
		if current {
			c.route = false
		}
		c.mu.Unlock()
		if current && c.reload != nil {
			c.reload(window)
		}
	}
	return true
}

// parseRouteReport reads "<document>:<seq>:<0|1>".
func parseRouteReport(report string) (document, seq uint64, route, ok bool) {
	parts := strings.Split(report, ":")
	if len(parts) != 3 || (parts[2] != "0" && parts[2] != "1") {
		return 0, 0, false, false
	}
	document, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false, false
	}
	seq, err = strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false, false
	}
	return document, seq, parts[2] == "1", true
}

// Routing reports whether the current document's newest report asked for
// the chord.
func (c *PageChords) Routing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.route
}

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
