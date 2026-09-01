package identity

import (
	"testing"
	"time"

	"agent-overflow/internal/store"

	"github.com/go-webauthn/webauthn/protocol"
)

// newPasskeyFixture is newFixture with a relying party installed, which
// is what every passkey surface refuses without.
func newPasskeyFixture(t *testing.T) (*Sessions, *store.Store, *clock, store.User) {
	t.Helper()
	sessions, st, c, owner, _ := newFixture(t)
	sessions.SetRelyingParty(testRelyingParty)
	return sessions, st, c, owner
}

// enroll runs a whole registration ceremony and returns the stored row.
func enroll(t *testing.T, s *Sessions, userID string, auth *softAuthenticator) store.Passkey {
	t.Helper()
	challenge, reason := s.BeginPasskeyRegistration(userID, "Phone")
	if reason.Refused() {
		t.Fatalf("BeginPasskeyRegistration: %s", reason)
	}
	row, reason := s.FinishPasskeyRegistration(challenge.CeremonyID, auth.register(t, challenge, testOrigin))
	if reason.Refused() {
		t.Fatalf("FinishPasskeyRegistration: %s", reason)
	}
	return row
}

// signIn runs a whole sign-in ceremony for a device presenting thumbprint.
func signIn(t *testing.T, s *Sessions, auth *softAuthenticator, thumbprint string) (PasskeySignIn, Reason) {
	t.Helper()
	challenge, reason := s.BeginPasskeySignIn()
	if reason.Refused() {
		t.Fatalf("BeginPasskeySignIn: %s", reason)
	}
	return s.FinishPasskeySignIn(PasskeySignInRequest{
		CeremonyID: challenge.CeremonyID,
		Response:   auth.assert(t, challenge, testOrigin),
		Proof:      bearerProof(thumbprint),
		Label:      "Laptop browser",
		Platform:   "linux",
		Peer:       "192.0.2.10:44321",
	})
}

func TestPasskeyRegistrationRecordsWhatTheCeremonyReported(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	auth.backupEligible, auth.backupState = true, true

	row := enroll(t, sessions, owner.ID, auth)

	if row.UserID != owner.ID || row.Label != "Phone" {
		t.Fatalf("the row did not record its account or label: %+v", row)
	}
	if string(row.CredentialID) != string(auth.credentialID) {
		t.Fatal("the row must record the authenticator's own credential id")
	}
	if row.RPID != testRPID {
		t.Fatalf("rpId = %q, want the relying party the ceremony ran under", row.RPID)
	}
	if !row.UserVerified {
		t.Fatal("a ceremony that verified the person must record it")
	}
	if !row.BackupEligible || !row.BackupState {
		t.Fatalf("the synced-credential flags did not survive: %+v", row)
	}
	if len(row.Transports) == 0 {
		t.Fatal("the browser's transport hints must be recorded")
	}
	if row.CloneWarning {
		t.Fatal("a fresh credential has no counter anomaly")
	}
}

// The library will validate the same SessionData twice quite happily, so
// single use is entirely this package's. Everything else in the passkey
// design rests on it: a captured assertion is only worthless because the
// challenge it answers is gone.
func TestAPasskeyChallengeIsAnsweredOnce(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)

	challenge, reason := sessions.BeginPasskeyRegistration(owner.ID, "Phone")
	if reason.Refused() {
		t.Fatalf("BeginPasskeyRegistration: %s", reason)
	}
	response := auth.register(t, challenge, testOrigin)
	if _, reason := sessions.FinishPasskeyRegistration(challenge.CeremonyID, response); reason.Refused() {
		t.Fatalf("first finish: %s", reason)
	}
	_, reason = sessions.FinishPasskeyRegistration(challenge.CeremonyID, response)
	if reason != ReasonPasskeyChallengeUnknown {
		t.Fatalf("a second finish answered %s, want %s", reason, ReasonPasskeyChallengeUnknown)
	}
}

