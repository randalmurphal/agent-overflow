package transport

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The port gateway: one TLS listener per port in this machine's preview
// set, each reverse-proxying to the dev server on the same port number
// of loopback (docs/specs/remote-access.md §7).
//
// What the dev server on the other end expects of the headers this
// forwards is recorded, with the version it was verified against, in
// docs/references/dev-server-proxy.md. Build to that file, not to
// guesses: two of its facts (the Host check on the UPGRADE, and the
// path having to match `base` byte for byte) fail in ways that look like
// they work.
//
// Three properties are the design:
//
//   - THE SAME PORT NUMBER on both sides. A dev server's absolute URLs,
//     its `<base href>` and every HMR client that derives its socket
//     from `location.host` keep working unmodified only if the port does
//     not move. A path-prefixed proxy would break Vite's client, which
//     is the client that matters.
//
//   - ITS OWN ORIGIN, NEVER THE APP'S. The bytes are agent-authored
//     (`internal/surfaces`, AuthorAgentOrUser) and a different port is a
//     different origin, so nothing served here reaches the SPA's scripts
//     or storage. What a shared HOST still leaks is cookies, which is
//     why the preview cookie is named per port and why the app's own
//     page cookie is honoured only by routes that also check an
//     exact-port Origin allow-list.
//
//   - THE SESSION CREDENTIAL NEVER ARRIVES HERE. A minted URL carries a
//     single-use ticket; the first hit spends it for an opaque cookie
//     and redirects to the same address without it, so a reload or a
//     pasted address bar carries nothing. Every later request re-checks
//     the principal against the live-session store, so revoking a device
//     ends its previews on the next request and a restart ends them all.

const (
	// previewTicketParam is the query parameter a minted URL carries.
	previewTicketParam = "ao_preview"

	// previewCookiePrefix names the cookie per PORT. The host is shared
	// with the SPA and with every other preview, and browsers scope
	// cookies by host, so the name is the only thing that keeps one
	// preview's grant out of another's requests.
	previewCookiePrefix = ReservedCookiePrefix + "preview_"

	// previewTicketTTL is how long a minted URL stays spendable. It is
	// produced by a click and opened immediately; a minute covers a slow
	// hand-off to another device and nothing longer.
	previewTicketTTL = 60 * time.Second

	// previewTicketMax bounds the outstanding book. A person opening
	// previews has one or two in flight; the cap is what stops a caller
	// that mints without ever opening from growing it.
	previewTicketMax = 32

	// previewGrantTTL is how long a spent ticket's cookie lasts. Long
	// enough for a working session with a dev server, short enough that
	// an unattended device stops being able to reach one overnight.
	previewGrantTTL = 12 * time.Hour

	// previewGrantMax bounds the live grants. One person on a handful of
	// devices with a handful of previews is a handful of entries; the
	// cap is the ceiling on a process that never restarts.
	previewGrantMax = 256

	// previewShutdownTimeout bounds the graceful stop of one retired
	// preview listener. Same budget the auxiliary listeners get, and it
	// covers the ordinary HTTP handlers that are still running.
	//
	// It does NOT cover the upgraded ones, in either direction: net/http
	// STOPS TRACKING a connection the moment a handler takes it over from
	// the server, which is what an UPGRADE does, so Shutdown
	// neither waits for an HMR socket nor severs it, and the listener's
	// own Close only stops it accepting new ones. A retired port left
	// every socket it had handed out streaming. Those are cut before this
	// budget starts (previewconns.go).
	previewShutdownTimeout = 5 * time.Second

	// previewLivenessInterval is how often the connections already open
	// are re-checked against their grant. Every REQUEST re-checks it
	// (previewproxy.go admits), which is the whole mechanism for a
	// browser that keeps asking for things; an upgraded socket asks for
	// nothing more after the upgrade, so it needs a clock instead.
	//
	// Coarse on purpose. The check exists so a revoked device stops
	// receiving within a human's idea of "at once", not within a frame,
	// and every tick costs one session-store read per open preview.
	previewLivenessInterval = 10 * time.Second

	// previewReadHeaderTimeout bounds a preview connection's headers.
	// The same budget the main server uses by default, and it is what
	// keeps an opened-but-silent connection from holding a slot.
	previewReadHeaderTimeout = 15 * time.Second
)

