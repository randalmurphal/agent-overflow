package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/nearby"
	"agent-overflow/internal/network"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
)

// Only the discovery window is installed directly: the fixture advertises no
// real LAN service and requires no private NIC. Every exchange after opening
// uses the actual TLS listener, HTTP adapter, identity store and paired client.
func computerPairingBackend(t *testing.T) (*pairedBackend, pairbootstrap.Snapshot) {
	t.Helper()
	b, view, _ := computerPairingBackendAt(t)
	return b, view
}

// computerPairingBackendAt is computerPairingBackend plus the database
// path, for a test that makes the store refuse writes.
func computerPairingBackendAt(t *testing.T) (*pairedBackend, pairbootstrap.Snapshot, string) {
	t.Helper()
	b, path := servedIdentityApp(t)
	b.app.computerPairing.book = pairbootstrap.NewBook(b.app.cancelComputerPairingLink, time.Hour)
	return b, openTestWindow(t, b.app), path
}

// servedIdentityApp is newPairedBackend plus the database path.
func servedIdentityApp(t *testing.T) (*pairedBackend, string) {
	t.Helper()
	a, path := newTestAppWithStorePath(t)
	if err := a.initIdentity("backend-under-test"); err != nil {
		t.Fatal(err)
	}
	return servePairedApp(t, a), path
}

