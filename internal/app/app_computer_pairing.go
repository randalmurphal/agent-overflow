package app

import (
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"sync"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/nearby"
	"agent-overflow/internal/network"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/transport"
)

type ComputerPairingWindow struct {
	ID          string `json:"id"`
	Address     string `json:"address"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

type ComputerPairingView struct {
	State              string `json:"state"`
	VerificationNumber string `json:"verificationNumber"`
	DeviceLabel        string `json:"deviceLabel"`
	LinkID             string `json:"linkId"`
	ExpiresAtMs        int64  `json:"expiresAtMs"`
	// DiscoveryError says why this window is not advertised on the local
	// network, so the owner can be told to type the address rather than wait
	// for a discovery that cannot happen. Empty while advertising, and for a
	// window this process does not advertise (LAN sharing off, or the
	// Windows launcher's own LAN listener).
	DiscoveryError string `json:"discoveryError"`
}

// computerPairingState is the owner surface's side of one bootstrap window:
// which connection owns it and its LAN advertisement. The book owns the
// window and the retirement of every invitation a window minted.
//
// mu guards the fields and is never held across I/O: SQLite writes, the
// multicast responders and the book's settling calls all run with it
// released, so the book's callbacks and the connection cleanup can take it
// without ordering against anything. ops serializes opens, the one
// multi-step act (close the old window, open, advertise); reads and closes
// never take it. Expiry is the book's timer. The advertisement is retired
// when the window is closed, replaced, or its owner disconnects; an expired
// window answers info with Open:false, which discovery discards.
type computerPairingState struct {
	mu             sync.Mutex
	ops            sync.Mutex
	book           *pairbootstrap.Book
	advertiser     *nearby.Server
	discoveryError string
	// windowID is the last window opened here, which owner and advertiser
	// belong to.
	windowID string
	owner    *transport.ConnState
	// listeners counts the listener changes that retired the open window
	// (closeComputerPairing). An open publishes its window only if none ran
	// since it chose the window's address.
	listeners uint64
}

// OpenComputerPairing opens a short-lived invitation window. Merely discovering
// the host or requesting a connection never authorizes a session.
//
//ao:scope access:admin
//ao:route home
//ao:stepup
//ao:remote
func (a *App) OpenComputerPairing(ctx context.Context, networkChoice, access string) (ComputerPairingWindow, error) {
	conn, err := transport.CallerConnection(ctx)
	if err != nil {
		return ComputerPairingWindow{}, err
	}
	return a.openComputerPairing(conn, networkChoice, access, "", nil)
}

const computerPairingCleanupKey = "computer-pairing"

var errComputerPairingListenerChanged = errors.New("the network settings changed while pairing opened; try again")

// openComputerPairing opens the window for conn, which owns it: the
// connection's end closes it. conn is admitted before anything is changed:
// a connection that is already closing retires no other connection's
// window, persists nothing through prepare, and exposes no invitation.
// prepare, when set, runs once the caller is admitted and before the old
// window is retired.
func (a *App) openComputerPairing(conn *transport.ConnState, networkChoice, access, purpose string, prepare func() error) (ComputerPairingWindow, error) {
	if networkChoice != "lan" && networkChoice != "tailnet" {
		return ComputerPairingWindow{}, errors.New("choose Local network or Tailscale")
	}
	if _, err := a.accessState(); err != nil {
		return ComputerPairingWindow{}, err
	}
	if _, err := identity.PairingAccess(access).Grants(); err != nil {
		return ComputerPairingWindow{}, err
	}
	// A network with no address is refused before prepare persists anything;
	// the window's own address is chosen again below, after prepare.
	if _, err := network.PairingAddressOnNetwork(a.transportServer.Load(), a.persistedNetworkSettings(), a.netReach, networkChoice); err != nil {
		return ComputerPairingWindow{}, err
	}
	state := &a.computerPairing
	state.ops.Lock()
	defer state.ops.Unlock()
	// Admission. The tie is the connection's for its life (the key is
	// bound once) and closes whichever window the connection owns when it
	// ends, so binding it before this connection owns one closes nothing.
	if _, err := conn.Bind(computerPairingCleanupKey, func() {
		state.mu.Lock()
		id, owned := state.windowID, state.owner == conn
		if owned {
			state.owner = nil
		}
		state.mu.Unlock()
		if owned {
			// A failed cancellation is logged by the book's callback and
			// stays refused until a later open or close retires it.
			_ = a.closeComputerPairingWindow(id)
		}
	}); err != nil {
		return ComputerPairingWindow{}, err
	}
	if prepare != nil {
		if err := prepare(); err != nil {
			return ComputerPairingWindow{}, err
		}
	}
	state.mu.Lock()
	if state.book == nil {
		state.book = pairbootstrap.NewBook(a.cancelComputerPairingLink, pairbootstrap.CancelRetry)
	}
	book, previous, listeners := state.book, state.windowID, state.listeners
	state.mu.Unlock()
	// Chosen after prepare, which can move the listener, and fenced by the
	// listener count read above.
	address, err := network.PairingAddressOnNetwork(a.transportServer.Load(), a.persistedNetworkSettings(), a.netReach, networkChoice)
	if err != nil {
		return ComputerPairingWindow{}, err
	}
	nativeLAN := a.nativeLANStatus() != nil
	// A window replaces its predecessor only once every earlier invitation's
	// cancellation is durable: the close answers for the previous window,
	// and the book's open for any other still unfinished.
	if err := a.closeComputerPairingWindow(previous); err != nil {
		return ComputerPairingWindow{}, err
	}
	view, err := book.Open(func(route string) (pairbootstrap.Invitation, error) {
		// route is the listener the admitted reveal arrived on, which the
		// HTTP adapter names. Both enabled routes enroll the same host; no
		// pin crosses listeners.
		invite, err := a.mintDevicePairingPurpose("desktop", access, route, purpose)
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	})
	if err != nil {
		return ComputerPairingWindow{}, err
	}
	// The window becomes conn's only while its tie is still bound, checked
	// in the critical section the tie's release takes: a connection that
	// ended while the window opened has already run that release, so the
	// window it would have owned is closed here instead.
	// A listener change since the address was chosen retired whatever
	// window was open then; this one would show an address that change made
	// stale, so it is closed the same way.
	state.mu.Lock()
	if !conn.Bound(computerPairingCleanupKey) || state.listeners != listeners {
		closing := !conn.Bound(computerPairingCleanupKey)
		state.mu.Unlock()
		// The callback logs a failed cancellation; the book keeps it.
		_ = book.Close(view.WindowID)
		if closing {
			return ComputerPairingWindow{}, transport.ErrConnectionClosing
		}
		return ComputerPairingWindow{}, errComputerPairingListenerChanged
	}
	state.windowID, state.owner, state.discoveryError = view.WindowID, conn, ""
	state.mu.Unlock()
	if a.currentSettings().Network.BindAll && !nativeLAN {
		a.advertiseComputerPairing(book, view.WindowID)
	}
	return ComputerPairingWindow{ID: view.WindowID, Address: address, ExpiresAtMs: view.ExpiresAtMs}, nil
}

// advertiseComputerPairing starts the LAN responders for one window with mu
// released, then adopts them only while that window is still the open one.
// A failure is the window's user-facing state, not a log line. A loopback
// Reach starts none and says so the same way.
func (a *App) advertiseComputerPairing(book *pairbootstrap.Book, windowID string) {
	srv := a.transportServer.Load()
	if srv == nil {
		return
	}
	var adv *nearby.Server
	err := nearby.ErrIsolated
	if !a.netReach.LoopbackOnly() {
		id, _ := a.backendIdentity()
		adv, err = nearby.Start(nearby.Advertisement{BackendID: id, Name: a.backendDisplayName, Port: portFromAddr(srv.Addr())})
	}
	s := &a.computerPairing
	s.mu.Lock()
	adopt := s.windowID == windowID && book.Snapshot(windowID).Open
	if adopt {
		s.advertiser = adv
		if err != nil {
			s.discoveryError = err.Error()
		}
	}
	s.mu.Unlock()
	if !adopt && adv != nil {
		_ = adv.Close()
	}
}

// CloseComputerPairing retires only the named window, never a replacement,
// and answers whether that window's invitation is durably cancelled. A
// window whose cancellation failed is tried again by closing it again.
//
//ao:scope access:admin
//ao:route home
func (a *App) CloseComputerPairing(id string) error {
	state := &a.computerPairing
	state.mu.Lock()
	if state.windowID == id {
		state.owner = nil
	}
	state.mu.Unlock()
	return a.closeComputerPairingWindow(id)
}

// closeComputerPairingWindow retires one window: the book removes it and
// cancels its invitation, answering for that cancellation, then the
// responders that window started are detached under mu and closed outside
// it.
func (a *App) closeComputerPairingWindow(id string) error {
	s := &a.computerPairing
	s.mu.Lock()
	book := s.book
	s.mu.Unlock()
	var err error
	if book != nil {
		err = book.Close(id)
	}
	var adv *nearby.Server
	s.mu.Lock()
	if s.windowID == id {
		adv, s.advertiser, s.discoveryError = s.advertiser, nil, ""
	}
	s.mu.Unlock()
	if adv != nil {
		_ = adv.Close()
	}
	return err
}

// closeComputerPairing retires the current window on a listener-affecting
// settings change and at shutdown. A failed cancellation is logged by the
// book's callback and stays refused until a later open or close retires it.
func (a *App) closeComputerPairing() {
	s := &a.computerPairing
	s.mu.Lock()
	id := s.windowID
	s.listeners++
	s.mu.Unlock()
	_ = a.closeComputerPairingWindow(id)
}

// cancelComputerPairingLink is the book's cancel callback, one cancellation
// statement per call. A link already settled is durable. Every failure is
// logged here, since the book's expiry has no caller to answer.
func (a *App) cancelComputerPairingLink(id string) error {
	err := a.CancelDevicePairing(id)
	if errors.Is(err, identity.ErrPairingRefused) {
		return nil
	}
	if err != nil {
		log.Printf("nearby pairing: cancel unfinished invitation %s: %v", id, err)
	}
	return err
}

func (a *App) computerPairingBook() *pairbootstrap.Book {
	s := &a.computerPairing
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.book
}

func (a *App) computerPairingOpen() bool {
	book := a.computerPairingBook()
	return book != nil && book.Snapshot("").Open
}

// ComputerPairingStatus shows independently derived digits only on the owner
// surface. Confirmation is enabled only after ordinary enrollment has completed.
//
//ao:scope access:admin
//ao:route home
func (a *App) ComputerPairingStatus(id string) (ComputerPairingView, error) {
	s := &a.computerPairing
	s.mu.Lock()
	book, discoveryError := s.book, s.discoveryError
	s.mu.Unlock()
	if book == nil {
		return ComputerPairingView{State: "expired"}, nil
	}
	view := book.Snapshot(id)
	if !view.Open {
		return ComputerPairingView{State: "expired"}, nil
	}
	out := ComputerPairingView{State: "waiting", VerificationNumber: view.VerificationNumber, DeviceLabel: view.Label, LinkID: view.LinkID, ExpiresAtMs: view.ExpiresAtMs, DiscoveryError: discoveryError}
	if view.RequestID != "" {
		out.State = "verifying"
	}
	if view.LinkID != "" {
		state, err := a.accessState()
		if err != nil {
			return out, err
		}
		link, _, err := state.sessions.PairingStatus(view.LinkID)
		if err != nil {
			return out, err
		}
		switch a.pairingState(link, state.sessions.Now()) {
		case "confirmed":
			out.State = "confirmed"
		case "redeemed":
			out.State = "ready"
		case "expired", "canceled":
			out.State = "expired"
		}
	}
	return out, nil
}

// computerPairingRefuses reports whether linkID is a computer pairing
// window's invitation that nothing may approve (pairbootstrap.Book.Refuses).
func (a *App) computerPairingRefuses(linkID string) bool {
	book := a.computerPairingBook()
	return book != nil && book.Refuses(linkID)
}

// computerPairingNumber is the number the owner compares for linkID: the
// window's own digits for its invitation, none for an invitation the book
// refuses, and otherwise the ordinary redemption's.
func (a *App) computerPairingNumber(linkID, fallback string) string {
	if a.computerPairingRefuses(linkID) {
		return ""
	}
	if book := a.computerPairingBook(); book != nil {
		view := book.Snapshot("")
		if view.LinkID == linkID && view.VerificationNumber != "" {
			return view.VerificationNumber
		}
	}
	return fallback
}

func (e authEndpoints) NearbyPair(_ context.Context, authority string, req pairbootstrap.Request) (any, error) {
	a := e.app
	book := a.computerPairingBook()
	if req.Op == "info" {
		id, _ := a.backendIdentity()
		return struct {
			BackendID string `json:"backendId"`
			Name      string `json:"name"`
			Open      bool   `json:"open"`
		}{id, a.backendDisplayName(), book != nil && book.Snapshot("").Open}, nil
	}
	if book == nil {
		return nil, pairbootstrap.ErrClosed
	}
	switch req.Op {
	case "begin":
		return book.Begin(req.Commitment)
	case "reveal":
		// The advertised LAN and tailnet authorities choose their own pins.
		host := authority
		if parsed, _, err := net.SplitHostPort(authority); err == nil {
			host = parsed
		}
		choice := "lan"
		if tail := a.tailnetStatus(); host != "" && strings.EqualFold(host, strings.TrimSuffix(tail.DNSName, ".")) {
			choice = "tailnet"
		}
		return book.Reveal(req.ID, req.Reveal, choice)
	default:
		return nil, pairbootstrap.ErrInvalid
	}
}