// A finish that FAILS spends the challenge too. Leaving it answerable
// would let a caller try responses against one challenge until one
// verified.
func TestAFailedFinishStillSpendsItsChallenge(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)

	challenge, reason := sessions.BeginPasskeyRegistration(owner.ID, "Phone")
	if reason.Refused() {
		t.Fatalf("BeginPasskeyRegistration: %s", reason)
	}
	if _, reason := sessions.FinishPasskeyRegistration(challenge.CeremonyID, []byte("{")); reason != ReasonPasskeyRefused {
		t.Fatalf("an unreadable response answered %s, want %s", reason, ReasonPasskeyRefused)
	}
	_, reason = sessions.FinishPasskeyRegistration(challenge.CeremonyID, auth.register(t, challenge, testOrigin))
	if reason != ReasonPasskeyChallengeUnknown {
		t.Fatalf("the challenge survived a failed finish: %s", reason)
	}
}

func TestAPasskeyChallengeExpires(t *testing.T) {
	sessions, _, c, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)

	challenge, reason := sessions.BeginPasskeyRegistration(owner.ID, "Phone")
	if reason.Refused() {
		t.Fatalf("BeginPasskeyRegistration: %s", reason)
	}
	response := auth.register(t, challenge, testOrigin)
	c.advance(PasskeyCeremonyTTL + time.Second)

	if _, reason := sessions.FinishPasskeyRegistration(challenge.CeremonyID, response); reason != ReasonPasskeyChallengeUnknown {
		t.Fatalf("an expired challenge answered %s, want %s", reason, ReasonPasskeyChallengeUnknown)
	}
}

// A registration answered at an origin this backend does not serve is
// refused. It is the one property the RP ID cannot carry by itself: an
// authenticator binds to a DOMAIN, and several ports on that domain are
// several origins.
func TestAPasskeyCeremonyIsBoundToItsOrigin(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)

	challenge, reason := sessions.BeginPasskeyRegistration(owner.ID, "Phone")
	if reason.Refused() {
		t.Fatalf("BeginPasskeyRegistration: %s", reason)
	}
	_, reason = sessions.FinishPasskeyRegistration(
		challenge.CeremonyID, auth.register(t, challenge, "http://localhost:9999"))
	if reason != ReasonPasskeyRefused {
		t.Fatalf("a response from another origin answered %s, want %s", reason, ReasonPasskeyRefused)
	}
}

func TestPasskeySignInMintsALiveSessionForANewDevice(t *testing.T) {
	sessions, st, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)

	result, reason := signIn(t, sessions, auth, "thumb-laptop")
	if reason.Refused() {
		t.Fatalf("FinishPasskeySignIn: %s", reason)
	}
	if result.Tokens.AwaitingConfirmation {
		t.Fatal("a passkey assertion is the owner; there is nothing to confirm")
	}
	if result.Tokens.Credential == "" || result.Tokens.RefreshSecret == "" {
		t.Fatalf("the sign-in must issue a renewable pair: %+v", result.Tokens)
	}
	if len(result.Tokens.Scopes) != len(Scopes) {
		t.Fatalf("a passkey sign-in holds full access, got %v", result.Tokens.Scopes)
	}
	if _, reason := sessions.Live(result.Tokens.SessionID); reason.Refused() {
		t.Fatalf("the session must admit calls immediately, got %s", reason)
	}
	device, err := st.GetDevice(result.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if device.Class != string(DeviceBrowser) {
		t.Fatalf("class = %q, want the class this backend decides, not one the request named", device.Class)
	}
	if device.UserID != owner.ID {
		t.Fatalf("the device must belong to the account the credential does, got %s", device.UserID)
	}
}

