package transport

import (
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// Credential is one launch's page-access credential, shared by this
// package's Server and by internal/clientmode's stub so both halves of
// the desktop app present the same channel to the same SPA code.
//
// Two carriers, one validation function (Authenticate):
//
//   - The page cookie. Issued by Exchange when a browser first loads the
//     page with a one-time ticket on its URL, HttpOnly so page script can
//     never read it back. Every later request from that page — the
//     manifest refetch on reconnect, the /ws upgrade — rides it.
//   - The session token, presented by a client that is not a browser
//     (the WSL launcher's probe and its notification socket, the
//     `ao-harness` CLI, the --connect stub dialing its upstream, the e2e
//     rig's Node socket) as `Authorization: Bearer` or `?token=`. A
//     browser cannot set headers on a WebSocket handshake, so the query
//     carrier is what keeps those clients on one credential instead of
//     a second code path.
//
// The one-time ticket is deliberately NOT the session token: a page URL
// travels through window history, shell arguments, launcher logs and
// screenshots, and after this change what travels there buys exactly one
// cookie and nothing else. Tickets are minted per page URL produced
// (Server.AppURL, the /pageurl route, the LAN share URL) and consumed by
// the first exchange that presents one.
type Credential struct {
	// token is the per-launch session credential. Immutable after New.
	token string

	// tickets holds the minted-but-unexchanged page tickets. A
	// ticketBook (ticket.go) with no deadline and no subject: a launch
	// has one page credential, so a page ticket only decides who receives
	// it, and a launcher's fixed `?t=` URL must still work an hour after
	// it was written. Bounded by count — a producer of page URLs (the
	// settings panel's LAN share URL rebuilds one per panel mount) must
	// not be able to grow it without limit, and a ticket older than the
	// last maxOutstandingTickets is one nobody is going to open.
	tickets *ticketBook
}

// maxOutstandingTickets caps how many minted-but-unexchanged page
// tickets a launch keeps live. Sixteen covers every real overlap (a boot
// URL, a reload URL, a share URL re-rendered a few times) at 16×43
// bytes, and evicting the oldest keeps the newest URL — the one a user
// just copied — always valid.
const maxOutstandingTickets = 16

// PageTicketParam is the query parameter carrying a one-time page
// ticket on a page URL, and the only place a ticket is ever accepted.
const PageTicketParam = "t"

// SessionTokenParam is the query parameter carrying the session token.
// Kept for clients that cannot set request headers — the browser
// WebSocket API and Node's global WebSocket both build a URL and
// nothing else.
const SessionTokenParam = "token"

// ReservedCookiePrefix is the namespace every cookie this app sets lives
// under. It exists so a hop that must not forward OUR cookies can drop
// them by one rule instead of by a list of names it has to keep current.
//
// The preview proxy is that hop (previewproxy.go). Browsers scope cookies
// by HOST and ignore the port, so a preview listener on the same host as
// the app receives the page cookie, the session cookie and every other
// preview port's cookie attached by the browser. Forwarding any of them
// hands a live credential to a dev server whose bytes are agent-authored,
// and a deny-list of one name is a rule that goes stale the day a fourth
// cookie is added. TestEveryAppCookieUsesTheReservedPrefix is the pin.
const ReservedCookiePrefix = "ao_"

// pageCookiePrefix names the page cookie. The listen port is appended
// (see pageCookieName) because cookies are scoped by host and path, not
// by port: two backends on one host — the user's app and a harness
// instance, or an app and a --connect stub — would otherwise write the
// same cookie name over each other and each would read the other's
// value as a bad credential.
const pageCookiePrefix = ReservedCookiePrefix + "page_"

// NewCredential mints a launch credential. An empty token asks for a
// fresh one; callers that already hold a token (the --connect stub is
// handed its upstream's) pass it in.
func NewCredential(token string) (*Credential, error) {
	if token == "" {
		minted, err := NewToken()
		if err != nil {
			return nil, err
		}
		token = minted
	}
	return &Credential{token: token, tickets: newTicketBook(maxOutstandingTickets, 0)}, nil
}

// Token returns the session token, the carrier for clients that are not
// browsers.
func (c *Credential) Token() string { return c.token }

// MintPageTicket returns a fresh one-time ticket for a page URL and
// records it as outstanding. Every page URL this process hands out gets
// its own, so opening one URL never invalidates another.
func (c *Credential) MintPageTicket() (string, error) { return c.tickets.mint("") }

// consumeTicket removes ticket from the outstanding set, reporting
// whether it was there. A ticket answers exactly one exchange: a URL
// that already bought its cookie cannot buy a second one.
func (c *Credential) consumeTicket(ticket string) bool {
	_, ok := c.tickets.consume(ticket)
	return ok
}

// Authenticate reports whether r carries a valid credential for this
// launch. It is the single validation path: the cookie a browser was
// issued, or the session token a non-browser client presents as a
// bearer header or query parameter.
//
// Every cookie of the expected name is checked, not just the first.
// Another page on the same host can write a same-named cookie on a
// different path, which a browser then sends ahead of ours; reading all
// of them means such a cookie is ignored rather than mistaken for the
// live one.
func (c *Credential) Authenticate(r *http.Request) bool {
	name := pageCookieName(r.Host)
	for _, cookie := range r.Cookies() {
		if cookie.Name != name {
			continue
		}
		if ConstantTimeEqual(c.token, cookie.Value) == nil {
			return true
		}
	}
	if presented := bearerToken(r.Header.Get("Authorization")); presented != "" {
		if ConstantTimeEqual(c.token, presented) == nil {
			return true
		}
	}
	return ConstantTimeEqual(c.token, r.URL.Query().Get(SessionTokenParam)) == nil
}

// Exchange authenticates a page load and issues the page cookie when
// the request paid for one with a ticket. Reports whether the request
// is authenticated at all; the caller answers a false with its own
// refusal shape.
//
// Order matters. A request that already holds a valid credential is
// answered as-is, which is what makes a reload of the scrubbed URL work
// and what keeps a launcher's fixed `?t=` URL usable after its ticket
// was spent. Only when nothing valid is presented does the ticket get
// consumed, and the cookie it buys replaces whatever stale value the
// browser was holding.
func (c *Credential) Exchange(w http.ResponseWriter, r *http.Request) bool {
	if c.Authenticate(r) {
		return true
	}
	if !c.consumeTicket(r.URL.Query().Get(PageTicketParam)) {
		return false
	}
	http.SetCookie(w, c.pageCookie(r))
	return true
}

// pageCookie builds the page cookie for this request's authority.
//
// Attributes, each a decision:
//
//   - HttpOnly: the point of the change. Page script cannot read the
//     credential, so an injection on our own origin cannot carry it off.
//   - SameSite=Strict: the app's own navigations (the webview's initial
//     load, a reload, a bookmarked URL) carry no initiator and count as
//     same-site, so Strict costs the app nothing while keeping the
//     cookie off requests another site initiates. It is NOT the defence
//     against another port on this host — same-site ignores ports, which
//     is why the Origin check on /ws is the load-bearing one.
//   - Path=/: the manifest, the upgrade and the assets all live under
//     the root, and a narrower path would just be a second thing to keep
//     in sync.
//   - No Domain: host-only, so the cookie never widens to sibling hosts.
//   - No Expires/Max-Age: a session cookie, matching a token that is
//     minted per launch and never persisted.
//   - Secure only under TLS: a request that arrived on the listener's
//     cleartext half would simply never store a Secure cookie. r.TLS is
//     the signal, and NOT the forwarded-proto header that deriveWSURL
//     reads (requestIsHTTPS says why the two differ): a caller-supplied
//     header is tolerable where being wrong produces a URL that does not
//     connect, and not where being wrong produces a credential the
//     browser drops. A proxy that terminates TLS also still reaches a
//     browser that accepts a non-Secure cookie over https, so honoring
//     it here would buy nothing either.
func (c *Credential) pageCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     pageCookieName(r.Host),
		Value:    c.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	}
}

