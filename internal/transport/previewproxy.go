package transport

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/loopback"
)

// One preview port's handler: the ticket exchange, the cookie, the
// per-request principal check, and the reverse proxy itself.
//
// Every header rule below is a VERIFIED fact about what a dev server
// does, recorded in docs/references/dev-server-proxy.md with the version
// and date it was verified against. Change one only with a new spike.

// previewEndedPage is what a request with no valid cookie gets. Plain
// text, deliberately: these bytes are served from an origin classified
// AuthorAgentOrUser, and a page with no markup is one with nothing to
// get wrong.
const previewEndedPage = "This preview session ended. Open the link again from Agent Overflow.\n"

// previewUpstreamDialTimeout bounds the dial to the dev server. It is on
// loopback, so this only ever covers a port that accepts and says
// nothing.
const previewUpstreamDialTimeout = 5 * time.Second

// handler builds the http.Handler for one preview port.
func (g *PreviewGateway) handler(target PreviewTarget, conns *previewConns) http.Handler {
	port := target.Port
	content := g.content
	if content == nil {
		content = g.proxy(target)
	}
	cookieName := previewCookiePrefix + strconv.Itoa(port)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ticket := r.URL.Query().Get(previewTicketParam); ticket != "" {
			if g.exchange(w, r, ticket, port, cookieName) {
				return
			}
			// A ticket that did not spend falls through to the cookie
			// check rather than refusing outright: a reload of an
			// already-spent URL is the common way to arrive here, and the
			// cookie that first hit bought is still good.
		}
		token, ok := g.admits(r, port, cookieName)
		if !ok {
			previewEnded(w)
			return
		}
		// Held for as long as this request is served, which for an
		// UPGRADE is the whole life of the socket: the reverse proxy does
		// not return until the copy in both directions is done. That one
		// registration is what lets a retired port and a revoked
		// principal each reach a connection nothing else can.
		release := conns.hold(r, token)
		defer release()
		content.ServeHTTP(w, r)
	})
}

// exchange spends a ticket for a cookie and redirects to the same
// address without it, so a reload or a shared address bar carries
// nothing. Reports whether it answered the request.
func (g *PreviewGateway) exchange(
	w http.ResponseWriter, r *http.Request, ticket string, port int, cookieName string,
) bool {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return false
	}
	g.exchanges++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.exchanges--
		g.mu.Unlock()
	}()
	subject, ok := g.tickets.consume(ticket)
	if !ok {
		return false
	}
	sessionID, ticketPort, parsed := parsePreviewSubject(subject)
	if !parsed || ticketPort != port {
		// A ticket minted for another port. Every preview on this machine
		// shares a host, so this is the check that keeps one preview's
		// ticket from buying another's cookie.
		return false
	}
	if !g.principalLive(sessionID) {
		// Revoked in the seconds the ticket was in flight. The ticket
		// NAMES a principal; it does not authorize one.
		return false
	}

	token, err := NewToken()
	if err != nil {
		log.Printf("transport: mint preview cookie: %v", err)
		return false
	}
	expires := g.now().Add(previewGrantTTL)
	g.storeGrant(token, previewGrant{sessionID: sessionID, port: port, expiresAtNanos: expires.UnixNano()})

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(previewGrantTTL / time.Second),
		HttpOnly: true,
		Secure:   g.scheme == "https",
		SameSite: http.SameSiteStrictMode,
	})

	redirect := *r.URL
	query := redirect.Query()
	query.Del(previewTicketParam)
	redirect.RawQuery = query.Encode()
	// Relative, so the redirect names the same origin whatever host the
	// browser used to get here.
	http.Redirect(w, r, redirect.RequestURI(), http.StatusFound)
	return true
}

// admits reports whether this request carries a live grant for this
// port, and names the grant's token when it does. The grant is re-checked
// EVERY time: revoking a device has to end its previews on the next
// request, and caching the answer is exactly what would stop that.
//
// The token comes back because the connection outlives the check on an
// upgrade, and the sweep that re-asks for it (stillAdmits) needs the
// grant this request was admitted on rather than whatever cookie the
// connection still carries.
func (g *PreviewGateway) admits(r *http.Request, port int, cookieName string) (token string, ok bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	if !g.stillAdmits(cookie.Value, port) {
		return "", false
	}
	return cookie.Value, true
}

