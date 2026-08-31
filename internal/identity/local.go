package identity

import (
	"fmt"
	"runtime"

	"agent-overflow/internal/store"
)

// The local page channel (docs/specs/remote-access.md §4 "Local clients").
//
// The embedded webview, the `--connect` stub, and the WSL launcher relay
// are one client as far as identity is concerned: a `loopback-only` session
// this backend mints for ITSELF at boot, delivered over the same bootstrap
// exchange that already hands out the page credential.
//
// It exists so that "the request arrived over loopback" stops being a trust
// basis. A same-host relay can carry a remote peer's traffic and it looks
// identical at the socket; a credential the backend minted and the relay
// forwards does not. Nothing in this wave removes the launch credential —
// it still authorizes every request — but every local connection now also
// NAMES a session, which is what gives revocation and attribution something
// to reach.

// LocalChannel is the `devices.channel` value of the local page channel's
// device row. One row, resolved on every boot rather than minted, so a
// machine does not accumulate a device per launch.
const LocalChannel = "local"

// localChannelLabel is what the device list calls it. Deliberately not the
// hostname: the row describes this backend's own page channel, and a name
// that looked like a paired device would invite revoking it.
const localChannelLabel = "This computer"

// EnsureLocalChannelSession resolves the local page channel's session and
// returns a fresh credential for it.
//
// Idempotent across boots in both halves. The DEVICE is resolved by channel
// (one row forever). The SESSION is reused while it is still live and
// re-minted when it is not — the credential itself cannot be reused because
// nothing stores it, but re-signing claims for the same session id keeps the
// row count at one per channel instead of one per launch, and keeps a
// revocation of that session meaningful across a restart.
//
// The credential carries no refresh secret: PolicyFor gives every
// `loopback-only` binding a zero refresh window, because this session is
// re-minted at boot and one that renewed itself would outlive the process
// it was minted to serve.
//
// Scopes are the full declared set. The local page is the host tier (§2),
// and it holds full access through the launch credential today regardless;
// granting less here would describe the surface inaccurately rather than
// narrow it. What each scope PERMITS is phase 3.
func (s *Sessions) EnsureLocalChannelSession(userID string) (store.Session, TokenSet, error) {
	if userID == "" {
		return store.Session{}, TokenSet{}, fmt.Errorf("identity: local channel session needs a user id")
	}
	device, err := s.store.EnsureChannelDevice(
		userID, LocalChannel, localChannelLabel, string(DeviceDesktop), runtime.GOOS)
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	if device.RevokedAt != 0 {
		// A revoked channel device is a deliberate act, and re-minting
		// around it would make the revocation unenforceable from the one
		// surface that can perform it. Say so rather than quietly
		// re-admitting.
		return store.Session{}, TokenSet{}, fmt.Errorf(
			"identity: the local channel device is revoked; clear the revocation to serve local clients")
	}

	now := s.now().UnixMilli()
	if session, ok := s.liveSessionFor(device.ID, now); ok {
		expiresAt := now + PolicyFor(DeviceClass(device.Class), BindingLoopbackOnly).
			Access.Milliseconds()
		if _, err := s.store.ExtendSession(session.ID, expiresAt, now); err != nil {
			return store.Session{}, TokenSet{}, err
		}
		generation := s.generationNow()
		refreshed, err := s.store.GetSession(session.ID)
		if err != nil {
			return store.Session{}, TokenSet{}, err
		}
		s.rememberAt(generation, refreshed)
		tokens, err := s.issueFor(refreshed, device, now)
		if err != nil {
			return store.Session{}, TokenSet{}, err
		}
		return refreshed, tokens, nil
	}

	session, _, err := s.Mint(MintRequest{
		UserID:       userID,
		DeviceID:     device.ID,
		BindingClass: BindingLoopbackOnly,
		Scopes:       Scopes,
		TTL:          PolicyFor(DeviceClass(device.Class), BindingLoopbackOnly).Access,
	})
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	// Mint's own credential is discarded and re-signed through issueFor so
	// every issuance in this package goes out of one function: one place
	// decides what a TokenSet contains, and a policy change cannot reach
	// some callers and miss others.
	tokens, err := s.issueFor(session, device, now)
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	return session, tokens, nil
}

// liveSessionFor returns the newest session of a device that would still
// admit a presentation. Scans the device's own rows, which is bounded by
// how often that device has re-paired, and is read once per boot.
func (s *Sessions) liveSessionFor(deviceID string, now int64) (store.Session, bool) {
	sessions, err := s.store.ListSessionsForDevice(deviceID)
	if err != nil {
		return store.Session{}, false
	}
	for _, session := range sessions {
		if session.Live(now) {
			return session, true
		}
	}
	return store.Session{}, false
}
