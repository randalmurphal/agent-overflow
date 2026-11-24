package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/network"
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

// A window approved on one surface and then canceled from another (the
// store refuses the cancel) reads confirmed in its own dialog even once a
// newer window opened: the status answers from the invitation's row, not
// from the ending the cancel recorded. Once the device is removed, the same
// window reads that its device was removed, not confirmed and not
// canceled, and the modal's poll says the same; Restore leaves its session
// ended.
func TestComputerPairingAnApprovedWindowReadsConfirmedAfterARefusedCancelAndANewerWindow(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	window, invite := windowAwaitingApproval(t, a)
	if err := a.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := a.CancelDevicePairing(invite.LinkID); !errors.Is(err, identity.ErrPairingRefused) {
		t.Fatalf("canceling the approved invitation = %v, want it refused", err)
	}
	if _, err := a.OpenComputerPairing(connCtx(t), "lan", "full"); err != nil {
		t.Fatal(err)
	}
	if status, err := a.ComputerPairingStatus(window.ID); err != nil || status.State != "confirmed" || status.LinkID != invite.LinkID {
		t.Fatalf("the approved window after a newer one opened = %+v (%v), want it confirmed", status, err)
	}
	link, err := a.store.GetPairingLink(invite.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RevokeAccessDevice(link.DeviceID); err != nil {
		t.Fatal(err)
	}
	requireAccessLost(t, a, window.ID, invite.LinkID, deviceRemoved, "the window before the newer one, after its device was removed")
	if err := a.RestoreAccessDevice(link.DeviceID); err != nil {
		t.Fatal(err)
	}
	requireAccessLost(t, a, window.ID, invite.LinkID, accessEnded, "the window before the newer one, after its device was restored")
}

// accessLost is one way an approved pairing lost its device's access, as
// each surface reads it.
type accessLost struct {
	err     error
	message string
	ending  string
}

var (
	accessEnded = accessLost{identity.ErrPairingAccessEnded,
		"this device was allowed, and its access has since ended or expired; pair it again", "accessEnded"}
	deviceRemoved = accessLost{identity.ErrPairingDeviceRemoved,
		"this device was allowed, and has since been removed in Devices; choose Restore or Forget on it there, then pair it again", "deviceRemoved"}
)

// requireAccessLost checks that the dialog of an approved window whose
// device lost its access reads that ending, that the ordinary modal's poll
// of its invitation does, and that a stale Allow on it is refused with
// words that say so.
func requireAccessLost(t *testing.T, a *App, windowID, linkID string, lost accessLost, when string) {
	t.Helper()
	err := a.ConfirmDevicePairing(linkID)
	if !errors.Is(err, lost.err) {
		t.Fatalf("%s: a stale approval = %v, want %v", when, err, lost.err)
	}
	requirePersonSees(t, err, lost.message)
	if status, err := a.ComputerPairingStatus(windowID); err != nil || status.State != "expired" || status.Ended != lost.ending {
		t.Fatalf("%s: the dialog's status read = %+v (%v), want expired with Ended %s", when, status, err, lost.ending)
	}
	if status, err := a.DevicePairingStatus(linkID); err != nil || status.State != lost.ending || !status.Settled() || status.Confirmed() {
		t.Fatalf("%s: the modal's poll = %+v (%v), want %s, settled and not confirmed", when, status, err, lost.ending)
	}
}

// An owner approves a window's invitation on one view and then ends the
// device's session in Devices: the other view's stale Allow is refused, and
// its follow-up status read of the still-open window says the access ended,
// so the dialog never shows the computer as paired. The ordinary pairing
// modal's poll of the same invitation says the same.
func TestComputerPairingAnApprovedWindowWhoseSessionEndedReadsAccessEnded(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	window, invite := windowAwaitingApproval(t, a)
	if err := a.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if status, err := a.DevicePairingStatus(invite.LinkID); err != nil || status.State != "confirmed" {
		t.Fatalf("the modal's poll of the approved invitation = %+v (%v), want confirmed", status, err)
	}
	link, err := a.store.GetPairingLink(invite.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RevokeAccessSession(link.SessionID); err != nil {
		t.Fatal(err)
	}
	if !a.computerPairingBook().Snapshot(window.ID).Open {
		t.Fatal("the approved window closed; this sequence needs it open")
	}
	requireAccessLost(t, a, window.ID, invite.LinkID, accessEnded, "the open window, after its session ended")
	status, err := a.DevicePairingStatus(invite.LinkID)
	if err != nil || status.State != "accessEnded" || !status.Settled() || status.Confirmed() {
		t.Fatalf("the modal's poll after the session ended = %+v (%v), want accessEnded, settled and not confirmed", status, err)
	}
}

// A second approval of a device another owner view already approved tells
// the owner the device is allowed, whether the window still holds its
// invitation (the store refuses the write) or has closed (the gate refuses
// it). Once the device's session is revoked, it is no longer reported as
// allowed.
func TestComputerPairingASecondApprovalSaysTheDeviceIsAlreadyAllowed(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	window, invite := windowAwaitingApproval(t, a)
	if err := a.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	allowed := func(when string) {
		t.Helper()
		err := a.ConfirmDevicePairing(invite.LinkID)
		if !errors.Is(err, identity.ErrPairingAlreadyConfirmed) {
			t.Fatalf("%s: a second approval = %v, want identity.ErrPairingAlreadyConfirmed", when, err)
		}
		requirePersonSees(t, err, "this device is already allowed")
	}
	allowed("with the window open")
	if err := a.CloseComputerPairing(window.ID); err != nil {
		t.Fatal(err)
	}
	allowed("with the window closed")
	link, err := a.store.GetPairingLink(invite.LinkID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RevokeAccessSession(link.SessionID); err != nil {
		t.Fatal(err)
	}
	requireAccessLost(t, a, window.ID, invite.LinkID, accessEnded, "after the device's session was revoked")
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

// A window depends on the whole pairing set: turning LAN access off under a
// Tailscale window, whose LAN responders advertise, changes it, and so
// retires that window; a change only to why a network has no address does
// not.
func TestComputerPairingLANTurnedOffChangesThePairingSet(t *testing.T) {
	on := pairingAddresses{
		lan: "https://192.168.1.55:4000", tailnet: "https://host.tail.ts.net", advertise: true,
		lanPage:     network.PairingPage{Scheme: "http", Authority: "192.168.1.55:4000", Fingerprint: "pin"},
		tailnetPage: network.PairingPage{Scheme: "https", Authority: "host.tail.ts.net"},
	}
	off := on
	off.lan, off.lanPage, off.advertise = "", network.PairingPage{}, false
	off.lanErr, off.lanPageErr = "enable Allow LAN connections before pairing", "enable Allow LAN connections before pairing over the local network"
	if off.set() == on.set() {
		t.Fatal("turning LAN off under a Tailscale window left the pairing set as it was")
	}
	reworded := off
	reworded.lanErr = "the computer's local network connection is not ready"
	if reworded.set() != off.set() {
		t.Fatal("a new reason for a network having no address moved the pairing set")
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
// ends, while the open waits for the previous window's retirement or for
// the publication after it enrolls nothing: both are read last, right before
// the enrollment.
func TestAnOwnPairingOpenThatChangesWhileWaitingEnrollsNothing(t *testing.T) {
	withdraw := func(a *App) {
		s := &a.nativeNetwork
		s.mu.Lock()
		s.addresses = nil
		s.mu.Unlock()
	}
	for name, change := range map[string]func(*App, *transport.ConnState){
		"the network loses its address": func(a *App, _ *transport.ConnState) { withdraw(a) },
		"the connection ends":           func(_ *App, conn *transport.ConnState) { conn.RunCleanups() },
		"the connection ends during the publication": func(a *App, conn *transport.ConnState) {
			onNextPublication(a, conn.RunCleanups)
		},
		// The moved address makes the open's publication retire the previous
		// window; the withdrawal, published as every move is, lands while
		// that publication closes it.
		"the address is withdrawn while the publication closes the previous window": func(a *App, _ *transport.ConnState) {
			moveNativeAddress(a, "192.168.1.62")
			onNextPublication(a, func() {
				withdraw(a)
				a.publishComputerRoutes()
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ownOpenThatChangesWhileWaiting(t, change)
		})
	}
}

// onNextPublication runs f once, in the next publication, after it has
// chosen the window to retire and released its lock and before it closes
// that window. A publication f runs itself passes straight through.
func onNextPublication(a *App, f func()) {
	var ran atomic.Bool
	state := &a.computerRoutesPublished
	state.mu.Lock()
	state.chosen = func() {
		if ran.CompareAndSwap(false, true) {
			f()
		}
	}
	state.mu.Unlock()
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

// publishTailnet makes the publication hold a Tailscale address after every
// publication until the returned stop runs, as publications would while
// Tailscale runs. This host has no Tailscale, so the first publication after
// stop reads it gone.
func publishTailnet(a *App) (stop func()) {
	state := &a.computerRoutesPublished
	seed := func() {
		state.mu.Lock()
		state.pairing.tailnet, state.pairing.tailnetErr = "https://host.tail.ts.net", ""
		state.pairing.tailnetPage, state.pairing.tailnetPageErr = network.PairingPage{Scheme: "https", Authority: "host.tail.ts.net"}, ""
		state.mu.Unlock()
	}
	seed()
	state.mu.Lock()
	state.chosen = seed
	state.mu.Unlock()
	return func() {
		state.mu.Lock()
		state.chosen = nil
		state.mu.Unlock()
	}
}

// A LAN window whose requester began, and has not reached the mint, is
// retired when Tailscale goes away: a new requester finds it closed, not
// busy.
func TestComputerPairingTailscaleLeavingRetiresALANWindowWithARequester(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	stop := publishTailnet(a)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	book := a.computerPairingBook()
	beginOnBook(t, book)
	stop()
	a.publishComputerRoutes()
	if status, err := a.ComputerPairingStatus(window.ID); err != nil || status.State != "expired" || status.Ended != "networkChanged" {
		t.Fatalf("the LAN window after Tailscale went away = %+v (%v), want it retired for the address change", status, err)
	}
	if _, err := book.Begin(pairbootstrapClient(t).Commitment()); !errors.Is(err, pairbootstrap.ErrClosed) {
		t.Fatalf("a new requester after Tailscale went away = %v, want pairbootstrap.ErrClosed", err)
	}
}

// A LAN address that moves during a reveal, before it is published, leaves
// the invitation on the address the window was admitted under; the
// publication of the move retires the window and its invitation, and the
// requester pairs on a fresh window at the new address.
func TestComputerPairingALANAddressMovingDuringARevealRetiresTheWindow(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	book := a.computerPairingBook()
	c, ch := beginOnBook(t, book)
	moveNativeAddress(a, "192.168.1.70")
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := book.Reveal(ch.ID, r, "lan")
	if err != nil {
		t.Fatal(err)
	}
	invite, _, err := c.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(invite.URL, "192.168.1.55") {
		t.Fatalf("the invitation = %s, want it on the address the window was admitted under", invite.URL)
	}
	a.publishComputerRoutes()
	if status, err := a.ComputerPairingStatus(window.ID); err != nil || status.State != "expired" || status.Ended != "networkChanged" {
		t.Fatalf("the window after its address moved = %+v (%v), want it retired for the address change", status, err)
	}
	requireCanceled(t, a, invite.LinkID)
	fresh, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	next := revealOnBook(t, book)
	if !strings.Contains(fresh.Address, "192.168.1.70") || !strings.Contains(next.URL, "192.168.1.70") {
		t.Fatalf("the fresh window shows %s and invites at %s, want the moved address", fresh.Address, next.URL)
	}
	redeemOnHost(t, a, next, "moved-address-device")
	requireAwaitingApproval(t, a, fresh, next, "on the fresh window")
}

// The owner's status of a window that is no longer open says why it ended,
// as each path that ends one names it: a close, a newer window, a cancel of
// its invitation and the end of the connection that opened it. The address
// change is checked where an address moves.
func TestComputerPairingStatusSaysWhyAWindowEnded(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	open := func(ctx context.Context) ComputerPairingWindow {
		t.Helper()
		window, err := a.OpenComputerPairing(ctx, "lan", "full")
		if err != nil {
			t.Fatal(err)
		}
		return window
	}
	ended := func(window ComputerPairingWindow, want string) {
		t.Helper()
		status, err := a.ComputerPairingStatus(window.ID)
		if err != nil || status.State != "expired" || status.Ended != want {
			t.Fatalf("window %s = %+v (%v), want it ended %q", window.ID, status, err, want)
		}
	}
	closed := open(connCtx(t))
	if err := a.CloseComputerPairing(closed.ID); err != nil {
		t.Fatal(err)
	}
	ended(closed, "closed")

	replaced := open(connCtx(t))
	canceled := open(connCtx(t))
	ended(replaced, "replaced")

	invite := revealOnBook(t, a.computerPairingBook())
	if err := a.CancelDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	ended(canceled, "canceled")

	ctx, conn := newTestConnection(t, transport.ConnPrincipal{})
	dropped := open(ctx)
	conn.RunCleanups()
	ended(dropped, "disconnected")
}

// An open on a network this computer cannot pair on, as Try again makes
// once that network went away, says which network, why, and the other
// choice.
func TestComputerPairingAnOpenOnAnUnavailableNetworkSaysWhatToDo(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	_, err := a.OpenComputerPairing(connCtx(t), "tailnet", "full")
	const want = "Tailscale is not available for pairing on this computer: turn on Join my tailnet in Tailscale settings, and turn HTTPS on in your tailnet's admin panel, before pairing. To pair now, close this and choose Local network"
	if err == nil || err.Error() != want {
		t.Fatalf("an open on Tailscale without it = %v, want %q", err, want)
	}
}

// An unavailable network's message offers the other network only when this
// computer's pairing set has it, and otherwise says why the other is
// unavailable too, which names what to turn on.
func TestTheUnavailableNetworkMessageOffersOnlyAnAvailableNetwork(t *testing.T) {
	const lanOff = "enable Allow LAN connections before pairing"
	const tailnetOff = "turn on Join my tailnet in Tailscale settings, and turn HTTPS on in your tailnet's admin panel, before pairing"
	for _, test := range []struct {
		name   string
		set    pairingAddresses
		choice string
		want   string
	}{
		{"Tailscale off, Local network on", pairingAddresses{lan: "https://192.168.1.5:8123", tailnetErr: tailnetOff}, "tailnet",
			"Tailscale is not available for pairing on this computer: " + tailnetOff + ". To pair now, close this and choose Local network"},
		{"Local network off, Tailscale on", pairingAddresses{tailnet: "https://desk.example.ts.net", lanErr: lanOff}, "lan",
			"Local network is not available for pairing on this computer: " + lanOff + ". To pair now, close this and choose Tailscale"},
		{"both off, Tailscale chosen", pairingAddresses{lanErr: lanOff, tailnetErr: tailnetOff}, "tailnet",
			"Tailscale is not available for pairing on this computer: " + tailnetOff + ". Local network is not available either: " + lanOff},
		{"both off, Local network chosen", pairingAddresses{lanErr: lanOff, tailnetErr: tailnetOff}, "lan",
			"Local network is not available for pairing on this computer: " + lanOff + ". Tailscale is not available either: " + tailnetOff},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.set.on(test.choice); err == nil || err.Error() != test.want {
				t.Fatalf("on(%s) = %v, want %q", test.choice, err, test.want)
			}
		})
	}
}

// Device access asked for before this computer has its session core, its
// identity or its listener says to wait, not which part is missing.
func TestDeviceAccessBeforeStartupFinishesSaysToWait(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	const starting = "this computer is still starting; try again in a moment"
	t.Run("no store", func(t *testing.T) {
		t.Parallel()
		requirePersonSees(t, (&App{}).CancelDevicePairing("link"), starting)
	})
	t.Run("no session core", func(t *testing.T) {
		t.Parallel()
		a := newTestAppWithStore(t)
		_, err := a.MintDevicePairing("phone", "full")
		requirePersonSees(t, err, starting)
		requirePersonSees(t, a.CancelDevicePairing("link"), starting)
		requirePersonSees(t, a.ConfirmDevicePairing("link"), starting)
	})
	t.Run("no listener", func(t *testing.T) {
		t.Parallel()
		a, _ := newTestAppWithStorePath(t)
		if err := a.initIdentity("backend-under-test"); err != nil {
			t.Fatal(err)
		}
		_, err := a.MintDevicePairing("phone", "full")
		requirePersonSees(t, err, starting)
	})
	t.Run("no backend identity", func(t *testing.T) {
		t.Parallel()
		a, _ := computerPairingHost(t)
		a.storeIdentity.Store(nil)
		_, err := a.MintDevicePairing("phone", "full")
		requirePersonSees(t, err, starting)
	})
}

// A cancel or a close whose cancellation the store refuses to write says so
// and what happens next, keeping the cause for errors.Is.
func TestComputerPairingAnUnsavedCancellationSaysWhatHappensNext(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	invite, err := a.MintDevicePairing("phone", "full")
	if err != nil {
		t.Fatal(err)
	}
	window, _ := windowAwaitingApproval(t, a)
	restore := rejectCancellations(t, path)
	defer restore()
	requirePersonSees(t, a.CancelDevicePairing(invite.LinkID), "this computer could not save the cancellation; try Cancel again")
	const unsaved = "this computer could not save the cancellation yet; it keeps the pairing refused and tries again on its own"
	requirePersonSees(t, a.CloseComputerPairing(window.ID), unsaved)
	// A reopen retires the same invitation and reads the same failure.
	_, err = a.OpenComputerPairing(connCtx(t), "lan", "full")
	requirePersonSees(t, err, unsaved)
}

// A reopen that replaces an open window whose invitation's cancellation
// cannot be saved says what happens next, as a close of it does.
func TestComputerPairingAReopenOverAnUnsavedCancellationSaysWhatHappensNext(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	windowAwaitingApproval(t, a)
	restore := rejectCancellations(t, path)
	defer restore()
	_, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	requirePersonSees(t, err, "this computer could not save the cancellation yet; it keeps the pairing refused and tries again on its own")
}
