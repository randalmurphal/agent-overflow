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

// computerPairingState is the owner surface's side of one bootstrap window.
//
// mu guards the fields and is never held across I/O: SQLite writes, the
// multicast responders and the book's own lock all run with it released, so
// the book's cancel callback (which writes) and the connection cleanup can
// take it without ordering against anything. ops serializes opens, the one
// multi-step act (retire the old window, fence its link, open, advertise);
// reads and closes never take it. Expiry is the book's timer. The
// advertisement is retired when the window is closed, replaced, or its owner
// disconnects; an expired window answers info with Open:false, which
// discovery discards.
type computerPairingState struct {
	mu             sync.Mutex
	ops            sync.Mutex
	book           *pairbootstrap.Book
	advertiser     *nearby.Server
	discoveryError string
	network        string
	// windowID outlives its window: a close whose link retirement failed
	// answers by this id until the cancellation is durable.
	windowID string
	// linkID fences the last minted invitation against confirmation once its
	// window is gone, until its cancellation is durable.
	linkID string
	owner  *transport.ConnState
	armed  map[*transport.ConnState]bool
}

// OpenComputerPairing opens a short-lived invitation window. Merely discovering
// the host or requesting a connection never authorizes a session.
//
//ao:scope access:admin
//ao:route home
//ao:stepup
func (a *App) OpenComputerPairing(ctx context.Context, networkChoice, access string) (ComputerPairingWindow, error) {
	return a.openComputerPairing(ctx, networkChoice, access, "")
}
func (a *App) openComputerPairing(ctx context.Context, networkChoice, access, purpose string) (ComputerPairingWindow, error) {
	if networkChoice != "lan" && networkChoice != "tailnet" {
		return ComputerPairingWindow{}, errors.New("choose Local network or Tailscale")
	}
	if _, err := a.accessState(); err != nil {
		return ComputerPairingWindow{}, err
	}
	if _, err := identity.PairingAccess(access).Grants(); err != nil {
		return ComputerPairingWindow{}, err
	}
	address, err := network.PairingAddressOnNetwork(a.transportServer.Load(), a.persistedNetworkSettings(), networkChoice)
	if err != nil {
		return ComputerPairingWindow{}, err
	}
	nativeLAN := a.nativeLANStatus() != nil
	state := &a.computerPairing
	state.ops.Lock()
	defer state.ops.Unlock()
	state.mu.Lock()
	if state.book == nil {
		state.book = pairbootstrap.NewBook(a.cancelComputerPairingLink)
	}
	book, previous := state.book, state.windowID
	state.network = networkChoice
	state.mu.Unlock()
	a.closeComputerPairingWindow(previous)
	// Keep the last link's denial fence until its retirement is durable. A
	// failed SQLite cancellation must not become confirmable merely because
	// the owner opens another window and replaces the ephemeral SAS mapping.
	if err := a.retireComputerPairingLink(); err != nil {
		return ComputerPairingWindow{}, err
	}
	view := book.Open(func() (pairbootstrap.Invitation, error) {
		// The HTTP adapter chooses the listener actually used for the reveal.
		// Both enabled routes enroll the same host; no pin crosses listeners.
		state.mu.Lock()
		choice := state.network
		state.mu.Unlock()
		invite, err := a.mintDevicePairingPurpose("desktop", access, choice, purpose)
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	})
	conn := transport.ConnStateFromContext(ctx)
	state.mu.Lock()
	state.windowID, state.owner, state.discoveryError = view.WindowID, conn, ""
	arm := conn != nil && !state.armed[conn]
	if arm {
		if state.armed == nil {
			state.armed = make(map[*transport.ConnState]bool)
		}
		state.armed[conn] = true
	}
	state.mu.Unlock()
	if arm {
		cleanup := func() {
			state.mu.Lock()
			delete(state.armed, conn)
			id, owned := state.windowID, state.owner == conn
			if owned {
				state.owner = nil
			}
			state.mu.Unlock()
			if owned {
				a.closeComputerPairingWindow(id)
			}
		}
		if !conn.RegisterCleanup(cleanup) {
			cleanup()
			return ComputerPairingWindow{}, errors.New("the pairing screen disconnected")
		}
	}
	if a.currentSettings().Network.BindAll && !nativeLAN {
		a.advertiseComputerPairing(book, view.WindowID)
	}
	return ComputerPairingWindow{ID: view.WindowID, Address: address, ExpiresAtMs: view.ExpiresAtMs}, nil
}

