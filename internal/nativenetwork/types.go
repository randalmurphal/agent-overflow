package nativenetwork

import "agent-overflow/internal/nearby"

// Config is one complete desired native-listener generation. Generation orders
// listener replacement and rejects reports from an earlier configuration.
// ScanID is independent: it correlates one bounded discovery request without
// allowing a later scan to replace the result a prior caller is consuming.
type Config struct {
	Generation  uint64 `json:"generation"`
	PairingOpen bool   `json:"pairingOpen"`
	Enabled     bool   `json:"enabled"`
	BackendID   string `json:"backendId"`
	Name        string `json:"name"`
	Target      string `json:"target"`
	ScanID      uint64 `json:"scanId"`
}

// State belongs to one Config generation. Reject a stale generation before
// publishing Addresses or Error. Nearby completes a discovery request only
// when ScanID also matches that pending request; listener updates need no scan.
type State struct {
	Generation uint64        `json:"generation"`
	Addresses  []string      `json:"addresses"`
	Error      string        `json:"error"`
	ScanID     uint64        `json:"scanId"`
	Nearby     []nearby.Host `json:"nearby"`
}
