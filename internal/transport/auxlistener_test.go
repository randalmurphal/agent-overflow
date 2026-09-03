package transport

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// An auxiliary listener is the tailnet's way in (spec §7, "Multi-listener,
// one session store"). Its peers are never on this machine, so the fixture
// reuses the admission tests' remotePeerListener: the same wrapper, the
// same TEST-NET-1 address, the same reason — net/http copies exactly that
// string into Request.RemoteAddr, so every rule downstream reads the input
// a real off-host client produces.
//
// What these cases hold is that attaching a second way in adds no second
// anything: the same routes, the same credential, the same admission rule,
// and a detach that touches nothing else.

// attachAux serves a loopback listener whose peers report a LAN address
// through ServeAuxiliary, and returns the handle plus the address to dial.
func attachAux(t *testing.T, srv *Server) (*AuxListener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the auxiliary leg: %v", err)
	}
	aux, err := srv.ServeAuxiliary(remotePeerListener{listener}, func(err error) {
		t.Errorf("auxiliary accept loop ended: %v", err)
	})
	if err != nil {
		t.Fatalf("serve auxiliary: %v", err)
	}
	t.Cleanup(func() { _ = aux.Close() })
	return aux, listener.Addr().String()
}

// getWithHost issues one GET, letting a case name the Host header
// independently of the address it dials — which is the only way to
// reproduce a browser that resolved a MagicDNS name to this listener.
func getWithHost(t *testing.T, addr, path, host string, header http.Header) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestAuxiliaryListenerAnswersTheSameRoutes is the whole promise: one
// server, one mux, one credential. A route that exists on the main bind
// exists here, and one that refuses there refuses here.
func TestAuxiliaryListenerAnswersTheSameRoutes(t *testing.T) {
	fixture := newAdmissionFixture(t)
	_, addr := attachAux(t, fixture.srv)

	if status, _ := getWithHost(t, addr, HealthPath, "", nil); status != http.StatusOK {
		t.Errorf("GET %s over the auxiliary listener = %d, want 200", HealthPath, status)
	}

	// No credential is refused with the same unfingerprintable 404 the
	// main bind answers with.
	if status, _ := getWithHost(t, addr, BootstrapPath, "", nil); status != http.StatusNotFound {
		t.Errorf("GET %s with no credential = %d, want 404", BootstrapPath, status)
	}

	// And the launch credential is honoured on exactly the routes it is
	// honoured on elsewhere. /bootstrap.json is deliberately not narrowed
	// by peer, because the person holding a share URL has to reach the
	// pairing prompt.
	status, _ := getWithHost(t, addr, BootstrapPath, "", http.Header{
		"Authorization": []string{"Bearer admission-token"},
	})
	if status != http.StatusOK {
		t.Errorf("GET %s with the launch credential = %d, want 200", BootstrapPath, status)
	}
}

// TestAuxiliaryListenerRequiresAnOffHostPeerToNameASession pins that the
// §4 admission rule is not something the main bind owns. A tailnet peer
// arrives at its 100.64/10 address, which is not this machine, so the
// launch credential alone must not open a socket nothing can revoke.
func TestAuxiliaryListenerRequiresAnOffHostPeerToNameASession(t *testing.T) {
	fixture := newAdmissionFixture(t)
	_, addr := attachAux(t, fixture.srv)

	if got := fixture.dial(t, addr, "token=admission-token", nil); got != http.StatusNotFound {
		t.Errorf("upgrade with the launch credential alone = %d, want 404", got)
	}
	if got := fixture.dial(t, addr, "token=admission-token", sessionHeader()); got != http.StatusSwitchingProtocols {
		t.Errorf("upgrade naming a live session = %d, want 101", got)
	}
	if got := fixture.dial(t, addr, "", nil); got != http.StatusNotFound {
		t.Errorf("upgrade presenting nothing = %d, want 404", got)
	}
}

// TestAuxiliaryHostsAdmitANodeNameAndAreWithdrawn covers the Host guard.
// On a loopback bind — the default — every DNS name is refused, so a
// tailnet request would 404 unless the node's own names are admitted
// while it is up. Withdrawing them is the half that matters: a name that
// stays admitted after its listener is gone is an admission nobody can
// reach.
func TestAuxiliaryHostsAdmitANodeNameAndAreWithdrawn(t *testing.T) {
	fixture := newAdmissionFixture(t)
	_, addr := attachAux(t, fixture.srv)

	const nodeName = "agent-overflow.example-tailnet.ts.net"
	if status, _ := getWithHost(t, addr, HealthPath, nodeName, nil); status != http.StatusNotFound {
		t.Fatalf("an unadmitted Host = %d, want 404 before the name is set", status)
	}

	fixture.srv.SetAuxiliaryHosts([]string{nodeName, "100.101.102.103"})
	for _, host := range []string{nodeName, nodeName + ":8080", "100.101.102.103:8080"} {
		if status, _ := getWithHost(t, addr, HealthPath, host, nil); status != http.StatusOK {
			t.Errorf("Host %q = %d, want 200 once the node's names are admitted", host, status)
		}
	}
	// A name nobody added is still refused: this admits exactly what was
	// named, and is not a switch that turns the guard off.
	if status, _ := getWithHost(t, addr, HealthPath, "somewhere.else.example", nil); status != http.StatusNotFound {
		t.Errorf("an unrelated Host = %d, want 404", status)
	}
	if got := fixture.srv.AuxiliaryHosts(); len(got) != 2 {
		t.Errorf("AuxiliaryHosts() = %v, want the two names that were set", got)
	}

	fixture.srv.SetAuxiliaryHosts(nil)
	if status, _ := getWithHost(t, addr, HealthPath, nodeName, nil); status != http.StatusNotFound {
		t.Errorf("Host %q = %d after the names were withdrawn, want 404", nodeName, status)
	}
	// Loopback never depended on any of it.
	if status, _ := getWithHost(t, addr, HealthPath, "", nil); status != http.StatusOK {
		t.Errorf("a loopback Host = %d after the withdrawal, want 200", status)
	}
}

