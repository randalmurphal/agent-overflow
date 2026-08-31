package app

import (
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
)

// Forgetting is the step AFTER revoking (docs/specs/remote-access.md §2):
// the revoke ends access, and this removes the row it emptied. The two are
// ordered rather than alternatives, which is what these pin.

// TestForgetAccessDevice_RemovesTheRowAndItsSessions — the cascade, stated
// as the surface's own answer: the device is gone from the overview, its
// sessions went with it, and the credential log that explains how it got
// there did not.
func TestForgetAccessDevice_RemovesTheRowAndItsSessions(t *testing.T) {
	app := accessApp(t)
	device, session := pairDevice(t, app, "A revoked browser", "thumb-forget")
	if _, err := app.RevokeAccessDevice(device.ID); err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}
	if err := app.ForgetAccessDevice(device.ID); err != nil {
		t.Fatalf("ForgetAccessDevice: %v", err)
	}

	overview, err := app.GetAccessOverview()
	if err != nil {
		t.Fatalf("GetAccessOverview: %v", err)
	}
	for _, row := range overview.Devices {
		if row.ID == device.ID {
			t.Fatal("a forgotten device is still in the overview")
		}
	}
	if _, err := app.store.GetSession(session.ID); err == nil {
		t.Fatal("the forgotten device's session row survived")
	}

	// The log outlives the row it names, which is the whole reason to
	// keep attribution out of the cascade.
	var sawRevoke, sawForget bool
	for _, entry := range overview.Audit {
		if entry.DeviceID != device.ID {
			continue
		}
		switch entry.Event {
		case string(identity.AuditDeviceRevoked):
			sawRevoke = true
		case string(identity.AuditDeviceForgotten):
			sawForget = true
		}
	}
	if !sawRevoke || !sawForget {
		t.Fatalf("audit lost the forgotten device's history: revoke=%t forget=%t", sawRevoke, sawForget)
	}
}

// TestForgetAccessDevice_RefusesADeviceThatStillHasAccess — revoking is
// what ENDS access, and it is the call that closes sockets and drops the
// device's UI state. Deleting the row first would take away the only
// handle on a device that still holds credentials.
func TestForgetAccessDevice_RefusesADeviceThatStillHasAccess(t *testing.T) {
	app := accessApp(t)
	device, session := pairDevice(t, app, "A live browser", "thumb-forget-live")

	err := app.ForgetAccessDevice(device.ID)
	if err == nil {
		t.Fatal("ForgetAccessDevice removed a device that still has access")
	}
	// The refusal names the remedy, because the person is one click away
	// from it.
	if !strings.Contains(err.Error(), "revoke") {
		t.Fatalf("the refusal does not name revoking: %v", err)
	}
	if !SessionLive(app, session.ID) {
		t.Fatal("the refused forget ended the device's session anyway")
	}
}

// TestForgetAccessDevice_RefusesTheLocalPageChannel — the row the embedded
// webview, the WSL relay and `--connect` all present. It is never revoked,
// so the ordering refusal would name a step this surface will not allow.
func TestForgetAccessDevice_RefusesTheLocalPageChannel(t *testing.T) {
	app := accessApp(t)
	session := localChannelSession(t, app)

	err := app.ForgetAccessDevice(session.DeviceID)
	if err == nil {
		t.Fatal("ForgetAccessDevice removed this backend's own page channel")
	}
	if strings.Contains(err.Error(), "revoke") {
		t.Fatalf("the refusal points at a revoke that is itself refused: %v", err)
	}
}

// TestForgetAccessDevice_FreesTheKeyToEnrollAgain — the intended
// difference from RestoreAccessDevice. Restoring says "that is still my
// device"; forgetting says it is nothing to me, and the same key may pair
// again — through an owner-minted link and the verification number, so
// nothing re-enrolls unwatched.
func TestForgetAccessDevice_FreesTheKeyToEnrollAgain(t *testing.T) {
	app := accessApp(t)
	const thumbprint = "thumb-forget-reenroll"
	device, _ := pairDevice(t, app, "A browser", thumbprint)
	if _, err := app.RevokeAccessDevice(device.ID); err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}

	// While the row is only revoked, the key is refused.
	state := app.identityState()
	_, token := mintLink(t, app, identity.DeviceBrowser)
	if _, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token: token, Proof: identity.DeviceProof{Value: thumbprint},
	}); reason != identity.ReasonRevokedDevice {
		t.Fatalf("re-pairing a revoked key = %s, want revoked_device", reason)
	}

	if err := app.ForgetAccessDevice(device.ID); err != nil {
		t.Fatalf("ForgetAccessDevice: %v", err)
	}
	_, fresh := mintLink(t, app, identity.DeviceBrowser)
	redemption, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token: fresh, Proof: identity.DeviceProof{Value: thumbprint}, Label: "The same browser",
	})
	if reason.Refused() {
		t.Fatalf("re-pairing a forgotten key = %s, want the enrolment to proceed", reason.Code())
	}
	if redemption.DeviceID == device.ID {
		t.Fatal("re-enrolment reused the forgotten device's row id")
	}
}