// advertiseComputerPairing starts the LAN responders for one window with mu
// released, then adopts them only while that window is still the open one.
// A failure is the window's user-facing state, not a log line.
func (a *App) advertiseComputerPairing(book *pairbootstrap.Book, windowID string) {
	srv := a.transportServer.Load()
	if srv == nil {
		return
	}
	id, _ := a.backendIdentity()
	adv, err := nearby.Start(nearby.Advertisement{BackendID: id, Name: a.backendDisplayName, Port: portFromAddr(srv.Addr())})
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

// CloseComputerPairing retires only the named window, never a replacement.
//
//ao:scope access:admin
//ao:route home
func (a *App) CloseComputerPairing(id string) error {
	state := &a.computerPairing
	state.mu.Lock()
	match := state.book != nil && state.windowID == id
	if match {
		state.owner = nil
	}
	state.mu.Unlock()
	if !match {
		return nil
	}
	a.closeComputerPairingWindow(id)
	return a.retireComputerPairingLink()
}

// closeComputerPairingWindow retires one window: the book cancels its
// invitation through the callback, then the responders that window started
// are detached under mu and closed outside it.
func (a *App) closeComputerPairingWindow(id string) {
	s := &a.computerPairing
	s.mu.Lock()
	book := s.book
	s.mu.Unlock()
	if book != nil {
		book.Close(id)
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

// closeComputerPairing retires the current window on a listener-affecting
// settings change and at shutdown.
func (a *App) closeComputerPairing() {
	s := &a.computerPairing
	s.mu.Lock()
	id := s.windowID
	s.mu.Unlock()
	a.closeComputerPairingWindow(id)
}

// cancelComputerPairingLink is the book's cancel callback: window close,
// expiry, and a mint that finished after its window was gone.
func (a *App) cancelComputerPairingLink(id string) {
	if err := a.retireComputerPairingLinkID(id); err != nil {
		log.Printf("nearby pairing: cancel unfinished invitation: %v", err)
	}
}

// retireComputerPairingLink retries the fenced link. Nothing fenced is not
// an error: the book already retired the link durably, or none was minted.
func (a *App) retireComputerPairingLink() error {
	s := &a.computerPairing
	s.mu.Lock()
	id := s.linkID
	s.mu.Unlock()
	if id == "" {
		return nil
	}
	return a.retireComputerPairingLinkID(id)
}

// retireComputerPairingLinkID cancels one link and keeps it fenced until the
// cancellation is durable. A link already settled needs no second write.
func (a *App) retireComputerPairingLinkID(id string) error {
	err := a.CancelDevicePairing(id)
	if errors.Is(err, identity.ErrPairingRefused) {
		err = nil
	}
	s := &a.computerPairing
	s.mu.Lock()
	if err != nil {
		s.linkID = id
	} else if s.linkID == id {
		s.linkID = ""
	}
	s.mu.Unlock()
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
		switch pairingState(link, state.sessions.Now()) {
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

func (a *App) computerPairingNumber(linkID, fallback string) string {
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
		s := &a.computerPairing
		s.mu.Lock()
		s.network = choice
		s.mu.Unlock()
		sealed, err := book.Reveal(req.ID, req.Reveal)
		// Fence the invitation the window now carries. A mint the book
		// discarded because its window closed meanwhile is canceled through
		// the callback, which fences it itself if that write fails.
		if current := book.Snapshot(""); current.LinkID != "" {
			s.mu.Lock()
			s.linkID = current.LinkID
			s.mu.Unlock()
		}
		return sealed, err
	default:
		return nil, pairbootstrap.ErrInvalid
	}
}
