package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
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
	previewCookiePrefix = "ao_preview_"

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
	// covers in-flight HTTP handlers only: an upgraded HMR socket owns
	// its own connection and the final Close is what severs it.
	previewShutdownTimeout = 5 * time.Second

	// previewReadHeaderTimeout bounds a preview connection's headers.
	// The same budget the main server uses by default, and it is what
	// keeps an opened-but-silent connection from holding a slot.
	previewReadHeaderTimeout = 15 * time.Second
)

// PreviewLANSource serves previews on this machine's LAN address under
// the app's own certificate. It is the fallback the spec names for a
// backend with no tailnet but LAN access on, and it is the reason the
// gateway takes an ORDERED list of sources rather than one.
//
// The address is fixed at construction. A LAN IP that changed means a
// different network, which is a rebind of the main listener too.
type PreviewLANSource struct {
	// LANIP is the address to bind, or "" when this backend is on
	// loopback only — in which case the source serves nothing and the
	// gateway falls through to the next one, or to a note.
	LANIP string

	// TLS is the app's certificate configuration, the same object the
	// main bind serves with. Nil means there is no certificate at all,
	// and a preview cookie cannot be set over cleartext.
	TLS *tls.Config
}

// PreviewLANSource returns the LAN source for this server's certificate
// configuration. The address is the caller's, because which of this
// machine's addresses is "the LAN one" is a question this package does
// not answer.
func (s *Server) PreviewLANSource(lanIP string) *PreviewLANSource {
	return &PreviewLANSource{LANIP: lanIP, TLS: s.tlsConfig}
}

// PreviewHost is the LAN address, or "" when there is nothing to serve
// on or nothing to serve it with.
func (p *PreviewLANSource) PreviewHost() string {
	if p == nil || p.LANIP == "" || p.TLS == nil {
		return ""
	}
	return p.LANIP
}

