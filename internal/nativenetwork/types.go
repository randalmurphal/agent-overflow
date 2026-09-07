package nativenetwork

import "agent-overflow/internal/nearby"

type Config struct {
	Generation  uint64 `json:"generation"`
	PairingOpen bool   `json:"pairingOpen"`
	Enabled     bool   `json:"enabled"`
	BackendID   string `json:"backendId"`
	Name        string `json:"name"`
	Target      string `json:"target"`
	ScanID      uint64 `json:"scanId"`
}

type State struct {
	Generation uint64        `json:"generation"`
	Addresses  []string      `json:"addresses"`
	Error      string        `json:"error"`
	ScanID     uint64        `json:"scanId"`
	Nearby     []nearby.Host `json:"nearby"`
}
