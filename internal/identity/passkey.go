package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/store"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// Passkeys (docs/specs/remote-access.md §4, "Passkeys").
//
// Pairing bootstraps; a passkey hardens. Everything pairing does keeps
// working, and nothing here is on the path of a device that never
// registers one. What a registered credential buys is three things that
// pairing cannot do on its own:
//
//  1. a browser this backend has never seen signs in with no code to type
//     and no second screen to read a number off;
//  2. a browser whose session family ended re-authenticates itself instead
//     of waiting for somebody to walk to the machine;
//  3. a REMOTE owner satisfies step-up. Until this file the only step-up
//     proof was host presence, so the catastrophic call set was reachable
//     only from the machine itself.
//
// # What the ceremonies are, and what this package owns of them
//
// Three ceremonies, all built out of the same two library calls:
//
//   - REGISTRATION runs only from an already-authenticated surface. It is
//     not a way in; it is a way to make the next way in stronger.
//   - SIGN-IN is an ordinary discoverable ("passkey") login. A valid
//     assertion IS owner presence, so it mints a device row and a session
//     through the same chokepoints pairing uses — Mint and issueFor, never
//     store.CreateSession directly.
//   - STEP-UP is the same discoverable login with user verification
//     REQUIRED, and it mints no session at all. It produces a single-use,
//     short-lived token bound to the session that asked, which the
//     transport's per-call gate spends.
//
// The library owns the cryptography and the WebAuthn state machine. What
// it deliberately does NOT own, and this file therefore must:
//
//   - SINGLE USE of a challenge. `SessionData` replay is accepted by the
//     library: hand it the same session twice and it will verify the same
//     assertion twice. The book below is the single-use half, and it
//     deletes an entry on the FIRST finish attempt, success or failure.
//   - EXPIRY. The library's default timeout config stamps a zero
//     `Expires` and performs no check at all, so `Timeouts.*.Enforce` is
//     set on every configuration this file builds, and the book expires
//     entries on its own besides.
//   - The RELYING PARTY. `SessionData.RelyingPartyID` and `Origin`
//     override the configuration at finish, so a value a request could
//     influence would be a request choosing what it is verified against.
//     Neither is ever set from a ceremony response: the whole relying
//     party is resolved once at BEGIN and pinned in the book beside the
//     challenge, and the finish rebuilds its configuration from that
//     record.
//
// # A passkey is not a device
//
// One synced credential appears on every phone a person owns. The
// credential rows therefore hang off the ACCOUNT (store migration v82),
// and the DEVICE a sign-in produces is resolved separately, from the
// device-key proof the finish carries — the same enrollment rules pairing
// redemption runs, reached through the same enrollmentFor.

const (
	// webAuthnUserHandleBytes is the entropy in an account's WebAuthn user
	// handle. The library accepts 1..64; 32 is the same budget every other
	// opaque identifier in this package draws, and the handle is what a
	// discoverable assertion returns INSTEAD of asking who is signing in,
	// so it must not be guessable from anything a person knows.
	webAuthnUserHandleBytes = 32

	// passkeyCeremonyIDBytes is the entropy in a ceremony id. It is the
	// only thing a finish presents to name its challenge, so it is drawn
	// with the same budget as a pairing token rather than being a uuid.
	passkeyCeremonyIDBytes = 32

	// stepUpTokenBytes is the entropy in a step-up token.
	stepUpTokenBytes = 32

	// PasskeyCeremonyTTL bounds how long a challenge stays answerable. Two
	// minutes is a person picking up a phone and touching a sensor; past it
	// the surface starts another ceremony, which costs nothing.
	PasskeyCeremonyTTL = 2 * time.Minute

	// StepUpTokenTTL bounds a proven step-up. The token exists to carry one
	// call, and its window is the time between the browser finishing the
	// ceremony and the retry landing — not a period of elevated standing.
	StepUpTokenTTL = 2 * time.Minute

	// passkeyCeremonyLimit and stepUpTokenLimit bound the two in-memory
	// books. Both are per-owner state driven by a person touching a sensor,
	// so a real one holds one or two entries; the caps exist so an
	// unauthenticated begin route cannot grow either without bound. At the
	// cap the OLDEST entry is dropped rather than the request refused —
	// refusing would let a flood of begins lock the owner out of their own
	// sign-in, which is the failure this bound must not create.
	//
	// passkeyCeremonyLimit is PER PURPOSE, and eviction only ever considers
	// entries of the same purpose. Sign-in is the one ceremony reachable
	// with no credential at all (/auth/passkey/begin), so a single book
	// would let a run of sign-in begins flush the registration and step-up
	// challenges a session-bound surface had just minted, and the person at
	// that surface would see their touch refused for no reason they could
	// act on. Three purposes therefore bound the whole book at 3x this
	// number, which is still a fixed, tiny ceiling.
	passkeyCeremonyLimit = 16
	stepUpTokenLimit     = 16
)

