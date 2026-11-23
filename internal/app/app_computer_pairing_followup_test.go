package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

// windowAwaitingApproval opens a LAN window on a, has a requester redeem its
// invitation, and returns the window and the invitation awaiting approval.
func windowAwaitingApproval(t *testing.T, a *App) (ComputerPairingWindow, pairbootstrap.Invitation) {
	t.Helper()
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	redeemOnHost(t, a, invite, "awaiting-approval-device")
	return window, invite
}

// requireAwaitingApproval checks that window is still open with its
// invitation ready, and that the owner can approve it.
func requireAwaitingApproval(t *testing.T, a *App, window ComputerPairingWindow, invite pairbootstrap.Invitation, when string) {
	t.Helper()
	if status, err := a.ComputerPairingStatus(window.ID); err != nil || status.State != "ready" {
		t.Fatalf("%s: the window = %+v (%v), want it awaiting approval", when, status, err)
	}
	if err := a.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("%s: approving the redemption = %v", when, err)
	}
}

// An owner's Cancel of a window's invitation another surface already
// approved is refused, as the store decides, and the window still closes;
// the approved device keeps its access.
func TestComputerPairingCancelOfAnApprovedWindowInvitationIsRefused(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	window, invite := windowAwaitingApproval(t, a)
	if err := a.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := a.CancelDevicePairing(invite.LinkID); !errors.Is(err, identity.ErrPairingRefused) {
		t.Fatalf("canceling an approved invitation = %v, want identity.ErrPairingRefused", err)
	}
	if a.computerPairingBook().Snapshot(window.ID).Open {
		t.Fatal("the window stayed open after its invitation's cancel was refused")
	}
	link, err := a.store.GetPairingLink(invite.LinkID)
	if err != nil || link.ConfirmedAt == 0 || link.CanceledAt != 0 {
		t.Fatalf("the approved invitation = %+v (%v), want confirmed and uncanceled", link, err)
	}
	if session, err := a.store.GetSession(link.SessionID); err != nil || session.RevokedAt != 0 || session.ActivatedAt == 0 {
		t.Fatalf("the approved device's session = %+v (%v), want it live", session, err)
	}
}

