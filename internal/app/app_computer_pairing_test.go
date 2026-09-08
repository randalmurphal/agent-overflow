package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/nearby"
	"agent-overflow/internal/network"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/settings"
)

// Only the discovery window is installed directly: the fixture advertises no
// real LAN service and requires no private NIC. Every exchange after opening
// uses the actual TLS listener, HTTP adapter, identity store and paired client.
func computerPairingBackend(t *testing.T) (*pairedBackend, pairbootstrap.Snapshot) {
	t.Helper()
	b := newPairedBackend(t)
	s := &b.app.computerPairing
	s.book = pairbootstrap.NewBook(b.app.cancelComputerPairingLink)
	view := s.book.Open(func() (pairbootstrap.Invitation, error) {
		invite, err := b.app.mintDevicePairing("browser", "full", "")
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	})
	s.windowID = view.WindowID
	book := s.book
	t.Cleanup(func() { book.Close(view.WindowID) })
	return b, view
}

func exchangeComputerPairing(t *testing.T, b *pairedBackend) (pairbootstrap.Invitation, string) {
	t.Helper()
	hc := pairbootstrap.NewHTTPClient(nil)
	defer hc.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	invite, sas, err := pairbootstrap.Exchange(ctx, hc, "https://"+b.srv.Addr(), "Windows desktop", "windows")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := deviceclient.Pair(ctx, t.TempDir(), link, "Windows desktop", "windows")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Retire() })
	return client
}

func TestComputerPairingRealTLSRequiresOwnerSASAndKeepsConfirmedSession(t *testing.T) {
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
				if _, _, err := deviceclient.Pair(ctx, t.TempDir(), link, "late", "linux"); err == nil {
					t.Fatal("canceled encrypted invite redeemed")
				}
			}
		})
	}
}