// RelyingParty is the WebAuthn relying party one ceremony runs under: the
// RP ID an authenticator binds a credential to, the name it shows a
// person, and the origins a response may declare.
//
// Resolved by the boot (internal/app, from the canonical domain in
// Settings → Remote access and the listener's own authority) because only it
// knows what this backend answers to. This package pins whatever it is
// given into the ceremony record and compares against that pinned copy at
// finish, so a domain change mid-ceremony refuses the ceremony rather than
// silently verifying it against a relying party the challenge was never
// issued for.
type RelyingParty struct {
	// ID is the RP ID: a bare domain, never a scheme, a port, or an
	// address. An authenticator binds a credential to this string, so
	// changing it makes every existing credential unusable — which is why
	// a row records the value it was registered under.
	ID string
	// DisplayName is what the authenticator's prompt calls this backend.
	DisplayName string
	// Origins is every origin a page running a ceremony may be loaded
	// from. Compared scheme-and-authority exact (default ports
	// normalized), so a backend reachable at two ports names both.
	Origins []string
}

// Configured reports whether a ceremony can run. A backend with no
// canonical domain has no RP ID, and the surfaces are absent rather than
// broken.
func (rp RelyingParty) Configured() bool {
	return strings.TrimSpace(rp.ID) != "" && len(rp.Origins) > 0
}

// passkeyPurpose is what one ceremony was begun for. It is recorded rather
// than inferred, so a challenge minted for a step-up cannot be finished as
// a sign-in — the two mint very different things from the same assertion
// shape.
type passkeyPurpose uint8

const (
	purposeRegistration passkeyPurpose = iota + 1
	purposeSignIn
	purposeStepUp
)

// passkeyCeremony is one outstanding challenge.
type passkeyCeremony struct {
	purpose passkeyPurpose
	session webauthn.SessionData
	rp      RelyingParty
	// userID is the account a registration enrolls into. Empty for the two
	// discoverable ceremonies, which learn their account from the handle
	// the assertion returns.
	userID string
	// label is what a registration will call the credential.
	label string
	// forSessionID is the durable session a step-up was begun for. The
	// token this ceremony produces is bound to it, so a proof obtained by
	// one session cannot be spent by another.
	forSessionID string
	expiresAt    int64
}

// stepUpGrant is one proven step-up waiting to be spent.
type stepUpGrant struct {
	sessionID string
	expiresAt int64
}

// PasskeyChallenge is what a begin hands the browser: the ceremony id it
// must name at finish, and the options blob to pass to
// `navigator.credentials`.
//
// Options is opaque JSON on purpose. It crosses internal/app and
// internal/transport unread — neither layer has a decision to make about
// it, and a typed copy in either would be a second shape to keep in step
// with a library's.
type PasskeyChallenge struct {
	CeremonyID string          `json:"ceremonyId"`
	Options    json.RawMessage `json:"options"`
}

// PasskeySignIn is what a completed sign-in produced.
type PasskeySignIn struct {
	// DeviceID is the device row the sign-in resolved to: an existing one
	// belonging to the same key, or a new one.
	DeviceID string
	// Tokens is the live credential pair. Unlike a pairing redemption's,
	// this one admits calls immediately — a valid assertion over a
	// registered credential IS the owner, so there is nothing for a second
	// screen to confirm.
	Tokens TokenSet
	// PasskeyID names the credential that signed in, so a surface can say
	// which one was used.
	PasskeyID string
	// CloneWarning is true when the authenticator's counter failed to
	// advance. Reported, never acted on (store migration v82): it is
	// surfaced as an anomaly beside the credential.
	CloneWarning bool
}

// StepUpGrant is a proven step-up: one token, spendable once, on the
// session that asked for it.
type StepUpGrant struct {
	Token           string `json:"token"`
	ExpiresAtMillis int64  `json:"expiresAtMillis"`
}

// SetRelyingParty installs the resolver every ceremony reads its relying
// party from. Called once during boot, like AttachConns. Until it is
// called — and whenever it answers an unconfigured party — every passkey
// surface refuses with ReasonPasskeyUnavailable rather than guessing a
// domain.
//
// A func rather than a value because the canonical domain is a live
// setting: a ceremony begun after the owner changes it must run under the
// new name, and one begun before must not.
func (s *Sessions) SetRelyingParty(resolve func() RelyingParty) {
	s.passkeyMu.Lock()
	defer s.passkeyMu.Unlock()
	s.relyingParty = resolve
}

// PasskeyRelyingParty is the party ceremonies run under right now. Exported
// so a surface can say "passkeys need a canonical domain" instead of
// offering a button that always refuses.
func (s *Sessions) PasskeyRelyingParty() RelyingParty {
	s.passkeyMu.Lock()
	resolve := s.relyingParty
	s.passkeyMu.Unlock()
	if resolve == nil {
		return RelyingParty{}
	}
	return resolve()
}

// PasskeysAvailable reports whether a ceremony could run at all.
func (s *Sessions) PasskeysAvailable() bool {
	return s.PasskeyRelyingParty().Configured()
}