// Re-authenticating from a browser this backend already knows is the same
// device, and pairing's adoption rule is what says so.
func TestPasskeySignInAdoptsADeviceItAlreadyKnows(t *testing.T) {
	sessions, st, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)

	first, reason := signIn(t, sessions, auth, "thumb-laptop")
	if reason.Refused() {
		t.Fatalf("first sign-in: %s", reason)
	}
	second, reason := signIn(t, sessions, auth, "thumb-laptop")
	if reason.Refused() {
		t.Fatalf("second sign-in: %s", reason)
	}
	if first.DeviceID != second.DeviceID {
		t.Fatalf("re-auth minted a second device row: %s then %s", first.DeviceID, second.DeviceID)
	}
	if first.Tokens.SessionID == second.Tokens.SessionID {
		t.Fatal("re-auth must mint a FRESH session, not hand back the old one")
	}
	devices, err := st.ListDevicesForUser(owner.ID)
	if err != nil {
		t.Fatalf("ListDevicesForUser: %v", err)
	}
	browsers := 0
	for _, device := range devices {
		if device.Class == string(DeviceBrowser) {
			browsers++
		}
	}
	if browsers != 1 {
		t.Fatalf("two sign-ins from one browser left %d device rows", browsers)
	}
}

// The passkey proves the PERSON. The device proof is what a revocation
// reaches, so a sign-in without one would mint a session nothing could
// withdraw.
func TestPasskeySignInRefusesWithoutADeviceProof(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)

	challenge, reason := sessions.BeginPasskeySignIn()
	if reason.Refused() {
		t.Fatalf("BeginPasskeySignIn: %s", reason)
	}
	_, reason = sessions.FinishPasskeySignIn(PasskeySignInRequest{
		CeremonyID: challenge.CeremonyID,
		Response:   auth.assert(t, challenge, testOrigin),
	})
	if reason != ReasonMissingProof {
		t.Fatalf("a sign-in with no device proof answered %s, want %s", reason, ReasonMissingProof)
	}
}

func TestPasskeySignInRefusesARevokedDevice(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)

	first, reason := signIn(t, sessions, auth, "thumb-laptop")
	if reason.Refused() {
		t.Fatalf("first sign-in: %s", reason)
	}
	if _, err := sessions.RevokeDevice(first.DeviceID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, reason := signIn(t, sessions, auth, "thumb-laptop"); reason != ReasonRevokedDevice {
		t.Fatalf("a revoked device signed back in with a passkey: %s", reason)
	}
}

// The counter is evidence, never a verdict: authenticators that keep none
// report zero forever, so refusing on it would sign people out of working
// keys.
func TestAPasskeyCounterThatDoesNotAdvanceIsFlaggedAndAdmitted(t *testing.T) {
	sessions, st, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	row := enroll(t, sessions, owner.ID, auth)

	if _, reason := signIn(t, sessions, auth, "thumb-laptop"); reason.Refused() {
		t.Fatalf("first sign-in: %s", reason)
	}
	auth.freezeCounter = true
	result, reason := signIn(t, sessions, auth, "thumb-laptop")
	if reason.Refused() {
		t.Fatalf("a stalled counter must not refuse the sign-in, got %s", reason)
	}
	if !result.CloneWarning {
		t.Fatal("a stalled counter must be reported to the caller")
	}
	stored, err := st.PasskeyByCredentialID(row.CredentialID)
	if err != nil {
		t.Fatalf("PasskeyByCredentialID: %v", err)
	}
	if !stored.CloneWarning {
		t.Fatal("the anomaly must be persisted, so a list can surface it later")
	}
	if stored.LastUsedAt == 0 {
		t.Fatal("a successful assertion must stamp the row")
	}
}

// The step-up decision reads the ASSERTION's own flags. The stored
// credential's UV bit latches at registration, so a credential enrolled
// with verification would answer yes forever.
func TestStepUpCeremonyDemandsVerificationOfThisTouch(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	device, err := sessions.store.CreatePairedDevice(
		owner.ID, "Laptop", string(DeviceBrowser), "linux", "thumb-laptop", string(ProofBearer))
	if err != nil {
		t.Fatalf("CreatePairedDevice: %v", err)
	}
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)
	session, _ := mustMint(t, sessions, owner, device, time.Hour)

	challenge, reason := sessions.BeginPasskeyStepUp(session.ID)
	if reason.Refused() {
		t.Fatalf("BeginPasskeyStepUp: %s", reason)
	}
	assertionDemandsVerification(t, challenge)

	// The same credential, presented without verifying the person.
	auth.userVerified = false
	if _, reason := sessions.FinishPasskeyStepUp(challenge.CeremonyID, auth.assert(t, challenge, testOrigin), ""); reason != ReasonPasskeyRefused {
		t.Fatalf("a presence-only assertion proved step-up: %s", reason)
	}
}

