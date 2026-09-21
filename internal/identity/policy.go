package identity

import "time"

// Credential windows for paired devices and pending enrollment.
const (
	// PairingLinkTTL is the window a pairing link may be redeemed in (§4
	// "Pairing" step 3). Five minutes is the spec's number: long enough to
	// walk a phone to a laptop, short enough that a link left in a chat
	// message is dead by the time anyone scrolls back.
	PairingLinkTTL = 5 * time.Minute

	// PairingConfirmWindow is how long a redeemed-but-unconfirmed session
	// stays presentable. It bounds the state where a credential exists and
	// admits nothing: past it the device needs a fresh link, which is the
	// same answer the owner would give by refusing the number.
	PairingConfirmWindow = 10 * time.Minute

	// browserAccessTTL is the short window the spec names for the browser
	// class. A browser profile is the only client with a script-execution
	// surface, so it renews often on purpose.
	browserAccessTTL = 15 * time.Minute
	// browserRefreshTTL keeps a browser signed in across a working day and
	// no further.
	//
	// Renewal is NOT passkey-gated, for any class. Rotation is the control
	// on a live family (refresh.go), and a passkey is what a browser
	// re-authenticates with once that family has ENDED — a fresh sign-in
	// with no code to type, rather than an extra prompt on a renewal that
	// is already single-use (passkey.go).
	browserRefreshTTL = 12 * time.Hour

	// nativeAccessTTL / nativeRefreshTTL cover desktop, phone, CLI, and
	// peer-backend devices: an app that holds its own key and is not a
	// page anyone can navigate away from.
	nativeAccessTTL  = time.Hour
	nativeRefreshTTL = 30 * 24 * time.Hour
)

// TokenPolicy contains the access and refresh windows for paired devices.
type TokenPolicy struct {
	Access  time.Duration
	Refresh time.Duration
}

func (p TokenPolicy) Renewable() bool { return p.Refresh > 0 }

// RenewablePolicyFor is used only by paired credential issuance.
// The local channel has a separate, process-bound issuance path.
func RenewablePolicyFor(device DeviceClass) TokenPolicy {
	if device == DeviceBrowser {
		return TokenPolicy{Access: browserAccessTTL, Refresh: browserRefreshTTL}
	}
	return TokenPolicy{Access: nativeAccessTTL, Refresh: nativeRefreshTTL}
}