// BeginPasskeyRegistration starts an enrollment for one account.
//
// Host-side Go API, like MintPairingLink: the caller is an authenticated
// admin surface that additionally passed step-up. Registration is not a
// way IN — it is only reachable by somebody who already got in — which is
// what lets the sign-in ceremony trust a credential registered here.
//
// The account's WebAuthn handle is minted here, lazily, on the first
// ceremony it ever runs. Its existing credentials are passed as the
// exclusion list, so an authenticator the owner already enrolled says so
// rather than silently making a second credential nothing distinguishes
// from the first.
func (s *Sessions) BeginPasskeyRegistration(userID, label string) (PasskeyChallenge, Reason) {
	rp := s.PasskeyRelyingParty()
	if !rp.Configured() {
		return PasskeyChallenge{}, ReasonPasskeyUnavailable
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "Passkey"
	}
	user, err := s.passkeyUser(userID)
	if err != nil {
		log.Printf("identity: resolve passkey user %s: %v", userID, err)
		return PasskeyChallenge{}, ReasonPasskeyUnavailable
	}
	auth, err := webAuthnFor(rp)
	if err != nil {
		log.Printf("identity: build relying party %q: %v", rp.ID, err)
		return PasskeyChallenge{}, ReasonPasskeyUnavailable
	}
	creation, session, err := auth.BeginRegistration(user,
		webauthn.WithExclusions(user.descriptors()),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			// Discoverable, because sign-in names no account: the browser
			// has nothing to look a credential up by, so a credential that
			// is not client-side discoverable could never be offered.
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
	)
	if err != nil {
		return PasskeyChallenge{}, s.refusePasskey(err, "registration begin", "")
	}
	return s.recordCeremony(passkeyCeremony{
		purpose: purposeRegistration,
		session: *session,
		rp:      rp,
		userID:  userID,
		label:   label,
	}, creation.Response)
}

// FinishPasskeyRegistration verifies an enrollment response and records
// the credential.
//
// The account is the one the BEGIN named, never one the response could
// claim: a registration response carries no user handle of its own that
// this backend has any reason to believe.
func (s *Sessions) FinishPasskeyRegistration(ceremonyID string, response []byte) (store.Passkey, Reason) {
	ceremony, ok := s.takeCeremony(ceremonyID, purposeRegistration)
	if !ok {
		return store.Passkey{}, ReasonPasskeyChallengeUnknown
	}
	auth, err := webAuthnFor(ceremony.rp)
	if err != nil {
		log.Printf("identity: rebuild relying party %q: %v", ceremony.rp.ID, err)
		return store.Passkey{}, ReasonPasskeyUnavailable
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return store.Passkey{}, s.refusePasskey(err, "registration parse", ceremony.userID)
	}
	user, err := s.passkeyUser(ceremony.userID)
	if err != nil {
		log.Printf("identity: resolve passkey user %s: %v", ceremony.userID, err)
		return store.Passkey{}, ReasonPasskeyUnavailable
	}
	credential, err := auth.CreateCredential(user, ceremony.session, parsed)
	if err != nil {
		return store.Passkey{}, s.refusePasskey(err, "registration finish", ceremony.userID)
	}
	row := store.Passkey{
		ID:                uuid.NewString(),
		UserID:            ceremony.userID,
		Label:             ceremony.label,
		CredentialID:      credential.ID,
		PublicKey:         credential.PublicKey,
		AttestationType:   credential.AttestationType,
		AttestationFormat: credential.AttestationFormat,
		Transports:        transportStrings(credential.Transport),
		AAGUID:            credential.Authenticator.AAGUID,
		Attachment:        string(credential.Authenticator.Attachment),
		RPID:              ceremony.rp.ID,
		SignCount:         credential.Authenticator.SignCount,
		CloneWarning:      credential.Authenticator.CloneWarning,
		// The enrollment facts, read from THIS ceremony's own flags rather
		// than from the credential record the library latches them into.
		UserVerified:   parsed.Response.AttestationObject.AuthData.Flags.HasUserVerified(),
		BackupEligible: credential.Flags.BackupEligible,
		BackupState:    credential.Flags.BackupState,
		CreatedAt:      s.now().UnixMilli(),
	}
	if err := s.store.CreatePasskey(row); err != nil {
		log.Printf("identity: record passkey for %s: %v", ceremony.userID, err)
		return store.Passkey{}, ReasonPasskeyRefused
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPasskeyRegistered), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: ceremony.userID, Detail: fmt.Sprintf("%s (%s)", row.Label, row.RPID),
	})
	return row, ReasonNone
}

// ListPasskeys returns one account's credentials, oldest first, including
// any registered under an RP ID this backend no longer answers to. Such a
// credential can never assert again and the surface says so; hiding it
// would leave a person unable to remove something their authenticator
// still offers.
func (s *Sessions) ListPasskeys(userID string) ([]store.Passkey, error) {
	return s.store.ListPasskeysForUser(userID)
}

// DeletePasskey removes one credential. Reports whether a row was there.
//
// It ends no session. A session minted by a passkey sign-in is an ordinary
// session on an ordinary device row, and the way to end one is to revoke
// it or its device — the same answer for every other session this backend
// ever minted. Removing the credential removes a way to sign in AGAIN,
// which is what the surface says.
func (s *Sessions) DeletePasskey(userID, passkeyID string) (bool, error) {
	removed, err := s.store.DeletePasskey(userID, passkeyID)
	if err != nil {
		return false, err
	}
	if removed {
		s.audit(store.AuthAuditEntry{
			Event: string(AuditPasskeyRemoved), Outcome: store.AuthAuditOutcomeAllowed,
			UserID: userID, Detail: passkeyID,
		})
	}
	return removed, nil
}

