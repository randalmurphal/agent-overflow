package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/testutil"
)

// A peer's introduction call is bound to its caller. One that waits on a
// person's pairing in flight and is cut by its caller, as the other
// computer's 12 s peer deadline cuts it, ends there: once the person's
// pairing is saved nothing redeems for it. Its retry has nothing to wait
// on and connects.
func TestAnIntroductionCutWhileWaitingEndsAndItsRetryConnects(t *testing.T) {
	remotetest.Require(t)
	t.Parallel()
	host := ownConnectionBackend(t)
	other := newIntroducingComputer(t, host, introducingHoldsRedemption)
	person := callOverTheWire(t, host, "AddBackend", other.personLink)
	awaitArrival(t, "the person's redemption reaching the other computer", other.pairArrived, person)

	probed := other.probes.Load()
	introduction, cut := callOverTheWireUntilCut(t, host, "AcceptOwnDeviceIntroduction", other.invitation)
	// The introduction's route check is its last request before it waits.
	for deadline := time.Now().Add(10 * time.Second); other.probes.Load() == probed; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the introduction never checked the other computer's address")
		}
	}
	select {
	case err := <-introduction:
		t.Fatalf("the introduction answered %v while the person's pairing was in flight; want it to wait", err)
	case <-time.After(200 * time.Millisecond):
	}
	cut()
	select {
	case <-introduction:
	case <-time.After(5 * time.Second):
		t.Fatal("the cut introduction call did not return")
	}

	other.release()
	select {
	case err := <-person:
		if err != nil {
			t.Fatalf("person's pairing: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the person's pairing did not finish once answered")
	}
	// A handler still working for the cut call would redeem now, on what
	// the person's pairing left.
	time.Sleep(500 * time.Millisecond)
	if n := other.redemptions.Load(); n != 1 {
		t.Fatalf("%d redemptions after the cut; want only the person's", n)
	}
	if saved, err := deviceclient.LoadSession(host.profile, other.backendID); err != nil || saved.OwnDevice {
		t.Fatalf("stored session after the cut = %+v, %v; want the person's pairing", saved, err)
	}

	select {
	case err := <-callOverTheWire(t, host, "AcceptOwnDeviceIntroduction", other.invitation):
		if err != nil {
			t.Fatalf("the retried introduction: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retried introduction did not return")
	}
	if saved, err := deviceclient.LoadSession(host.profile, other.backendID); err != nil || !saved.OwnDevice {
		t.Fatalf("stored session after the retry = %+v, %v; want the own-device pairing", saved, err)
	}
	if n := other.redemptions.Load(); n != 2 {
		t.Fatalf("%d redemptions; want the person's and the retry's", n)
	}
}

// A peer's introduction call cut by its caller during the redemption ends
// the redemption and stores nothing.
func TestAnIntroductionCutDuringItsRedemptionStoresNothing(t *testing.T) {
	remotetest.Require(t)
	t.Parallel()
	host := ownConnectionBackend(t)
	other := newIntroducingComputer(t, host, introducingHoldsRedemption)
	introduction, cut := callOverTheWireUntilCut(t, host, "AcceptOwnDeviceIntroduction", other.invitation)
	awaitArrival(t, "the redemption reaching the other computer", other.pairArrived, introduction)
	cut()
	select {
	case <-introduction:
	case <-time.After(5 * time.Second):
		t.Fatal("the cut introduction call did not return")
	}
	for deadline := time.Now().Add(5 * time.Second); !other.pairAbandoned.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the redemption was still waiting on the other computer after its caller left")
		}
	}
	if _, err := deviceclient.LoadSession(host.profile, other.backendID); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("stored session after the cut = %v; want none", err)
	}
}

// A cut after the other computer answered the redemption leaves a session
// there that this computer never stored or acknowledged. That computer's
// next introduction of this device revokes it, and the introduction that
// follows connects.
func TestAnIntroductionsOrphanedSessionIsRevokedAtTheNextMint(t *testing.T) {
	remotetest.Require(t)
	ownConnectionNetwork(t)
	a, b := ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, b, a.app.backends, true)
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitBudget(t))
	defer cancel()
	bkey, _, link := mintOwnIntroduction(ctx, t, a, b)

	// The redemption b's cut introduction sent, answered by a, with b's key
	// and nothing stored in b's profile.
	scratch := t.TempDir()
	key, err := os.ReadFile(filepath.Join(b.profile, deviceclient.KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, deviceclient.KeyFileName), key, 0o600); err != nil {
		t.Fatal(err)
	}
	redeemed, err := redeemTestDevice(t, scratch, link, "Own device", "linux", deviceclient.WithDialContext(dialOwnFixture))
	if err != nil {
		t.Fatal(err)
	}
	var orphan *deviceclient.Client
	if err := deviceclient.WithSession(scratch, link.BackendID, func(held *deviceclient.HeldSession) (err error) {
		orphan, _, err = redeemed.Save(held, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := deviceclient.LoadSession(b.profile, a.id); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("b's stored session = %v; want none", err)
	}

	next, err := a.app.MintOwnDeviceIntroduction(context.Background(), bkey)
	if err != nil {
		t.Fatal(err)
	}
	if err := orphan.Renew(ctx); !errors.Is(err, deviceclient.ErrSessionEnded) {
		t.Fatalf("renewing the orphaned session after the next introduction = %v; want %v", err, deviceclient.ErrSessionEnded)
	}
	route := computerroute.Route{Endpoint: link.Endpoint, CertFingerprint: link.CertFingerprint}
	if added, err := b.app.backends.AcceptOwnDeviceIntroduction(ctx, next.URL, []computerroute.Route{route}); err != nil || !added {
		t.Fatalf("the next introduction: added=%v %v", added, err)
	}
	var list owndevices.List
	if err := b.app.backends.CallOwnDevice(ctx, a.id, "ListOwnDevices", &list); err != nil || !list.Enabled {
		t.Fatalf("the next introduction's session: %+v %v", list, err)
	}
}
