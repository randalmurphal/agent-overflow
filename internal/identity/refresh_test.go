package identity

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// pairedDevice returns a confirmed pairing: the device, its key thumbprint,
// and the credential pair it holds. The shape every renewal test starts
// from, because a refresh secret only exists where an issuance did.
func pairedDevice(t *testing.T, s *Sessions, owner store.User, thumbprint string) (store.Device, TokenSet) {
	t.Helper()
	link := mustMintLink(t, s, owner)
	redemption := mustRedeem(t, s, link.Token, thumbprint)
	if _, err := s.ConfirmPairing(redemption.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	device, err := s.store.GetDevice(redemption.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	return device, redemption.Tokens
}

func TestRefreshRotatesAndKeepsTheSession(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	_, first := pairedDevice(t, sessions, owner, "thumb-phone")
	if first.RefreshSecret == "" {
		t.Fatal("a device-bound issuance carried no refresh secret")
	}

	c.advance(time.Minute)
	second, reason := sessions.Refresh(RefreshRequest{
		Secret: first.RefreshSecret, KeyThumbprint: "thumb-phone",
	})
	if reason.Refused() {
		t.Fatalf("Refresh: %s", reason)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("renewal moved the session id %q -> %q; the live-connection registry keys on it",
			first.SessionID, second.SessionID)
	}
	if second.RefreshSecret == first.RefreshSecret {
		t.Fatal("renewal re-issued the same refresh secret")
	}
	if second.ExpiresAtMillis <= first.ExpiresAtMillis {
		t.Fatalf("renewal did not move the access window forward: %d -> %d",
			first.ExpiresAtMillis, second.ExpiresAtMillis)
	}
	if _, refused := sessions.Verify(second.Credential); refused.Refused() {
		t.Fatalf("the renewed credential does not verify: %s", refused)
	}
	// The predecessor is spent the moment its successor exists.
	if _, refused := sessions.Refresh(RefreshRequest{
		Secret: first.RefreshSecret, KeyThumbprint: "thumb-phone",
	}); refused != ReasonRevokedSession {
		t.Fatalf("replayed predecessor = %s, want revoked_session", refused)
	}
}

// TestRefreshReuseRevokesTheWholeFamily is the leaked-copy detector. A
// spent secret presented again must end the session, every socket carrying
// it, and every outstanding secret in the chain.
func TestRefreshReuseRevokesTheWholeFamily(t *testing.T) {
	sessions, st, c, owner, _ := newFixture(t)
	conns := &recordingConns{}
	sessions.AttachConns(conns)
	_, first := pairedDevice(t, sessions, owner, "thumb-phone")

	c.advance(time.Minute)
	second, reason := sessions.Refresh(RefreshRequest{
		Secret: first.RefreshSecret, KeyThumbprint: "thumb-phone",
	})
	if reason.Refused() {
		t.Fatalf("Refresh: %s", reason)
	}

	// The spent predecessor, presented again.
	if _, refused := sessions.Refresh(RefreshRequest{
		Secret: first.RefreshSecret, KeyThumbprint: "thumb-phone",
	}); refused != ReasonRevokedSession {
		t.Fatalf("reuse = %s, want revoked_session", refused)
	}
	if _, refused := sessions.Verify(second.Credential); refused != ReasonRevokedSession {
		t.Fatalf("the access credential survived the family revocation: %s", refused)
	}
	// The successor the real device is holding is dead too, which is the
	// whole point: the copy and the original are indistinguishable, so
	// both stop.
	if _, refused := sessions.Refresh(RefreshRequest{
		Secret: second.RefreshSecret, KeyThumbprint: "thumb-phone",
	}); refused != ReasonRevokedSession {
		t.Fatalf("the outstanding successor still renewed: %s", refused)
	}
	conns.mustHaveClosed(t, first.SessionID)

	chain, err := st.ListRefreshSecretsForSession(first.SessionID)
	if err != nil {
		t.Fatalf("ListRefreshSecretsForSession: %v", err)
	}
	for _, secret := range chain {
		if !secret.Spent() {
			t.Fatalf("secret %s survived the family revocation", secret.ID)
		}
	}
	entries, err := st.ListRecentAuthAudit(50)
	if err != nil {
		t.Fatalf("ListRecentAuthAudit: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Event == string(AuditRefreshReuseDetected) {
			found = true
			if entry.Outcome != store.AuthAuditOutcomeRefused || entry.SessionID != first.SessionID {
				t.Fatalf("reuse entry does not name the family: %+v", entry)
			}
		}
	}
	if !found {
		t.Fatal("a detected reuse wrote no audit entry")
	}
}

// TestRefreshBindsToTheDeviceKey — "a bare bearer refresh cannot self-renew"
// (spec §4). The secret alone is not enough on ANY listener.
func TestRefreshBindsToTheDeviceKey(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	_, tokens := pairedDevice(t, sessions, owner, "thumb-phone")

	if _, reason := sessions.Refresh(RefreshRequest{Secret: tokens.RefreshSecret}); reason != ReasonMissingProof {
		t.Fatalf("bare bearer refresh = %s, want missing_proof", reason)
	}
	if _, reason := sessions.Refresh(RefreshRequest{
		Secret: tokens.RefreshSecret, KeyThumbprint: "thumb-someone-else",
	}); reason != ReasonKeyMismatch {
		t.Fatalf("wrong key refresh = %s, want key_mismatch", reason)
	}
	// Neither refusal may have spent the secret: a proof mistake is
	// recoverable, and consuming first would sign the device out for one.
	if _, reason := sessions.Refresh(RefreshRequest{
		Secret: tokens.RefreshSecret, KeyThumbprint: "thumb-phone",
	}); reason.Refused() {
		t.Fatalf("a refused proof spent the secret: %s", reason)
	}
}

func TestRefreshRefusesAnUnknownSecret(t *testing.T) {
	sessions, st, _, _, _ := newFixture(t)
	if _, reason := sessions.Refresh(RefreshRequest{
		Secret: "never-issued", KeyThumbprint: "thumb",
	}); reason != ReasonUnknownCredential {
		t.Fatalf("unknown secret = %s, want unknown_credential", reason)
	}
	if _, reason := sessions.Refresh(RefreshRequest{}); reason != ReasonMissingProof {
		t.Fatalf("empty secret = %s, want missing_proof", reason)
	}
	entries, err := st.ListRecentAuthAudit(10)
	if err != nil {
		t.Fatalf("ListRecentAuthAudit: %v", err)
	}
	if len(entries) == 0 || entries[0].Event != string(AuditRefreshRefused) {
		t.Fatalf("an unknown secret wrote no refusal entry: %+v", entries)
	}
}

func TestRefreshRefusesALapsedSecret(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	_, tokens := pairedDevice(t, sessions, owner, "thumb-phone")
	c.advance(PolicyFor(DevicePhone, BindingDeviceBound).Refresh + time.Minute)

	if _, reason := sessions.Refresh(RefreshRequest{
		Secret: tokens.RefreshSecret, KeyThumbprint: "thumb-phone",
	}); reason != ReasonExpiredSession {
		t.Fatalf("lapsed refresh = %s, want expired_session", reason)
	}
}

func TestRefreshRefusesARevokedDevice(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	device, tokens := pairedDevice(t, sessions, owner, "thumb-phone")
	if _, err := sessions.RevokeDevice(device.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, reason := sessions.Refresh(RefreshRequest{
		Secret: tokens.RefreshSecret, KeyThumbprint: "thumb-phone",
	}); !reason.Refused() {
		t.Fatal("a revoked device renewed its credential")
	}
}

// TestRefreshRefusesAnUnconfirmedSession — the confirmation gate covers the
// renewal path too, by the same predicate rather than a second check.
func TestRefreshRefusesAnUnconfirmedSession(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	redemption := mustRedeem(t, sessions, link.Token, "thumb-phone")

	if _, reason := sessions.Refresh(RefreshRequest{
		Secret: redemption.Tokens.RefreshSecret, KeyThumbprint: "thumb-phone",
	}); reason != ReasonPendingConfirmation {
		t.Fatalf("unconfirmed renewal = %s, want pending_confirmation", reason)
	}
}

// TestBrowserClassGetsAShortWindow — the spec names the browser class
// specifically (§4 "Sessions"), and the distinction is the one property a
// browser profile has that no other class does.
func TestBrowserClassGetsAShortWindow(t *testing.T) {
	browser := PolicyFor(DeviceBrowser, BindingDeviceBound)
	native := PolicyFor(DeviceDesktop, BindingDeviceBound)
	if browser.Access >= native.Access {
		t.Fatalf("browser access window %s is not shorter than %s", browser.Access, native.Access)
	}
	if browser.Refresh >= native.Refresh {
		t.Fatalf("browser refresh window %s is not shorter than %s", browser.Refresh, native.Refresh)
	}
	if !browser.Renewable() {
		t.Fatal("the browser class is not renewable; passkey gating is phase 5, rotation is now")
	}
	local := PolicyFor(DeviceDesktop, BindingLoopbackOnly)
	if local.Renewable() {
		t.Fatal("the local page channel issued a refresh secret; it is re-minted at boot instead")
	}
	if PolicyFor(DeviceBrowser, BindingLoopbackOnly) != local {
		t.Fatal("binding class must decide before device class for the local channel")
	}
}

func TestLocalChannelSessionIsOneRowAcrossBoots(t *testing.T) {
	sessions, st, c, owner, _ := newFixture(t)
	first, firstTokens, err := sessions.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatalf("EnsureLocalChannelSession: %v", err)
	}
	if firstTokens.RefreshSecret != "" {
		t.Fatal("the local channel session carries a refresh secret")
	}
	if first.BindingClass != string(BindingLoopbackOnly) {
		t.Fatalf("local channel binding class = %q", first.BindingClass)
	}
	if len(first.Scopes) != len(Scopes) {
		t.Fatalf("local channel scopes = %v, want the full declared set", first.Scopes)
	}

	c.advance(time.Hour)
	second, secondTokens, err := sessions.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatalf("second EnsureLocalChannelSession: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("the local channel session id moved across boots: %q -> %q", first.ID, second.ID)
	}
	if second.DeviceID != first.DeviceID {
		t.Fatalf("the local channel device moved: %q -> %q", first.DeviceID, second.DeviceID)
	}
	if secondTokens.Credential == firstTokens.Credential {
		t.Fatal("the second boot re-served the first boot's credential")
	}
	if _, reason := sessions.Verify(secondTokens.Credential); reason.Refused() {
		t.Fatalf("the re-issued local credential does not verify: %s", reason)
	}
	devices, err := st.ListDevicesForUser(owner.ID)
	if err != nil {
		t.Fatalf("ListDevicesForUser: %v", err)
	}
	channels := 0
	for _, device := range devices {
		if device.Channel == LocalChannel {
			channels++
		}
	}
	if channels != 1 {
		t.Fatalf("%d devices claim the local channel", channels)
	}
}

func TestLocalChannelSessionReMintsAfterExpiry(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	first, _, err := sessions.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatalf("EnsureLocalChannelSession: %v", err)
	}
	c.advance(localChannelTTL + time.Hour)
	second, tokens, err := sessions.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatalf("second EnsureLocalChannelSession: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("a lapsed local channel session was reused rather than re-minted")
	}
	if _, reason := sessions.Verify(tokens.Credential); reason.Refused() {
		t.Fatalf("the re-minted local credential does not verify: %s", reason)
	}
}

func TestLocalChannelSessionRefusesARevokedChannelDevice(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	session, _, err := sessions.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatalf("EnsureLocalChannelSession: %v", err)
	}
	if _, err := sessions.RevokeDevice(session.DeviceID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, _, err := sessions.EnsureLocalChannelSession(owner.ID); err == nil {
		t.Fatal("a boot re-minted around a revoked local channel device")
	}
}

func TestPruneCredentialsKeepsWhatCouldStillAdmitOrExplain(t *testing.T) {
	sessions, st, c, owner, _ := newFixture(t)
	_, tokens := pairedDevice(t, sessions, owner, "thumb-phone")
	stale := mustMintLink(t, sessions, owner)

	// Past the link's window but well inside the session's, so anything the
	// prune takes here it took by choice rather than by the clock.
	c.advance(PairingLinkTTL + time.Minute)
	sessions.PruneCredentials(0)

	if _, reason := sessions.Verify(tokens.Credential); reason.Refused() {
		t.Fatalf("the prune reached a live credential: %s", reason)
	}
	if _, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: stale.Token, KeyThumbprint: "thumb",
	}); reason != ReasonUnknownCredential {
		t.Fatalf("the expired link survived the prune: %s", reason)
	}
	chain, err := st.ListRefreshSecretsForSession(tokens.SessionID)
	if err != nil {
		t.Fatalf("ListRefreshSecretsForSession: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("the prune took the live refresh secret: %d rows", len(chain))
	}
}

// A revocation that lands between the slow-path row read and the fast-path
// install must win: rememberAt declines a stale generation rather than
// resurrecting the dead session in the in-memory table. The direct-call
// shape stages the interleaving Reissue/Revoke race deterministically.
func TestRememberAtDeclinesAcrossARevocation(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	_, tokens := pairedDevice(t, sessions, owner, "thumb-race")

	row, err := sessions.store.GetSession(tokens.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	generation := sessions.generationNow()
	if _, err := sessions.RevokeSession(tokens.SessionID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	sessions.rememberAt(generation, row)
	if _, reason := sessions.Live(tokens.SessionID); reason != ReasonRevokedSession {
		t.Fatalf("Live after a raced install = %s, want %s", reason, ReasonRevokedSession)
	}

	// The same install with a CURRENT generation is the ordinary path and
	// must still work — the guard declines staleness, not remembering.
	_, fresh := pairedDevice(t, sessions, owner, "thumb-race-2")
	freshRow, err := sessions.store.GetSession(fresh.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	sessions.rememberAt(sessions.generationNow(), freshRow)
	if _, reason := sessions.Live(fresh.SessionID); reason.Refused() {
		t.Fatalf("Live after an ordinary install refused: %s", reason)
	}
}