// BeginPasskeySignIn starts a discoverable login. Unauthenticated: it is
// spoken by a browser that holds no credential yet, which is the whole
// point of it.
//
// It names no account and offers no credential list, so the challenge
// discloses nothing about who this backend knows — the browser asks the
// platform which credentials it holds for this RP ID and the person picks
// one.
func (s *Sessions) BeginPasskeySignIn() (PasskeyChallenge, Reason) {
	return s.beginDiscoverable(purposeSignIn, "")
}

// PasskeySignInRequest is what a browser presents to finish a sign-in.
type PasskeySignInRequest struct {
	// CeremonyID names the challenge this response answers.
	CeremonyID string
	// Response is the raw assertion JSON the browser produced.
	Response []byte
	// Proof is the device-key proof, exactly as pairing redemption takes
	// one, and it is REQUIRED for the same reason: the passkey proves the
	// PERSON, and the device row is what a revocation reaches. A session
	// with no device row is one nothing can withdraw.
	Proof DeviceProof
	// Label and Platform are what the browser calls itself. Presentation
	// only, like pairing's.
	Label    string
	Platform string
	// Peer is the source address, for audit attribution.
	Peer string
}

// FinishPasskeySignIn verifies an assertion and mints a live session for
// the device that presented it.
//
// The order is the contract, and each step refuses before the next can
// cost anything:
//
//  1. the ceremony exists and is unspent (the book, which deletes it here
//     whatever happens next);
//  2. the device-key proof resolves an enrollment — verified BEFORE the
//     assertion, so a client-side signing bug is not reported as a passkey
//     failure;
//  3. the assertion verifies against a credential this backend holds, and
//     the account is the one that credential belongs to;
//  4. the device row is resolved or created, on pairing's adoption rules;
//  5. only then is a session minted, through Mint and issueFor.
//
// A valid assertion mints a LIVE session — no confirmation step. Pairing
// needs one because a link is a secret somebody could have read over a
// shoulder; a passkey assertion is a signature by a key the owner
// registered from an already-authenticated surface, and there is nothing
// a second screen could add to it.
func (s *Sessions) FinishPasskeySignIn(req PasskeySignInRequest) (PasskeySignIn, Reason) {
	ceremony, ok := s.takeCeremony(req.CeremonyID, purposeSignIn)
	if !ok {
		return PasskeySignIn{}, ReasonPasskeyChallengeUnknown
	}
	if req.Proof.Value == "" {
		s.auditPasskeyRefusal(ReasonMissingProof, req.Peer, "")
		return PasskeySignIn{}, ReasonMissingProof
	}
	enrollment, reason := s.enrollmentFor(req.Proof)
	if reason.Refused() {
		s.auditPasskeyRefusal(reason, req.Peer, "")
		return PasskeySignIn{}, reason
	}
	verified, reason := s.validateAssertion(ceremony, req.Response, req.Peer)
	if reason.Refused() {
		return PasskeySignIn{}, reason
	}
	device, reason := s.resolvePasskeyDevice(verified.userID, req, enrollment)
	if reason.Refused() {
		s.auditPasskeyRefusal(reason, req.Peer, verified.row.ID)
		return PasskeySignIn{}, reason
	}
	now := s.now().UnixMilli()
	policy := PolicyFor(DeviceClass(device.Class), BindingDeviceBound)
	session, _, err := s.Mint(MintRequest{
		UserID:       verified.userID,
		DeviceID:     device.ID,
		BindingClass: BindingDeviceBound,
		// Full access. A registered passkey is the owner's own credential,
		// enrolled from a surface that already held admin, so the grant is
		// what pairing's `full` level gives — narrowing a device is still a
		// pairing link, which is where that choice is offered.
		Scopes: Scopes,
		TTL:    policy.Access,
	})
	if err != nil {
		log.Printf("identity: mint passkey session for %s: %v", device.ID, err)
		return PasskeySignIn{}, ReasonUnknownCredential
	}
	// Mint's own credential is discarded for the same reason pairing
	// discards it: issueFor is the ONE TokenSet builder, and a device must
	// never hold an access credential whose renewal row does not exist.
	tokens, err := s.issueFor(session, device, now)
	if err != nil {
		log.Printf("identity: issue passkey session %s: %v", session.ID, err)
		if _, revokeErr := s.RevokeSession(session.ID); revokeErr != nil {
			log.Printf("identity: revoke unissued passkey session %s: %v", session.ID, revokeErr)
		}
		return PasskeySignIn{}, ReasonUnknownCredential
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPasskeySignedIn), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: verified.userID, DeviceID: device.ID, SessionID: session.ID, Peer: req.Peer,
		Detail: verified.row.Label,
	})
	return PasskeySignIn{
		DeviceID:     device.ID,
		Tokens:       tokens,
		PasskeyID:    verified.row.ID,
		CloneWarning: verified.cloneWarning,
	}, ReasonNone
}

// BeginPasskeyStepUp starts the ceremony that proves a person is at the
// keyboard right now, for the session that asked.
//
// The session id is recorded on the ceremony so the token it produces can
// be bound to it. A caller passing a session it does not own gains
// nothing: the token is then spendable only by that other session, which
// is not the one making the call.
func (s *Sessions) BeginPasskeyStepUp(sessionID string) (PasskeyChallenge, Reason) {
	if sessionID == "" {
		return PasskeyChallenge{}, ReasonUnknownSession
	}
	if _, reason := s.Live(sessionID); reason.Refused() {
		return PasskeyChallenge{}, reason
	}
	return s.beginDiscoverable(purposeStepUp, sessionID)
}

