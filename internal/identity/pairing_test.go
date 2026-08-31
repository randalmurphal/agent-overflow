package identity

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

func mustMintLink(t *testing.T, s *Sessions, owner store.User, scopes ...Scope) PairingLink {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []Scope{ScopeThreadsRead, ScopeFilesRead}
	}
	link, err := s.MintPairingLink(PairingRequest{
		UserID:       owner.ID,
		DeviceClass:  DevicePhone,
		BindingClass: BindingDeviceBound,
		Scopes:       scopes,
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	return link
}

func mustRedeem(t *testing.T, s *Sessions, token, thumbprint string) Redemption {
	t.Helper()
	redemption, reason := s.RedeemPairing(RedemptionRequest{
		Token: token, Proof: bearerProof(thumbprint), Label: "A Phone", Platform: "ios",
	})
	if reason.Refused() {
		t.Fatalf("RedeemPairing: %s", reason)
	}
	return redemption
}

// TestPairingAdmitsADeviceOnlyAfterConfirmation is the whole flow: mint,
// redeem with a key, and the credential admits NOTHING until the owner has
// matched the verification number.
func TestPairingAdmitsADeviceOnlyAfterConfirmation(t *testing.T) {
	sessions, st, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)

	redemption := mustRedeem(t, sessions, link.Token, "thumb-phone")
	if !redemption.Tokens.AwaitingConfirmation {
		t.Fatal("a redemption reported a credential that was already live")
	}
	if _, reason := sessions.Verify(redemption.Tokens.Credential); reason != ReasonPendingConfirmation {
		t.Fatalf("unconfirmed credential = %s, want pending_confirmation", reason)
	}

	confirmed, err := sessions.ConfirmPairing(redemption.PairingID)
	if err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	if confirmed.SessionID != redemption.Tokens.SessionID {
		t.Fatalf("confirmation named session %q, redemption named %q",
			confirmed.SessionID, redemption.Tokens.SessionID)
	}
	session, reason := sessions.Verify(redemption.Tokens.Credential)
	if reason.Refused() {
		t.Fatalf("confirmed credential = %s, want admitted", reason)
	}
	if session.UserID != owner.ID || session.BindingClass != string(BindingDeviceBound) {
		t.Fatalf("session did not carry the link's terms: %+v", session)
	}

	device, err := st.GetDevice(redemption.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if device.KeyThumbprint != "thumb-phone" {
		t.Fatalf("device key thumbprint = %q", device.KeyThumbprint)
	}
	if device.Class != string(DevicePhone) {
		t.Fatalf("device class = %q; the LINK decides it, not the device", device.Class)
	}
	if device.Label != "A Phone" || device.Platform != "ios" {
		t.Fatalf("device did not keep the label it reported: %+v", device)
	}
}

// TestConfirmationReplacesTheConfirmationWindow — the unconfirmed window is
// a deadline on the owner's decision, not the device's access window.
func TestConfirmationReplacesTheConfirmationWindow(t *testing.T) {
	sessions, st, c, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	redemption := mustRedeem(t, sessions, link.Token, "thumb-phone")

	pending, err := st.GetSession(redemption.Tokens.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	wantPending := c.now().Add(PairingConfirmWindow).UnixMilli()
	if pending.ExpiresAt != wantPending {
		t.Fatalf("pending window ends at %d, want the confirmation window %d",
			pending.ExpiresAt, wantPending)
	}
	if _, err := sessions.ConfirmPairing(redemption.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	live, err := st.GetSession(redemption.Tokens.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	wantLive := c.now().Add(PolicyFor(DevicePhone, BindingDeviceBound).Access).UnixMilli()
	if live.ExpiresAt != wantLive {
		t.Fatalf("confirmed window ends at %d, want the device class's access window %d",
			live.ExpiresAt, wantLive)
	}
}

func TestPairingLinkIsSingleUseAcrossRedeemers(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	mustRedeem(t, sessions, link.Token, "thumb-phone")

	_, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: link.Token, Proof: bearerProof("thumb-other"),
	})
	if reason != ReasonUnknownCredential {
		t.Fatalf("second redemption = %s, want unknown_credential", reason)
	}
}

// TestPairingLinkRedemptionUnderConcurrency — the CAS is what makes exactly
// one key win a link, and the loser must learn nothing about why.
func TestPairingLinkRedemptionUnderConcurrency(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)

	const racers = 6
	var wg sync.WaitGroup
	admitted := make([]bool, racers)
	wg.Add(racers)
	for i := range racers {
		go func() {
			defer wg.Done()
			_, reason := sessions.RedeemPairing(RedemptionRequest{
				Token: link.Token, Proof: bearerProof("thumb-" + string(rune('a'+i))),
			})
			admitted[i] = !reason.Refused()
		}()
	}
	wg.Wait()
	winners := 0
	for _, ok := range admitted {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d keys redeemed one link; exactly one may win", winners)
	}
}

func TestPairingRedemptionRequiresAKeyThumbprint(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)

	if _, reason := sessions.RedeemPairing(RedemptionRequest{Token: link.Token}); reason != ReasonMissingProof {
		t.Fatalf("redemption with no key = %s, want missing_proof", reason)
	}
	if _, reason := sessions.RedeemPairing(RedemptionRequest{Proof: bearerProof("thumb")}); reason != ReasonMissingProof {
		t.Fatalf("redemption with no token = %s, want missing_proof", reason)
	}
	// Neither refusal may have spent the link.
	if _, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: link.Token, Proof: bearerProof("thumb"),
	}); reason.Refused() {
		t.Fatalf("a refused redemption spent the link: %s", reason)
	}
}

