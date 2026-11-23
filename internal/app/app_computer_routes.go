package app

import (
	"errors"
	"slices"
	"sync"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/network"
	"agent-overflow/internal/transport"
)

// computerRoutesPublication owns this computer's published addresses: the
// routes peers reach it on and the address each pairing network resolves to.
// Every change to a listener or to a reported address publishes right after
// it moves, so this is the one answer to "the live address and whether it is
// still live".
//
// Only the previous publication is retained, to avoid network-status polls
// invalidating every connected client when no usable address changed.
type computerRoutesPublication struct {
	mu     sync.Mutex
	routes []computerroute.Route
	// pairing is the address each pairing network resolved to.
	pairing pairingAddresses
	// serial moves whenever routes or pairing change, so a reader can tell
	// whether what it read is still what is published.
	serial uint64
}

// pairingAddresses is the address a pairing window opened on each network
// shows, or why that network has none.
type pairingAddresses struct {
	lan, tailnet       string
	lanErr, tailnetErr string
}

func (p pairingAddresses) on(choice string) (string, error) {
	address, reason := p.lan, p.lanErr
	if choice == "tailnet" {
		address, reason = p.tailnet, p.tailnetErr
	}
	if address == "" {
		return "", errors.New(reason)
	}
	return address, nil
}

// publishedNetwork is one reading of the publication.
type publishedNetwork struct {
	serial  uint64
	pairing pairingAddresses
	// advertise is whether a pairing window advertises on the LAN itself:
	// LAN access is on and the Windows launcher is not this backend's LAN
	// ingress.
	advertise bool
}

// publishComputerRoutes reads the listener and the reported addresses once,
// publishes what changed, and retires a pairing window whose address is no
// longer live. Call it right after a listener or a reported address moves,
// with the network-state locks released. The event carries no route trust:
// delayed replay only asks clients for a fresh authenticated bootstrap.
func (a *App) publishComputerRoutes() publishedNetwork {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	srv := a.transportServer.Load()
	settings := a.persistedNetworkSettings()
	reach := a.netReach.Pinned()
	var routes []computerroute.Route
	if srv != nil {
		routes = network.ComputerRoutes(srv, settings, reach)
	}
	pairing := pairingAddressesOf(srv, settings, reach)
	routesChanged := !slices.Equal(state.routes, routes)
	pairingChanged := pairing != state.pairing
	if routesChanged || pairingChanged {
		state.routes, state.pairing = routes, pairing
		state.serial++
	}
	published := publishedNetwork{serial: state.serial, pairing: pairing, advertise: settings.BindAll && settings.LAN == nil}
	state.mu.Unlock()
	if pairingChanged {
		a.retireStaleComputerPairing(pairing)
	}
	if routesChanged {
		a.emit(eventchan.ComputerRoutesChanged, struct{}{})
		// The own-device reconciler runs on demand, not on a timer: a peer
		// that was unreachable from the old addresses may be reachable now.
		if a.backends != nil {
			a.backends.WakeOwnDevices()
		}
	}
	return published
}

func pairingAddressesOf(srv *transport.Server, s network.Settings, reach network.Reach) pairingAddresses {
	var p pairingAddresses
	var err error
	if p.lan, err = network.PairingAddressOnNetwork(srv, s, reach, "lan"); err != nil {
		p.lanErr = err.Error()
	}
	if p.tailnet, err = network.PairingAddressOnNetwork(srv, s, reach, "tailnet"); err != nil {
		p.tailnetErr = err.Error()
	}
	return p
}

// publishedSerial is the publication's current serial.
func (a *App) publishedSerial() uint64 {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.serial
}

// GetComputerRoutes refreshes this computer's public addresses over an already
// authenticated socket when a listener move made HTTP bootstrap unreachable.
//
//ao:scope session
//ao:route selected
func (a *App) GetComputerRoutes() []computerroute.Route { return ComputerRoutes(a) }