// FinishPasskeyStepUp verifies a step-up assertion and mints the token the
// per-call gate spends.
//
// Two properties beyond an ordinary login, and both are checked HERE
// rather than trusted from the ceremony:
//
//   - the assertion's OWN flags must carry user verification. The stored
//     credential's flags latch (the library's Credential.Flags.Update
//     never clears UV once set), so reading the record would answer a
//     question about the enrollment rather than about this touch.
//   - the credential must belong to the account the asking session belongs
//     to. A discoverable login names whoever holds a credential; step-up
//     is an elevation of ONE session, so a different account's passkey
//     proves nothing about it.
func (s *Sessions) FinishPasskeyStepUp(ceremonyID string, response []byte, peer string) (StepUpGrant, Reason) {
	ceremony, ok := s.takeCeremony(ceremonyID, purposeStepUp)
	if !ok {
		return StepUpGrant{}, ReasonPasskeyChallengeUnknown
	}
	session, reason := s.Live(ceremony.forSessionID)
	if reason.Refused() {
		return StepUpGrant{}, reason
	}
	verified, reason := s.validateAssertion(ceremony, response, peer)
	if reason.Refused() {
		return StepUpGrant{}, reason
	}
	if !verified.userVerified {
		// Unreachable while the ceremony is begun with verification
		// required — the library refuses first. It is checked anyway
		// because this is the one decision in the file that a silently
		// downgraded ceremony would turn into presence-only, and the cost
		// of the check is one bit.
		s.auditPasskeyRefusal(ReasonPasskeyRefused, peer, verified.row.ID)
		return StepUpGrant{}, ReasonPasskeyRefused
	}
	if verified.userID != session.UserID {
		s.auditPasskeyRefusal(ReasonKeyMismatch, peer, verified.row.ID)
		return StepUpGrant{}, ReasonKeyMismatch
	}
	token, err := newOpaqueSecret(stepUpTokenBytes)
	if err != nil {
		log.Printf("identity: draw step-up token: %v", err)
		return StepUpGrant{}, ReasonPasskeyRefused
	}
	expiresAt := s.now().Add(StepUpTokenTTL).UnixMilli()
	s.recordStepUp(token, stepUpGrant{sessionID: session.ID, expiresAt: expiresAt})
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPasskeyStepUp), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: session.UserID, DeviceID: session.DeviceID, SessionID: session.ID, Peer: peer,
		Detail: verified.row.Label,
	})
	return StepUpGrant{Token: token, ExpiresAtMillis: expiresAt}, ReasonNone
}

// SpendStepUpToken is the per-call gate's half: it reports whether this
// token proves step-up for this session, and consumes it either way.
//
// Single use is the whole property. A token that survived its call would
// be a standing elevation, which is exactly what step-up exists to refuse
// — the spec's argument is that a grant cannot supply this proof, and a
// reusable token would be a grant.
//
// Consuming on a FAILED match too is deliberate: the alternative leaves a
// token spendable after a wrong-session attempt, so a caller could probe
// session ids until one worked.
func (s *Sessions) SpendStepUpToken(sessionID, token string) bool {
	if sessionID == "" || token == "" {
		return false
	}
	s.passkeyMu.Lock()
	grant, ok := s.stepUps[token]
	delete(s.stepUps, token)
	s.passkeyMu.Unlock()
	if !ok {
		return false
	}
	if grant.expiresAt <= s.now().UnixMilli() {
		return false
	}
	// Constant time on the id, because this is the one comparison an
	// off-host caller can drive with a value of its own choosing.
	return subtle.ConstantTimeCompare([]byte(grant.sessionID), []byte(sessionID)) == 1
}

// beginDiscoverable is the shared body of the two discoverable ceremonies.
// They differ only in what a finish MINTS, so the challenge they issue is
// the same one: user verification required, no credential list, no account
// named.
func (s *Sessions) beginDiscoverable(purpose passkeyPurpose, forSessionID string) (PasskeyChallenge, Reason) {
	rp := s.PasskeyRelyingParty()
	if !rp.Configured() {
		return PasskeyChallenge{}, ReasonPasskeyUnavailable
	}
	auth, err := webAuthnFor(rp)
	if err != nil {
		log.Printf("identity: build relying party %q: %v", rp.ID, err)
		return PasskeyChallenge{}, ReasonPasskeyUnavailable
	}
	assertion, session, err := auth.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return PasskeyChallenge{}, s.refusePasskey(err, "login begin", "")
	}
	return s.recordCeremony(passkeyCeremony{
		purpose:      purpose,
		session:      *session,
		rp:           rp,
		forSessionID: forSessionID,
	}, assertion.Response)
}

// verifiedAssertion is what a discoverable login resolved to.
type verifiedAssertion struct {
	userID string
	row    store.Passkey
	// userVerified is read from THIS assertion's flags, never from the
	// credential record, whose UV bit latches at registration.
	userVerified bool
	cloneWarning bool
}

