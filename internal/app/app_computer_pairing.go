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
	// Ended says why a window in state "expired" is no longer open, one of
	// pairbootstrap's Ending values: "deadline", "closed", "canceled",
	// "replaced", "addressChanged", "disconnected" or "stopped". Empty
	// while the window is open, and when nothing records why it ended: a
	// window from before this process started or older than the one before
	// the last, or an invitation the store settled while its window was
	// open.
	Ended string `json:"ended"`
}

// computerPairingState is the owner surface's side of one bootstrap window:
// which connection owns it and its LAN advertisement. The book owns the
// window and the retirement of every invitation a window minted; the address
// publication (computerRoutesPublication) records which window it admitted
// and retires that window when it moves.
//
// mu guards the fields and is never held across I/O: SQLite writes, the
// multicast responders and the book's settling calls all run with it
// released, so the book's callbacks and the connection cleanup can take it
// without ordering against anything. ops serializes opens, the one
// multi-step act (close the old window, prepare, open, advertise); reads and
// closes never take it. Expiry is the book's timer. The advertisement is
// retired when the window is closed, replaced, retired by a publication or
// its owner disconnects; an expired window answers info with Open:false,
// which discovery discards. The lock order is stated on pairbootstrap.Book.
type computerPairingState struct {
	mu             sync.Mutex
	ops            sync.Mutex
	book           *pairbootstrap.Book
	advertiser     *nearby.Server
	discoveryError string
	// windowID is the last window opened here, which owner, advertiser and
	// advertising belong to.
	windowID string
	owner    *transport.ConnState
	// advertising is closed once the window's responder start has finished
	// and its responders are adopted or closed; nil when no start is due. A
	// close of the window waits for it, so no responder outlives the close.
	advertising chan struct{}
	// awaitingStart, when set before the state is used, runs when a close
	// finds a responder start in flight, before it waits for it.
	awaitingStart func()
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

// openComputerPairing opens the window for conn, which owns it: the
// connection's end closes it. Everything that can refuse the open runs before
// prepare, which persists (own-device enrollment): the arguments, the
// previous window's retirement, and, rechecked after every wait and right
// before prepare, the caller's connection, whether the chosen network has an
// address, and whether pairing is shutting down. Only those changing while
// prepare runs, or prepare's own failure, fail the open after it.
//
// The window is admitted under the current publication, from which its
// address comes: a publication between the eligibility check and the
// admission changes which address the window shows, not whether it opens.
func (a *App) openComputerPairing(conn *transport.ConnState, networkChoice, access, purpose string, prepare func() error) (ComputerPairingWindow, error) {
	if err := checkPairingNetwork(networkChoice); err != nil {
		return ComputerPairingWindow{}, err
	}
	if _, err := a.accessState(); err != nil {
		return ComputerPairingWindow{}, err
	}
	if _, err := identity.PairingAccess(access).Grants(); err != nil {
		return ComputerPairingWindow{}, err
	}
	if _, err := a.publishComputerRoutes().pairing.on(networkChoice); err != nil {
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
			_ = a.closeComputerPairingWindow(id, pairbootstrap.EndedDisconnected)
		}
	}); err != nil {
		return ComputerPairingWindow{}, err
	}
	book, previous := a.computerPairingBookAndWindow()
	// A window replaces its predecessor only once every earlier invitation's
	// cancellation is durable: the close answers for the previous window,
	// and the book's open for any other still unfinished.
	if err := a.closeComputerPairingWindow(previous, pairbootstrap.EndedReplaced); err != nil {
		return ComputerPairingWindow{}, err
	}
	// The publication, and the close of any window it retires, run first;
	// the last reads before prepare are the published address, the
	// connection and shutdown, with nothing after the first that waits.
	a.publishComputerRoutes()
	if _, err := a.publishedPairingAddress(networkChoice); err != nil {
		return ComputerPairingWindow{}, err
	}
	if !conn.Bound(computerPairingCleanupKey) {
		return ComputerPairingWindow{}, transport.ErrConnectionClosing
	}
	if book.Stopped() {
		return ComputerPairingWindow{}, pairbootstrap.ErrStopped
	}
	if prepare != nil {
		if err := prepare(); err != nil {
			return ComputerPairingWindow{}, err
		}
	}
	var address, windowID string
	var advertise bool
	view, err := book.Open(func(route string) (pairbootstrap.Invitation, error) {
		// route is the listener the admitted reveal arrived on, which the
		// HTTP adapter names. Both enabled routes enroll the same host; no
		// pin crosses listeners. The page comes from the pairing set the
		// window was admitted under, never from live settings.
		invite, err := a.mintDevicePairingOn("desktop", access, purpose, true, func() (string, string, string, error) {
			page, err := a.pairingPageOfWindow(windowID, route)
			if err != nil {
				return "", "", "", err
			}
			return a.pairingPageAt(page)
		})
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	}, func(w pairbootstrap.Snapshot) error {
		// The window becomes conn's, before anything can see it, only while
		// its tie is still bound, checked in the critical section the tie's
		// release takes: a connection that ended while the window opened has
		// already run that release. Its address and its admission come from
		// the publication as it stands now.
		state.mu.Lock()
		defer state.mu.Unlock()
		if !conn.Bound(computerPairingCleanupKey) {
			return transport.ErrConnectionClosing
		}
		var err error
		if address, advertise, err = a.admitComputerPairingWindow(book, w.WindowID, networkChoice); err != nil {
			return err
		}
		// The mint reads windowID only once the book installs this window,
		// after this returns.
		windowID = w.WindowID
		state.windowID, state.owner, state.discoveryError = w.WindowID, conn, ""
		state.advertising = nil
		if advertise {
			state.advertising = make(chan struct{})
		}
		return nil
	})
	if err != nil {
		return ComputerPairingWindow{}, err
	}
	if advertise {
		a.advertiseComputerPairing(book, view.WindowID)
	}
	return ComputerPairingWindow{ID: view.WindowID, Address: address, ExpiresAtMs: view.ExpiresAtMs}, nil
}