// TestForgetAccessDevice_IsIdempotent — the overview a person clicks from
// is a snapshot, so a second click (or one from another screen) must
// answer what it asked for rather than a lookup failure.
func TestForgetAccessDevice_IsIdempotent(t *testing.T) {
	app := accessApp(t)
	device, _ := pairDevice(t, app, "A browser", "thumb-forget-twice")
	if _, err := app.RevokeAccessDevice(device.ID); err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}
	if err := app.ForgetAccessDevice(device.ID); err != nil {
		t.Fatalf("first ForgetAccessDevice: %v", err)
	}
	if err := app.ForgetAccessDevice(device.ID); err != nil {
		t.Fatalf("second ForgetAccessDevice: %v", err)
	}
}

// TestAccessOverview_CarriesTheGrantSetPerSession — the surface labels a
// device "View only" from what its session actually holds, so the grant
// set has to reach it. Carried verbatim rather than reduced here: what
// the label MEANS is one definition, and it lives on the page that also
// applies it to itself.
func TestAccessOverview_CarriesTheGrantSetPerSession(t *testing.T) {
	app := accessApp(t)
	state := app.identityState()
	observe, err := identity.PairingAccessViewOnly.Grants()
	if err != nil {
		t.Fatalf("view-only grants: %v", err)
	}
	link, err := state.sessions.MintPairingLink(identity.PairingRequest{
		UserID:       state.owner.ID,
		DeviceClass:  identity.DeviceBrowser,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       observe,
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	if _, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token: link.Token, Proof: identity.DeviceProof{Value: "thumb-view-only"},
		Label: "A shared browser",
	}); reason.Refused() {
		t.Fatalf("RedeemPairing: %s", reason.Code())
	}
	if _, err := state.sessions.ConfirmPairing(link.Link.ID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}

	overview, err := app.GetAccessOverview()
	if err != nil {
		t.Fatalf("GetAccessOverview: %v", err)
	}
	device := findDevice(t, overview, "A shared browser")
	if len(device.Sessions) != 1 {
		t.Fatalf("device carries %d sessions, want 1", len(device.Sessions))
	}
	got := device.Sessions[0].Scopes
	if len(got) != len(observe) {
		t.Fatalf("session scopes = %v, want the %d observe-tier names", got, len(observe))
	}
	for _, want := range observe {
		if !slices.Contains(got, string(want)) {
			t.Fatalf("session scopes = %v, missing %q", got, want)
		}
	}
}

// TestPresentableSession_SurfacesASessionThatOutlivedItsDeviceRevocation
// — the state that should not exist. store.RevokeDevice moves the device
// row and its sessions in ONE transaction, so a credential standing on a
// revoked device means something wrote around that.
//
// A PREDICATE test rather than a staged overview, precisely because the
// state is unreachable through this package's API: producing one would
// mean writing around the store, which would pin the workaround instead
// of the rule. The rule is that the overview carries such a row marked,
// rather than filtering it away with the ordinary revoked history.
func TestPresentableSession_SurfacesASessionThatOutlivedItsDeviceRevocation(t *testing.T) {
	const now = 1_000_000
	live := store.Session{ActivatedAt: now - 10, ExpiresAt: now + 10}
	cases := []struct {
		name      string
		session   store.Session
		present   bool
		anomalous bool
	}{
		{"live on a live device", live, true, false},
		{
			"revoked with its device, the ordinary case",
			store.Session{ActivatedAt: now - 10, ExpiresAt: now + 10, RevokedAt: now - 1, DeviceRevokedAt: now - 1},
			false, false,
		},
		{
			"standing on a revoked device",
			store.Session{ActivatedAt: now - 10, ExpiresAt: now + 10, DeviceRevokedAt: now - 1},
			true, true,
		},
		{
			// Untidy, not reachable: it stopped admitting anything by
			// itself. Calling it an anomaly trains the owner to ignore
			// the one that is.
			"expired on a revoked device",
			store.Session{ActivatedAt: now - 100, ExpiresAt: now - 10, DeviceRevokedAt: now - 1},
			false, false,
		},
		{
			"awaiting confirmation",
			store.Session{ExpiresAt: now + 10},
			true, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := presentableSession(tc.session, now); got != tc.present {
				t.Errorf("presentableSession = %t, want %t", got, tc.present)
			}
			if got := survivedRevocation(tc.session, now); got != tc.anomalous {
				t.Errorf("survivedRevocation = %t, want %t", got, tc.anomalous)
			}
		})
	}
}