// validateAssertion runs the library's discoverable-login validation and
// writes back what the assertion reported about the authenticator.
//
// The user handler resolves the ACCOUNT from the handle the assertion
// returned, then hands the library that account's credentials — which is
// what makes "the credential must belong to the handle's owner" a fact the
// library checks rather than one this file has to remember to.
func (s *Sessions) validateAssertion(ceremony passkeyCeremony, response []byte, peer string) (verifiedAssertion, Reason) {
	auth, err := webAuthnFor(ceremony.rp)
	if err != nil {
		log.Printf("identity: rebuild relying party %q: %v", ceremony.rp.ID, err)
		return verifiedAssertion{}, ReasonPasskeyUnavailable
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return verifiedAssertion{}, s.refusePasskeyAt(err, "assertion parse", peer)
	}
	var resolved *passkeyAccount
	handler := func(_, handle []byte) (webauthn.User, error) {
		user, err := s.store.UserByWebAuthnHandle(handle)
		if err != nil {
			return nil, err
		}
		account, err := s.passkeyUser(user.ID)
		if err != nil {
			return nil, err
		}
		resolved = account
		return account, nil
	}
	_, credential, err := auth.ValidatePasskeyLogin(handler, ceremony.session, parsed)
	if err != nil {
		return verifiedAssertion{}, s.refusePasskeyAt(err, "assertion finish", peer)
	}
	if resolved == nil {
		// Not reachable through the library, which refuses a nil user
		// before it validates anything. Stated rather than assumed,
		// because the next line dereferences it.
		return verifiedAssertion{}, ReasonPasskeyRefused
	}
	row, err := s.store.PasskeyByCredentialID(credential.ID)
	if err != nil {
		log.Printf("identity: read passkey by credential id: %v", err)
		return verifiedAssertion{}, ReasonPasskeyRefused
	}
	flags := parsed.Response.AuthenticatorData.Flags
	if err := s.store.RecordPasskeyAssertion(row.ID,
		credential.Authenticator.SignCount, credential.Authenticator.CloneWarning,
		flags.HasBackupState(), s.now().UnixMilli()); err != nil {
		// The assertion was valid; failing the sign-in over a bookkeeping
		// write would turn a disk problem into a lockout. Logged, and the
		// counter is re-read from the next assertion anyway.
		log.Printf("identity: record passkey assertion %s: %v", row.ID, err)
	}
	if credential.Authenticator.CloneWarning {
		log.Printf("identity: passkey %s reported a counter that did not advance (surfaced, not refused)", row.ID)
	}
	return verifiedAssertion{
		userID:       resolved.id,
		row:          row,
		userVerified: flags.HasUserVerified(),
		cloneWarning: credential.Authenticator.CloneWarning,
	}, ReasonNone
}

// resolvePasskeyDevice finds or creates the device row a sign-in binds to.
//
// The rules are pairing's, deliberately: this is the second path that
// writes a device row, and two paths with different adoption rules would
// be two answers to "is this the same phone". resolveRedeemingDevice
// cannot be shared directly because it takes a pairing link — what it
// reads off one is the user id and the device class, which a passkey
// sign-in decides here instead.
func (s *Sessions) resolvePasskeyDevice(userID string, req PasskeySignInRequest, enrollment deviceEnrollment) (store.Device, Reason) {
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = defaultDeviceLabel(DeviceBrowser)
	}
	existing, err := s.store.DeviceByKeyThumbprint(enrollment.thumbprint)
	switch {
	case err == nil:
		if proofKindOf(existing) != enrollment.kind {
			return store.Device{}, ReasonProofDowngraded
		}
		if existing.RevokedAt != 0 {
			return store.Device{}, ReasonRevokedDevice
		}
		if existing.UserID != userID {
			return store.Device{}, ReasonKeyMismatch
		}
		if err := s.store.RelabelDevice(existing.ID, label, req.Platform); err != nil {
			log.Printf("identity: relabel device %s: %v", existing.ID, err)
		}
		existing.Label, existing.Platform = label, req.Platform
		return existing, ReasonNone
	case !errors.Is(err, sql.ErrNoRows):
		log.Printf("identity: read device by thumbprint: %v", err)
		return store.Device{}, ReasonKeyMismatch
	}
	// The class is decided here and not by the request, on pairing's rule:
	// a device that could name its own class could name one whose policy it
	// was never meant to inherit. Only a browser can run a WebAuthn
	// ceremony against this backend today, and the browser class is the one
	// with the shortest windows — the safe answer if that ever stops being
	// true.
	device, err := s.store.CreatePairedDevice(
		userID, label, string(DeviceBrowser), req.Platform,
		enrollment.thumbprint, string(enrollment.kind))
	if err != nil {
		log.Printf("identity: create passkey device: %v", err)
		return store.Device{}, ReasonKeyMismatch
	}
	return device, ReasonNone
}

// passkeyAccount adapts an account plus its credentials to the library's
// User interface.
type passkeyAccount struct {
	id          string
	handle      []byte
	name        string
	credentials []webauthn.Credential
}

func (u *passkeyAccount) WebAuthnID() []byte                         { return u.handle }
func (u *passkeyAccount) WebAuthnName() string                       { return u.name }
func (u *passkeyAccount) WebAuthnDisplayName() string                { return u.name }
func (u *passkeyAccount) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// descriptors renders the account's credentials as an exclusion list, so a
// registration on an authenticator that already holds one says so.
func (u *passkeyAccount) descriptors() []protocol.CredentialDescriptor {
	out := make([]protocol.CredentialDescriptor, 0, len(u.credentials))
	for _, credential := range u.credentials {
		out = append(out, credential.Descriptor())
	}
	return out
}