// The tripwire for the one field whose loss is silent: a ceremony whose
// SessionData no longer demands verification downgrades step-up to
// presence and nothing else changes.
func TestEveryDiscoverableCeremonyRecordsThatVerificationIsRequired(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	device, err := sessions.store.CreatePairedDevice(
		owner.ID, "Laptop", string(DeviceBrowser), "linux", "thumb-laptop", string(ProofBearer))
	if err != nil {
		t.Fatalf("CreatePairedDevice: %v", err)
	}
	session, _ := mustMint(t, sessions, owner, device, time.Hour)

	signInChallenge, reason := sessions.BeginPasskeySignIn()
	if reason.Refused() {
		t.Fatalf("BeginPasskeySignIn: %s", reason)
	}
	stepUpChallenge, reason := sessions.BeginPasskeyStepUp(session.ID)
	if reason.Refused() {
		t.Fatalf("BeginPasskeyStepUp: %s", reason)
	}
	for name, id := range map[string]string{
		"sign-in": signInChallenge.CeremonyID,
		"step-up": stepUpChallenge.CeremonyID,
	} {
		sessions.passkeyMu.Lock()
		ceremony := sessions.ceremonies[id]
		sessions.passkeyMu.Unlock()
		if ceremony.session.UserVerification != protocol.VerificationRequired {
			t.Fatalf("the %s ceremony recorded userVerification=%q, want %q — the library reads THIS "+
				"field at finish, so a lost value is a silent downgrade to presence-only",
				name, ceremony.session.UserVerification, protocol.VerificationRequired)
		}
	}
}

func TestStepUpTokenIsSpentOnceAndBoundToItsSession(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	device, err := sessions.store.CreatePairedDevice(
		owner.ID, "Laptop", string(DeviceBrowser), "linux", "thumb-laptop", string(ProofBearer))
	if err != nil {
		t.Fatalf("CreatePairedDevice: %v", err)
	}
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)
	session, _ := mustMint(t, sessions, owner, device, time.Hour)
	other, _ := mustMint(t, sessions, owner, device, time.Hour)

	challenge, reason := sessions.BeginPasskeyStepUp(session.ID)
	if reason.Refused() {
		t.Fatalf("BeginPasskeyStepUp: %s", reason)
	}
	grant, reason := sessions.FinishPasskeyStepUp(challenge.CeremonyID, auth.assert(t, challenge, testOrigin), "")
	if reason.Refused() {
		t.Fatalf("FinishPasskeyStepUp: %s", reason)
	}
	if sessions.SpendStepUpToken(other.ID, grant.Token) {
		t.Fatal("a token proved step-up for a session that did not earn it")
	}
	if sessions.SpendStepUpToken(session.ID, grant.Token) {
		t.Fatal("a wrong-session attempt must consume the token, so ids cannot be probed")
	}
}

func TestStepUpTokenProvesOneCall(t *testing.T) {
	sessions, _, c, owner := newPasskeyFixture(t)
	device, err := sessions.store.CreatePairedDevice(
		owner.ID, "Laptop", string(DeviceBrowser), "linux", "thumb-laptop", string(ProofBearer))
	if err != nil {
		t.Fatalf("CreatePairedDevice: %v", err)
	}
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)
	session, _ := mustMint(t, sessions, owner, device, 24*time.Hour)

	prove := func() StepUpGrant {
		t.Helper()
		challenge, reason := sessions.BeginPasskeyStepUp(session.ID)
		if reason.Refused() {
			t.Fatalf("BeginPasskeyStepUp: %s", reason)
		}
		grant, reason := sessions.FinishPasskeyStepUp(challenge.CeremonyID, auth.assert(t, challenge, testOrigin), "")
		if reason.Refused() {
			t.Fatalf("FinishPasskeyStepUp: %s", reason)
		}
		return grant
	}

	first := prove()
	if !sessions.SpendStepUpToken(session.ID, first.Token) {
		t.Fatal("a fresh token must prove step-up")
	}
	if sessions.SpendStepUpToken(session.ID, first.Token) {
		t.Fatal("a token that survived its call would be standing elevation")
	}

	second := prove()
	c.advance(StepUpTokenTTL + time.Second)
	if sessions.SpendStepUpToken(session.ID, second.Token) {
		t.Fatal("an expired token must prove nothing")
	}
}

