package app

import (
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"time"

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
}

type computerPairingState struct {
	mu         sync.Mutex
	book       *pairbootstrap.Book
	advertiser *nearby.Server
	timer      *time.Timer
	network    string
	windowID   string
	linkID     string
	owner      *transport.ConnState
	armed      map[*transport.ConnState]bool
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
	state.mu.Lock()
	state.close()
	// Keep the last link's denial fence until its retirement is durable. A
	// failed SQLite cancellation must not become confirmable merely because
	// the owner opens another window and replaces the ephemeral SAS mapping.
	if err := a.retireComputerPairingLink(state.linkID); err != nil {
		state.mu.Unlock()
		return ComputerPairingWindow{}, err
	}
	if state.book == nil {
		state.book = pairbootstrap.NewBook(func(id string) {
			if err := a.retireComputerPairingLink(id); err != nil {
				log.Printf("nearby pairing: cancel unfinished invitation: %v", err)
			}
		})
	}
	state.closeAdvertisement()
	state.network = networkChoice
	view := state.book.Open(func() (pairbootstrap.Invitation, error) {
		// The HTTP adapter chooses the listener actually used for the reveal.
		// Both enabled routes enroll the same host; no pin crosses listeners.
		invite, err := a.mintDevicePairingPurpose("desktop", access, state.network, purpose)
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	})
	state.windowID, state.linkID = view.WindowID, ""
	if a.currentSettings().Network.BindAll && !nativeLAN {
		if srv := a.transportServer.Load(); srv != nil {
			id, _ := a.backendIdentity()
			state.advertiser, err = nearby.Start(nearby.Advertisement{BackendID: id, Name: a.backendDisplayName, Port: portFromAddr(srv.Addr())})
			if err != nil {
				log.Printf("nearby pairing: %v", err)
			}
		}
	}
	state.timer = time.AfterFunc(time.Until(time.UnixMilli(view.ExpiresAtMs)), func() { _ = a.CloseComputerPairing(view.WindowID) })
	conn := transport.ConnStateFromContext(ctx)
	state.owner = conn
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
			if state.owner == conn {
				state.close()
				state.owner = nil
			}
			state.mu.Unlock()
		}
		if !conn.RegisterCleanup(cleanup) {
			cleanup()
			return ComputerPairingWindow{}, errors.New("the pairing screen disconnected")
		}
	}
	return ComputerPairingWindow{ID: view.WindowID, Address: address, ExpiresAtMs: view.ExpiresAtMs}, nil
}

// CloseComputerPairing retires only the named window, never a replacement.
//
//ao:scope access:admin
//ao:route home
func (a *App) CloseComputerPairing(id string) error {
	state := &a.computerPairing
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.book != nil {
		if state.windowID == id {
			state.close()
			state.owner = nil
			return a.retireComputerPairingLink(state.linkID)
		}
	}
	return nil
}

func (a *App) retireComputerPairingLink(id string) error {
	if id == "" {
		return nil
	}
	err := a.CancelDevicePairing(id)
	if errors.Is(err, identity.ErrPairingRefused) {
		return nil
	}
	return err
}

func (s *computerPairingState) closeAdvertisement() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.advertiser != nil {
		_ = s.advertiser.Close()
		s.advertiser = nil
	}
}

func (s *computerPairingState) close() {
	s.closeAdvertisement()
	if s.book != nil {
		s.book.Close(s.windowID)
	}
}

func (a *App) closeComputerPairing() {
	s := &a.computerPairing
	s.mu.Lock()
	defer s.mu.Unlock()
	s.close()
}

// ComputerPairingStatus shows independently derived digits only on the owner
// surface. Confirmation is enabled only after ordinary enrollment has completed.
//
//ao:scope access:admin
//ao:route home
func (a *App) ComputerPairingStatus(id string) (ComputerPairingView, error) {
	s := &a.computerPairing
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.book == nil {
		return ComputerPairingView{State: "expired"}, nil
	}
	view := s.book.Snapshot(id)
	if !view.Open {
		return ComputerPairingView{State: "expired"}, nil
	}
	out := ComputerPairingView{State: "waiting", VerificationNumber: view.VerificationNumber, DeviceLabel: view.Label, LinkID: view.LinkID, ExpiresAtMs: view.ExpiresAtMs}
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
	s := &a.computerPairing
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.book != nil {
		view := s.book.Snapshot("")
		if view.LinkID == linkID && view.VerificationNumber != "" {
			return view.VerificationNumber
		}
	}
	return fallback
}

func (e authEndpoints) NearbyPair(_ context.Context, authority string, req pairbootstrap.Request) (any, error) {
	a := e.app
	s := &a.computerPairing
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Op == "info" {
		id, _ := a.backendIdentity()
		return struct {
			BackendID string `json:"backendId"`
			Name      string `json:"name"`
			Open      bool   `json:"open"`
		}{id, a.backendDisplayName(), s.book != nil && s.book.Snapshot("").Open}, nil
	}
	if s.book == nil {
		return nil, pairbootstrap.ErrClosed
	}
	switch req.Op {
	case "begin":
		return s.book.Begin(req.Commitment)
	case "reveal":
		// The advertised LAN and tailnet authorities choose their own pins.
		host := authority
		if parsed, _, err := net.SplitHostPort(authority); err == nil {
			host = parsed
		}
		tail := a.tailnetStatus()
		if strings.EqualFold(host, strings.TrimSuffix(tail.DNSName, ".")) && host != "" {
			s.network = "tailnet"
		} else {
			s.network = "lan"
		}
		sealed, err := s.book.Reveal(req.ID, req.Reveal)
		if current := s.book.Snapshot(""); current.LinkID != "" {
			s.linkID = current.LinkID
		}
		return sealed, err
	default:
		return nil, pairbootstrap.ErrInvalid
	}
}