// passkeyUser loads an account and its credentials, minting the WebAuthn
// handle on first use.
//
// The handle is minted lazily rather than at account creation because a
// handle no authenticator has ever seen is 32 bytes of nothing, and
// backfilling one for every account would hand every row a value that can
// never be presented.
func (s *Sessions) passkeyUser(userID string) (*passkeyAccount, error) {
	user, err := s.store.GetUser(userID)
	if err != nil {
		return nil, err
	}
	handle, err := s.store.EnsureUserWebAuthnHandle(userID, mustHandle())
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListPasskeysForUser(userID)
	if err != nil {
		return nil, err
	}
	credentials := make([]webauthn.Credential, 0, len(rows))
	for _, row := range rows {
		credentials = append(credentials, credentialFor(row))
	}
	name := strings.TrimSpace(user.DisplayName)
	if name == "" {
		name = "Owner"
	}
	return &passkeyAccount{id: user.ID, handle: handle, name: name, credentials: credentials}, nil
}

// credentialFor rebuilds the library's credential record from a stored row.
//
// UserPresent is true because a credential only exists if a person touched
// the authenticator to create it. The other three flags are the persisted
// ones, and BackupEligible in particular MUST round-trip exactly: the
// library refuses an assertion whose eligibility differs from the record,
// which is the check that makes a synced credential's identity stable.
func credentialFor(row store.Passkey) webauthn.Credential {
	transports := make([]protocol.AuthenticatorTransport, 0, len(row.Transports))
	for _, transport := range row.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(transport))
	}
	return webauthn.Credential{
		ID:                row.CredentialID,
		PublicKey:         row.PublicKey,
		AttestationType:   row.AttestationType,
		AttestationFormat: row.AttestationFormat,
		Transport:         transports,
		Flags: webauthn.CredentialFlags{
			UserPresent:    true,
			UserVerified:   row.UserVerified,
			BackupEligible: row.BackupEligible,
			BackupState:    row.BackupState,
		},
		Authenticator: webauthn.Authenticator{
			AAGUID:       row.AAGUID,
			SignCount:    row.SignCount,
			CloneWarning: row.CloneWarning,
			Attachment:   protocol.AuthenticatorAttachment(row.Attachment),
		},
	}
}

func transportStrings(transports []protocol.AuthenticatorTransport) []string {
	out := make([]string, 0, len(transports))
	for _, transport := range transports {
		if transport == "" {
			continue
		}
		out = append(out, string(transport))
	}
	return out
}

// webAuthnFor builds one ceremony's library configuration.
//
// Rebuilt per ceremony rather than cached, because the relying party is a
// live setting and a cached one would run a ceremony under a name the
// owner already changed. The cost is a struct and a validation pass.
//
// Timeouts.Enforce is set on BOTH halves. The library's default stamps a
// zero Expires and performs no check at all, so without this a challenge
// would be answerable forever as far as the library is concerned — the
// book expires it too, and both are wanted: one bounds the memory, the
// other bounds what the cryptography will accept.
func webAuthnFor(rp RelyingParty) (*webauthn.WebAuthn, error) {
	name := strings.TrimSpace(rp.DisplayName)
	if name == "" {
		name = "Agent Overflow"
	}
	timeout := webauthn.TimeoutConfig{
		Enforce: true, Timeout: PasskeyCeremonyTTL, TimeoutUVD: PasskeyCeremonyTTL,
	}
	return webauthn.New(&webauthn.Config{
		RPID:          strings.TrimSpace(rp.ID),
		RPDisplayName: name,
		RPOrigins:     rp.Origins,
		Timeouts:      webauthn.TimeoutsConfig{Login: timeout, Registration: timeout},
	})
}

// recordCeremony stores a challenge and renders the browser's options.
func (s *Sessions) recordCeremony(ceremony passkeyCeremony, options any) (PasskeyChallenge, Reason) {
	encoded, err := json.Marshal(options)
	if err != nil {
		log.Printf("identity: encode passkey options: %v", err)
		return PasskeyChallenge{}, ReasonPasskeyUnavailable
	}
	id, err := newOpaqueSecret(passkeyCeremonyIDBytes)
	if err != nil {
		log.Printf("identity: draw passkey ceremony id: %v", err)
		return PasskeyChallenge{}, ReasonPasskeyUnavailable
	}
	ceremony.expiresAt = s.now().Add(PasskeyCeremonyTTL).UnixMilli()

	s.passkeyMu.Lock()
	if s.ceremonies == nil {
		s.ceremonies = make(map[string]passkeyCeremony)
	}
	s.expireCeremoniesLocked()
	if countPurpose(s.ceremonies, ceremony.purpose) >= passkeyCeremonyLimit {
		dropOldest(
			s.ceremonies,
			func(c passkeyCeremony) int64 { return c.expiresAt },
			func(c passkeyCeremony) bool { return c.purpose == ceremony.purpose },
		)
	}
	s.ceremonies[id] = ceremony
	s.passkeyMu.Unlock()

	return PasskeyChallenge{CeremonyID: id, Options: encoded}, ReasonNone
}

