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
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/store"
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
// window answers info with Open:false, which discovery discards. The address
// publication (computerRoutesPublication) retires a window whose address is
// no longer live. Lock order: the book's locks, mu, the publication's mu.
type computerPairingState struct {
	mu             sync.Mutex
	ops            sync.Mutex
	book           *pairbootstrap.Book
	advertiser     *nearby.Server
	discoveryError string
	// windowID is the last window opened here, which owner, advertiser,
	// choice and address belong to.
	windowID string
	owner    *transport.ConnState
	// choice is the network the window was opened on and address the
	// address it shows.
	choice, address string
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

var errComputerPairingListenerChanged = errors.New("this computer's network address changed while pairing opened; try again")

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
	book, previous := a.computerPairingBookAndWindow()
	// One reading of the published addresses, after prepare: the window's
	// address comes from it, and the admission below holds the window to it.
	live := a.publishComputerRoutes()
	address, err := live.pairing.on(networkChoice)
	if err != nil {
		return ComputerPairingWindow{}, err
	}
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
	}, func(w pairbootstrap.Snapshot) error {
		// The window becomes conn's, before anything can see it, only while
		// its tie is still bound, checked in the critical section the tie's
		// release takes: a connection that ended while the window opened has
		// already run that release. And only while its address is still the
		// published one: an address that moved since it was read retired
		// every window showing it, and this one would show it too.
		state.mu.Lock()
		defer state.mu.Unlock()
		if !conn.Bound(computerPairingCleanupKey) {
			return transport.ErrConnectionClosing
		}
		if a.publishedSerial() != live.serial {
			return errComputerPairingListenerChanged
		}
		state.windowID, state.owner, state.discoveryError = w.WindowID, conn, ""
		state.choice, state.address = networkChoice, address
		return nil
	})
	if err != nil {
		return ComputerPairingWindow{}, err
	}
	if live.advertise {
		a.advertiseComputerPairing(book, view.WindowID)
	}
	return ComputerPairingWindow{ID: view.WindowID, Address: address, ExpiresAtMs: view.ExpiresAtMs}, nil
}

// computerPairingBookAndWindow is the book, made on first use, and the last
// window opened in it.
func (a *App) computerPairingBookAndWindow() (*pairbootstrap.Book, string) {
	s := &a.computerPairing
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.book == nil {
		s.book = pairbootstrap.NewBook(a.cancelComputerPairingLink, pairbootstrap.CancelRetry)
	}
	return s.book, s.windowID
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

// retireStaleComputerPairing closes the window when the address it shows is
// no longer the one its network publishes. The publication calls it after
// every change. A failed cancellation is logged by the book's callback and
// retried by the book.
func (a *App) retireStaleComputerPairing(live pairingAddresses) {
	s := &a.computerPairing
	s.mu.Lock()
	id, choice, address := s.windowID, s.choice, s.address
	s.mu.Unlock()
	if id == "" {
		return
	}
	if current, err := live.on(choice); err == nil && current == address {
		return
	}
	_ = a.closeComputerPairingWindow(id)
}

// stopComputerPairing ends pairing at shutdown, before the store closes: the
// book retires the open window and refuses every later open and close, and
// no retry outlives it. A cancellation still unfinished is the restart
// recovery's (store.RetirePendingPairings).
func (a *App) stopComputerPairing() {
	book, id := a.computerPairingBookAndWindow()
	if err := book.Stop(); err != nil {
		log.Printf("nearby pairing: an invitation's cancellation is unfinished at shutdown; the next start retires it: %v", err)
	}
	var adv *nearby.Server
	s := &a.computerPairing
	s.mu.Lock()
	if s.windowID == id {
		adv, s.advertiser, s.discoveryError = s.advertiser, nil, ""
	}
	s.mu.Unlock()
	if adv != nil {
		_ = adv.Close()
	}
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
	discoveryError := s.discoveryError
	s.mu.Unlock()
	var view pairbootstrap.Snapshot
	var link store.PairingLink
	var standing pairbootstrap.Standing
	var now int64
	err := a.readPairingStanding(func(st pairbootstrap.Standing, sessions *identity.Sessions) error {
		standing, view, now = st, st.Window(), sessions.Now()
		if !view.Open || view.WindowID != id || view.LinkID == "" {
			return nil
		}
		var err error
		link, err = sessions.PairingLink(view.LinkID)
		return err
	})
	if err != nil {
		return ComputerPairingView{}, err
	}
	if !view.Open || view.WindowID != id {
		return ComputerPairingView{State: "expired"}, nil
	}
	out := ComputerPairingView{State: "waiting", VerificationNumber: view.VerificationNumber, DeviceLabel: view.Label, LinkID: view.LinkID, ExpiresAtMs: view.ExpiresAtMs, DiscoveryError: discoveryError}
	if view.RequestID != "" {
		out.State = "verifying"
	}
	if view.LinkID != "" {
		switch pairingState(link, now, standing) {
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

// readPairingStanding runs read with the computer pairing book's standing and
// the identity sessions, inside the book's Read: a status read and the book's
// retirement of the same invitation never interleave. read does only the
// pairing rows' reads; derivation, audit and events run after it returns.
func (a *App) readPairingStanding(read func(pairbootstrap.Standing, *identity.Sessions) error) error {
	state, err := a.accessState()
	if err != nil {
		return err
	}
	book := a.computerPairingBook()
	if book == nil {
		return read(pairbootstrap.Standing{}, state.sessions)
	}
	return book.Read(func(st pairbootstrap.Standing) error { return read(st, state.sessions) })
}

// computerPairingNumber is the number the owner compares for linkID, a
// redemption awaiting the owner: the open window's own digits for its
// invitation, and otherwise the ordinary redemption's. A computer pairing
// window's invitation awaits the owner only while its window is open
// (pairingState), so it never falls back.
func computerPairingNumber(linkID, fallback string, standing pairbootstrap.Standing) string {
	if view := standing.Window(); view.LinkID == linkID && view.VerificationNumber != "" {
		return view.VerificationNumber
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
