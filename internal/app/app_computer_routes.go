package app

import (
	"errors"
	"fmt"
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
	// mu's place in the lock order is stated on pairbootstrap.Book.
	mu     sync.Mutex
	routes []computerroute.Route
	// pairing is this computer's published pairing set, with the reason
	// each network has none.
	pairing pairingAddresses
	// serial moves only when the pairing set changes
	// (pairingAddresses.set), so a pairing window can tell whether what it
	// was admitted under is still what is published.
	serial uint64
	// window is the computer pairing window admitted under this
	// publication, recorded at its admission (openComputerPairing) with the
	// serial it was admitted under. A publication with a newer serial
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

// pairingAddresses is this computer's pairing set: on each pairing network,
// the address a window opened there shows and the page an invitation minted
// there sends the device to (with its certificate pin), and whether a
// window advertises on the LAN itself. The reasons a network has none are
// carried beside the set and are not part of it.
type pairingAddresses struct {
	lan, tailnet         string
	lanPage, tailnetPage network.PairingPage
	// advertise is whether a pairing window advertises on the LAN itself:
	// LAN access is on and the Windows launcher is not this backend's LAN
	// ingress.
	advertise bool

	lanErr, tailnetErr         string
	lanPageErr, tailnetPageErr string
}

// set is p without the reasons, the part a pairing window depends on.
func (p pairingAddresses) set() pairingAddresses {
	p.lanErr, p.tailnetErr, p.lanPageErr, p.tailnetPageErr = "", "", "", ""
	return p
}

// checkPairingNetwork refuses a network choice that is neither of the two a
// person can pick. Every pairing choice a caller names passes it before
// anything reads the choice, and NearbyPair names a reveal's route as one of
// the two, so on and pageOn take any choice but "tailnet" as "lan".
func checkPairingNetwork(choice string) error {
	if choice != "lan" && choice != "tailnet" {
		return errors.New("choose Local network or Tailscale")
	}
	return nil
}

// on is the address a window opened on choice shows, or, for a person
// opening one, which network is unavailable, why, and the other choice.
func (p pairingAddresses) on(choice string) (string, error) {
	address, reason, name, other := p.lan, p.lanErr, "Local network", "Tailscale"
	if choice == "tailnet" {
		address, reason, name, other = p.tailnet, p.tailnetErr, "Tailscale", "Local network"
	}
	if address == "" {
		return "", fmt.Errorf("%s is not available for pairing on this computer: %s. To pair now, close this and choose %s", name, reason, other)
	}
	return address, nil
}

// pageOn is the page an invitation minted on choice sends the device to, or
// why choice has none.
func (p pairingAddresses) pageOn(choice string) (network.PairingPage, error) {
	page, reason := p.lanPage, p.lanPageErr
	if choice == "tailnet" {
		page, reason = p.tailnetPage, p.tailnetPageErr
	}
	if page.Authority == "" {
		return network.PairingPage{}, errors.New(reason)
	}
	return page, nil
}

// publishedNetwork is one reading of the publication.
type publishedNetwork struct {
	pairing pairingAddresses
}

// publishComputerRoutes reads the listener and the reported addresses once,
// publishes what changed, and retires the pairing window admitted under an
// earlier pairing set. Call it right after a listener or a reported address
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
	if routesChanged {
		state.routes = routes
	}
	if pairing.set() != state.pairing.set() {
		state.serial++
	}
	state.pairing = pairing
	published := publishedNetwork{pairing: pairing}
	retired := state.window
	if retired.id != "" && retired.serial < state.serial {
		state.window = admittedPairingWindow{}
		retired.book.Expire(retired.id, pairbootstrap.EndedNetworkChanged)
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
		_ = a.closeComputerPairingWindow(retired.id, pairbootstrap.EndedNetworkChanged)
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
	p := pairingAddresses{advertise: s.BindAll && s.LAN == nil}
	var err error
	if p.lan, err = network.PairingAddressOnNetwork(srv, s, reach, "lan"); err != nil {
		p.lanErr = err.Error()
	}
	if p.tailnet, err = network.PairingAddressOnNetwork(srv, s, reach, "tailnet"); err != nil {
		p.tailnetErr = err.Error()
	}
	if p.lanPage, err = network.PairingPageOnNetwork(srv, s, reach, "lan"); err != nil {
		p.lanPageErr = err.Error()
	}
	if p.tailnetPage, err = network.PairingPageOnNetwork(srv, s, reach, "tailnet"); err != nil {
		p.tailnetPageErr = err.Error()
	}
	return p
}

// admitComputerPairingWindow records windowID as admitted under the current
// pairing set and answers the address it shows on choice, or why choice has
// none now. The caller holds the pairing state's lock.
func (a *App) admitComputerPairingWindow(book *pairbootstrap.Book, windowID, choice string) (address string, advertise bool, err error) {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	defer state.mu.Unlock()
	if address, err = state.pairing.on(choice); err != nil {
		return "", false, err
	}
	state.window = admittedPairingWindow{id: windowID, serial: state.serial, book: book}
	return address, state.pairing.advertise, nil
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

// pairingPageOfWindow is the page windowID's invitation minted on route sends
// the device to, from the pairing set the window was admitted under: the
// window is recorded only while that set is published. A window no longer
// recorded was retired, and mints nothing.
func (a *App) pairingPageOfWindow(windowID, route string) (network.PairingPage, error) {
	state := &a.computerRoutesPublished
	state.mu.Lock()
	defer state.mu.Unlock()
	if windowID == "" || state.window.id != windowID {
		return network.PairingPage{}, pairbootstrap.ErrClosed
	}
	return state.pairing.pageOn(route)
}

// GetComputerRoutes refreshes this computer's public addresses over an already
// authenticated socket when a listener move made HTTP bootstrap unreachable.
//
//ao:scope session
//ao:route selected
func (a *App) GetComputerRoutes() []computerroute.Route { return ComputerRoutes(a) }