// A publication that retires a window the book is still admitting, between
// the window's admission and its installation, leaves it installed expired:
// it never reads open and admits no requester.
func TestComputerPairingAWindowRetiredWhileItIsAdmittedOpensExpired(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	book := a.computerPairingBook()
	a.publishComputerRoutes()
	g := holdFirstPublication(t, a)
	published := make(chan struct{})
	view, err := book.Open(invitationMint, func(w pairbootstrap.Snapshot) error {
		s := &a.computerPairing
		s.mu.Lock()
		_, _, err := a.admitComputerPairingWindow(book, w.WindowID, "lan")
		s.windowID = w.WindowID
		s.mu.Unlock()
		// The publication chooses this window after its admission and
		// before Open installs it.
		moveNativeAddress(a, "192.168.1.61")
		go func() { a.publishComputerRoutes(); close(published) }()
		<-g.reached
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if book.Snapshot(view.WindowID).Open {
		t.Fatal("a window the publication retired before it was installed reads open")
	}
	if _, err := book.Begin(pairbootstrapClient(t).Commitment()); !errors.Is(err, pairbootstrap.ErrClosed) {
		t.Fatalf("a requester to the retired window = %v, want pairbootstrap.ErrClosed", err)
	}
	g.open()
	<-published
}

func pairbootstrapClient(t *testing.T) *pairbootstrap.Client {
	t.Helper()
	c, err := pairbootstrap.Start("desktop", "windows")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A second address the Windows launcher forwards changes this computer's
// routes but not what a LAN window shows: the window and the redemption
// awaiting its approval stay.
func TestComputerPairingASecondForwardedAddressKeepsTheWindow(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	// The LAN routes carry the listener's certificate pin.
	SetCertFingerprint(a, "sha256:"+strings.Repeat("ab", 32))
	window, invite := windowAwaitingApproval(t, a)
	before := ComputerRoutes(a)
	s := &a.nativeNetwork
	port := portFromAddr(a.transportServer.Load().Addr())
	s.mu.Lock()
	s.addresses = append(s.addresses, fmt.Sprintf("https://10.0.0.7:%d", port))
	s.mu.Unlock()
	a.publishComputerRoutes()
	if after := ComputerRoutes(a); len(after) == len(before) {
		t.Fatalf("the routes did not change (%v), so the case is not exercised", after)
	}
	requireAwaitingApproval(t, a, window, invite, "after a second forwarded address")
}

// A saved domain whose certificate finishes provisioning adds a route but
// leaves what a LAN window shows alone: the window and the redemption
// awaiting its approval stay.
func TestComputerPairingADomainCertificateArrivingKeepsTheWindow(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, source := computerPairingHostWithCertificates(t, "pair.example.test")
	window, invite := windowAwaitingApproval(t, a)
	source.SetDomain("pair.example.test", &tls.Certificate{})
	a.publishComputerRoutes()
	found := false
	for _, route := range ComputerRoutes(a) {
		found = found || strings.Contains(route.Endpoint, "pair.example.test")
	}
	if !found {
		t.Fatalf("the domain's route is not published (%v), so the case is not exercised", ComputerRoutes(a))
	}
	requireAwaitingApproval(t, a, window, invite, "after the domain's certificate arrived")
}

// computerPairingHostWithCertificates is computerPairingHost on a listener
// that resolves its certificates from the returned source, with domain saved
// as the canonical domain.
func computerPairingHostWithCertificates(t *testing.T, domain string) (*App, *transport.CertificateSource) {
	t.Helper()
	a, _ := newTestAppWithStorePath(t)
	if err := a.initIdentity("backend-under-test"); err != nil {
		t.Fatal(err)
	}
	stored, err := a.store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	a.storeIdentity.Store(&stored)
	source := transport.NewCertificateSource()
	srv, err := transport.New(transport.Config{
		Dispatcher:   transport.NewDispatcher(),
		EventBus:     transport.NewEventBus(8),
		Token:        "test-network-token",
		Certificates: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	if err := srv.Rebind(net.JoinHostPort("0.0.0.0", strconv.Itoa(portFromAddr(srv.Addr()))), nil); err != nil {
		t.Fatal(err)
	}
	a.SetTransportServer(srv)
	if _, err := a.settings.SetNetwork(settings.NetworkSettings{BindAll: true, CanonicalDomain: domain}); err != nil {
		t.Fatal(err)
	}
	a.nativeNetwork.seen = true
	a.nativeNetwork.addresses = []string{fmt.Sprintf("https://192.168.1.55:%d", portFromAddr(srv.Addr()))}
	t.Cleanup(a.stopComputerPairing)
	return a, source
}

// What a window depends on decides its retirement: turning LAN access off
// under a Tailscale window whose LAN responders run changes what they
// advertise, so the window is retired; a route change that leaves a window's
// address, pin and responders alone does not.
func TestComputerPairingWindowTermsFollowItsResponders(t *testing.T) {
	p := &computerRoutesPublication{
		pairing:   pairingAddresses{lan: "https://192.168.1.55:4000", tailnet: "https://host.tail.ts.net", lanPin: "pin"},
		advertise: true,
	}
	admitted := p.termsLocked("tailnet")
	p.routes = nil
	if p.termsLocked("tailnet") != admitted {
		t.Fatal("a window's terms moved with nothing it depends on")
	}
	p.pairing.lan, p.pairing.lanPin, p.pairing.lanErr = "", "", "enable Allow LAN connections before pairing"
	p.advertise = false
	if p.termsLocked("tailnet") == admitted {
		t.Fatal("turning LAN off under a Tailscale window with responders left its terms as they were")
	}
}

// A window's invitation is admitted only while a live window holds it: an
// invitation carrying the mark that no window published cannot be redeemed.
func TestComputerPairingAMarkedInvitationNoWindowHoldsIsRefused(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	invite, err := a.mintDevicePairingPurpose("desktop", "full", "lan", "", true)
	if err != nil {
		t.Fatal(err)
	}
	link, err := deviceclient.DecodeLink(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := a.identityState().sessions.RedeemPairing(identity.RedemptionRequest{Token: link.Token, Proof: identity.DeviceProof{Value: "stray-device"}}); reason != identity.ReasonUnknownCredential {
		t.Fatalf("redeeming a marked invitation no window holds = %s, want %s", reason, identity.ReasonUnknownCredential)
	}
	if stored, err := a.store.GetPairingLink(invite.LinkID); err != nil || stored.RedeemedAt != 0 {
		t.Fatalf("the marked invitation = %+v (%v), want it unspent", stored, err)
	}
}

// An own-device open whose network loses its address, or whose connection
// ends, while the open waits for the previous window's retirement enrolls
// nothing: both are checked again right before the enrollment.
func TestAnOwnPairingOpenThatChangesWhileWaitingEnrollsNothing(t *testing.T) {
	for name, change := range map[string]func(*App, *transport.ConnState){
		"the network loses its address": func(a *App, _ *transport.ConnState) {
			s := &a.nativeNetwork
			s.mu.Lock()
			s.addresses = nil
			s.mu.Unlock()
		},
		"the connection ends": func(_ *App, conn *transport.ConnState) { conn.RunCleanups() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ownOpenThatChangesWhileWaiting(t, change)
		})
	}
}

func ownOpenThatChangesWhileWaiting(t *testing.T, change func(*App, *transport.ConnState)) {
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	l := countCancellations(a)
	manager, err := newTestBackends(t, t.TempDir(), "Host", "test")
	if err != nil {
		t.Fatal(err)
	}
	SetAttachedBackends(a, manager)
	if _, err := a.OpenComputerPairing(connCtx(t), "lan", "full"); err != nil {
		t.Fatal(err)
	}
	revealOnBook(t, a.computerPairingBook())
	g := newTestGate()
	t.Cleanup(g.open)
	l.mu.Lock()
	l.hold = func(string) { g.wait() }
	l.mu.Unlock()
	ctx, conn := newTestConnection(t, transport.ConnPrincipal{})
	opened := make(chan error, 1)
	go func() { _, err := a.OpenOwnComputerPairing(ctx, "lan"); opened <- err }()
	// The open passed its first check and waits for the previous window's
	// retirement.
	<-g.reached
	change(a, conn)
	l.mu.Lock()
	l.hold = nil
	l.mu.Unlock()
	g.open()
	if err := <-opened; err == nil {
		t.Fatal("the own pairing open succeeded")
	}
	if _, err := a.backends.OwnIdentity(); !errors.Is(err, deviceclient.ErrNoDeviceKey) {
		t.Fatalf("this computer's own identity = %v, want none enrolled", err)
	}
}
