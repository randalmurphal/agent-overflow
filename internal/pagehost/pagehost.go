// Package pagehost is the contract between a loaded page and the shell
// hosting it: which shell that is, and how a shell that owns the window
// hands its page the one-time transport ticket instead of writing it
// into the URL.
//
// A browser can only be told things through the URL it is opened with,
// so a browser's page ticket rides `?t=` (internal/transport
// credential.go). A webview window is different: the Go process that
// mints the ticket also owns the window and can evaluate script in the
// document it just loaded. Nothing then has to travel in the URL — and
// a URL is copyable, lands in launcher logs and window diagnostics, and
// outlives its single use in shell history and error reports, so the
// windows we own keep the credential out of it entirely.
//
// Three window hosts deliver a ticket this way (the desktop and
// isolated windowed boots, the Windows WSL launcher, and the --connect
// stub's window) and one page reads it, so the marker, the two names the
// injected script writes, and the script itself live here rather than
// being spelled out per host. The TypeScript half is
// frontend/src/lib/transport/pageHost.ts.
//
// Stdlib-only by construction: the Windows launcher links this and does
// not link internal/transport.
package pagehost

import (
	"errors"
	"net/url"
	"strings"
)

// Param names the page's host on the page URL, and Webview is the value
// a window-owning Go process stamps. It is a marker, never a
// credential: it says "your ticket is arriving by injection, wait for
// it" and grants nothing. A page URL without it is a browser's, and its
// ticket is on the URL.
const (
	Param   = "host"
	Webview = "webview"
)

// TicketGlobal is the window property the injected script assigns, and
// TicketEvent the window event it then dispatches. Two names rather than
// one because the page and the injection race in both directions: the
// global answers an injection that landed before the page's boot code
// ran, and the event answers one that lands after. Delivering both makes
// the order irrelevant, and re-delivery is a re-assignment plus an event
// nobody is listening for.
const (
	TicketGlobal = "__aoPageTicket"
	TicketEvent  = "ao:page-ticket"
)

// Answer is what the /pageurl route hands a webview host: the bare URL
// to navigate to and, separately, the ticket to inject into whatever
// document that navigation produces. Declared here because the Windows
// launcher decodes it without linking the transport server that encodes
// it.
type Answer struct {
	URL    string `json:"url"`
	Ticket string `json:"ticket"`
}

// MarkWebview stamps Param on a page URL. The empty URL passes through:
// every producer treats "" as "no page to open yet".
func MarkWebview(pageURL string) string {
	if pageURL == "" {
		return pageURL
	}
	separator := "&"
	if !strings.Contains(pageURL, "?") {
		separator = "?"
	}
	return pageURL + separator + Param + "=" + url.QueryEscape(Webview)
}

// IsWebviewURL reports whether a page URL is marked as webview-hosted.
// For tests and for a host confirming what it is about to navigate to.
func IsWebviewURL(pageURL string) bool {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return false
	}
	return parsed.Query().Get(Param) == Webview
}

// ErrTicketNotRenderable rejects a ticket that could be anything other
// than a JavaScript string literal once written into the script below.
var ErrTicketNotRenderable = errors.New("pagehost: ticket is not a bare token")

// DeliveryScript renders the one script a window host evaluates in its
// page to hand over a ticket.
//
// It is built from the constants above rather than spelled per host, so
// the three hosts and the one reader cannot drift, and it is refused
// outright for a ticket carrying anything outside the base64url alphabet
// transport.NewToken emits. Validating rather than escaping is the
// point: an escaping bug is invisible until the day a value needs it,
// while a token that cannot contain a quote can never close the literal.
func DeliveryScript(ticket string) (string, error) {
	if ticket == "" || !bareToken(ticket) {
		return "", ErrTicketNotRenderable
	}
	return "(function(){window." + TicketGlobal + "=\"" + ticket +
		"\";window.dispatchEvent(new Event(\"" + TicketEvent + "\"));})();", nil
}

// bareToken reports whether s is base64url with padding and nothing
// else — the alphabet base64.URLEncoding produces.
func bareToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '=':
		default:
			return false
		}
	}
	return true
}