// openTestWindow opens a window on the host's installed book, minting
// browser invitations, and closes it when the test ends.
func openTestWindow(t *testing.T, a *App) pairbootstrap.Snapshot {
	t.Helper()
	s := &a.computerPairing
	book := s.book
	view, err := book.Open(func(route string) (pairbootstrap.Invitation, error) {
		invite, err := a.mintDevicePairing("browser", "full", "")
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.windowID = view.WindowID
	s.mu.Unlock()
	t.Cleanup(func() { _ = book.Close(view.WindowID) })
	return view
}

const rejectPairingCancel = `CREATE TRIGGER reject_pairing_cancel BEFORE UPDATE OF canceled_at ON pairing_links BEGIN SELECT RAISE(ABORT, 'test cancellation failure'); END`

// rejectCancellations makes the store refuse every pairing cancellation
// while confirmations still write, until the returned func restores it.
func rejectCancellations(t *testing.T, path string) (restore func()) {
	t.Helper()
	execOnStore(t, path, rejectPairingCancel)
	return func() { execOnStore(t, path, `DROP TRIGGER reject_pairing_cancel`) }
}

// cancellationLedger counts the cancellation statements the app's book
// issues per invitation. hold, when set before the book is used, runs
// before each statement.
type cancellationLedger struct {
	mu    sync.Mutex
	calls map[string]int
	hold  func(linkID string)
}

// countCancellations installs the host's book with its production cancel
// callback, counted. The book's own retries wait an hour, so every
// statement a test counts is one it caused.
func countCancellations(a *App) *cancellationLedger {
	l := &cancellationLedger{calls: map[string]int{}}
	s := &a.computerPairing
	s.mu.Lock()
	s.book = pairbootstrap.NewBook(func(id string) error {
		l.mu.Lock()
		l.calls[id]++
		hold := l.hold
		l.mu.Unlock()
		if hold != nil {
			hold(id)
		}
		return a.cancelComputerPairingLink(id)
	}, time.Hour)
	s.mu.Unlock()
	return l
}

func (l *cancellationLedger) count(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[id]
}

// testGate holds whatever waits on it until released. The test's cleanup
// releases it, so a failure cannot leave a goroutine blocked until the
// package timeout. Cleanups run last-registered first, so a gate must be
// made after anything whose cleanup waits on what the gate holds.
type testGate struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newTestGate(t *testing.T) *testGate {
	g := &testGate{reached: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *testGate) wait() {
	select {
	case g.reached <- struct{}{}:
	default:
	}
	<-g.release
}

func (g *testGate) open() { g.once.Do(func() { close(g.release) }) }

// revealOnBook runs one requester's exchange against the book directly,
// with nothing but the book seeing the invitation it reveals.
func revealOnBook(t *testing.T, book *pairbootstrap.Book) pairbootstrap.Invitation {
	t.Helper()
	c, ch := beginOnBook(t, book)
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
	return invite
}

func beginOnBook(t *testing.T, book *pairbootstrap.Book) (*pairbootstrap.Client, pairbootstrap.Challenge) {
	t.Helper()
	c, err := pairbootstrap.Start("desktop", "windows")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := book.Begin(c.Commitment())
	if err != nil {
		t.Fatal(err)
	}
	return c, ch
}

// redeemOnHost redeems an invitation with the device key named by proof,
// leaving a pending session that only a confirmation activates.
func redeemOnHost(t *testing.T, a *App, invite pairbootstrap.Invitation, proof string) {
	t.Helper()
	link, err := deviceclient.DecodeLink(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := a.identityState().sessions.RedeemPairing(identity.RedemptionRequest{Token: link.Token, Proof: identity.DeviceProof{Value: proof}}); reason.Refused() {
		t.Fatal(reason)
	}
}

// requireCanceled checks that an invitation's cancellation is durable: its
// row is canceled and so is the pending session its redemption created.
func requireCanceled(t *testing.T, a *App, linkID string) {
	t.Helper()
	link, err := a.store.GetPairingLink(linkID)
	if err != nil || link.CanceledAt == 0 {
		t.Fatalf("invitation %s is not durably cancelled (%+v, %v)", linkID, link, err)
	}
	if link.SessionID == "" {
		return
	}
	session, err := a.store.GetSession(link.SessionID)
	if err != nil || session.RevokedAt == 0 {
		t.Fatalf("invitation %s is canceled but its pending session is not revoked (%+v, %v)", linkID, session, err)
	}
}

func exchangeComputerPairing(t *testing.T, b *pairedBackend) (pairbootstrap.Invitation, string) {
	t.Helper()
	invite, sas, err := nearbyTestExchange(t, "https://"+b.srv.Addr(), "Windows desktop", "windows")
	if err != nil {
		t.Fatal(err)
	}
	return invite, sas
}

func redeemComputerInvitation(t *testing.T, b *pairedBackend, invite pairbootstrap.Invitation) *deviceclient.Client {
	t.Helper()
	link, err := deviceclient.DecodeLink(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if link.CertFingerprint != b.fingerprint {
		t.Fatal("bootstrap did not carry the actual host certificate")
	}
	client, _, err := pairTestDevice(t, t.TempDir(), link, "Windows desktop", "windows")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Retire() })
	return client
}

func TestComputerPairingRealTLSRequiresOwnerSASAndKeepsConfirmedSession(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	b, window := computerPairingBackend(t)
	invite, sas := exchangeComputerPairing(t, b)
	view, err := b.app.ComputerPairingStatus(window.WindowID)
	if err != nil || view.State != "verifying" || view.VerificationNumber != sas || view.LinkID != invite.LinkID {
		t.Fatalf("before redemption: %+v %v", view, err)
	}
	client := redeemComputerInvitation(t, b, invite)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Ticket(ctx); err == nil {
		t.Fatal("unapproved bootstrap admitted a device")
	}
	view, err = b.app.ComputerPairingStatus(window.WindowID)
	if err != nil || view.State != "ready" || view.VerificationNumber != sas {
		t.Fatalf("after redemption: %+v %v", view, err)
	}
	ordinary, err := b.app.DevicePairingStatus(invite.LinkID)
	if err != nil || ordinary.VerificationNumber != sas {
		t.Fatal("alternate owner surface showed a second comparison", err)
	}
	if err := b.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.app.CloseComputerPairing(window.WindowID); err != nil {
		t.Fatal(err)
	}
	// Closing the setup window must not revoke the now-confirmed device.
	conn := dialPaired(t, ctx, client)
	defer conn.CloseNow()
	projects := callOverWS(t, ctx, conn, "ListProjects")
	var rows []json.RawMessage
	if json.Unmarshal(projects, &rows) != nil || len(rows) == 0 {
		t.Fatal("paired socket could not read projects")
	}
}

func TestComputerPairingCancelAndStaleConfirmationAdmitNothing(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for _, redeem := range []bool{false, true} {
		name := "before redemption"
		if redeem {
			name = "after redemption"
		}
		t.Run(name, func(t *testing.T) {
			b, window := computerPairingBackend(t)
			invite, _ := exchangeComputerPairing(t, b)
			var client *deviceclient.Client
			if redeem {
				client = redeemComputerInvitation(t, b, invite)
			}
			if err := b.app.CloseComputerPairing(window.WindowID); err != nil {
				t.Fatal(err)
			}
			if err := b.app.ConfirmDevicePairing(invite.LinkID); err == nil {
				t.Fatal("closed window confirmed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if redeem {
				if _, err := client.Ticket(ctx); err == nil {
					t.Fatal("canceled pending session admitted")
				}
			} else {
				link, _ := deviceclient.DecodeLink(invite.URL)
				if _, _, err := pairTestDevice(t, t.TempDir(), link, "late", "linux"); err == nil {
					t.Fatal("canceled encrypted invite redeemed")
				}
			}
		})
	}
}

func TestComputerPairingClosedWindowCannotConfirmWhileCancellationIsPending(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	b, window, path := computerPairingBackendAt(t)
	invite, _ := exchangeComputerPairing(t, b)
	client := redeemComputerInvitation(t, b, invite)
	// The store refuses the cancellation: a still-pending five-minute link
	// must not outlive its bootstrap window's shorter lifetime.
	restore := rejectCancellations(t, path)
	if err := b.app.CloseComputerPairing(window.WindowID); err == nil {
		t.Fatal("a refused cancellation was reported as success")
	}
	// A late unauthenticated packet cannot erase the retirement and turn the
	// ordinary confirmation route into an expiry bypass.
	_, _ = (authEndpoints{app: b.app}).NearbyPair(context.Background(), b.srv.Addr(), pairbootstrap.Request{Op: "reveal", ID: "late"})
	if err := b.app.ConfirmDevicePairing(invite.LinkID); !errors.Is(err, identity.ErrPairingRefused) {
		t.Fatalf("stale invitation confirmation = %v, want refused", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Ticket(ctx); err == nil {
		t.Fatal("stale bootstrap credential admitted")
	}
	restore()
	if err := b.app.CloseComputerPairing(window.WindowID); err != nil {
		t.Fatalf("closing again after storage recovered = %v", err)
	}
	requireCanceled(t, b.app, invite.LinkID)
	if err := b.app.ConfirmDevicePairing(invite.LinkID); err == nil {
		t.Fatal("a durably cancelled invitation was confirmed")
	}
}

func TestComputerPairingRequiresExplicitOpenAndLeaksNoOwnerData(t *testing.T) {
	t.Parallel()
	b := newPairedBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := nearbyTestExchange(t, "https://"+b.srv.Addr(), "uninvited", "windows"); err == nil {
		t.Fatal("closed host accepted nearby pairing")
	}
	info, err := (authEndpoints{app: b.app}).NearbyPair(ctx, b.srv.Addr(), pairbootstrap.Request{Op: "info"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(info)
	for _, secret := range []string{"verificationNumber", "linkId", "credential", "commitment"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("info leaked owner-only field", secret)
		}
	}
}

func TestComputerPairingRestartRetiresLostComparisonButKeepsExistingDevice(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	b, window := computerPairingBackend(t)
	ordinary, link := b.mintLink(t, "full")
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitBudget(t))
	defer cancel()
	existing, _, err := pairTestDevice(t, t.TempDir(), link, "existing", "linux")
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Retire()
	if err := b.app.ConfirmDevicePairing(ordinary.LinkID); err != nil {
		t.Fatal(err)
	}
	invite, _ := exchangeComputerPairing(t, b)
	pending := redeemComputerInvitation(t, b, invite)
	// Simulate process loss: no graceful book.Close/cancel before boot.
	// The immutable transport fixture remains so both saved clients can
	// prove the new identity core's actual credential decisions over TLS.
	s := &b.app.computerPairing
	s.mu.Lock()
	old := s.book
	s.book = nil
	s.windowID = ""
	s.mu.Unlock()
	t.Cleanup(func() { _ = old.Close(window.WindowID) })
	if err := b.app.initIdentity("backend-under-test"); err != nil {
		t.Fatal(err)
	}
	if err := b.app.ConfirmDevicePairing(invite.LinkID); err == nil {
		t.Fatal("lost bootstrap comparison remained confirmable")
	}
	if _, err := pending.Ticket(ctx); err == nil {
		t.Fatal("unfinished pairing admitted after reboot")
	}
	if _, err := existing.Ticket(ctx); err != nil {
		t.Fatal("confirmed pairing did not survive reboot", err)
	}
}

// computerPairingHost is a host OpenComputerPairing opens windows on: an
// identity, a listener bound to every interface, LAN sharing on, and a
// synthetic native ingress backed by that listener, which keeps the test
// independent of physical interfaces and suppresses multicast on Open. It
// answers the store's path too.
func computerPairingHost(t *testing.T) (*App, string) {
	t.Helper()
	a, path := newTestAppWithStorePath(t)
	if err := a.initIdentity("backend-under-test"); err != nil {
		t.Fatal(err)
	}
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
	a.nativeNetwork.seen = true
	a.nativeNetwork.addresses = []string{fmt.Sprintf("https://192.168.1.55:%d", portFromAddr(srv.Addr()))}
	t.Cleanup(a.closeComputerPairing)
	return a, path
}

// computerPairingConnectionTie is an OpenComputerPairing method's row of
// connectionTieChecks: the window, which any caller closes by its id; the
// connection binds one release for its life. setup, when set, prepares
// what the method needs beyond the host.
func computerPairingConnectionTie(t *testing.T, setup func(*testing.T, *App), open func(*App, context.Context) (ComputerPairingWindow, error)) connectionTie {
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
	if setup != nil {
		setup(t, a)
	}
	return connectionTie{
		open: func(ctx context.Context) (string, error) {
			window, err := open(a, ctx)
			return window.ID, err
		},
		held: func() int {
			if a.computerPairingOpen() {
				return 1
			}
			return 0
		},
		release:   func(_ context.Context, id string) error { return a.CloseComputerPairing(id) },
		anyCaller: true,
	}
}

func TestComputerPairingReplacementRequiresDurableCancellation(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, path := computerPairingHost(t)
	countCancellations(a)
	w, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	book := a.computerPairingBook()
	invite := revealOnBook(t, book)
	redeemOnHost(t, a, invite, "desktop-key")
	restore := rejectCancellations(t, path)
	if _, err := a.OpenComputerPairing(connCtx(t), "lan", "full"); err == nil {
		t.Fatal("replacement discarded unresolved invitation fence")
	}
	if book.Snapshot("").Open {
		t.Fatal("failed replacement left a comparison active")
	}
	if err := a.CloseComputerPairing(w.ID); err == nil {
		t.Fatal("failed durable cancellation was reported as success")
	}
	if err := a.ConfirmDevicePairing(invite.LinkID); err == nil {
		t.Fatal("failed cancellation made old invitation confirmable")
	}
	restore()
	next, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatal("replacement did not recover after storage recovered", err)
	}
	defer a.CloseComputerPairing(next.ID)
	requireCanceled(t, a, invite.LinkID)
	if next.ID == w.ID {
		t.Fatal("the replacement reused the closed window")
	}
}

// TestComputerPairingCloseRetiresTheLinkOnceAndIsIdempotent: closing the
// window cancels its invitation with exactly one statement; closing the
// same window again is a no-op, never a second write or an error.
func TestComputerPairingCloseRetiresTheLinkOnceAndIsIdempotent(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	b, _ := servedIdentityApp(t)
	ledger := countCancellations(b.app)
	window := openTestWindow(t, b.app)
	invite, _ := exchangeComputerPairing(t, b)
	redeemComputerInvitation(t, b, invite)
	for i := range 2 {
		if err := b.app.CloseComputerPairing(window.WindowID); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
		if n := ledger.count(invite.LinkID); n != 1 {
			t.Fatalf("after close %d the invitation had %d cancellation statements, want 1", i, n)
		}
	}
	requireCanceled(t, b.app, invite.LinkID)
	entries, err := b.app.store.ListRecentAuthAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	var canceled int
	for _, entry := range entries {
		if entry.Event == string(identity.AuditPairingCanceled) && entry.Detail == invite.LinkID {
			canceled++
		}
	}
	if canceled != 1 {
		t.Fatalf("the invitation was canceled %d times, want once", canceled)
	}
	if err := b.app.ConfirmDevicePairing(invite.LinkID); err == nil {
		t.Fatal("closed window's invitation remained confirmable")
	}
}

// TestComputerPairingReportsDiscoveryFailureOnTheWindow: a host sharing on
// the LAN whose multicast responders cannot start still opens the window,
// since typed-address pairing works without them, and the window says why
// nobody will find it instead of leaving the owner waiting.
func TestComputerPairingReportsDiscoveryFailureOnTheWindow(t *testing.T) {
	remotetest.Require(t)
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
	// No multicast-capable interface: the responders cannot start.
	nearby.Interfaces = func() ([]net.Interface, error) { return nil, nil }
	w, err := a.OpenComputerPairing(connCtx(t), "lan", "full")
	if err != nil {
		t.Fatalf("a discovery failure closed the typed-address path: %v", err)
	}
	defer a.CloseComputerPairing(w.ID)
	if w.Address == "" {
		t.Fatal("window carries no address to type")
	}
	status, err := a.ComputerPairingStatus(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "waiting" || !strings.Contains(status.DiscoveryError, "nearby discovery unavailable") {
		t.Fatalf("status = %+v, want an open window carrying the discovery failure", status)
	}
	s := &a.computerPairing
	s.mu.Lock()
	adv := s.advertiser
	s.mu.Unlock()
	if adv != nil {
		t.Fatal("a failed start left an advertiser attached")
	}
	if err := a.CloseComputerPairing(w.ID); err != nil {
		t.Fatal(err)
	}
	if status, _ := a.ComputerPairingStatus(w.ID); status.State != "expired" || status.DiscoveryError != "" {
		t.Fatalf("closed window reports %+v", status)
	}
}

// An own-device pairing window persists this computer's enrollment only
// for a caller that is admitted with valid arguments: an open from a
// connection that is already closing, or naming no network the window
// can use, enrolls nothing, and an admitted open does.
func TestAnOwnPairingOpenEnrollsOnlyOnceAdmitted(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := computerPairingHost(t)
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
	if enrolled() {
		t.Fatal("the fixture's computer is enrolled before any open")
	}
	closing, state := newTestConnection(t, transport.ConnPrincipal{})
	state.RunCleanups()
	if _, err := a.OpenOwnComputerPairing(closing, "lan"); !errors.Is(err, transport.ErrConnectionClosing) {
		t.Fatalf("an own pairing open from a closing connection = %v, want transport.ErrConnectionClosing", err)
	}
	if enrolled() {
		t.Fatal("an open refused for its closing connection enrolled this computer")
	}
	if _, err := a.OpenOwnComputerPairing(connCtx(t), "bluetooth"); err == nil {
		t.Fatal("an own pairing open on no usable network succeeded")
	}
	if enrolled() {
		t.Fatal("an open refused for its network enrolled this computer")
	}
	if _, err := a.OpenOwnComputerPairing(connCtx(t), "lan"); err != nil {
		t.Fatal(err)
	}
	if !enrolled() {
		t.Fatal("an admitted own pairing open did not enroll this computer")
	}
}