// computerPairingBookAndWindow is the book, made on first use, and the last
// window opened in it. Every reader and writer of the book takes it here, so
// a status read and the first open share one book.
func (a *App) computerPairingBookAndWindow() (*pairbootstrap.Book, string) {
	s := &a.computerPairing
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.book == nil {
		s.book = pairbootstrap.NewBook(a.cancelComputerPairingLink, pairbootstrap.CancelRetry)
	}
	return s.book, s.windowID
}

// computerPairingBook is computerPairingBookAndWindow's book.
func (a *App) computerPairingBook() *pairbootstrap.Book {
	book, _ := a.computerPairingBookAndWindow()
	return book
}

// advertiseComputerPairing starts the LAN responders for one window with mu
// released, then adopts them only while that window is still the open one,
// and ends the start a close of that window waits for. A failure is the
// window's user-facing state, not a log line. A loopback Reach starts none
// and says so the same way.
func (a *App) advertiseComputerPairing(book *pairbootstrap.Book, windowID string) {
	s := &a.computerPairing
	s.mu.Lock()
	done := s.advertising
	s.mu.Unlock()
	if done == nil {
		return
	}
	defer close(done)
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
	return a.closeComputerPairingWindow(id, pairbootstrap.EndedClosed)
}

// closeComputerPairingWindow retires one window for why: the book removes it
// and cancels its invitation, answering for that cancellation, then the
// responders that window started are ended (endComputerPairingAdvertisement).
func (a *App) closeComputerPairingWindow(id string, why pairbootstrap.Ending) error {
	err := a.computerPairingBook().Close(id, why)
	a.endComputerPairingAdvertisement(id)
	return err
}