// TestOriginAllowedAdmitsATailnetBrowser proves rather than assumes what
// the design leans on: a page loaded from the node's own address opens a
// socket with no allow-list entry at all, because the origin it sends IS
// the authority the request was addressed to. Both halves — the cleartext
// listener and the one tsnet terminates TLS on — and the negative case
// that keeps the rule meaningful.
func TestOriginAllowedAdmitsATailnetBrowser(t *testing.T) {
	const nodeName = "agent-overflow.example-tailnet.ts.net"

	plain := httpRequestForOrigin(t, nodeName+":5173", "http://"+nodeName+":5173")
	if !OriginAllowed(plain, nil) {
		t.Error("a browser on the node's cleartext address was refused with no allow-list entry")
	}

	secure := httpRequestForOrigin(t, nodeName, "https://"+nodeName)
	secure.TLS = &tls.ConnectionState{}
	if !OriginAllowed(secure, nil) {
		t.Error("a browser on the node's HTTPS address was refused with no allow-list entry")
	}

	// The scheme is read off the TLS state, so a cleartext request
	// claiming an https origin does not talk its way past the check.
	mismatched := httpRequestForOrigin(t, nodeName, "https://"+nodeName)
	if OriginAllowed(mismatched, nil) {
		t.Error("an https origin was admitted on a cleartext request")
	}
	other := httpRequestForOrigin(t, nodeName, "http://somewhere.else.example")
	if OriginAllowed(other, nil) {
		t.Error("a page from another origin was admitted")
	}
}

func httpRequestForOrigin(t *testing.T, host, origin string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+WSPath, nil)
	req.Host = host
	req.Header.Set("Origin", origin)
	return req
}

// TestClosingAnAuxiliaryListenerLeavesTheMainOneServing is the isolation
// property stated as a test. Each auxiliary listener has its own
// http.Server precisely so a detach — or a rebind, which shuts the active
// server down — cannot take the other way in with it.
func TestClosingAnAuxiliaryListenerLeavesTheMainOneServing(t *testing.T) {
	fixture := newAdmissionFixture(t)
	aux, addr := attachAux(t, fixture.srv)

	if status, _ := getWithHost(t, addr, HealthPath, "", nil); status != http.StatusOK {
		t.Fatalf("the auxiliary listener answered %d before the detach", status)
	}
	if err := aux.Close(); err != nil {
		t.Fatalf("close auxiliary: %v", err)
	}
	// Idempotent: a reconciler that detaches and then shuts down calls
	// this twice.
	if err := aux.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+HealthPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Errorf("the auxiliary listener answered %d after being detached", resp.StatusCode)
	}

	if status, _ := getWithHost(t, fixture.loopback, HealthPath, "", nil); status != http.StatusOK {
		t.Errorf("the main listener answered %d after an auxiliary detach, want 200", status)
	}
	if got := fixture.dial(t, fixture.loopback, "token=admission-token", nil); got != http.StatusSwitchingProtocols {
		t.Errorf("the main listener refused an upgrade (%d) after an auxiliary detach", got)
	}
}

// TestServeAuxiliaryRefusesAServerThatCannotServe covers the two states
// where handing a listener over would be worse than refusing it: before
// Start there is no root context for a connection to inherit, and after
// Shutdown there is nothing left to serve with.
func TestServeAuxiliaryRefusesAServerThatCannotServe(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&integrationStub{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv, err := New(Config{Dispatcher: d, EventBus: NewEventBus(8), Token: "tok"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	if _, err := srv.ServeAuxiliary(listener, nil); err == nil {
		t.Error("a server that has not started accepted an auxiliary listener")
	}
	if _, err := srv.ServeAuxiliary(nil, nil); err == nil {
		t.Error("a nil listener was accepted")
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := srv.ServeAuxiliary(listener, nil); err == nil {
		t.Error("a shut-down server accepted an auxiliary listener")
	}
}

// TestShutdownDetachesEveryAuxiliaryListener keeps the process teardown
// honest: Shutdown waits on the serve goroutines those listeners feed, so
// one it did not close would hold the wait forever.
func TestShutdownDetachesEveryAuxiliaryListener(t *testing.T) {
	fixture := newAdmissionFixture(t)
	_, addr := attachAux(t, fixture.srv)
	if status, _ := getWithHost(t, addr, HealthPath, "", nil); status != http.StatusOK {
		t.Fatalf("the auxiliary listener answered %d before shutdown", status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fixture.srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer reqCancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+HealthPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Errorf("the auxiliary listener answered %d after the server shut down", resp.StatusCode)
	}
}