func TestComputerPairingClosedWindowCannotConfirmWhileCancellationIsPending(t *testing.T) {
	b, window := computerPairingBackend(t)
	invite, _ := exchangeComputerPairing(t, b)
	client := redeemComputerInvitation(t, b, invite)
	// Retire the book while its ordinary store cancellation is delayed. This
	// is the timer/confirmation race: a still-pending five-minute link must
	// not bypass its bootstrap window's shorter lifetime.
	s := &b.app.computerPairing
	s.mu.Lock()
	old := s.book
	s.book = pairbootstrap.NewBook(nil)
	s.mu.Unlock()
	t.Cleanup(func() { old.Close(window.WindowID) })
	// A late unauthenticated packet cannot erase the last invitation mapping
	// and turn the ordinary confirmation route into an expiry bypass.
	_, _ = (authEndpoints{app: b.app}).NearbyPair(context.Background(), b.srv.Addr(), pairbootstrap.Request{Op: "reveal", ID: "late"})
	if err := b.app.ConfirmDevicePairing(invite.LinkID); err == nil {
		t.Fatal("stale invitation bypassed retired bootstrap window")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Ticket(ctx); err == nil {
		t.Fatal("stale bootstrap credential admitted")
	}
}

func TestComputerPairingRequiresExplicitOpenAndLeaksNoOwnerData(t *testing.T) {
	b := newPairedBackend(t)
	hc := pairbootstrap.NewHTTPClient(nil)
	defer hc.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := pairbootstrap.Exchange(ctx, hc, "https://"+b.srv.Addr(), "uninvited", "windows"); err == nil {
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
	b, window := computerPairingBackend(t)
	ordinary, link := b.mintLink(t, "full")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	existing, _, err := deviceclient.Pair(ctx, t.TempDir(), link, "existing", "linux")
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
	s.linkID = ""
	s.windowID = ""
	s.mu.Unlock()
	t.Cleanup(func() { old.Close(window.WindowID) })
	b.app.initIdentity("backend-under-test")
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

func TestComputerPairingReplacementRequiresDurableCancellation(t *testing.T) {
	a, path := newTestAppWithStorePath(t)
	a.initIdentity("backend-under-test")
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
	// A synthetic native ingress backed by a LAN listener keeps this test
	// independent of physical interfaces and suppresses multicast on Open.
	a.nativeNetwork.seen = true
	a.nativeNetwork.addresses = []string{fmt.Sprintf("https://192.168.1.55:%d", portFromAddr(srv.Addr()))}
	s := &a.computerPairing
	s.book = pairbootstrap.NewBook(func(id string) { _ = a.CancelDevicePairing(id) })
	w := s.book.Open(func() (pairbootstrap.Invitation, error) {
		invite, err := a.mintDevicePairing("desktop", "full", "")
		return pairbootstrap.Invitation{LinkID: invite.LinkID, URL: invite.URL}, err
	})
	s.windowID = w.WindowID
	c, err := pairbootstrap.Start("desktop", "windows")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.book.Begin(c.Commitment())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Reveal(ch)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.book.Reveal(ch.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	invite, _, err := c.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	s.linkID = invite.LinkID
	link, err := deviceclient.DecodeLink(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := a.identityState().sessions.RedeemPairing(identity.RedemptionRequest{Token: link.Token, Proof: identity.DeviceProof{Value: "desktop-key"}}); reason.Refused() {
		t.Fatal(reason)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_pairing_cancel BEFORE UPDATE OF canceled_at ON pairing_links BEGIN SELECT RAISE(ABORT, 'test cancellation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.OpenComputerPairing(context.Background(), "lan", "full"); err == nil {
		t.Fatal("replacement discarded unresolved invitation fence")
	}
	if s.book.Snapshot(w.WindowID).Open {
		t.Fatal("failed replacement left old comparison active")
	}
	if err := a.CloseComputerPairing(w.WindowID); err == nil {
		t.Fatal("failed durable cancellation was reported as success")
	}
	if err := a.ConfirmDevicePairing(invite.LinkID); err == nil {
		t.Fatal("failed cancellation made old invitation confirmable")
	}
	if _, err := raw.Exec(`DROP TRIGGER reject_pairing_cancel`); err != nil {
		t.Fatal(err)
	}
	next, err := a.OpenComputerPairing(context.Background(), "lan", "full")
	if err != nil {
		t.Fatal("replacement did not recover after storage recovered", err)
	}
	defer a.CloseComputerPairing(next.ID)
	old, err := a.store.GetPairingLink(invite.LinkID)
	if err != nil || old.CanceledAt == 0 || next.ID == w.WindowID {
		t.Fatal("replacement did not durably retire old invitation", err)
	}
}

// TestComputerPairingCloseRetiresTheLinkOnceAndIsIdempotent: closing the
// window cancels its invitation through the book exactly once and clears
// the fence; closing the same window again is a no-op, never a second
// write or an error.
func TestComputerPairingCloseRetiresTheLinkOnceAndIsIdempotent(t *testing.T) {
	b, window := computerPairingBackend(t)
	invite, _ := exchangeComputerPairing(t, b)
	redeemComputerInvitation(t, b, invite)
	s := &b.app.computerPairing
	s.mu.Lock()
	fenced := s.linkID
	s.mu.Unlock()
	if fenced != invite.LinkID {
		t.Fatalf("minted link %q is not fenced (%q)", invite.LinkID, fenced)
	}
	for i := range 2 {
		if err := b.app.CloseComputerPairing(window.WindowID); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	s.mu.Lock()
	fenced = s.linkID
	s.mu.Unlock()
	if fenced != "" {
		t.Fatalf("durable cancellation left %q fenced", fenced)
	}
	link, err := b.app.store.GetPairingLink(invite.LinkID)
	if err != nil || link.CanceledAt == 0 {
		t.Fatal("close did not retire the invitation", err)
	}
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
	w, err := a.OpenComputerPairing(context.Background(), "lan", "full")
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
