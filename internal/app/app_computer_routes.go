package app

import (
	"errors"
	"slices"
	"sync"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/network"
	"agent-overflow/internal/pairbootstrap"
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
	// advertise is publishedNetwork.advertise as last published.
	advertise bool
	// window is the computer pairing window admitted under this
	// publication, recorded at its admission (openComputerPairing) with the
	// serial it was admitted under. A publication that moves the serial
	// retires it.
	window admittedPairingWindow
	// chosen, when set before the publication is used, runs after each
	// publication releases mu and before it closes the window it retired,
	// so a test can order other publications and opens in that gap.
	chosen func()
}

// admittedPairingWindow is computerRoutesPublication.window.
type admittedPairingWindow struct {
	id     string
	serial uint64
	book   *pairbootstrap.Book
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
// publishes what changed, and retires the pairing window admitted under an
// earlier publication. Call it right after a listener or a reported address
// moves, with the network-state locks released. The event carries no route
// trust: delayed replay only asks clients for a fresh authenticated
// bootstrap.
//
// The window to retire is chosen under the lock that moves the serial and
// refused there (Book.Expire), so no decision on it is admitted after this
// publication, and a publication that runs late never chooses a window
// admitted under a newer one. Its close, which retires its invitation and
// its LAN responders, runs after the lock is released and before this
// returns.
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
	state.advertise = settings.BindAll && settings.LAN == nil
	published := publishedNetwork{serial: state.serial, pairing: pairing, advertise: state.advertise}
	retired := state.window
	if retired.id != "" && retired.serial < state.serial {
		state.window = admittedPairingWindow{}
		retired.book.Expire(retired.id)
	} else {
		retired = admittedPairingWindow{}
	}
	chosen := state.chosen
	state.mu.Unlock()
	if chosen != nil {
		chosen()
	}
	if retired.id != "" {
		// A failed cancellation is logged by the book's callback and retried
		// by the book.
		_ = a.closeComputerPairingWindow(retired.id)
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

// admitComputerPairingWindow records windowID as admitted under the current
// publication and answers the address it shows on choice, or why choice has
// none now. The caller holds the pairing state's lock (lock order: the
// pairing state's mu, then this publication's mu, then the book's mu).
func (a *App) admitComputerPairingWindow(book *pairbootstrap.Book, windowID, choice string) (address string, advertise bool, err error) {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	defer state.mu.Unlock()
	if address, err = state.pairing.on(choice); err != nil {
		return "", false, err
	}
	state.window = admittedPairingWindow{id: windowID, serial: state.serial, book: book}
	return address, state.advertise, nil
}

// GetComputerRoutes refreshes this computer's public addresses over an already
// authenticated socket when a listener move made HTTP bootstrap unreachable.
//
//ao:scope session
//ao:route selected
func (a *App) GetComputerRoutes() []computerroute.Route { return ComputerRoutes(a) }