// pageCookieName derives the cookie name from the authority the client
// dialled, so the name is a fact about what this server serves rather
// than a constant two instances could collide on. Both the issuing and
// the reading side derive it from the same request field, so a client
// that reaches the same listener the same way always agrees with us.
func pageCookieName(host string) string { return cookieNameForHost(pageCookiePrefix, host) }

// cookieNameForHost is the shared derivation, so the page cookie and the
// session cookie (authroutes.go) can never disagree about which instance
// they belong to.
func cookieNameForHost(prefix, host string) string {
	port := ""
	if _, p, err := net.SplitHostPort(host); err == nil {
		port = p
	}
	if port == "" {
		// A hostname with no port (a reverse proxy fronting us on 443).
		// One name is right there: the authority itself is unique.
		return strings.TrimSuffix(prefix, "_")
	}
	return prefix + port
}

// bearerToken extracts the credential from an Authorization header,
// returning "" when the header is absent or not a bearer.
func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// OriginAllowed reports whether r may be served given the origins this
// server actually serves.
//
// A request with no Origin header is a client that is not a browser (or
// a same-origin GET, which browsers send without one) and passes: the
// credential and the peer rules are its gate. A request WITH an Origin
// passes only when that origin is the one this request was addressed to,
// or matches the configured allow-list a LAN bind adds.
//
// This is what stands between the page cookie and another local page.
// Cookies are scoped by host, not by port, so a page served by any other
// listener on this host is sent our cookie by the browser, and a
// WebSocket handshake is not subject to the cross-origin read rules that
// keep such a page from reading /bootstrap.json. Without this check that
// page could open an authenticated socket with a credential it can
// neither see nor need to see. With it, the handshake is refused before
// the credential is even looked at.
//
// The allowed origin is derived from the request (scheme from the TLS
// state, authority from the Host header) rather than from a stored
// string, so it stays true across a rebind, a port change, and every
// spelling of loopback the host resolves. In loopback mode the Host
// header itself is already constrained by the loopback host guard.
//
// One origin is admitted that no request can derive: the phone shell's
// (shellorigin.go). It serves the same bundle from its own fixed origin
// and reaches this backend across a network, so its every request is
// cross-origin — and it holds no cookie here, which is why admitting it
// widens nothing this function was protecting. Every route behind it
// still demands its own credential.
func OriginAllowed(r *http.Request, patterns []string) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		return true
	}
	origin, err := url.Parse(raw)
	if err != nil || origin.Host == "" {
		// Includes the literal "null" origin a sandboxed document sends,
		// which names no authority and can never be ours.
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if strings.EqualFold(origin.Host, r.Host) && strings.EqualFold(origin.Scheme, scheme) {
		return true
	}
	// The phone shell's fixed page origin, which is cross-origin by
	// construction rather than by configuration and so cannot be derived
	// from this request. One helper answers it for both callers of this
	// function — the HTTP routes and the WebSocket upgrade — and for the
	// CORS middleware beside it, so "may open a socket" and "may read the
	// answer" cannot drift apart (shellorigin.go).
	if shellOriginAllowed(raw) {
		return true
	}
	for _, pattern := range patterns {
		if originMatchesPattern(origin, pattern) {
			return true
		}
	}
	return false
}

// originMatchesPattern applies one allow-list entry. A pattern carrying
// a scheme is matched against `scheme://host`, otherwise against the
// host alone — the shape internal/network.OriginPatterns emits, and the
// same rule the WebSocket library applied when it owned this check.
func originMatchesPattern(origin *url.URL, pattern string) bool {
	target := origin.Host
	if strings.Contains(pattern, "://") {
		target = origin.Scheme + "://" + origin.Host
	}
	matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(target))
	return err == nil && matched
}
