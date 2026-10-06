package nearby

import (
	"errors"
	"net"
)

// Advertisement contains public installation metadata, never pairing secrets.
// Name is read for every query so a rename takes effect without restarting.
type Advertisement struct {
	// Addresses optionally restricts advertisements to the native listener addresses.
	Addresses []string
	BackendID string
	Name      func() string
	Port      int
}

// Host is an untrusted hint. Pairing must independently verify the host and
// receive owner confirmation before granting access or persisting trust.
type Host struct {
	BackendID string `json:"backendId"`
	Name      string `json:"name"`
	Address   string `json:"address"`
}

// Interfaces enumerates the host's interfaces; a test seam, as in
// internal/network, so a fixture can advertise on none.
var Interfaces = net.Interfaces

// ErrIsolated is the scan result of an isolated instance confined to this
// machine. It sends no multicast, so nothing on the LAN can find it or be
// found by it; typed addresses still work.
var ErrIsolated = errors.New("nearby discovery is off in an isolated instance")
