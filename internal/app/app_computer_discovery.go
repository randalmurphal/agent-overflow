package app

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/attachedbackends"
)

// DiscoverComputers searches this frontend's LAN and enabled tailnet. It never
// enrolls devices, reads projects or borrows another frontend's connections.
//
//ao:scope host
//ao:route home
func (a *App) DiscoverComputers(ctx context.Context) ([]attachedbackends.DiscoveredComputer, error) {
	if a.backends == nil {
		return nil, errNoBackendProfiles
	}
	a.tailnet.mu.Lock()
	node := a.tailnet.node
	a.tailnet.mu.Unlock()
	var native []attachedbackends.DiscoveredComputer
	var scan sync.WaitGroup
	scan.Go(func() { native = a.discoverNativeComputers(ctx) })
	var extra []attachedbackends.DiscoveredComputer
	if node != nil && node.Status().Running() {
		candidates, err := node.DiscoverCandidates(ctx)
		if err == nil {
			for _, c := range candidates {
				extra = append(extra, attachedbackends.DiscoveredComputer{Name: c.Name, Address: c.Address, Network: "tailnet"})
			}
		}
	}
	scan.Wait()
	return a.backends.Discover(ctx, append(native, extra...))
}

func (a *App) dialComputer(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	a.tailnet.mu.Lock()
	node := a.tailnet.node
	a.tailnet.mu.Unlock()
	if node != nil {
		status := node.Status()
		if status.Running() && tailnetDestination(host, status.DNSName) {
			return node.DialContext(ctx, network, address)
		}
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
}

func tailnetDestination(host, ownName string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		return netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(ip)
	}
	// Match this node's actual tailnet DNS suffix, including private control
	// servers. A lookalike suffix or ordinary LAN hostname stays on OS routing.
	_, suffix, ok := strings.Cut(strings.TrimSuffix(strings.ToLower(ownName), "."), ".")
	return ok && suffix != "" && strings.HasSuffix(host, "."+suffix)
}
