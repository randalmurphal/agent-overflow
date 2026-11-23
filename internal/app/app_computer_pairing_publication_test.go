package app

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/nearby"
	"agent-overflow/internal/network"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/settings"
)

// moveNativeAddress has the synthetic launcher of computerPairingHost report
// ip as the LAN address, on the listener's port; the caller publishes it.
func moveNativeAddress(a *App, ip string) {
	s := &a.nativeNetwork
	port := portFromAddr(a.transportServer.Load().Addr())
	s.mu.Lock()
	s.addresses = []string{fmt.Sprintf("https://%s:%d", ip, port)}
	s.mu.Unlock()
}

// holdFirstPublication makes the next publication stop after it has chosen
// the window to retire and released its lock, until the returned gate opens.
// Later publications run straight through.
func holdFirstPublication(t *testing.T, a *App) *testGate {
	t.Helper()
	g := newTestGate()
	t.Cleanup(g.open)
	var held atomic.Bool
	state := &a.computerRoutesPublished
	state.mu.Lock()
	state.chosen = func() {
		if held.CompareAndSwap(false, true) {
			g.wait()
		}
	}
	state.mu.Unlock()
	return g
}

// A publication that runs late retires only the window admitted before it:
// a newer publication and a window opened under it, both between the late
// publication's choice and its close, keep that window open.
func TestComputerPairingALatePublicationKeepsTheNewerWindow(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	first, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	g := holdFirstPublication(t, a)
	moveNativeAddress(a, "192.168.1.56")
	late := make(chan struct{})
	go func() { a.publishComputerRoutes(); close(late) }()
	<-g.reached
	moveNativeAddress(a, "192.168.1.57")
	a.publishComputerRoutes()
	newer, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("https://192.168.1.57:%d", portFromAddr(a.transportServer.Load().Addr())); newer.Address != want {
		t.Fatalf("the newer window shows %s, want %s", newer.Address, want)
	}
	g.open()
	<-late
	if status, err := a.ComputerPairingStatus(newer.ID); err != nil || status.State == "expired" {
		t.Fatalf("the newer window after the late publication = %+v (%v), want it open", status, err)
	}
	if status, err := a.ComputerPairingStatus(first.ID); err != nil || status.State != "expired" {
		t.Fatalf("the first window = %+v (%v), want it retired", status, err)
	}
	// A publication that moves nothing, as a network-status poll makes,
	// keeps the window admitted under the current publication.
	a.publishComputerRoutes()
	if status, err := a.ComputerPairingStatus(newer.ID); err != nil || status.State == "expired" {
		t.Fatalf("the newer window after a publication that moved nothing = %+v (%v), want it open", status, err)
	}
}

// A publication refuses a decision on the window it retires from the moment
// it chooses that window, before the window is closed.
func TestComputerPairingADecisionOnAWindowAPublicationRetiredIsRefused(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	if _, err := a.OpenComputerPairing(connCtx(t), "lan", "full"); err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	redeemOnHost(t, a, invite, "retired-window-device")
	g := holdFirstPublication(t, a)
	moveNativeAddress(a, "192.168.1.58")
	published := make(chan struct{})
	go func() { a.publishComputerRoutes(); close(published) }()
	<-g.reached
	refusedConfirmation(t, a, invite.LinkID, "after the publication chose its window, before the close")
	g.open()
	<-published
	requireCanceled(t, a, invite.LinkID)
}

// advertisingHost is a host whose pairing windows start LAN responders:
// LAN sharing on with no native ingress, on a stubbed interface. start runs
// in place of the responders' interface listing, holding a start in flight.
func advertisingHost(t *testing.T, start func()) *App {
	t.Helper()
	a := identityApp(t)
	stored, err := a.store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	a.storeIdentity.Store(&stored)
	srv := startTestTransportServer(t)
	if err := srv.Rebind(net.JoinHostPort("0.0.0.0", strconv.Itoa(portFromAddr(srv.Addr()))), nil); err != nil {
		t.Fatal(err)
	}
	a.SetTransportServer(srv)
	if _, err := a.settings.SetNetwork(settings.NetworkSettings{BindAll: true}); err != nil {
		t.Fatal(err)
	}
	previousInterfaces, previousAddrs, previousNearby := network.Interfaces, network.InterfaceAddrs, nearby.Interfaces
	t.Cleanup(func() {
		network.Interfaces, network.InterfaceAddrs, nearby.Interfaces = previousInterfaces, previousAddrs, previousNearby
	})
	network.Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 1, Name: "lan", Flags: net.FlagUp | net.FlagRunning}}, nil
	}
	network.InterfaceAddrs = func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.20"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	nearby.Interfaces = func() ([]net.Interface, error) { start(); return nil, nil }
	t.Cleanup(a.stopComputerPairing)
	return a
}

