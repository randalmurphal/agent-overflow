package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Where a preview listener comes from and how one is opened and retired:
// the ordered sources the gateway asks for an address, the shape of one
// port it is asked to serve, and the reconcile that binds what is new and
// cuts what is gone. The gateway's own state (tickets, grants and the
// URLs it mints) is previewgateway.go.
//
// The bind and the mux registration live in ONE file on purpose:
// internal/surfaces attributes a listener's routes to the file that binds
// it, so splitting them would leave the port's one route attributed to
// nothing.

// PreviewLANSource serves previews on this machine's LAN address under
// the app's own certificate. It is the fallback the spec names for a
// backend with no tailnet but LAN access on, and it is the reason the
// gateway takes an ORDERED list of sources rather than one.
//
// The address is answered per bind rather than captured, because LAN
// access is a setting somebody toggles and the address itself moves with
// the network.
type PreviewLANSource struct {
	// LANIP answers this machine's LAN address, or "" when the backend
	// is on loopback only — in which case the source serves nothing and
	// the gateway falls through to the next one, or to a note.
	//
	// A FUNCTION, because the answer moves: LAN access is a setting
	// somebody toggles, and the address itself changes with the network.
	// A value captured at construction would leave the gateway binding
	// an address this machine no longer has.
	LANIP func() string

	// TLS is the app's certificate configuration, the same object the
	// main bind serves with. Nil means there is no certificate at all,
	// and a preview cookie cannot be set over cleartext.
	TLS *tls.Config
}

// PreviewLANSource returns the LAN source for this server's certificate
// configuration. The address is the caller's to answer, because which of
// this machine's addresses is "the LAN one" is a question this package
// does not decide.
func (s *Server) PreviewLANSource(lanIP func() string) *PreviewLANSource {
	return &PreviewLANSource{LANIP: lanIP, TLS: s.tlsConfig}
}

// PreviewHost is the LAN address, or "" when there is nothing to serve
// on or nothing to serve it with.
func (p *PreviewLANSource) PreviewHost() string {
	if p == nil || p.LANIP == nil || p.TLS == nil {
		return ""
	}
	return p.LANIP()
}

// ListenPreview binds this machine's LAN address on port, under TLS.
//
// The bind is to the LAN ADDRESS specifically, not to 0.0.0.0: the dev
// server already holds the same port on loopback, and a wildcard bind
// would collide with it on every machine.
func (p *PreviewLANSource) ListenPreview(port int) (net.Listener, error) {
	host := p.PreviewHost()
	if host == "" {
		return nil, errors.New("no LAN address to serve previews on")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return tls.NewListener(ln, p.TLS), nil
}

// PreviewListenerSource opens the listener for one preview port. Network
// sources are the tailnet node (internal/app) and PreviewLANSource above.
// The private contentLoopbackSource serves on-host file previews only.
// Network sources are tried in the order the
// caller lists them, and the first that answers wins — which is what
// makes the tailnet the preferred address without anything here knowing
// why it is preferred.
type PreviewListenerSource interface {
	// PreviewHost is the authority a URL served by this source names,
	// or "" when the source cannot serve anything right now. Asked
	// before every bind, because a tailnet node comes and goes.
	PreviewHost() string

	// ListenPreview opens a listener on port. Network sources always use TLS.
	// Only the private, literal-loopback content source serves HTTP.
	ListenPreview(port int) (net.Listener, error)
}

// PreviewTarget is one port to serve a preview of, and what that port
// speaks. The scheme travels with the port from discovery all the way to
// the dial: a dev server on https that was proxied to over http answered
// every request with a gateway error, while the list happily called it
// previewable.
type PreviewTarget struct {
	Port   int
	Scheme string
}

// SetPorts reconciles the served set against want: bind what is new,
// retire what is gone, leave what is unchanged alone. Called on every
// discovery tick, so doing nothing when nothing moved is the common
// case and the only one that has to be cheap.
//
// A bind that fails is not an error the caller has to handle — the other
// ports still serve — so the reason is recorded as this port's note and
// the next tick tries again.
func (g *PreviewGateway) SetPorts(want []PreviewTarget) {
	wanted := make(map[int]string, len(want))
	for _, target := range want {
		if target.Port <= 0 || target.Port > 65535 {
			continue
		}
		scheme := target.Scheme
		if scheme != "https" {
			// http for anything unset or unrecognised. The two schemes
			// are the only ones a browser would render, and defaulting
			// is what keeps a discovery row that never got probed
			// serving rather than refused.
			scheme = "http"
		}
		wanted[target.Port] = scheme
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	var retire []*previewListener
	for port, listener := range g.ports {
		if scheme, keep := wanted[port]; keep && scheme == listener.scheme {
			continue
		}
		// Gone from the set, or still in it speaking something else.
		// Either way this listener is not the one to keep.
		retire = append(retire, listener)
		delete(g.ports, port)
	}
	for port := range g.notes {
		if _, keep := wanted[port]; !keep {
			delete(g.notes, port)
		}
	}
	var fresh []PreviewTarget
	for port, scheme := range wanted {
		if _, served := g.ports[port]; !served {
			fresh = append(fresh, PreviewTarget{Port: port, Scheme: scheme})
		}
	}
	g.mu.Unlock()

	for _, listener := range retire {
		closePreviewListener(listener)
	}
	// Sorted, so the log of a machine that could bind nothing reads in
	// port order rather than in map order.
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Port < fresh[j].Port })
	for _, target := range fresh {
		g.bind(target)
	}
}

// bind opens one port through the first source that answers.
func (g *PreviewGateway) bind(target PreviewTarget) {
	port := target.Port
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
		conns := &previewConns{}
		mux := http.NewServeMux()
		mux.Handle("/", g.handler(target, conns))
		listener := &previewListener{
			ln: ln,
			srv: &http.Server{
				Handler:           mux,
				ReadHeaderTimeout: previewReadHeaderTimeout,
				// ConnContext is the only place net/http offers the
				// accepted connection before the handler runs, and the
				// handler is the only place that knows which principal
				// this request was admitted for.
				ConnContext: withPreviewConn,
			},
			host:   host,
			scheme: target.Scheme,
			conns:  conns,
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

func closePreviewListener(listener *previewListener) {
	// The open connections go first, and the upgraded ones are why: a
	// connection a handler took over from the server is no longer tracked
	// by net/http, so neither the Shutdown below nor the listener Close
	// after it reaches one. Cutting is also what retiring a port MEANS:
	// the port is no longer shared, so nothing may still be streaming
	// from it.
	listener.conns.cutAll()
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