// endComputerPairingAdvertisement ends the LAN responders of window id once
// the window is removed: it waits for a responder start in flight, which
// then adopts nothing, and closes the adopted responders outside mu. Any
// other id is a no-op.
func (a *App) endComputerPairingAdvertisement(id string) {
	s := &a.computerPairing
	s.mu.Lock()
	var starting chan struct{}
	if s.windowID == id {
		starting = s.advertising
	}
	awaiting := s.awaitingStart
	s.mu.Unlock()
	if starting != nil {
		if awaiting != nil {
			select {
			case <-starting:
			default:
				awaiting()
			}
		}
		<-starting
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
	a.endComputerPairingAdvertisement(id)
}

// cancelComputerPairingLink is the book's cancel callback, one cancellation
// statement per call. The store's refusal of a link already settled (or
// unknown) is durable, and is what an owner's cancel answers. Every failure
// is logged here, since the book's expiry has no caller to answer.
func (a *App) cancelComputerPairingLink(id string) error {
	state, err := a.accessState()
	if err == nil {
		_, err = state.sessions.CancelPairing(id)
	}
	if errors.Is(err, identity.ErrPairingRefused) {
		return pairbootstrap.Settled(err)
	}
	if err != nil {
		log.Printf("pairing: cancel invitation %s: %v", id, err)
	}
	return err
}

// computerPairingGate is the identity pairing gate (identity.PairingGate):
// every redemption's spend and every owner confirmation is admitted by the
// computer pairing book, which refuses a window's invitation once its
// window is gone and orders that refusal against the window's removal.
type computerPairingGate struct{ app *App }

func (g computerPairingGate) Admit(linkID string, bootstrap bool, write func() error) error {
	err := g.app.computerPairingBook().Admit(linkID, bootstrap, write)
	if errors.Is(err, pairbootstrap.ErrClosed) {
		return identity.ErrPairingRefused
	}
	return err
}

func (a *App) computerPairingOpen() bool {
	return a.computerPairingBook().Snapshot("").Open
}

// ComputerPairingStatus shows independently derived digits only on the owner
// surface. Confirmation is enabled only after ordinary enrollment has completed.
// A window that is gone still answers for the invitation it published while
// it is the window opened last: a confirmation admitted before the window's
// deadline whose write lands after it reads confirmed, so a dialog whose
// approval reply was lost reaches the durable result.
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
	var sessions *identity.Sessions
	var now int64
	err := a.readPairingStanding(func(st pairbootstrap.Standing, ss *identity.Sessions) error {
		standing, view, sessions, now = st, st.Window(), ss, ss.Now()
		linkID := st.LinkOf(id)
		if linkID == "" {
			return nil
		}
		var err error
		link, err = ss.PairingLink(linkID)
		return err
	})
	if err != nil {
		return ComputerPairingView{}, err
	}
	if !view.Open || view.WindowID != id {
		if link.ConfirmedAt != 0 {
			return ComputerPairingView{State: "confirmed", LinkID: link.ID, DeviceLabel: a.pairedDeviceLabel(link)}, nil
		}
		return ComputerPairingView{State: "expired", Ended: string(standing.EndingOf(id))}, nil
	}
	out := ComputerPairingView{State: "waiting", DeviceLabel: view.Label, LinkID: view.LinkID, ExpiresAtMs: view.ExpiresAtMs, DiscoveryError: discoveryError}
	if view.RequestID != "" {
		out.State = "verifying"
	}
	if link.ID != "" {
		if out.VerificationNumber, err = ownerPairingNumber(link, standing, sessions); err != nil {
			return ComputerPairingView{}, err
		}
		switch pairingState(link, now, standing) {
		case "confirmed":
			out.State = "confirmed"
		case "redeemed":
			out.State = "ready"
		case "expired", "canceled":
			// The store settled the invitation while its window is open,
			// as a confirmation that could not finish does; the window
			// itself did not end, so no ending is named.
			out.State = "expired"
		}
	}
	return out, nil
}

// readPairingStanding runs read with the computer pairing book's standing and
// the identity sessions, inside the book's Read: a status read and the book's
// retirement or publication of the same invitation never interleave. read
// does only the pairing rows' reads; derivation, audit and events run after
// it returns.
func (a *App) readPairingStanding(read func(pairbootstrap.Standing, *identity.Sessions) error) error {
	state, err := a.accessState()
	if err != nil {
		return err
	}
	return a.computerPairingBook().Read(func(st pairbootstrap.Standing) error { return read(st, state.sessions) })
}

// ownerPairingNumber is the number the owner compares for link, read with
// standing (readPairingStanding). A link a computer pairing window minted
// (store.PairingLink.Bootstrap) shows the digits that window's exchange
// derived, which the requesting computer derived too
// (pairbootstrap.Client.Open), while the window holds it, and none
// otherwise: never the ordinary number, which that computer does not show.
// Any other link shows the ordinary redemption's number.
func ownerPairingNumber(link store.PairingLink, standing pairbootstrap.Standing, sessions *identity.Sessions) (string, error) {
	if link.Bootstrap {
		if view := standing.Window(); view.LinkID == link.ID {
			return view.VerificationNumber, nil
		}
		return "", nil
	}
	return sessions.VerificationNumber(link.ID, link.KeyThumbprint)
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
		}{id, a.backendDisplayName(), book.Snapshot("").Open}, nil
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
