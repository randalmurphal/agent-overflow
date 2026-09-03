package app

import (
	"testing"

	"agent-overflow/internal/identity"
)

// HasEnrolledDevice is what the `serve` console asks before it offers to
// pair the first device. Its whole value is in the two exclusions, so each
// gets its own case: the local page channel is not a paired device, and a
// revoked row is not access.

func TestHasEnrolledDevice_FalseOnAFreshBackend(t *testing.T) {
	app := accessApp(t)

	enrolled, err := HasEnrolledDevice(app)
	if err != nil {
		t.Fatalf("HasEnrolledDevice: %v", err)
	}
	if enrolled {
		t.Fatal("a backend nobody has paired with reports an enrolled device")
	}
}

// The local page channel exists on every boot, for the window a serve host
// never opens. Counting it would mean a headless host was never fresh and
// the console would never offer to enroll anything.
func TestHasEnrolledDevice_IgnoresTheLocalPageChannel(t *testing.T) {
	app := accessApp(t)
	local := localChannelSession(t, app)
	if local.DeviceID == "" {
		t.Fatal("the local channel session names no device")
	}

	enrolled, err := HasEnrolledDevice(app)
	if err != nil {
		t.Fatalf("HasEnrolledDevice: %v", err)
	}
	if enrolled {
		t.Fatal("the local page channel counts as an enrolled device")
	}
}

func TestHasEnrolledDevice_TrueOnceADeviceIsPaired(t *testing.T) {
	app := accessApp(t)
	localChannelSession(t, app)
	pairDevice(t, app, "A browser", "thumb-browser")

	enrolled, err := HasEnrolledDevice(app)
	if err != nil {
		t.Fatalf("HasEnrolledDevice: %v", err)
	}
	if !enrolled {
		t.Fatal("a paired browser does not count as an enrolled device")
	}
}

// An owner who revoked their last device on a machine with no screen has no
// remaining way in. That is the one moment the console most needs to offer
// enrollment, so a revoked row must not read as access.
func TestHasEnrolledDevice_FalseAfterTheLastDeviceIsRevoked(t *testing.T) {
	app := accessApp(t)
	device, _ := pairDevice(t, app, "A browser", "thumb-browser")

	if _, err := app.RevokeAccessDevice(device.ID); err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}

	enrolled, err := HasEnrolledDevice(app)
	if err != nil {
		t.Fatalf("HasEnrolledDevice: %v", err)
	}
	if enrolled {
		t.Fatal("a revoked device still reports as enrolled, which would lock a headless host out of itself")
	}
}

// One surviving device is enough: the question is "can anything still reach
// this backend", not "is every device live".
func TestHasEnrolledDevice_TrueWhileAnyDeviceSurvives(t *testing.T) {
	app := accessApp(t)
	revoked, _ := pairDevice(t, app, "An old phone", "thumb-phone")
	pairDevice(t, app, "A browser", "thumb-browser")

	if _, err := app.RevokeAccessDevice(revoked.ID); err != nil {
		t.Fatalf("RevokeAccessDevice: %v", err)
	}

	enrolled, err := HasEnrolledDevice(app)
	if err != nil {
		t.Fatalf("HasEnrolledDevice: %v", err)
	}
	if !enrolled {
		t.Fatal("revoking one of two devices reported the backend as unpaired")
	}
}

// A backend whose session core is not running cannot answer, and saying
// "no devices" there would have the serve console mint a link against an
// identity that does not exist.
func TestHasEnrolledDevice_ReportsWhenIdentityIsNotRunning(t *testing.T) {
	app := newTestAppWithStore(t)

	if _, err := HasEnrolledDevice(app); err == nil {
		t.Fatal("HasEnrolledDevice answered on a backend with no session core")
	}
}

// The pairing-state predicates are the vocabulary package main branches on,
// and the constants behind them stay unexported on purpose. Pin all five
// spellings and the three groupings here, where the constants are visible.
func TestPairingStatusViewPredicates(t *testing.T) {
	tests := []struct {
		state     string
		redeemed  bool
		settled   bool
		confirmed bool
	}{
		{state: pairingStatePending},
		{state: pairingStateRedeemed, redeemed: true},
		{state: pairingStateConfirmed, settled: true, confirmed: true},
		{state: pairingStateCanceled, settled: true},
		{state: pairingStateExpired, settled: true},
	}
	for _, test := range tests {
		t.Run(test.state, func(t *testing.T) {
			view := PairingStatusView{State: test.state}
			if view.Redeemed() != test.redeemed {
				t.Errorf("Redeemed() = %v, want %v", view.Redeemed(), test.redeemed)
			}
			if view.Settled() != test.settled {
				t.Errorf("Settled() = %v, want %v", view.Settled(), test.settled)
			}
			if view.Confirmed() != test.confirmed {
				t.Errorf("Confirmed() = %v, want %v", view.Confirmed(), test.confirmed)
			}
		})
	}

	// A waiting caller must not read an unknown state as "keep waiting
	// forever" AND as "nothing to act on" at once; every declared state is
	// covered above, so this pins that the set has not grown silently.
	if got := len([]string{
		pairingStatePending, pairingStateRedeemed, pairingStateConfirmed,
		pairingStateCanceled, pairingStateExpired,
	}); got != len(tests) {
		t.Fatalf("%d declared pairing states but %d covered", got, len(tests))
	}
}

// The class the serve console enrolls has to be one identity declares, and
// the enrollment path calls the same MintDevicePairing the settings screen
// does. This is the app-side half of that: a browser-class full-access mint
// through the ordinary surface, with no session context, is admitted.
func TestMintDevicePairing_AdmitsTheHostPresentInProcessCaller(t *testing.T) {
	app := accessApp(t)

	invite, err := app.MintDevicePairing(string(identity.DeviceBrowser), string(identity.PairingAccessFull))
	if err != nil {
		t.Fatalf("MintDevicePairing from an in-process caller: %v", err)
	}
	if invite.LinkID == "" || invite.URL == "" {
		t.Fatalf("the invite is incomplete: %+v", invite)
	}

	status, err := app.DevicePairingStatus(invite.LinkID)
	if err != nil {
		t.Fatalf("DevicePairingStatus: %v", err)
	}
	if status.Settled() || status.Redeemed() {
		t.Fatalf("a freshly minted link reports state %q", status.State)
	}
}
