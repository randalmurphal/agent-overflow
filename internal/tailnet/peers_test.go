//go:build !noremote

package tailnet

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

func TestDiscoveryCandidatesAreOnlineBoundedAndDeterministic(t *testing.T) {
	status := &ipnstate.Status{Self: &ipnstate.PeerStatus{ID: "self"}, Peer: make(map[key.NodePublic]*ipnstate.PeerStatus)}
	add := func(peer *ipnstate.PeerStatus) { status.Peer[key.NewNode().Public()] = peer }
	add(&ipnstate.PeerStatus{ID: "self", Online: true, DNSName: "self.tail.test."})
	add(&ipnstate.PeerStatus{ID: "offline", DNSName: "offline.tail.test."})
	add(&ipnstate.PeerStatus{ID: "invalid", Online: true, DNSName: "invalid.tail.test/path"})
	for i := 0; i < maxDiscoveryCandidates+10; i++ {
		add(&ipnstate.PeerStatus{ID: tailcfg.StableNodeID(fmt.Sprint(i)), HostName: "ordinary-host", Online: true, DNSName: fmt.Sprintf("host-%02d.tail.test.", i)})
	}
	got := discoveryCandidates(status)
	if len(got) != maxDiscoveryCandidates {
		t.Fatalf("candidates=%d", len(got))
	}
	for i, peer := range got {
		want := fmt.Sprintf("host-%02d.tail.test", i)
		if peer.DNSName != want || peer.Address != "https://"+want {
			t.Fatalf("candidate[%d]=%+v", i, peer)
		}
	}
}

func TestOutboundOperationsRefuseUnstartedOrClosedNode(t *testing.T) {
	node, err := New(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		if _, err := node.DiscoverCandidates(context.Background()); err == nil {
			t.Fatal("discovered on inactive node")
		}
		if _, err := node.DialContext(context.Background(), "tcp", "100.64.0.1:443"); err == nil {
			t.Fatal("dialed inactive node")
		}
	}
	check()
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	check()
}

// This uses the local control/DERP rig, proving no OS Tailscale daemon is needed.
func TestOutboundDialAndPeerDiscoveryUseApplicationNode(t *testing.T) {
	requireBringUpCapableHost(t)
	ctx := testContext(t)
	controlURL, _ := startControl(t)
	source := startTestNode(t, controlURL, "source-app")
	awaitRunning(t, source)
	peer := startPeerNode(t, ctx, controlURL, "ordinary-workstation")
	lc, err := peer.LocalClient()
	if err != nil {
		t.Fatal(err)
	}
	status, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := strings.TrimSuffix(status.Self.DNSName, ".")
	sourceStatus, err := source.lc.StatusWithoutPeers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for found := false; !found; {
		candidates, err := source.DiscoverCandidates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range candidates {
			found = found || candidate.DNSName == target
		}
		if !found {
			pollAgain(t, ctx, fmt.Sprintf("discovery of peer %q", target))
		}
	}
	// Discovery needs only the peer's name and presence. A packet also needs
	// the peer's home DERP, which reaches the source in a later map response,
	// and the peer needs the source's to answer. A dial before both are known
	// loses its first WireGuard handshake and waits out the 5s retry.
	awaitHomeDERP(t, ctx, source.lc, "the source", status.Self.PublicKey)
	awaitHomeDERP(t, ctx, lc, "the peer", sourceStatus.Self.PublicKey)
	listener, err := peer.Listen("tcp", ":443")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "from peer") })}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	transport := &http.Transport{DialContext: source.DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(target, "443"), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "from peer" {
		t.Fatalf("body=%q error=%v", body, err)
	}
}

// awaitHomeDERP waits until lc's node knows the home DERP region of the
// node with key want, which is the route every first packet takes.
func awaitHomeDERP(t *testing.T, ctx context.Context, lc *local.Client, who string, want key.NodePublic) {
	t.Helper()
	for {
		status, err := lc.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if peer := status.Peer[want]; peer != nil && peer.Relay != "" {
			return
		}
		pollAgain(t, ctx, who+" learning its peer's home DERP")
	}
}

// pollAgain pauses between reads of state that tsnet publishes without an
// event, failing once the case's context has expired.
func pollAgain(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s: %v", what, ctx.Err())
	case <-time.After(20 * time.Millisecond):
	}
}