func TestPairingLinkExpires(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	c.advance(PairingLinkTTL + time.Second)

	if _, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: link.Token, Proof: bearerProof("thumb"),
	}); reason != ReasonUnknownCredential {
		t.Fatalf("expired link = %s, want unknown_credential", reason)
	}
}

// TestUnconfirmedSessionExpiresWithTheConfirmationWindow — a credential
// nobody confirmed must not sit presentable-once-confirmed forever.
func TestUnconfirmedSessionExpiresWithTheConfirmationWindow(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	redemption := mustRedeem(t, sessions, link.Token, "thumb-phone")

	c.advance(PairingConfirmWindow + time.Second)
	if _, reason := sessions.Verify(redemption.Tokens.Credential); reason != ReasonExpiredSession {
		t.Fatalf("lapsed unconfirmed credential = %s, want expired_session", reason)
	}
	if _, err := sessions.ConfirmPairing(redemption.PairingID); !errors.Is(err, ErrPairingRefused) {
		t.Fatalf("confirming a lapsed pairing err = %v, want ErrPairingRefused", err)
	}
}

// TestCancelPairingRevokesWhatTheRedemptionCreated — the refusal half of
// the verification number. Saying "that is not my device" has to end the
// credential that device already holds.
func TestCancelPairingRevokesWhatTheRedemptionCreated(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	redemption := mustRedeem(t, sessions, link.Token, "thumb-phone")

	if _, err := sessions.CancelPairing(redemption.PairingID); err != nil {
		t.Fatalf("CancelPairing: %v", err)
	}
	if _, reason := sessions.Verify(redemption.Tokens.Credential); reason != ReasonRevokedSession {
		t.Fatalf("credential after cancel = %s, want revoked_session", reason)
	}
	if _, err := sessions.ConfirmPairing(redemption.PairingID); !errors.Is(err, ErrPairingRefused) {
		t.Fatalf("confirming a canceled pairing err = %v, want ErrPairingRefused", err)
	}
}

// TestVerificationNumberIsKeyedToTheDeviceThatRedeemed is the property the
// silent-race case rests on: a link redeemed by a different key produces a
// different number on the minting surface, so the owner sees a mismatch.
func TestVerificationNumberIsKeyedToTheDeviceThatRedeemed(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	redemption := mustRedeem(t, sessions, link.Token, "thumb-phone")

	_, shown, err := sessions.PairingStatus(redemption.PairingID)
	if err != nil {
		t.Fatalf("PairingStatus: %v", err)
	}
	if shown != redemption.VerificationNumber {
		t.Fatalf("minting surface shows %q, device shows %q; the two must agree",
			shown, redemption.VerificationNumber)
	}
	other, err := sessions.VerificationNumber(redemption.PairingID, "thumb-someone-else")
	if err != nil {
		t.Fatalf("VerificationNumber: %v", err)
	}
	if other == redemption.VerificationNumber {
		t.Fatal("a different device key produced the same verification number")
	}
}

func TestVerificationNumberIsBoundToTheLink(t *testing.T) {
	sessions, _, _, _, _ := newFixture(t)
	first, err := sessions.VerificationNumber("link-one", "thumb")
	if err != nil {
		t.Fatalf("VerificationNumber: %v", err)
	}
	second, err := sessions.VerificationNumber("link-two", "thumb")
	if err != nil {
		t.Fatalf("VerificationNumber: %v", err)
	}
	if first == second {
		t.Fatal("the same key produced the same number for two different links")
	}
	if len(first) != verificationDigits {
		t.Fatalf("verification number %q is %d digits, want %d", first, len(first), verificationDigits)
	}
	if strings.Trim(first, "0123456789") != "" {
		t.Fatalf("verification number %q is not decimal", first)
	}
}