// PreviewGateway holds one listener per port in the preview set.
//
// It is constructed once and reconciled with SetPorts. The zero value is
// not usable; NewPreviewGateway builds one.
type PreviewGateway struct {
	sources     []PreviewListenerSource
	sessionLive func(sessionID string) bool
	now         func() time.Time
	tickets     *ticketBook

	// stop ends the liveness sweep. Closed exactly once, by Close.
	stop chan struct{}

	mu     sync.Mutex
	closed bool
	// ports is the live listener per port. A port in the preview set
	// with no entry here is a port whose bind did not work; its note
	// says why.
	ports map[int]*previewListener
	// notes is why a port in the set is not being served, keyed by port.
	// Read back into the dev-server list, so the sentence a person sees
	// is the one this file wrote.
	notes map[int]string
	// grants maps an opaque cookie token to what it admits.
	grants map[string]previewGrant
}

// previewListener is one served port.
type previewListener struct {
	ln   net.Listener
	srv  *http.Server
	host string
	// scheme is what this listener's proxy dials the dev server with. It
	// is part of the listener's identity: a port that changed scheme is a
	// different upstream, so SetPorts rebuilds rather than keeping one
	// that would 502.
	scheme string
	// conns is what this listener is currently carrying, so retiring it
	// and revoking a principal both reach an upgraded socket that
	// nothing else can (previewconns.go).
	conns *previewConns
}

// previewGrant is what one preview cookie admits.
type previewGrant struct {
	// sessionID is the principal. EMPTY means the host presence itself,
	// which is the principal for a preview opened from the page on this
	// machine: there is no session row to re-check, and the grant dies
	// with the process because this map is in memory.
	sessionID string
	// port is the listener the cookie was minted on. Checked on every
	// request, so a cookie that reached another port's handler through
	// a shared host buys nothing there.
	port           int
	expiresAtNanos int64
}

// PreviewGatewayConfig is what a gateway needs from the outside.
type PreviewGatewayConfig struct {
	// Sources are tried in order for every bind.
	Sources []PreviewListenerSource

	// SessionLive re-checks a principal on EVERY request. It is the same
	// conjunction the WebSocket path consults (the session's own row and
	// its device's row, both unrevoked), and nothing here may cache its
	// answer: re-asking is the whole mechanism. Nil means no session is
	// ever live, which refuses every non-host principal rather than
	// admitting one.
	SessionLive func(sessionID string) bool

	// Now is injectable so tests move time instead of sleeping.
	Now func() time.Time
}

// NewPreviewGateway builds a gateway that is serving nothing yet.
func NewPreviewGateway(cfg PreviewGatewayConfig) *PreviewGateway {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	tickets := newTicketBook(previewTicketMax, previewTicketTTL)
	tickets.now = now
	g := &PreviewGateway{
		sources:     cfg.Sources,
		sessionLive: cfg.SessionLive,
		now:         now,
		tickets:     tickets,
		stop:        make(chan struct{}),
		ports:       make(map[int]*previewListener),
		notes:       make(map[int]string),
		grants:      make(map[string]previewGrant),
	}
	go g.sweepLiveness()
	return g
}

// sweepLiveness re-checks every open preview connection against its
// principal on a coarse tick, and severs the ones whose principal stopped
// being live. One goroutine for the whole gateway: the work is a map walk
// per listener and is nothing at all while no preview is open.
func (g *PreviewGateway) sweepLiveness() {
	ticker := time.NewTicker(previewLivenessInterval)
	defer ticker.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-ticker.C:
			g.cutRefusedConns()
		}
	}
}

