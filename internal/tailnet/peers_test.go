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
	controlURL, control := startControl(t)
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
	// testcontrol omits peer presence; publish the production online update.
	sourceStatus, err := source.lc.StatusWithoutPeers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if !control.AddRawMapResponse(sourceStatus.Self.PublicKey, &tailcfg.MapResponse{OnlineChange: map[tailcfg.NodeID]bool{status.Self.NodeID: true}}) {
			t.Fatal("could not publish peer online state")
		}
		candidates, err := source.DiscoverCandidates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range candidates {
			if candidate.DNSName == target {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatalf("peer %q not discovered", target)
	}
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
