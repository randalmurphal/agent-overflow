package transport

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The /ws admission matrix (docs/specs/remote-access.md §4, "Local
// clients"): the launch credential alone admits an upgrade only from
// this machine, and a peer that is not on it must NAME a live durable
// session.
//
// HOW A NON-LOOPBACK PEER IS PRODUCED here. The predicate under test is
// loopback.PeerAddress over the kernel-reported r.RemoteAddr, and a test
// process cannot make the kernel report a LAN address for a connection
// it makes to itself. So the fixture serves the SAME *http.Server the
// production boot builds over a second listener whose accepted
// connections report a non-loopback peer — net/http copies exactly that
// string into Request.RemoteAddr, so handleWS reads the input a real LAN
// browser produces. Everything past that point is the real path: the
// real mux, the real Origin check, the real credential, the real
// handshake.

// remotePeerAddr is the peer every connection on the fixture's second
// listener reports. TEST-NET-1 (RFC 5737), which is documentation-only
// and can never be a real peer of anything.
const remotePeerAddr = "192.0.2.7:52001"

type lanAddr struct{}

func (lanAddr) Network() string { return "tcp" }
func (lanAddr) String() string  { return remotePeerAddr }

type remotePeerConn struct{ net.Conn }

func (c remotePeerConn) RemoteAddr() net.Addr { return lanAddr{} }

type remotePeerListener struct{ net.Listener }

func (l remotePeerListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return remotePeerConn{conn}, nil
}

// admissionFixture is one server reachable two ways: its own listener,
// whose peers really are loopback, and a second listener carrying the
// same handler whose peers read as a LAN address.
type admissionFixture struct {
	srv      *Server
	loopback string
	remote   string
}

// admissionSessionCredential is the one credential the fixture's session
// hook recognises, and admissionSessionID is what it resolves to.
const (
	admissionSessionCredential = "session-credential-under-test"
	admissionSessionID         = "sess-admission"
)

func newAdmissionFixture(t *testing.T) *admissionFixture {
	return newAdmissionFixtureWith(t, nil)
}

// newAdmissionFixtureWith lets a case drop one of the session hooks
// before Start, which is the only safe moment: every hook is read live
// off Config by the request goroutines.
func newAdmissionFixtureWith(t *testing.T, mutate func(*Config)) *admissionFixture {
	t.Helper()
	d := NewDispatcher()
	if _, err := d.Register(&integrationStub{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg := Config{
		Dispatcher: d,
		EventBus:   NewEventBus(8),
		Token:      "admission-token",
		// The production shape: a request presenting no session
		// credential proceeds naming none, and one presenting the known
		// credential names its session.
		SessionForRequest: func(r *http.Request) (string, bool) {
			switch SessionCredential(r) {
			case "":
				return "", true
			case admissionSessionCredential:
				return admissionSessionID, true
			default:
				return "", false
			}
		},
		SessionLive: func(id string) bool { return id == admissionSessionID },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the remote-peer leg: %v", err)
	}
	// Built after Start, because BaseContext reads the root context Start
	// installs. Same handler, same credential, same ticket book — only
	// the reported peer differs.
	httpSrv := srv.buildHTTPServer()
	go func() { _ = httpSrv.Serve(remotePeerListener{listener}) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})

	return &admissionFixture{srv: srv, loopback: srv.Addr(), remote: listener.Addr().String()}
}

// dial performs one upgrade and reports the refusal status, or 101 when
// the socket opened. A refusal is a status rather than an error string
// because the whole point of the shape is that every refusal looks the
// same on the wire.
func (f *admissionFixture) dial(t *testing.T, addr, query string, header http.Header) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws://" + addr + "/ws"
	if query != "" {
		url += "?" + query
	}
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return http.StatusSwitchingProtocols
	}
	if resp == nil {
		t.Fatalf("dial %s: %v (no HTTP response to read a status from)", url, err)
	}
	return resp.StatusCode
}

func sessionHeader() http.Header {
	return http.Header{SessionCredentialHeader: []string{admissionSessionCredential}}
}