// ListenPreview binds this machine's LAN address on port, under TLS.
//
// The bind is to the LAN ADDRESS specifically, not to 0.0.0.0: the dev
// server already holds the same port on loopback, and a wildcard bind
// would collide with it on every machine.
func (p *PreviewLANSource) ListenPreview(port int) (net.Listener, error) {
	if p.PreviewHost() == "" {
		return nil, errors.New("no LAN address to serve previews on")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(p.LANIP, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return tls.NewListener(ln, p.TLS), nil
}

// PreviewListenerSource opens the listener for one preview port. Two
// implementations exist: the tailnet node (internal/app, beside the node
// it needs) and PreviewLANSource above. They are tried in the order the
// caller lists them, and the first that answers wins — which is what
// makes the tailnet the preferred address without anything here knowing
// why it is preferred.
type PreviewListenerSource interface {
	// PreviewHost is the authority a URL served by this source names,
	// or "" when the source cannot serve anything right now. Asked
	// before every bind, because a tailnet node comes and goes.
	PreviewHost() string

	// ListenPreview opens a TLS listener on port. The listener is TLS
	// on every path: the preview cookie is `Secure`, and a browser will
	// not store one from a cleartext origin that is not localhost.
	ListenPreview(port int) (net.Listener, error)
}

// PreviewGateway holds one listener per port in the preview set.
//
// It is constructed once and reconciled with SetPorts. The zero value is
// not usable; NewPreviewGateway builds one.
type PreviewGateway struct {
	sources     []PreviewListenerSource
	sessionLive func(sessionID string) bool
	now         func() time.Time
	tickets     *ticketBook

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
	return &PreviewGateway{
		sources:     cfg.Sources,
		sessionLive: cfg.SessionLive,
		now:         now,
		tickets:     tickets,
		ports:       make(map[int]*previewListener),
		notes:       make(map[int]string),
		grants:      make(map[string]previewGrant),
	}
}

// SetPorts reconciles the served set against want: bind what is new,
// retire what is gone, leave what is unchanged alone. Called on every
// discovery tick, so doing nothing when nothing moved is the common
// case and the only one that has to be cheap.
//
// A bind that fails is not an error the caller has to handle — the other
// ports still serve — so the reason is recorded as this port's note and
// the next tick tries again.
func (g *PreviewGateway) SetPorts(want []int) {
	wanted := make(map[int]struct{}, len(want))
	for _, port := range want {
		if port > 0 && port <= 65535 {
			wanted[port] = struct{}{}
		}
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	var retire []*previewListener
	for port, listener := range g.ports {
		if _, keep := wanted[port]; keep {
			continue
		}
		retire = append(retire, listener)
		delete(g.ports, port)
	}
	for port := range g.notes {
		if _, keep := wanted[port]; !keep {
			delete(g.notes, port)
		}
	}
	var fresh []int
	for port := range wanted {
		if _, served := g.ports[port]; !served {
			fresh = append(fresh, port)
		}
	}
	g.mu.Unlock()

	for _, listener := range retire {
		closePreviewListener(listener)
	}
	// Sorted, so the log of a machine that could bind nothing reads in
	// port order rather than in map order.
	sort.Ints(fresh)
	for _, port := range fresh {
		g.bind(port)
	}
}

// bind opens one port through the first source that answers.
func (g *PreviewGateway) bind(port int) {
	var reasons []string
	for _, source := range g.sources {
		host := source.PreviewHost()
		if host == "" {
			continue
		}
		ln, err := source.ListenPreview(port)
		if err != nil {
			reasons = append(reasons, previewBindReason(port, err))
			continue
		}
		// A mux with one pattern rather than the handler directly: the
		// whole port is one route, and registering it says so where
		// internal/surfaces' gate can read it.
		mux := http.NewServeMux()
		mux.Handle("/", g.handler(port))
		listener := &previewListener{
			ln:   ln,
			srv:  &http.Server{Handler: mux, ReadHeaderTimeout: previewReadHeaderTimeout},
			host: host,
		}
		g.mu.Lock()
		if g.closed || g.ports[port] != nil {
			// A concurrent SetPorts or Close got there first. Whatever it
			// installed is the live one.
			g.mu.Unlock()
			closePreviewListener(listener)
			return
		}
		g.ports[port] = listener
		delete(g.notes, port)
		g.mu.Unlock()

		go func() {
			err := listener.srv.Serve(ln)
			if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("transport: preview listener on port %d stopped: %v", port, err)
			g.drop(port, listener)
		}()
		return
	}

	note := "No tailnet or LAN address to share this port on."
	if len(reasons) > 0 {
		note = strings.Join(reasons, " ")
	}
	g.mu.Lock()
	if !g.closed {
		g.notes[port] = note
	}
	g.mu.Unlock()
}

// previewBindReason turns a failed bind into the sentence a person
// reads. The address-in-use case is the one worth naming: it means the
// dev server itself already bound beyond loopback on that port, so the
// page is reachable already and a proxy in front of it would be a second
// answer to a question nobody asked.
func previewBindReason(port int, err error) string {
	if addrInUse(err) {
		return fmt.Sprintf("Port %d is already reachable on the LAN.", port)
	}
	return fmt.Sprintf("Port %d could not be opened for previews: %v", port, err)
}

// drop retires a listener that stopped accepting, unless something has
// already replaced it.
func (g *PreviewGateway) drop(port int, listener *previewListener) {
	g.mu.Lock()
	if g.ports[port] == listener {
		delete(g.ports, port)
	}
	g.mu.Unlock()
	closePreviewListener(listener)
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

	ticket, err := g.tickets.mint(previewSubject(sessionID, port))
	if err != nil {
		return "", err
	}

	target, err := url.Parse(path)
	if err != nil || target.IsAbs() {
		// A path that is not a path is the caller's bug, and guessing at
		// what was meant would produce a link to somewhere nobody named.
		return "", fmt.Errorf("preview path must be a relative path, got %q", path)
	}
	if !strings.HasPrefix(target.Path, "/") {
		target.Path = "/" + target.Path
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

func closePreviewListener(listener *previewListener) {
	ctx, cancel := context.WithTimeout(context.Background(), previewShutdownTimeout)
	defer cancel()
	if err := listener.srv.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("transport: shut down preview listener: %v", err)
	}
	// Shutdown closes the listener it was serving; a Serve that never
	// started leaves it open.
	if err := listener.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("transport: close preview listener: %v", err)
	}
}
