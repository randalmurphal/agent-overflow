package tailnet

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

// The rig: an in-process coordination server plus a loopback DERP/STUN
// pair, so no test in this package can reach Tailscale's real control
// plane, its relay fleet, or anybody's tailnet. Ported from the spike that
// validated this dependency, which took its shape from tsnet's own tests.
//
// Two properties are enforced rather than assumed. The ambient environment
// is checked for anything that could redirect a node at a real control
// server, and the control URL handed to every node is asserted loopback
// before a node is allowed to use it. A test that silently registered a
// device on the developer's own tailnet would leave a machine in their
// admin panel and a node key on their disk.
//
// tstest/integration is a TEST-ONLY import. It must never appear in a
// production file in this package: `go mod why -m tailscale.com` should
// answer through internal/tailnet's own tsnet import, and the binary must
// not link a DERP server.

// verboseNodes turns on tsnet's (very chatty) backend logs, which are the
// only way to see why a bring-up stalled.
var verboseNodes = os.Getenv("AO_TAILNET_TEST_VERBOSE") != ""

// requireBringUpCapableHost skips when this machine cannot bring a node up
// at all. netstack needs a usable non-loopback interface with a route to
// build its endpoint set from; without one the node registers and then
// parks, never erroring and never joining, and every case below would hang
// until its own timeout instead of saying why.
func requireBringUpCapableHost(t *testing.T) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate network interfaces (%v), so a tailnet node cannot be brought up here", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && ipNet.IP.IsGlobalUnicast() {
				return
			}
		}
	}
	t.Skip("no non-loopback interface with a routable address: a tsnet node parks without joining on such a host, " +
		"so this case would time out rather than fail")
}

// refuseRealControlEnv fails the run if the ambient environment could
// point a node at the production coordination server or hand it a real
// auth key.
func refuseRealControlEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"TS_CONTROL_URL", "TS_AUTHKEY", "TS_CLIENT_SECRET", "TS_ID_TOKEN"} {
		if value := os.Getenv(name); value != "" {
			t.Fatalf("the environment carries %s=%q, which could point this test at a real tailnet; unset it", name, value)
		}
	}
}

// startControl brings up the fake coordination server and returns its URL.
func startControl(t *testing.T) (string, *testcontrol.Server) {
	t.Helper()
	refuseRealControlEnv(t)

	// netns interferes with a loopback-only rig (tailscale/corp#4520).
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })

	logf := logger.Discard
	if verboseNodes {
		logf = t.Logf
	}
	// One loopback region, so no node can select a public relay.
	derpMap := integration.RunDERPAndSTUN(t, logf, "127.0.0.1")

	control := &testcontrol.Server{
		DERPMap:        derpMap,
		DNSConfig:      &tailcfg.DNSConfig{Proxied: true},
		MagicDNSDomain: "test-tailnet.ts.net",
		Logf:           logf,
	}
	// A local port-discovery probe is not a control request. testcontrol
	// deliberately panics on unknown routes, so keep GET / outside it.
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", http.NotFound)
	mux.Handle("/", control)
	control.HTTPTestServer = httptest.NewUnstartedServer(mux)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)

	url := control.HTTPTestServer.URL
	if !strings.Contains(url, "127.0.0.1") {
		t.Fatalf("the fake control server answered on %q, which is not loopback; refusing to point a node at it", url)
	}
	return url, control
}

// newTestNode builds a node against the fake control server, rooted at a
// caller-chosen directory so a case can reopen the same identity.
func newTestNode(t *testing.T, controlURL, hostname, dir string) *Node {
	t.Helper()
	opts := Options{Dir: dir, Hostname: hostname, ControlURL: controlURL}
	if verboseNodes {
		opts.Logf = t.Logf
	}
	node, err := New(opts)
	if err != nil {
		t.Fatalf("build node %q: %v", hostname, err)
	}
	return node
}

// startTestNode builds a node in a fresh directory, starts it, and closes
// it at the end of the test.
func startTestNode(t *testing.T, controlURL, hostname string) *Node {
	t.Helper()
	node := newTestNode(t, controlURL, hostname, filepath.Join(t.TempDir(), hostname))
	t.Cleanup(func() { _ = node.Close() })
	if err := node.Start(); err != nil {
		t.Fatalf("start node %q: %v", hostname, err)
	}
	return node
}

// awaitStatus blocks until the node's published status satisfies want, or
// fails naming what it last saw. It waits on the node's own event channel
// rather than polling, which is also what the reconciler does — so a
// status change this package forgets to signal shows up here as a timeout.
func awaitStatus(t *testing.T, node *Node, what string, want func(Status) bool) Status {
	t.Helper()
	deadline := time.After(90 * time.Second)
	for {
		status := node.Status()
		if want(status) {
			return status
		}
		select {
		case <-node.Events():
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s; last status was state=%q authURL=%q dnsName=%q err=%q",
				what, status.State, status.AuthURL, status.DNSName, status.LastErr)
		}
	}
}

// awaitRunning is the common wait: the node has joined and knows its own
// name.
func awaitRunning(t *testing.T, node *Node) Status {
	t.Helper()
	return awaitStatus(t, node, "the node to join the tailnet", func(s Status) bool {
		return s.Running() && s.DNSName != "" && len(s.IPs) > 0
	})
}

// testContext bounds a whole case, so a stalled bring-up ends the test
// rather than the package.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}
