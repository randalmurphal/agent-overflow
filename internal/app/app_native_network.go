package app

import (
	"context"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/nativenetwork"
	"agent-overflow/internal/nearby"
	"agent-overflow/internal/network"
	"agent-overflow/internal/transport"
)

// The native launcher's observations are ephemeral ingress state. They never
// persist as host preferences, device trust or a second connection catalog.
type nativeNetworkState struct {
	mu         sync.Mutex
	seen       bool
	owner      *transport.ConnState
	armed      map[*transport.ConnState]bool
	addresses  []string
	err        string
	generation uint64
	scanID     uint64
	scan       *nativeScan
}

type nativeScan struct {
	id      uint64
	done    chan struct{}
	results []nearby.Host
	timer   *time.Timer
}

func (s *nativeNetworkState) finishScan(results []nearby.Host) {
	if s.scan == nil {
		return
	}
	scan := s.scan
	s.scan = nil
	scan.timer.Stop()
	scan.results = slices.Clone(results)
	close(scan.done)
}

func (s *nativeNetworkState) invalidate() {
	s.generation++
	s.addresses = nil
	s.err = ""
	s.finishScan(nil)
}

// invalidateNativeNetwork runs inside the network settings apply, which
// publishes the routes once for the whole change.
func (a *App) invalidateNativeNetwork() {
	s := &a.nativeNetwork
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidate()
}

// GetNativeNetworkConfig is the Windows launcher's local control channel. It
// receives the WSL address, never its own published forwarding address.
//
//ao:scope host
//ao:route home
func (a *App) GetNativeNetworkConfig(ctx context.Context) (nativenetwork.Config, error) {
	defer a.publishComputerRoutes()
	srv := a.transportServer.Load()
	if srv == nil {
		return nativenetwork.Config{}, errors.New("network listener is unavailable")
	}
	s := &a.nativeNetwork
	s.mu.Lock()
	s.seen = true
	conn := transport.ConnStateFromContext(ctx)
	if s.owner != conn {
		s.invalidate()
	}
	s.owner = conn
	arm := conn != nil && !s.armed[conn]
	if arm {
		if s.armed == nil {
			s.armed = make(map[*transport.ConnState]bool)
		}
		s.armed[conn] = true
	}
	var scanID uint64
	if s.scan != nil {
		scanID = s.scan.id
	}
	generation := s.generation
	s.mu.Unlock()
	if arm {
		cleanup := func() {
			defer a.publishComputerRoutes()
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.armed, conn)
			if s.owner == conn {
				s.owner = nil
				s.invalidate()
				s.err = "Windows local network access is unavailable. Reopen the Windows app."
			}
		}
		if !conn.RegisterCleanup(cleanup) {
			cleanup()
			return nativenetwork.Config{}, errors.New("the Windows app disconnected")
		}
	}
	id, _ := a.backendIdentity()
	cfg := nativenetwork.Config{Enabled: a.currentSettings().Network.BindAll, BackendID: id, Name: a.backendDisplayName(), ScanID: scanID, Generation: generation}
	cfg.Enabled = cfg.Enabled && nativeLANListenerError(srv.Addr()) == ""
	if cfg.Enabled {
		cfg.Target = "https://" + net.JoinHostPort(network.DiscoverLocalLANIP(), strconv.Itoa(portFromAddr(srv.Addr())))
	}
	cfg.PairingOpen = a.computerPairingOpen()
	return cfg, nil
}

// ReportNativeNetworkState accepts observations only from the current local
// launcher. A stale report cannot restore a closed or rebound listener.
//
//ao:scope host
//ao:route home
func (a *App) ReportNativeNetworkState(ctx context.Context, report nativenetwork.State) error {
	defer a.publishComputerRoutes()
	srv := a.transportServer.Load()
	if srv == nil {
		return errors.New("network listener is unavailable")
	}
	if len(report.Addresses) > 16 || len(report.Nearby) > 64 || len(report.Error) > 1024 {
		return errors.New("invalid Windows network report")
	}
	for _, address := range report.Addresses {
		u, err := url.Parse(address)
		if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" {
			return errors.New("invalid Windows LAN address")
		}
		ip := net.ParseIP(u.Hostname())
		port, _ := strconv.Atoi(u.Port())
		if ip == nil || ip.To4() == nil || !ip.IsPrivate() || port != portFromAddr(srv.Addr()) {
			return errors.New("Windows LAN address does not match this listener")
		}
	}
	for _, h := range report.Nearby {
		u, err := url.Parse(h.Address)
		if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" || !entityid.Valid(h.BackendID) || len(h.Name) > 256 {
			return errors.New("invalid nearby computer report")
		}
		ip := net.ParseIP(u.Hostname())
		port, _ := strconv.Atoi(u.Port())
		if ip == nil || ip.To4() == nil || !ip.IsPrivate() || port < 1 || port > 65535 {
			return errors.New("invalid nearby computer address")
		}
	}
	s := &a.nativeNetwork
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen || s.owner != transport.ConnStateFromContext(ctx) {
		return errors.New("this Windows network connection was replaced")
	}
	if report.Generation != s.generation {
		return errors.New("the Windows network configuration changed")
	}
	if !a.currentSettings().Network.BindAll && len(report.Addresses) > 0 {
		return errors.New("LAN access is disabled")
	}
	s.addresses, s.err = slices.Clone(report.Addresses), report.Error
	if s.scan != nil && report.ScanID == s.scan.id {
		s.finishScan(report.Nearby)
	}
	return nil
}

func nativeLANListenerError(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil && net.ParseIP(host).IsLoopback() {
		return "The WSL backend is listening only on localhost. Turn local network access off and back on; remove any loopback --listen override before restarting."
	}
	return ""
}

func (a *App) nativeLANStatus() *network.LANStatus {
	port := 0
	listenerError := ""
	if srv := a.transportServer.Load(); srv != nil {
		port = portFromAddr(srv.Addr())
		listenerError = nativeLANListenerError(srv.Addr())
	}
	s := &a.nativeNetwork
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen {
		return nil
	}
	if a.currentSettings().Network.BindAll && listenerError != "" {
		return &network.LANStatus{Error: listenerError}
	}
	message := s.err
	addresses := make([]string, 0, len(s.addresses))
	for _, address := range s.addresses {
		u, err := url.Parse(address)
		if err != nil {
			continue
		}
		p, _ := strconv.Atoi(u.Port())
		if p == port && port != 0 {
			addresses = append(addresses, address)
		}
	}
	if len(addresses) == 0 && message == "" {
		message = "Starting local network access…"
	}
	return &network.LANStatus{Addresses: addresses, Error: message}
}

func (a *App) discoverNativeComputers(ctx context.Context) []attachedbackends.DiscoveredComputer {
	s := &a.nativeNetwork
	s.mu.Lock()
	if !s.seen || s.owner == nil {
		s.mu.Unlock()
		return nil
	}
	if s.scan == nil {
		s.scanID++
		scan := &nativeScan{id: s.scanID, done: make(chan struct{})}
		s.scan = scan
		scan.timer = time.AfterFunc(6*time.Second, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.scan == scan {
				s.finishScan(nil)
			}
		})
	}
	scan := s.scan
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil
	case <-scan.done:
	}
	// Closing done publishes this scan's immutable results, even if another
	// caller has already started or completed the next scan.
	out := make([]attachedbackends.DiscoveredComputer, 0, len(scan.results))
	for _, h := range scan.results {
		out = append(out, attachedbackends.DiscoveredComputer{BackendID: h.BackendID, Name: h.Name, Address: h.Address, Network: "lan"})
	}
	return out
}