// TestVerificationNumberKeepsItsLeadingZeros — a number formatted as an
// integer would show four digits one time in ten, and the owner would be
// comparing two strings that differ by presentation alone.
func TestVerificationNumberKeepsItsLeadingZeros(t *testing.T) {
	sessions, _, _, _, _ := newFixture(t)
	for i := range 400 {
		number, err := sessions.VerificationNumber("link-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "thumb")
		if err != nil {
			t.Fatalf("VerificationNumber: %v", err)
		}
		if len(number) != verificationDigits {
			t.Fatalf("verification number %q is %d digits, want %d",
				number, len(number), verificationDigits)
		}
	}
}

// TestPairingScopeSubsetSurvivesToTheSession — a viewer link is this and
// nothing else, so the narrowed grant has to reach the session row.
func TestPairingScopeSubsetSurvivesToTheSession(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner, ScopeThreadsRead)
	redemption := mustRedeem(t, sessions, link.Token, "thumb-phone")
	if _, err := sessions.ConfirmPairing(redemption.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	session, reason := sessions.Verify(redemption.Tokens.Credential)
	if reason.Refused() {
		t.Fatalf("Verify: %s", reason)
	}
	if len(session.Scopes) != 1 || session.Scopes[0] != string(ScopeThreadsRead) {
		t.Fatalf("session scopes = %v, want only the link's subset", session.Scopes)
	}
}

func TestMintPairingLinkRefusesUndeclaredTerms(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	for name, req := range map[string]PairingRequest{
		"no user":       {DeviceClass: DevicePhone, BindingClass: BindingDeviceBound},
		"bad device":    {UserID: owner.ID, DeviceClass: "watch", BindingClass: BindingDeviceBound},
		"bad binding":   {UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: "sort-of"},
		"bad scope set": {UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: BindingDeviceBound, Scopes: []Scope{"threads:reed"}},
	} {
		if _, err := sessions.MintPairingLink(req); err == nil {
			t.Fatalf("MintPairingLink accepted %s", name)
		}
	}
}

// TestRepairingAKnownKeyReusesItsDevice — the thumbprint column is uniquely
// indexed, so a second pairing of the same physical device must resolve to
// the row that already holds its key rather than failing on the constraint.
func TestRepairingAKnownKeyReusesItsDevice(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	first := mustRedeem(t, sessions, mustMintLink(t, sessions, owner).Token, "thumb-phone")
	if _, err := sessions.ConfirmPairing(first.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	second := mustRedeem(t, sessions, mustMintLink(t, sessions, owner).Token, "thumb-phone")
	if second.DeviceID != first.DeviceID {
		t.Fatalf("re-pairing minted a second device row: %q then %q", first.DeviceID, second.DeviceID)
	}
	if second.Tokens.SessionID == first.Tokens.SessionID {
		t.Fatal("re-pairing reused the previous session rather than issuing a new one")
	}
}

// TestRepairingARevokedDeviceIsRefused — a fresh link must not be able to
// undo a revocation. The remedy is on the device surface, not here.
func TestRepairingARevokedDeviceIsRefused(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	first := mustRedeem(t, sessions, mustMintLink(t, sessions, owner).Token, "thumb-phone")
	if _, err := sessions.RevokeDevice(first.DeviceID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	link := mustMintLink(t, sessions, owner)
	if _, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: link.Token, Proof: bearerProof("thumb-phone"),
	}); reason != ReasonRevokedDevice {
		t.Fatalf("re-pairing a revoked device = %s, want revoked_device", reason)
	}
	// The refused redemption must have settled the link, not released it.
	if _, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: link.Token, Proof: bearerProof("thumb-fresh"),
	}); reason != ReasonUnknownCredential {
		t.Fatalf("a link survived a refused redemption: %s", reason)
	}
}