func TestUpgradeAdmitsTheLaunchCredentialOnlyFromThisMachine(t *testing.T) {
	f := newAdmissionFixture(t)

	cases := []struct {
		name   string
		addr   string
		query  string
		header http.Header
		want   int
	}{{
		// Every one of this host's own processes: the embedded webview's
		// page cookie, ao-harness, the e2e rig, the WSL launcher's
		// notification socket before it has a credential to forward.
		name:  "loopback peer, launch credential, no session",
		addr:  f.loopback,
		query: "token=admission-token",
		want:  http.StatusSwitchingProtocols,
	}, {
		name:   "loopback peer naming a session",
		addr:   f.loopback,
		query:  "token=admission-token",
		header: sessionHeader(),
		want:   http.StatusSwitchingProtocols,
	}, {
		// The paired device, and the same shape a relay forwarding the
		// local channel's credential arrives in.
		name:   "non-loopback peer naming a session",
		addr:   f.remote,
		query:  "token=admission-token",
		header: sessionHeader(),
		want:   http.StatusSwitchingProtocols,
	}, {
		// The change this test exists for: a LAN browser holding the
		// page cookie the share URL planted, and nothing else.
		name:  "non-loopback peer with the launch credential alone",
		addr:  f.remote,
		query: "token=admission-token",
		want:  http.StatusNotFound,
	}, {
		name: "non-loopback peer presenting nothing",
		addr: f.remote,
		want: http.StatusNotFound,
	}, {
		// Not a new refusal — the credential gate already answered this
		// one — but it must stay indistinguishable from the rule above.
		name:  "non-loopback peer with a session credential the hook refuses",
		addr:  f.remote,
		query: "token=admission-token",
		header: http.Header{
			SessionCredentialHeader: []string{"a credential this backend never issued"},
		},
		want: http.StatusNotFound,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.dial(t, tc.addr, tc.query, tc.header); got != tc.want {
				t.Fatalf("upgrade status = %d, want %d", got, tc.want)
			}
		})
	}
}

// A ticket is how a paired browser names its session: it cannot set a
// header on a WebSocket handshake, and after a backend restart it holds
// no page credential either. So the ticket arm has to admit a
// non-loopback peer carrying NO launch credential at all, or the one
// client this wave exists to serve could never connect.
func TestUpgradeAdmitsANonLoopbackPeerOnATicketAlone(t *testing.T) {
	f := newAdmissionFixture(t)

	ticket, err := f.srv.wsTickets.mint(admissionSessionID)
	if err != nil {
		t.Fatalf("mint ws ticket: %v", err)
	}
	if got := f.dial(t, f.remote, WSTicketParam+"="+ticket, nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("ticketed upgrade status = %d, want %d", got, http.StatusSwitchingProtocols)
	}
}

// TestBootstrapPlantsTheLocalChannelForLoopbackPeersOnly is the PLANTING
// half of the binding-class rule (docs/specs/remote-access.md §2).
//
// The credential Config.PageSessionCredential hands out is the backend's
// own `loopback-only` session, and the presentation side refuses one from
// an off-host peer (internal/app bindingAdmitsPeer). Planting it there
// anyway would hand a page a credential that stops working the moment it
// is used, so the two ends state one rule.
//
// The share URL still LOADS: the person holding it has to reach the SPA's
// pairing prompt, so the page cookie is planted exactly as before. What
// the off-host exchange no longer carries is the local channel.
func TestBootstrapPlantsTheLocalChannelForLoopbackPeersOnly(t *testing.T) {
	const credential = "ao1.local-channel-credential"
	f := newAdmissionFixtureWith(t, func(cfg *Config) {
		cfg.PageSessionCredential = func() string { return credential }
	})
	f.srv.MarkReady()

	for _, tc := range []struct {
		name        string
		addr        string
		wantSession bool
	}{
		{name: "loopback peer", addr: f.loopback, wantSession: true},
		{name: "off-host peer", addr: f.remote, wantSession: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A page ticket, because that is what a share URL carries and
			// what makes the exchange plant the page cookie at all.
			ticket, err := f.srv.MintPageTicket()
			if err != nil {
				t.Fatalf("mint page ticket: %v", err)
			}
			resp, err := http.Get("http://" + tc.addr + "/bootstrap.json?" + PageTicketParam + "=" + ticket)
			if err != nil {
				t.Fatalf("bootstrap request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("bootstrap status = %d, want 200 — the share URL must keep loading the page", resp.StatusCode)
			}

			page, session := false, false
			for _, cookie := range resp.Cookies() {
				switch {
				case strings.HasPrefix(cookie.Name, sessionCookiePrefix):
					session = true
					if cookie.Value != credential {
						t.Fatalf("session cookie value = %q", cookie.Value)
					}
				case strings.HasPrefix(cookie.Name, pageCookiePrefix):
					page = true
				}
			}
			if !page {
				t.Error("the exchange planted no page cookie; the pairing prompt could not load")
			}
			if session != tc.wantSession {
				t.Errorf("session cookie planted = %t, want %t", session, tc.wantSession)
			}
		})
	}
}

// The rule is about the peer, not about the hook being wired: a server
// that cannot resolve a session at all cannot admit a session-naming
// connection either, so off-host peers are refused rather than admitted
// unattributable.
func TestUpgradeRefusesEveryNonLoopbackPeerWithNoSessionResolver(t *testing.T) {
	f := newAdmissionFixtureWith(t, func(cfg *Config) { cfg.SessionForRequest = nil })

	if got := f.dial(t, f.remote, "token=admission-token", sessionHeader()); got != http.StatusNotFound {
		t.Fatalf("upgrade status = %d, want %d", got, http.StatusNotFound)
	}
	// The host's own processes are unaffected by the hook being absent,
	// which is what keeps a boot whose session core failed usable from
	// the desktop.
	if got := f.dial(t, f.loopback, "token=admission-token", nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("loopback upgrade status = %d, want %d", got, http.StatusSwitchingProtocols)
	}
}
