package identity

import "time"

// How long an issued credential pair lives (docs/specs/remote-access.md §4
// "Sessions"). One table, consulted by every mint, so no call site can
// invent its own window.
//
// Access windows are short because the row is what a revocation reaches
// first: a revoked session stops the next call immediately, and the access
// TTL only bounds how long a credential that never reaches this backend
// again stays presentable somewhere else. Refresh windows are long because
// they are the thing a person notices — reaching one means signing in
// again.
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
	// surface, and it is the class whose re-auth becomes a passkey prompt
	// in phase 5, so it renews often on purpose.
	browserAccessTTL = 15 * time.Minute
	// browserRefreshTTL keeps a browser signed in across a working day and
	// no further. Phase 5 gates its renewal on a passkey where one is
	// available; until then it rotates on the same rule as every other
	// class (§4, and see refresh.go).
	browserRefreshTTL = 12 * time.Hour

	// nativeAccessTTL / nativeRefreshTTL cover desktop, phone, CLI, and
	// peer-backend devices: an app that holds its own key and is not a
	// page anyone can navigate away from.
	nativeAccessTTL  = time.Hour
	nativeRefreshTTL = 30 * 24 * time.Hour

	// localChannelTTL is the implicit local-page session's window (§4
	// "Local clients"). It has no refresh secret at all: the session is
	// re-minted on every boot, and one that outlived the process it was
	// minted for would be a credential with nothing left to serve.
	localChannelTTL = 24 * time.Hour
)

// TokenPolicy is one issuance's two windows. Refresh zero means the
// credential is not renewable at all, which is a real answer and not a
// missing one: the local channel re-mints instead.
type TokenPolicy struct {
	Access  time.Duration
	Refresh time.Duration
}

// Renewable reports whether this policy issues a refresh secret.
func (p TokenPolicy) Renewable() bool { return p.Refresh > 0 }

// PolicyFor resolves the windows for one device.
//
// Binding class is read FIRST because it is the stronger statement: a
// `loopback-only` credential is minted by this backend for its own page
// channel whatever the device row says, and it is re-minted at boot rather
// than renewed. Only past that does the device class decide, because the
// distinction that matters there is a script-execution surface (browser)
// versus an app holding its own key — never the device's size.
func PolicyFor(device DeviceClass, binding BindingClass) TokenPolicy {
	if binding == BindingLoopbackOnly {
		return TokenPolicy{Access: localChannelTTL}
	}
	if device == DeviceBrowser {
		return TokenPolicy{Access: browserAccessTTL, Refresh: browserRefreshTTL}
	}
	return TokenPolicy{Access: nativeAccessTTL, Refresh: nativeRefreshTTL}
}