// TestRestoreReadmitsARevokedKeyToPairing — the remedy the revoked-key
// refusal names. Restoring moves no credential: the device's old sessions
// stay revoked, and only a FRESH link (the refused attempt spent its own)
// redeems, adopting the existing device row.
func TestRestoreReadmitsARevokedKeyToPairing(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	first := mustRedeem(t, sessions, mustMintLink(t, sessions, owner).Token, "thumb-phone")
	if _, err := sessions.ConfirmPairing(first.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	if _, err := sessions.RevokeDevice(first.DeviceID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	moved, err := sessions.RestoreDevice(first.DeviceID)
	if err != nil || !moved {
		t.Fatalf("RestoreDevice = (%v, %v), want a moved row", moved, err)
	}
	// Restoring twice is a no-op, not an error.
	if moved, err := sessions.RestoreDevice(first.DeviceID); err != nil || moved {
		t.Fatalf("second RestoreDevice = (%v, %v), want no-op", moved, err)
	}
	// The old session did not come back with the device.
	if _, reason := sessions.Verify(first.Tokens.Credential); reason != ReasonRevokedSession {
		t.Fatalf("old credential after restore = %s, want revoked_session", reason)
	}
	second := mustRedeem(t, sessions, mustMintLink(t, sessions, owner).Token, "thumb-phone")
	if second.DeviceID != first.DeviceID {
		t.Fatalf("restored key minted a second device row: %q then %q", first.DeviceID, second.DeviceID)
	}
}

func TestPairingPayloadRoundTripsAndRefusesAnUnknownVersion(t *testing.T) {
	payload := PairingPayload{
		Version:     PairingPayloadVersion,
		BackendID:   testBackendID,
		BackendName: "home-server",
		Endpoint:    "https://192.168.1.20:7423",
		Token:       "a-token",
	}
	encoded, err := payload.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.ContainsAny(encoded, "=+/") {
		t.Fatalf("encoded payload %q is not fragment-safe base64url", encoded)
	}
	decoded, err := DecodePairingPayload("#" + encoded)
	if err != nil {
		t.Fatalf("DecodePairingPayload: %v", err)
	}
	if decoded != payload {
		t.Fatalf("payload did not round-trip: %+v", decoded)
	}
	if decoded.CertFingerprint != "" {
		t.Fatalf("cert fingerprint = %q, want the reserved field empty", decoded.CertFingerprint)
	}

	future := payload
	future.Version = PairingPayloadVersion + 1
	encodedFuture, err := future.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := DecodePairingPayload(encodedFuture); err == nil {
		t.Fatal("a payload from a later version decoded as this one")
	}
	if _, err := DecodePairingPayload("not base64!!"); err == nil {
		t.Fatal("garbage decoded as a payload")
	}
}

// TestPairingPayloadCarriesTheCertFingerprintWhenOneExists — phase 5 fills
// the field; the shape has to carry it today or the payload moves then.
func TestPairingPayloadCarriesTheCertFingerprintWhenOneExists(t *testing.T) {
	sessions, st, _, owner, _ := newFixture(t)
	link, err := sessions.MintPairingLink(PairingRequest{
		UserID: owner.ID, DeviceClass: DevicePhone, BindingClass: BindingDeviceBound,
		Scopes: []Scope{ScopeThreadsRead}, CertFingerprint: "sha256:abcd",
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	stored, err := st.GetPairingLink(link.Link.ID)
	if err != nil {
		t.Fatalf("GetPairingLink: %v", err)
	}
	if stored.CertFingerprint != "sha256:abcd" {
		t.Fatalf("cert fingerprint = %q", stored.CertFingerprint)
	}
	payload := PairingPayload{
		Version: PairingPayloadVersion, BackendID: testBackendID,
		Endpoint: "https://host:1", Token: link.Token,
		CertFingerprint: stored.CertFingerprint,
	}
	encoded, err := payload.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := DecodePairingPayload(encoded)
	if err != nil {
		t.Fatalf("DecodePairingPayload: %v", err)
	}
	if decoded.CertFingerprint != "sha256:abcd" {
		t.Fatalf("fingerprint did not survive the payload: %+v", decoded)
	}
}

func TestPairingAuditTrailNamesEveryStep(t *testing.T) {
	sessions, st, _, owner, _ := newFixture(t)
	link := mustMintLink(t, sessions, owner)
	redemption := mustRedeem(t, sessions, link.Token, "thumb-phone")
	if _, err := sessions.ConfirmPairing(redemption.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	if _, reason := sessions.RedeemPairing(RedemptionRequest{
		Token: "never-minted", Proof: bearerProof("thumb"),
	}); !reason.Refused() {
		t.Fatal("an unknown token was admitted")
	}
	entries, err := st.ListRecentAuthAudit(50)
	if err != nil {
		t.Fatalf("ListRecentAuthAudit: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Event] = true
	}
	for _, want := range []AuditEvent{
		AuditPairingLinkMinted, AuditPairingRedeemed, AuditPairingConfirmed, AuditPairingRefused,
	} {
		if !seen[string(want)] {
			t.Fatalf("the credential log carries no %q entry: %v", want, seen)
		}
	}
}