// A challenge minted for one ceremony must not be finishable as another:
// a sign-in and a step-up produce very different things from the same
// assertion shape.
func TestAPasskeyChallengeCannotChangeItsPurpose(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	device, err := sessions.store.CreatePairedDevice(
		owner.ID, "Laptop", string(DeviceBrowser), "linux", "thumb-laptop", string(ProofBearer))
	if err != nil {
		t.Fatalf("CreatePairedDevice: %v", err)
	}
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)
	session, _ := mustMint(t, sessions, owner, device, time.Hour)

	stepUp, reason := sessions.BeginPasskeyStepUp(session.ID)
	if reason.Refused() {
		t.Fatalf("BeginPasskeyStepUp: %s", reason)
	}
	_, reason = sessions.FinishPasskeySignIn(PasskeySignInRequest{
		CeremonyID: stepUp.CeremonyID,
		Response:   auth.assert(t, stepUp, testOrigin),
		Proof:      bearerProof("thumb-laptop"),
	})
	if reason != ReasonPasskeyChallengeUnknown {
		t.Fatalf("a step-up challenge was finished as a sign-in: %s", reason)
	}

	signInChallenge, reason := sessions.BeginPasskeySignIn()
	if reason.Refused() {
		t.Fatalf("BeginPasskeySignIn: %s", reason)
	}
	if _, reason := sessions.FinishPasskeyStepUp(signInChallenge.CeremonyID, auth.assert(t, signInChallenge, testOrigin), ""); reason != ReasonPasskeyChallengeUnknown {
		t.Fatalf("a sign-in challenge was finished as a step-up: %s", reason)
	}
}

// No canonical domain means no RP ID, and a credential cannot be bound to
// an address. Every surface says so rather than guessing a name.
func TestPasskeysAreUnavailableWithoutARelyingParty(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	session, _ := mustMint(t, sessions, owner, device, time.Hour)

	if sessions.PasskeysAvailable() {
		t.Fatal("a backend with no relying party must not offer passkeys")
	}
	if _, reason := sessions.BeginPasskeyRegistration(owner.ID, "Phone"); reason != ReasonPasskeyUnavailable {
		t.Fatalf("registration answered %s, want %s", reason, ReasonPasskeyUnavailable)
	}
	if _, reason := sessions.BeginPasskeySignIn(); reason != ReasonPasskeyUnavailable {
		t.Fatalf("sign-in answered %s, want %s", reason, ReasonPasskeyUnavailable)
	}
	if _, reason := sessions.BeginPasskeyStepUp(session.ID); reason != ReasonPasskeyUnavailable {
		t.Fatalf("step-up answered %s, want %s", reason, ReasonPasskeyUnavailable)
	}

	sessions.SetRelyingParty(func() RelyingParty { return RelyingParty{ID: "backend.example"} })
	if sessions.PasskeysAvailable() {
		t.Fatal("a relying party naming no origin can verify nothing and must not be offered")
	}
}

