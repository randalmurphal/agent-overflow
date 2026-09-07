package tailnet

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"tailscale.com/ipn/ipnstate"
)

const maxDiscoveryCandidates = 64

// Candidate is an online tailnet peer, not an authenticated AO installation.
// Callers probe only its HTTPS endpoint and still require ordinary pairing.
type Candidate struct {
	Name    string
	DNSName string
	Address string
}

// DiscoverCandidates reads a fresh bounded list from this application's own
// tailnet node. It needs neither an OS Tailscale installation nor LAN multicast.
func (n *Node) DiscoverCandidates(ctx context.Context) ([]Candidate, error) {
	if _, err := n.runningServer(); err != nil {
		return nil, err
	}
	n.mu.Lock()
	lc := n.lc
	n.mu.Unlock()
	if lc == nil {
		return nil, fmt.Errorf("tailnet: node is closed")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := lc.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailnet: discover peers: %w", err)
	}
	return discoveryCandidates(status), nil
}

func discoveryCandidates(status *ipnstate.Status) []Candidate {
	result := make([]Candidate, 0, maxDiscoveryCandidates)
	if status == nil {
		return result
	}
	for _, peer := range status.Peer {
		if peer == nil || !peer.Online || (status.Self != nil && peer.ID == status.Self.ID) {
			continue
		}
		name := strings.TrimSuffix(peer.DNSName, ".")
		if name == "" || len(name) > 253 || strings.ContainsFunc(name, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-')
		}) {
			continue
		}
		candidate := Candidate{Name: peer.HostName, DNSName: name, Address: "https://" + name}
		if len(result) < maxDiscoveryCandidates {
			result = append(result, candidate)
		} else if name < result[len(result)-1].DNSName {
			result[len(result)-1] = candidate
		} else {
			continue
		}
		sort.Slice(result, func(i, j int) bool { return result[i].DNSName < result[j].DNSName })
	}
	return result
}

// DialContext reaches a peer using this application's userspace Tailscale node.
// The caller chooses this route only for tailnet destinations; ordinary LAN
// traffic continues through the OS dialer. No server is implicitly started.
func (n *Node) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	srv, err := n.runningServer()
	if err != nil {
		return nil, err
	}
	return srv.Dial(ctx, network, address)
}
