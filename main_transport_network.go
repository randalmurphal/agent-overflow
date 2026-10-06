package main

import (
	"fmt"
	"net"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/transport"
)

// configureTransportNetwork is shared by every ordinary shell. Persisted
// settings describe this host; an explicit CLI bind describes one launch and
// must never be widened by a saved preference. Isolated harnesses opt out.
// The App's network reach confines both: a loopback Reach refuses a
// non-loopback --listen and keeps a saved LAN bind on 127.0.0.1.
func configureTransportNetwork(cfg *transport.Config, appService *App, listenAddr string, ignorePersisted bool) (settingsPort int, canonicalDomain string, err error) {
	reach := appservice.NetworkReach(appService.App)
	if listenAddr != "" {
		cfg.BindAddr, cfg.Port, err = splitListenAddr(listenAddr)
		if err != nil {
			return 0, "", err
		}
		if ip := net.ParseIP(cfg.BindAddr); reach.LoopbackOnly() && (ip == nil || !ip.IsLoopback()) {
			return 0, "", fmt.Errorf("--listen %q: an isolated instance outside the test network namespace listens only on loopback", listenAddr)
		}
	}
	if !ignorePersisted {
		persisted := loadPersistedNetworkSettings()
		if persisted.BindAll && listenAddr == "" {
			cfg.BindAddr = reach.BindHost(true)
		}
		settingsPort = persisted.ListenPort
		cfg.CanonicalHost = persisted.CanonicalDomain
		canonicalDomain = persisted.CanonicalDomain
	}
	// Origin patterns are installed only after the bind resolves the actual port.
	return settingsPort, canonicalDomain, nil
}