// A route change returns only once the retired window's responder start has
// finished and adopted nothing: no responder it started outlives the change.
func TestComputerPairingARouteChangeWaitsForTheRetiredWindowsResponders(t *testing.T) {
	remotetest.Require(t)
	start := newTestGate()
	t.Cleanup(start.open)
	a := advertisingHost(t, start.wait)
	opened := make(chan error, 1)
	go func() {
		_, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
		opened <- err
	}()
	<-start.reached
	awaiting := make(chan struct{}, 1)
	s := &a.computerPairing
	s.mu.Lock()
	s.awaitingStart = func() {
		select {
		case awaiting <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	moved := make(chan error, 1)
	go func() { moved <- moveListener(t, a) }()
	select {
	case <-awaiting:
	case err := <-moved:
		t.Fatalf("the route change returned while the retired window's responders were starting (%v)", err)
	}
	select {
	case err := <-moved:
		t.Fatalf("the route change returned before the responder start finished (%v)", err)
	default:
	}
	start.open()
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	adv := s.advertiser
	s.mu.Unlock()
	if adv != nil || a.computerPairingOpen() {
		t.Fatal("a responder or the retired window outlived the route change")
	}
}

// A window's decision that commits after the window is gone is still the
// window's answer: its status reads confirmed, not expired.
func TestComputerPairingStatusReadsADecisionAfterItsWindowIsGone(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	redeemOnHost(t, a, invite, "decided-device")
	if err := a.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := a.CloseComputerPairing(window.ID); err != nil {
		t.Fatal(err)
	}
	status, err := a.ComputerPairingStatus(window.ID)
	if err != nil || status.State != "confirmed" || status.LinkID != invite.LinkID {
		t.Fatalf("the closed window's status = %+v (%v), want its confirmation", status, err)
	}
}

// The owner sees a window's own invitation with the digits the requesting
// computer derived from the same exchange, a window's invitation the window
// no longer holds with none, and only an ordinary invitation with the
// ordinary number.
func TestTheOwnerNumberOfAWindowsInvitationIsTheWindows(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	if _, err := a.OpenComputerPairing(connCtx(t), "lan", "full"); err != nil {
		t.Fatal(err)
	}
	book := a.computerPairingBook()
	c, ch := beginOnBook(t, book)
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := book.Reveal(ch.ID, r, "lan")
	if err != nil {
		t.Fatal(err)
	}
	invite, requesterDigits, err := c.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	redeemOnHost(t, a, invite, "numbered-device")
	link, err := a.store.GetPairingLink(invite.LinkID)
	if err != nil || !link.Bootstrap {
		t.Fatalf("the window's invitation = %+v (%v), want it marked as the window's", link, err)
	}
	status, err := a.DevicePairingStatus(invite.LinkID)
	if err != nil || status.VerificationNumber != requesterDigits {
		t.Fatalf("the owner is shown %q (%v), the requesting computer %q", status.VerificationNumber, err, requesterDigits)
	}
	sessions := a.identityState().sessions
	held := standingOf(t, a)
	notHeld := link
	notHeld.ID = "another-window-link"
	if got, err := ownerPairingNumber(notHeld, held, sessions); err != nil || got != "" {
		t.Fatalf("a window's invitation the window does not hold shows %q (%v), want none", got, err)
	}
	ordinary := link
	ordinary.ID, ordinary.Bootstrap = "ordinary-link", false
	want, err := sessions.VerificationNumber(ordinary.ID, ordinary.KeyThumbprint)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ownerPairingNumber(ordinary, held, sessions); err != nil || got != want || got == requesterDigits {
		t.Fatalf("an ordinary invitation shows %q (%v), want the ordinary number %q", got, err, want)
	}
	plain, err := a.mintDevicePairing("desktop", "full", "lan")
	if err != nil {
		t.Fatal(err)
	}
	if minted, err := a.store.GetPairingLink(plain.LinkID); err != nil || minted.Bootstrap {
		t.Fatalf("an ordinary invitation = %+v (%v), want it unmarked", minted, err)
	}
}

// A window's invitation the book refuses, after a close whose cancellation
// failed, cannot be redeemed: the redemption spends nothing and writes no
// device.
func TestComputerPairingARefusedInvitationCannotBeRedeemed(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	countCancellations(a)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	restore := rejectCancellations(t, path)
	if err := a.CloseComputerPairing(window.ID); err == nil {
		t.Fatal("a refused cancellation was reported as success")
	}
	link := mustDecodeInvite(t, invite)
	if _, reason := a.identityState().sessions.RedeemPairing(identity.RedemptionRequest{Token: link.Token, Proof: identity.DeviceProof{Value: "refused-device"}}); reason != identity.ReasonUnknownCredential {
		t.Fatalf("redeeming a refused invitation = %v, want unknown_credential", reason)
	}
	if row, err := a.store.GetPairingLink(invite.LinkID); err != nil || row.Redeemed() {
		t.Fatalf("the refused invitation = %+v (%v), want it unspent", row, err)
	}
	if _, err := a.store.DeviceByKeyThumbprint("refused-device"); err == nil {
		t.Fatal("a refused redemption wrote a device")
	}
	restore()
	if err := a.CloseComputerPairing(window.ID); err != nil {
		t.Fatal(err)
	}
	requireCanceled(t, a, invite.LinkID)
}

// The owner's Cancel on a window's invitation retires that window too: the
// invitation is canceled with its pending session and the window is gone
// from the owner and from discovery.
func TestComputerPairingCancelingAWindowsInvitationRetiresTheWindow(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	window, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	invite := revealOnBook(t, a.computerPairingBook())
	redeemOnHost(t, a, invite, "canceled-device")
	if err := a.CancelDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	requireCanceled(t, a, invite.LinkID)
	if status, err := a.ComputerPairingStatus(window.ID); err != nil || status.State != "expired" {
		t.Fatalf("the window after its invitation was canceled = %+v (%v), want it gone", status, err)
	}
	requireNoPublishedWindow(t, a)
	if err := a.CancelDevicePairing(invite.LinkID); !errors.Is(err, identity.ErrPairingRefused) {
		t.Fatalf("canceling a settled invitation again = %v, want identity.ErrPairingRefused", err)
	}
}

// standingOf is the computer pairing book's standing now.
func standingOf(t *testing.T, a *App) (standing pairbootstrap.Standing) {
	t.Helper()
	if err := a.computerPairingBook().Read(func(st pairbootstrap.Standing) error { standing = st; return nil }); err != nil {
		t.Fatal(err)
	}
	return standing
}

func mustDecodeInvite(t *testing.T, invite pairbootstrap.Invitation) deviceclient.Link {
	t.Helper()
	link, err := deviceclient.DecodeLink(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	return link
}

// An own-device open that fails before its window can open changes no
// membership: a recognized network with no address and a previous window
// whose cancellation the store refuses each leave this computer unenrolled,
// and a removed member's generation and removal as they were.
func TestAFailedOwnPairingOpenChangesNoMembership(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	countCancellations(a)
	manager, err := newTestBackends(t, t.TempDir(), "Host", "test")
	if err != nil {
		t.Fatal(err)
	}
	SetAttachedBackends(a, manager)
	enrolled := func() bool {
		t.Helper()
		_, err := a.backends.OwnIdentity()
		if err != nil && !errors.Is(err, deviceclient.ErrNoDeviceKey) {
			t.Fatal(err)
		}
		return err == nil
	}
	refusedPrevious := func() (restore func()) {
		t.Helper()
		previous, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
		if err != nil {
			t.Fatal(err)
		}
		revealOnBook(t, a.computerPairingBook())
		restore = rejectCancellations(t, path)
		if err := a.CloseComputerPairing(previous.ID); err == nil {
			t.Fatal("a refused cancellation was reported as success")
		}
		return restore
	}
	failures := func(check func(when string)) {
		t.Helper()
		if _, err := a.OpenOwnComputerPairing(connCtx(t), "tailnet"); err == nil {
			t.Fatal("an own pairing open on a network with no address succeeded")
		}
		check("after an open on a network with no address")
		restore := refusedPrevious()
		if _, err := a.OpenOwnComputerPairing(connCtx(t), "lan"); err == nil {
			t.Fatal("an own pairing open succeeded while the previous window's cancellation failed")
		}
		check("after an open refused for the previous window's cancellation")
		restore()
		if _, err := a.OpenComputerPairing(connCtx(t), "lan", "full"); err != nil {
			t.Fatal(err)
		}
	}
	failures(func(when string) {
		if enrolled() {
			t.Fatalf("%s: this computer is enrolled", when)
		}
	})
	key, err := manager.EnrollOwnIdentity()
	if err != nil {
		t.Fatal(err)
	}
	execOnStore(t, path, `INSERT INTO own_devices (key_thumbprint, member, generation, removed) VALUES (?, ?, 3, 1)`,
		key, fmt.Sprintf(`{"keyThumbprint":%q,"name":"Host","routes":[],"generation":3,"removed":true}`, key))
	failures(func(when string) {
		member, err := a.store.OwnDevice(key)
		if err != nil || member.Generation != 3 || !member.Removed {
			t.Fatalf("%s: the removed member = %+v (%v), want generation 3 and removed", when, member, err)
		}
	})
}