// stillAdmits is the whole of a grant check: the grant exists, is for
// this port, has not lapsed, and names a principal that is still live.
// The same question on a request and on the sweep, so a grant that lapses
// under an open HMR socket is refused exactly the way it would be on the
// socket's next request, if it ever made one.
func (g *PreviewGateway) stillAdmits(token string, port int) bool {
	grant, found := g.grant(token)
	if !found || grant.port != port {
		return false
	}
	if !g.now().Before(time.Unix(0, grant.expiresAtNanos)) {
		g.dropGrant(token)
		return false
	}
	return g.principalLive(grant.sessionID)
}

// principalLive answers for both kinds of principal. An empty session id
// is the host presence, which has no row to re-check and lives as long
// as this process does.
func (g *PreviewGateway) principalLive(sessionID string) bool {
	if sessionID == "" {
		return true
	}
	if g.sessionLive == nil {
		return false
	}
	return g.sessionLive(sessionID)
}

func (g *PreviewGateway) storeGrant(token string, grant previewGrant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	if len(g.grants) >= previewGrantMax {
		g.evictGrantsLocked()
	}
	g.grants[token] = grant
}

// evictGrantsLocked drops lapsed grants, then the one closest to expiry
// if that was not enough, so a store always has room.
func (g *PreviewGateway) evictGrantsLocked() {
	nowNanos := g.now().UnixNano()
	for token, grant := range g.grants {
		if grant.expiresAtNanos <= nowNanos {
			delete(g.grants, token)
		}
	}
	for len(g.grants) >= previewGrantMax {
		oldestToken := ""
		oldest := int64(0)
		for token, grant := range g.grants {
			if oldestToken == "" || grant.expiresAtNanos < oldest {
				oldestToken, oldest = token, grant.expiresAtNanos
			}
		}
		delete(g.grants, oldestToken)
	}
}

func (g *PreviewGateway) grant(token string) (previewGrant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	grant, ok := g.grants[token]
	return grant, ok
}

func (g *PreviewGateway) dropGrant(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.grants, token)
}

// previewEnded is the one answer a request with no valid cookie gets.
// No security headers: this route is excluded from the policy by name
// (csp_test.go), because everything else it serves is the dev server's
// bytes under PostureProxied.
func previewEnded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(previewEndedPage))
}

// proxy builds the reverse proxy for one port.
//
// Every rewrite here is from docs/references/dev-server-proxy.md:
//
//   - HOST becomes localhost:<port>, on the HTTP path AND on the
//     WebSocket upgrade. Vite checks it on both and refuses anything
//     else (403 on HTTP, 400 on the upgrade); `localhost` always passes.
//     Rewriting only the HTTP half yields a page that loads and an HMR
//     socket that never connects, which is the failure that looks like
//     success.
//   - ORIGIN is REWRITTEN when present, never stripped, to the upstream's
//     OWN scheme. Vite does not
//     compare the value; what its presence does is make the HMR token
//     mandatory, and the token reaches the browser through this proxy
//     for free. Stripping would send a weaker request than the browser
//     made. Rewriting is also what Next.js 15+'s allowedDevOrigins check
//     wants, though that half is unverified.
//   - PATH AND QUERY go through byte for byte. The upgrade is routed
//     only when the path equals the dev server's `base` exactly, and a
//     changed path hangs the socket with no response at all.
//   - Sec-WebSocket-Protocol is forwarded unchanged (`vite-hmr`).
//     httputil copies it with every other header; it is named here so a
//     future header filter does not quietly drop it.
func (g *PreviewGateway) proxy(target PreviewTarget) *httputil.ReverseProxy {
	port := target.Port
	upstreamHost := net.JoinHostPort("localhost", strconv.Itoa(port))
	upstreamOrigin := target.Scheme + "://" + upstreamHost

	return &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext:         loopback.Dialer(previewUpstreamDialTimeout),
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			// A dev server serving TLS on loopback with a certificate
			// nothing can verify is the norm, and this hop never leaves
			// the machine: loopback.Dialer connects to a literal this
			// code chose, not to a name anything else resolved.
			// Verifying would refuse every https dev server and prove
			// nothing about the one hop involved. Same reasoning, same
			// wording, as the devscan probe that found it.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback-only, see above
		},
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = target.Scheme
			r.Out.URL.Host = upstreamHost
			// Both spellings, so a path with encoded segments in it
			// reaches the upstream exactly as it arrived.
			r.Out.URL.Path = r.In.URL.Path
			r.Out.URL.RawPath = r.In.URL.RawPath
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			r.Out.Host = upstreamHost
			if r.In.Header.Get("Origin") != "" {
				r.Out.Header.Set("Origin", upstreamOrigin)
			}
			// The dev server's bytes are agent-authored, and every cookie
			// this app sets is a credential. None has any business past
			// this proxy, so the whole reserved namespace goes rather
			// than this port's cookie alone.
			stripAppCookies(r.Out)
		},
		ModifyResponse: previewRewriteLocation(upstreamHost),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// The dev server is down or restarting. Say so as the dev
			// server's own condition rather than as a failure of this
			// app: the person is looking at somebody's `npm run dev`.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("The dev server on port " + strconv.Itoa(port) + " did not answer: " + err.Error() + "\n"))
		},
	}
}