// cutRefusedConns is one pass of the sweep, called directly by the tests
// so they move nothing but the clock and the session store. Each
// listener's connections are re-asked the question its port admitted
// them on (stillAdmits), so a lapsed grant and a revoked principal end a
// held socket alike.
func (g *PreviewGateway) cutRefusedConns() {
	type held struct {
		port  int
		conns *previewConns
	}
	g.mu.Lock()
	listeners := make([]held, 0, len(g.ports))
	for port, listener := range g.ports {
		listeners = append(listeners, held{port: port, conns: listener.conns})
	}
	g.mu.Unlock()
	for _, entry := range listeners {
		port := entry.port
		entry.conns.cutRefused(func(token string) bool { return g.stillAdmits(token, port) })
	}
}

// Notes returns why each unserved port in the set is unserved, so the
// dev-server list can carry the sentence rather than inventing one.
func (g *PreviewGateway) Notes() map[int]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.notes) == 0 {
		return nil
	}
	notes := make(map[int]string, len(g.notes))
	for port, note := range g.notes {
		notes[port] = note
	}
	return notes
}

// MintURL returns the URL that opens port's dev server from another
// device, carrying a single-use ticket bound to this principal and this
// port. sessionID is empty for the host presence.
//
// path is the dev server's own path, taken verbatim from the link the
// person clicked; it is preserved byte for byte, because a dev server's
// upgrade is routed only when the path matches its `base` exactly.
func (g *PreviewGateway) MintURL(sessionID string, port int, path string) (string, error) {
	g.mu.Lock()
	listener := g.ports[port]
	note := g.notes[port]
	g.mu.Unlock()
	if listener == nil {
		if note == "" {
			note = fmt.Sprintf("Port %d is not shared from this machine.", port)
		}
		return "", errors.New(note)
	}

	// The path is validated BEFORE anything is minted. A ticket book is
	// bounded and evicts its oldest entry to make room, so a call that
	// was always going to be refused would still have spent a slot and
	// invalidated a ticket some other page was about to present.
	target, err := url.Parse(path)
	if err != nil || target.IsAbs() {
		// A path that is not a path is the caller's bug, and guessing at
		// what was meant would produce a link to somewhere nobody named.
		return "", fmt.Errorf("preview path must be a relative path, got %q", path)
	}
	if !strings.HasPrefix(target.Path, "/") {
		target.Path = "/" + target.Path
	}

	ticket, err := g.tickets.mint(previewSubject(sessionID, port))
	if err != nil {
		return "", err
	}

	query := target.Query()
	query.Set(previewTicketParam, ticket)
	target.RawQuery = query.Encode()
	target.Scheme = "https"
	target.Host = net.JoinHostPort(listener.host, strconv.Itoa(port))
	return target.String(), nil
}

// previewSubject encodes what a ticket buys. The PORT is in the subject
// so a ticket minted for one preview cannot buy a cookie on another,
// which matters because every preview on this machine shares a host.
func previewSubject(sessionID string, port int) string {
	return strconv.Itoa(port) + "\n" + sessionID
}

// parsePreviewSubject reads back what previewSubject wrote.
func parsePreviewSubject(subject string) (sessionID string, port int, ok bool) {
	head, tail, found := strings.Cut(subject, "\n")
	if !found {
		return "", 0, false
	}
	port, err := strconv.Atoi(head)
	if err != nil {
		return "", 0, false
	}
	return tail, port, true
}

// Ports returns the ports currently served, sorted.
func (g *PreviewGateway) Ports() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	ports := make([]int, 0, len(g.ports))
	for port := range g.ports {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

// Close retires every listener and refuses every later request. The
// grants go with it, which is the restart half of the spec's rule: a
// backend that restarted ends every preview it had handed out.
func (g *PreviewGateway) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	close(g.stop)
	listeners := make([]*previewListener, 0, len(g.ports))
	for port, listener := range g.ports {
		listeners = append(listeners, listener)
		delete(g.ports, port)
	}
	g.notes = make(map[int]string)
	g.grants = make(map[string]previewGrant)
	g.mu.Unlock()

	for _, listener := range listeners {
		closePreviewListener(listener)
	}
}