// A credential registered under a domain the owner has since changed is
// still LISTED, and can no longer assert. Both halves matter: the person
// needs to see the row to delete it, and the ceremony must not verify an
// assertion bound to a name this backend no longer answers to.
func TestAPasskeyOutlivesItsDomainAsAListingAndNotAsACredential(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)

	moved := RelyingParty{
		ID: "backend.example", DisplayName: testRPLabel,
		Origins: []string{"https://backend.example"},
	}
	sessions.SetRelyingParty(func() RelyingParty { return moved })

	listed, err := sessions.ListPasskeys(owner.ID)
	if err != nil {
		t.Fatalf("ListPasskeys: %v", err)
	}
	if len(listed) != 1 || listed[0].RPID != testRPID {
		t.Fatalf("the stale credential must still be listed with its own RP ID: %+v", listed)
	}

	challenge, reason := sessions.BeginPasskeySignIn()
	if reason.Refused() {
		t.Fatalf("BeginPasskeySignIn: %s", reason)
	}
	// The authenticator still holds the old RP ID, which is exactly what
	// makes the credential dead: it will only ever answer for that name.
	_, reason = sessions.FinishPasskeySignIn(PasskeySignInRequest{
		CeremonyID: challenge.CeremonyID,
		Response:   auth.assert(t, challenge, "https://backend.example"),
		Proof:      bearerProof("thumb-laptop"),
	})
	if reason != ReasonPasskeyRefused {
		t.Fatalf("a credential bound to another domain signed in: %s", reason)
	}
}

// Deleting a credential removes a way to sign in again. It does not end a
// session that credential already minted — that is what revocation is
// for, and the surface's wording says so.
func TestDeletingAPasskeyEndsNoSession(t *testing.T) {
	sessions, _, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	row := enroll(t, sessions, owner.ID, auth)

	result, reason := signIn(t, sessions, auth, "thumb-laptop")
	if reason.Refused() {
		t.Fatalf("sign-in: %s", reason)
	}
	removed, err := sessions.DeletePasskey(owner.ID, row.ID)
	if err != nil || !removed {
		t.Fatalf("DeletePasskey: %v %v", removed, err)
	}
	if _, reason := sessions.Live(result.Tokens.SessionID); reason.Refused() {
		t.Fatalf("deleting a credential ended a session it had already minted: %s", reason)
	}
	removed, err = sessions.DeletePasskey(owner.ID, row.ID)
	if err != nil || removed {
		t.Fatalf("a second delete is a no-op, not an error: %v %v", removed, err)
	}
}

// The book is bounded, and it drops the oldest entry rather than refusing
// a begin: refusing would let a flood of begins lock the owner out of
// their own sign-in, which is worse than the flood.
func TestTheCeremonyBookIsBoundedAndKeepsTheNewest(t *testing.T) {
	sessions, _, _, _ := newPasskeyFixture(t)

	var newest PasskeyChallenge
	for range passkeyCeremonyLimit * 3 {
		challenge, reason := sessions.BeginPasskeySignIn()
		if reason.Refused() {
			t.Fatalf("BeginPasskeySignIn: %s", reason)
		}
		newest = challenge
	}
	sessions.passkeyMu.Lock()
	held := len(sessions.ceremonies)
	_, keptNewest := sessions.ceremonies[newest.CeremonyID]
	sessions.passkeyMu.Unlock()

	if held > passkeyCeremonyLimit {
		t.Fatalf("the book held %d challenges, want at most %d", held, passkeyCeremonyLimit)
	}
	if !keptNewest {
		t.Fatal("the cap dropped the challenge somebody is about to answer")
	}
}

// The account's handle is minted once and never re-minted, because it is
// what an authenticator stored alongside the credential: a second one
// would make every existing credential resolve to nothing.
func TestTheWebAuthnHandleIsMintedOncePerAccount(t *testing.T) {
	sessions, st, _, owner := newPasskeyFixture(t)
	auth := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, auth)

	first := auth.userHandle
	if len(first) != webAuthnUserHandleBytes {
		t.Fatalf("handle is %d bytes, want %d", len(first), webAuthnUserHandleBytes)
	}
	second := newSoftAuthenticator(t)
	enroll(t, sessions, owner.ID, second)
	if string(second.userHandle) != string(first) {
		t.Fatal("a second registration must enroll against the account's existing handle")
	}
	resolved, err := st.UserByWebAuthnHandle(first)
	if err != nil || resolved.ID != owner.ID {
		t.Fatalf("the handle must resolve its account: %v %v", resolved.ID, err)
	}
}
