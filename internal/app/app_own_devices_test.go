package app

import (
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/identity"
	"context"
	"database/sql"
	"testing"
)

// An introduction whose target cannot be read from storage is this
// computer's to retry; a target that is not a member is refused.
func TestAnIntroductionWhoseTargetCannotBeReadSaysToTryAgain(t *testing.T) {
	t.Parallel()
	a, path := newTestAppWithStorePath(t)
	raw, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, e := raw.Exec(`INSERT INTO own_devices (key_thumbprint, member, generation, removed) VALUES ('unreadable', '[]', 1, 0)`); e != nil {
		t.Fatal(e)
	}
	_, e = a.mintOwnIntroduction(OwnDeviceMember{}, "unreadable")
	if code, _, ok := errorsx.PublicDetails(e); !ok || code != identity.ReasonTemporarilyUnavailable.Code() {
		t.Fatalf("an introduction whose target could not be read = %v, want %s", e, identity.ReasonTemporarilyUnavailable)
	}
	if _, e = a.mintOwnIntroduction(OwnDeviceMember{}, "absent"); e == nil || e.Error() != "target is not an active own device" {
		t.Fatalf("an introduction of a device that is not a member = %v, want it refused", e)
	}
}

func TestLegacyFullSessionCannotJoinOrIntroduceOwnDevices(t *testing.T) {
	t.Parallel()
	b := newPairedBackend(t)
	manager, e := newTestBackends(t, t.TempDir(), "Host", "test")
	if e != nil {
		t.Fatal(e)
	}
	SetAttachedBackends(b.app, manager)
	grants, e := identity.PairingAccessFull.Grants()
	if e != nil {
		t.Fatal(e)
	}
	session := pairSessionWithScopes(t, b.app, "legacy-full", grants)
	ctx := callFrom(session.ID, true)
	list, e := b.app.ListOwnDevices(ctx)
	if e != nil || list.Enabled || len(list.Members) > 0 || list.CanEnroll {
		t.Fatal(list, e)
	}
	// Another's paired browser is not the person's screen; the host's
	// window is, on a computer that never turned own devices on.
	if own, e := b.app.ownScreen(ctx); own || e != nil {
		t.Fatalf("a paired browser that is no own device is the person's screen: %v, %v", own, e)
	}
	if own, e := b.app.ownScreen(context.Background()); !own || e != nil {
		t.Fatalf("the host's window is not the person's screen: %v, %v", own, e)
	}
	// The host's own window may enroll before any membership exists; that
	// verdict is what lets the modal offer "My device" there and withhold
	// it from the legacy session above (settings/AGENTS.md).
	if local, e := b.app.ListOwnDevices(context.Background()); e != nil || !local.CanEnroll {
		t.Fatal("local window refused enrollment", local, e)
	}
	if _, e = b.app.MintOwnDevicePairingOnNetwork(ctx, "phone", "lan"); e == nil {
		t.Fatal("legacy admin could create group")
	}
	if _, e = b.app.OpenOwnComputerPairing(ctx, "lan"); e == nil {
		t.Fatal("legacy admin could open personal pairing")
	}
	if _, e = b.app.SyncOwnDevices(ctx, nil); e == nil {
		t.Fatal("legacy session could modify group")
	}
	if _, e = b.app.IntroduceOwnDevice(ctx, "other"); e == nil {
		t.Fatal("legacy session could introduce member")
	}
	if _, e = manager.OwnIdentity(); e != deviceclient.ErrNoDeviceKey {
		t.Fatal("reading or refused request created device key", e)
	}
}
func TestPersonalPhoneEnrollmentReadsOwnCatalogOverRealTLS(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	b := newPairedBackend(t)
	manager, e := newTestBackends(t, t.TempDir(), "Host", "test")
	if e != nil {
		t.Fatal(e)
	}
	SetAttachedBackends(b.app, manager)
	if e = b.app.enableOwnDevices(); e != nil {
		t.Fatal(e)
	}
	invite, e := b.app.mintDevicePairingPurpose("phone", "full", "", "own-device", false)
	if e != nil {
		t.Fatal(e)
	}
	payload, e := deviceclient.DecodeLink(invite.URL)
	if e != nil {
		t.Fatal(e)
	}
	if payload.Purpose != "own-device" {
		t.Fatal("invitation omitted intent")
	}
	client, _, e := pairTestDevice(t, t.TempDir(), payload, "Pixel", "android")
	if e != nil {
		t.Fatal(e)
	}
	defer client.Retire()
	if e = b.app.ConfirmDevicePairing(invite.LinkID); e != nil {
		t.Fatal(e)
	}
	if e = client.AwaitActivation(context.Background()); e != nil {
		t.Fatal(e)
	}
	ctx := callFrom(client.Session().SessionID, false)
	list, e := b.app.ListOwnDevices(ctx)
	if e != nil || !list.Enabled || len(list.Members) != 2 {
		t.Fatal(list, e)
	}
	if own, e := b.app.ownScreen(ctx); !own || e != nil {
		t.Fatalf("an own device is not the person's screen: %v, %v", own, e)
	}
	var self OwnDeviceMember
	for _, m := range list.Members {
		if m.KeyThumbprint == list.SelfKeyThumbprint {
			self = m
		}
	}
	if _, e = b.app.RegisterOwnDevice(ctx, self); e == nil {
		t.Fatal("phone overwrote host identity")
	}
	revoked, e := b.app.store.OwnDevice(clientKey(t, b.app, client.Session().SessionID))
	if e != nil {
		t.Fatal(e)
	}
	revoked.Removed = true
	if _, e = b.app.SyncOwnDevices(ctx, []OwnDeviceMember{revoked}); e != nil {
		t.Fatal(e)
	}
	if _, e = b.app.ListOwnDevices(ctx); e == nil {
		t.Fatal("revoked device read membership")
	}
	// Its removal ends its session, which is refused before membership.
	if own, e := b.app.ownScreen(ctx); own {
		t.Fatalf("a removed own device is the person's screen: %v, %v", own, e)
	}
}
func clientKey(t *testing.T, a *App, sessionID string) string {
	t.Helper()
	m, e := a.store.OwnDeviceForSession(sessionID)
	if e != nil {
		t.Fatal(e)
	}
	return m.KeyThumbprint
}
