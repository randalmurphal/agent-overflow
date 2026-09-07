package main

import "agent-overflow/internal/transport"

// configureTransportNetwork is shared by every ordinary shell. Persisted
// settings describe this host; an explicit CLI bind describes one launch and
// must never be widened by a saved preference. Isolated harnesses opt out.
func configureTransportNetwork(cfg *transport.Config, listenAddr string, ignorePersisted bool) (settingsPort int, canonicalDomain string, err error) {
	if listenAddr != "" {
		cfg.BindAddr, cfg.Port, err = splitListenAddr(listenAddr)
		if err != nil {
			return 0, "", err
		}
	}
	if !ignorePersisted {
		persisted := loadPersistedNetworkSettings()
		if persisted.BindAll && listenAddr == "" {
			cfg.BindAddr = "0.0.0.0"
		}
		settingsPort = persisted.ListenPort
		cfg.CanonicalHost = persisted.CanonicalDomain
		canonicalDomain = persisted.CanonicalDomain
	}
	// Origin patterns are installed only after the bind resolves the actual port.
	return settingsPort, canonicalDomain, nil
}
