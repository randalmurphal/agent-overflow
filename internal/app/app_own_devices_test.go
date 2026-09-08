package app

import (
	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"context"
	"testing"
)

func TestLegacyFullSessionCannotJoinOrIntroduceOwnDevices(t *testing.T) {
	b := newPairedBackend(t)
	manager, e := attachedbackends.New(t.TempDir(), "Host", "test")
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
	b := newPairedBackend(t)
	manager, e := attachedbackends.New(t.TempDir(), "Host", "test")
	if e != nil {
		t.Fatal(e)
	}
	SetAttachedBackends(b.app, manager)
	if e = b.app.enableOwnDevices(); e != nil {
		t.Fatal(e)
	}
	invite, e := b.app.mintDevicePairingPurpose("phone", "full", "", "own-device")
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
	client, _, e := deviceclient.Pair(context.Background(), t.TempDir(), payload, "Pixel", "android")
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
	var self OwnDeviceMember
	for _, m := range list.Members {
		if m.KeyThumbprint == list.SelfKeyThumbprint {
			self = m
		}
	}
	if _, e = b.app.RegisterOwnDevice(ctx, self); e == nil {
		t.Fatal("phone overwrote host identity")
	}
	if e = b.app.RemoveOwnDevice(ctx, clientKey(t, b.app, client.Session().SessionID)); e != nil {
		t.Fatal(e)
	}
	if _, e = b.app.ListOwnDevices(ctx); e == nil {
		t.Fatal("revoked device read membership")
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
