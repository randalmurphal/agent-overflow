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
	// serial it was admitted under and what it depends on. A publication
	// that moves the serial retires it only when what it depends on changed.
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
	choice string
	terms  pairingWindowTerms
	// minted is the network a reveal mints the window's invitation for,
	// which may differ from choice, and mintedTerms what that invitation
	// depends on; "" until a reveal starts minting.
	minted      string
	mintedTerms pairingWindowTerms
}

// retiresLocked reports whether this publication retires w: w was admitted
// under an earlier serial and what it depends on changed, which is its own
// network's terms and, once a reveal started minting, the terms of the
// network its invitation uses. The caller holds mu.
func (p *computerRoutesPublication) retiresLocked(w admittedPairingWindow) bool {
	if w.id == "" || w.serial >= p.serial {
		return false
	}
	return w.terms != p.termsLocked(w.choice) || (w.minted != "" && w.mintedTerms != p.termsLocked(w.minted))
}

// pairingWindowTerms is what a pairing window on one network depends on: the
// address and port it shows, the certificate pin that address presents, and
// what its LAN responders advertise (whether they run, and the LAN address).
type pairingWindowTerms struct {
	address, pin string
	advertise    bool
	lan          string
}

// termsLocked is what a window on choice depends on under this publication.
// The caller holds mu.
func (p *computerRoutesPublication) termsLocked(choice string) pairingWindowTerms {
	t := pairingWindowTerms{advertise: p.advertise}
	t.address, _ = p.pairing.on(choice)
	if choice == "lan" {
		t.pin = p.pairing.lanPin
	}
	if t.advertise {
		t.lan = p.pairing.lan
	}
	return t
}

// pairingAddresses is the address a pairing window opened on each network
// shows, or why that network has none.
type pairingAddresses struct {
	lan, tailnet       string
	lanErr, tailnetErr string
	// lanPin is the certificate pin the LAN address presents.
	lanPin string
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
// earlier publication when what that window depends on changed
// (pairingWindowTerms). Call it right after a listener or a reported address
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
	advertise := settings.BindAll && settings.LAN == nil
	pairingChanged := pairing != state.pairing || advertise != state.advertise
	if routesChanged || pairingChanged {
		state.routes, state.pairing, state.advertise = routes, pairing, advertise
		state.serial++
	}
	published := publishedNetwork{serial: state.serial, pairing: pairing, advertise: state.advertise}
	retired := state.window
	if state.retiresLocked(retired) {
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
	} else {
		p.lanPin = s.TLS.SelfSignedFingerprint
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
	state.window = admittedPairingWindow{id: windowID, serial: state.serial, book: book, choice: choice, terms: state.termsLocked(choice)}
	return address, state.advertise, nil
}

// publishedPairingAddress is the address a window opened on choice shows
// under the current publication, or why choice has none, read as an
// admission reads it.
func (a *App) publishedPairingAddress(choice string) (string, error) {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.pairing.on(choice)
}

// mintingComputerPairingWindow records, before windowID's invitation is
// minted for route, that the window depends on route's terms from now on, so
// a publication that changes them retires it. A window this publication no
// longer records was retired, and mints nothing.
func (a *App) mintingComputerPairingWindow(windowID, route string) error {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	defer state.mu.Unlock()
	if windowID == "" || state.window.id != windowID {
		return pairbootstrap.ErrClosed
	}
	state.window.minted, state.window.mintedTerms = route, state.termsLocked(route)
	return nil
}

// GetComputerRoutes refreshes this computer's public addresses over an already
// authenticated socket when a listener move made HTTP bootstrap unreachable.
//
//ao:scope session
//ao:route selected
func (a *App) GetComputerRoutes() []computerroute.Route { return ComputerRoutes(a) }