// previewRewriteLocation sends a redirect that names the upstream back
// to the origin the browser is actually on. A dev server serving its app
// under a base path answers `/` with exactly this, and an unrewritten
// Location would send the browser to a localhost that is a different
// machine.
func previewRewriteLocation(upstreamHost string) func(*http.Response) error {
	return func(resp *http.Response) error {
		location := resp.Header.Get("Location")
		if location == "" {
			return nil
		}
		target, err := url.Parse(location)
		if err != nil || !target.IsAbs() {
			// A relative Location already names the preview origin.
			return nil
		}
		if !strings.EqualFold(target.Host, upstreamHost) && !previewIsLoopbackHost(target.Host, upstreamHost) {
			return nil
		}
		// Relative, so it resolves against whichever host the browser
		// used — this gateway does not know which of its addresses that
		// was, and does not need to.
		target.Scheme = ""
		target.Host = ""
		if target.Path == "" {
			// `http://localhost:5173` with no path would rewrite to the
			// empty string, which is not a Location at all: a browser
			// resolves it against the current URL and the redirect
			// becomes a loop. The authority-only form names the root.
			target.Path = "/"
		}
		resp.Header.Set("Location", target.String())
		return nil
	}
}

// previewIsLoopbackHost reports whether host is another spelling of the
// same upstream: a dev server that answers with `127.0.0.1:<port>` where
// we dialled `localhost:<port>` is naming itself.
func previewIsLoopbackHost(host, upstreamHost string) bool {
	_, upstreamPort, err := net.SplitHostPort(upstreamHost)
	if err != nil {
		return false
	}
	name, port, err := net.SplitHostPort(host)
	if err != nil || port != upstreamPort {
		return false
	}
	if strings.EqualFold(name, "localhost") {
		return true
	}
	addr := net.ParseIP(strings.Trim(name, "[]"))
	return addr != nil && addr.IsLoopback()
}

// stripAppCookies removes every cookie in this app's reserved namespace
// from an outbound request, leaving every other cookie the browser sent
// in place: a dev server may have set its own and would notice them
// going missing.
//
// A PREFIX rule rather than this port's cookie name, because a browser
// scopes cookies by host and ignores the port. A preview listener shares
// its host with the SPA and with every other preview, so the request
// arriving here carries the page cookie, the session cookie and the
// other ports' preview cookies as well as this one, and each of those is
// a credential the routes on the main listener honour. Dropping one name
// left the rest crossing to bytes this app does not author.
//
// Every app cookie name is derived from ReservedCookiePrefix, and
// TestEveryAppCookieUsesTheReservedPrefix is what keeps that true, so
// this rule cannot go stale behind a cookie somebody adds later.
func stripAppCookies(r *http.Request) {
	cookies := r.Cookies()
	kept := cookies[:0]
	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Name, ReservedCookiePrefix) {
			kept = append(kept, cookie)
		}
	}
	if len(kept) == len(cookies) {
		return
	}
	r.Header.Del("Cookie")
	for _, cookie := range kept {
		r.AddCookie(cookie)
	}
}