// takeCeremony consumes a challenge. It removes the entry on the FIRST
// attempt whatever the outcome, which is what makes a challenge single-use
// — the library will happily verify the same SessionData twice, so this is
// the only thing standing between a captured assertion and a replay.
//
// The purpose must match: a challenge minted for a step-up cannot be
// finished as a sign-in, and vice versa. Without it the weaker ceremony
// would be a way to obtain the stronger one's outcome.
func (s *Sessions) takeCeremony(id string, purpose passkeyPurpose) (passkeyCeremony, bool) {
	if id == "" {
		return passkeyCeremony{}, false
	}
	s.passkeyMu.Lock()
	ceremony, ok := s.ceremonies[id]
	delete(s.ceremonies, id)
	s.passkeyMu.Unlock()
	if !ok || ceremony.purpose != purpose {
		return passkeyCeremony{}, false
	}
	if ceremony.expiresAt <= s.now().UnixMilli() {
		return passkeyCeremony{}, false
	}
	return ceremony, true
}

func (s *Sessions) recordStepUp(token string, grant stepUpGrant) {
	s.passkeyMu.Lock()
	defer s.passkeyMu.Unlock()
	if s.stepUps == nil {
		s.stepUps = make(map[string]stepUpGrant)
	}
	now := s.now().UnixMilli()
	for key, held := range s.stepUps {
		if held.expiresAt <= now {
			delete(s.stepUps, key)
		}
	}
	if len(s.stepUps) >= stepUpTokenLimit {
		dropOldest(s.stepUps, func(g stepUpGrant) int64 { return g.expiresAt }, nil)
	}
	s.stepUps[token] = grant
}

func (s *Sessions) expireCeremoniesLocked() {
	now := s.now().UnixMilli()
	for id, ceremony := range s.ceremonies {
		if ceremony.expiresAt <= now {
			delete(s.ceremonies, id)
		}
	}
}

// dropOldest removes the entry expiring soonest among those match accepts; a
// nil match considers every entry. Used only at the cap, so the scan is paid
// once per overflowing begin rather than per call.
//
// The predicate is what keeps the ceremony book's cap PER PURPOSE: an
// eviction may only ever spend an entry of the purpose that is over its own
// bound, so the one ceremony a caller holding nothing can begin cannot reach
// the challenges a session-bound surface minted.
func dropOldest[V any](book map[string]V, expiry func(V) int64, match func(V) bool) {
	var oldestKey string
	var oldest int64
	for key, value := range book {
		if match != nil && !match(value) {
			continue
		}
		at := expiry(value)
		if oldestKey == "" || at < oldest {
			oldestKey, oldest = key, at
		}
	}
	if oldestKey != "" {
		delete(book, oldestKey)
	}
}

// countPurpose counts the ceremonies of one purpose. Paid only on the begin
// path, over a map whose whole size is three small caps.
func countPurpose(book map[string]passkeyCeremony, purpose passkeyPurpose) int {
	n := 0
	for _, ceremony := range book {
		if ceremony.purpose == purpose {
			n++
		}
	}
	return n
}

// refusePasskey maps a library error onto this package's closed reason set
// and records it.
//
// Every library refusal answers ReasonPasskeyRefused. The library's own
// taxonomy is a dozen `protocol.Error` types describing which step of the
// ceremony failed, and none of them names a different REMEDY: a person
// whose assertion did not verify tries again with the right authenticator,
// whatever the step was. The Details and DevInfo go to the server log,
// which is where a developer debugging a ceremony can read them and a
// caller cannot.
//
// Switched on Type rather than matched with errors.Is: the exported
// sentinels are copied by WithDetails, so errors.Is against them is always
// false.
func (s *Sessions) refusePasskey(err error, stage, userID string) Reason {
	logPasskeyError(err, stage)
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPasskeyRefused), Outcome: store.AuthAuditOutcomeRefused,
		Reason: ReasonPasskeyRefused.Code(), UserID: userID, Detail: stage,
	})
	return ReasonPasskeyRefused
}

func (s *Sessions) refusePasskeyAt(err error, stage, peer string) Reason {
	logPasskeyError(err, stage)
	s.auditPasskeyRefusal(ReasonPasskeyRefused, peer, stage)
	return ReasonPasskeyRefused
}

func (s *Sessions) auditPasskeyRefusal(reason Reason, peer, detail string) {
	s.audit(store.AuthAuditEntry{
		Event: string(AuditPasskeyRefused), Outcome: store.AuthAuditOutcomeRefused,
		Reason: reason.Code(), Peer: peer, Detail: detail,
	})
}

func logPasskeyError(err error, stage string) {
	var refusal *protocol.Error
	if errors.As(err, &refusal) {
		log.Printf("identity: passkey %s refused (%s): %s %s",
			stage, refusal.Type, refusal.Details, refusal.DevInfo)
		return
	}
	log.Printf("identity: passkey %s refused: %v", stage, err)
}

// mustHandle draws a WebAuthn user handle. A failure of the system CSPRNG
// returns zero bytes, which EnsureUserWebAuthnHandle refuses as a required
// field — so the ceremony fails loudly rather than enrolling an account
// under a predictable handle.
func mustHandle() []byte {
	buf := make([]byte, webAuthnUserHandleBytes)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("identity: draw webauthn user handle: %v", err)
		return nil
	}
	return buf
}

// newOpaqueSecret draws n bytes and renders them base64url without
// padding, the same shape as a pairing token.
func newOpaqueSecret(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("identity: draw secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
