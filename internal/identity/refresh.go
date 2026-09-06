package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/store"
)

// Token exchange and rotating refresh (docs/specs/remote-access.md §4
// "Sessions").
//
// The access credential is the wave-5a signed claims, unchanged. What this
// file adds is the other half of the pair: a secret that buys a fresh
// access generation once. A recorded successor makes that operation
// recoverable after a lost reply; a different successor is reuse evidence.
//
// The family is the SESSION. A renewal extends the session row's window and
// issues the next secret in the same chain, rather than minting a new
// session per renewal — which keeps one durable id for the live-connection
// registry to key on (a new id per hour would leave every open socket
// attached under a dead one) and makes "revoke the whole family" exactly
// "revoke the session", with no second notion of membership that could
// disagree with the first.

// refreshSecretBytes is the entropy in one renewal secret. 32 bytes, the
// same budget as the launch token, encoded base64url without padding.
const refreshSecretBytes = 32

// refreshHashDomain separates a refresh digest from every other SHA-256 in
// this package, so a value that happens to hash equal somewhere else cannot
// be presented here.
const refreshHashDomain = "agent-overflow/refresh-secret/v1\x00"

// reuseDetectedDetail is what the family spend writes into `consumed_by` for
// the secrets it invalidates, so a later reader can tell a rotation apart
// from a teardown.
const reuseDetectedDetail = "reuse-detected"

// TokenSet is one issuance: the short-lived access credential and the
// single-use secret that renews it.
//
// Neither bearer is stored on the server. Recoverable renewals return the
// successor the client already saved; the receipt records only its digest.
type TokenSet struct {
	// SessionID is the durable session both halves belong to. Returned so
	// a caller can attribute a connection without parsing the credential.
	SessionID string
	// Credential is the signed access credential (claims.go).
	Credential string
	// ExpiresAtMillis is when Credential stops being presentable.
	ExpiresAtMillis int64
	// RefreshSecret is empty when the policy is not renewable — the local
	// page channel, which is re-minted at boot instead. Empty is an answer,
	// not an omission.
	RefreshSecret string
	// RefreshExpiresAtMillis is zero alongside an empty RefreshSecret.
	RefreshExpiresAtMillis int64
	// AwaitingConfirmation is true when the session was minted by a pairing
	// redemption and the owner has not confirmed the verification number
	// yet. Both credentials are real; neither admits anything until then.
	AwaitingConfirmation bool
	// Scopes is the session row's grant set, returned so the device that
	// just paired (or just rotated) can tell its own screens which
	// surfaces it holds instead of discovering each one by being refused
	// (docs/specs/remote-access.md §5, frontend capability model).
	//
	// The issuance is the only moment a device learns this without a
	// dedicated round trip, and grants are immutable for a session's
	// lifetime, so one copy at issue time stays true until the session
	// ends. It is a disclosure of what the caller already holds, never an
	// authorization: every RPC re-checks against the row.
	//
	// The row's own slice, read-only to its caller.
	Scopes []string
}

// RefreshRequest is one renewal presentation.
type RefreshRequest struct {
	// Secret is the refresh secret as the device holds it.
	Secret string
	// NextSecret is chosen and persisted by a retry-capable client before sending.
	NextSecret string
	// Proof is the device's proof of possession. Required for every
	// `device-bound` and `public` session on EVERY listener, so a bare
	// bearer copy of a refresh secret cannot renew itself even from
	// loopback (§4: "Refresh binds to the device key on every listener").
	//
	// A signed proof over this request for a device that enrolled a key,
	// the bare enrollment thumbprint for one that could not (§15
	// constraint 6). CheckDeviceProof decides which is acceptable from the
	// device row, never from what arrived.
	Proof DeviceProof
	// Peer is the request's source address, for audit attribution only.
	Peer string
}

// Refresh advances one durable refresh generation. Retry-capable clients
// propose their saved successor, so a lost reply can recover the same operation
// without treating every second presentation as a stolen secret. Legacy
// callers omit NextSecret and retain strict single-use rotation.
func (s *Sessions) Refresh(req RefreshRequest) (TokenSet, Reason) {
	if req.Secret == "" {
		return TokenSet{}, ReasonMissingProof
	}
	digest := hashRefreshSecret(req.Secret)
	nextSecret := req.NextSecret
	recoverable := nextSecret != ""
	var nextDigest [sha256.Size]byte
	if recoverable {
		decoded, err := base64.RawURLEncoding.DecodeString(nextSecret)
		if err != nil || len(decoded) != refreshSecretBytes || nextSecret == req.Secret {
			return TokenSet{}, ReasonMalformedProof
		}
		nextDigest = hashRefreshSecret(nextSecret)
	}
	now := s.now().UnixMilli()
	held, err := s.store.GetRefreshSecretByHash(digest[:])
	if errors.Is(err, sql.ErrNoRows) && recoverable {
		// The predecessor may have aged out while the proposed successor is
		// still live. Possessing that successor and the device key can recover.
		held, err = s.store.GetRefreshSecretByHash(nextDigest[:])
	}
	if errors.Is(err, sql.ErrNoRows) {
		s.audit(store.AuthAuditEntry{Event: string(AuditRefreshRefused), Outcome: store.AuthAuditOutcomeRefused, Reason: ReasonUnknownCredential.Code(), Peer: req.Peer})
		return TokenSet{}, ReasonUnknownCredential
	}
	if err != nil {
		return s.refreshUnavailable("read renewal", err)
	}
	session, reason := s.confirmedSession(held.SessionID, now)
	if reason.Refused() {
		s.RecordRefusal(reason, req.Peer, held.SessionID)
		return TokenSet{}, reason
	}
	// Prove possession BEFORE declaring reuse. A spent bearer alone must not
	// let someone without the device key revoke its still-live family.
	if reason := s.CheckDeviceProof(session, req.Proof); reason.Refused() {
		s.RecordRefusal(reason, req.Peer, session.ID)
		return TokenSet{}, reason
	}
	if !recoverable {
		if held.Spent() {
			s.revokeFamilyForReuse(held, req.Peer)
			return TokenSet{}, ReasonRevokedSession
		}
		if held.ExpiresAt <= now {
			s.audit(store.AuthAuditEntry{Event: string(AuditRefreshRefused), Outcome: store.AuthAuditOutcomeRefused, Reason: ReasonExpiredSession.Code(), SessionID: held.SessionID, Peer: req.Peer})
			return TokenSet{}, ReasonExpiredSession
		}
		nextSecret, nextDigest, err = newRefreshSecret()
		if err != nil {
			return s.refreshUnavailable("choose successor", err)
		}
	}
	device, err := s.store.GetDevice(session.DeviceID)
	if err != nil {
		return s.refreshUnavailable("read device", err)
	}
	policy := PolicyFor(DeviceClass(device.Class), BindingClass(session.BindingClass))
	generation := s.generationNow()
	rotated, err := s.store.RotateRefreshSecret(context.Background(), store.RefreshRotation{
		SessionID: session.ID, DeviceID: session.DeviceID, OldHash: digest[:], NextHash: nextDigest[:],
		Now: now, AccessUntil: now + policy.Access.Milliseconds(), RefreshUntil: now + policy.Refresh.Milliseconds(), Recoverable: recoverable,
	})
	switch {
	case errors.Is(err, store.ErrRefreshReuse):
		s.revokeFamilyForReuse(held, req.Peer)
		return TokenSet{}, ReasonRevokedSession
	case errors.Is(err, store.ErrRefreshSuperseded):
		return TokenSet{}, ReasonRefreshSuperseded
	case errors.Is(err, sql.ErrNoRows):
		if _, reason := s.confirmedSession(session.ID, now); reason.Refused() {
			return TokenSet{}, reason
		}
		return TokenSet{}, ReasonExpiredSession
	case err != nil:
		return s.refreshUnavailable("commit renewal", err)
	}
	s.rememberAt(generation, rotated.Session)
	tokens, err := s.accessTokensFor(rotated.Session, device, now)
	if err != nil {
		return s.refreshUnavailable("sign renewal", err)
	}
	tokens.RefreshSecret = nextSecret
	tokens.RefreshExpiresAtMillis = rotated.Secret.ExpiresAt
	if !rotated.Replayed {
		s.audit(store.AuthAuditEntry{Event: string(AuditSessionRefreshed), Outcome: store.AuthAuditOutcomeAllowed,
			UserID: session.UserID, DeviceID: session.DeviceID, SessionID: session.ID, Peer: req.Peer})
	}
	return tokens, ReasonNone
}

func (s *Sessions) refreshUnavailable(step string, err error) (TokenSet, Reason) {
	log.Printf("identity: %s: %v", step, err)
	return TokenSet{}, ReasonTemporarilyUnavailable
}

func (s *Sessions) revokeFamilyForReuse(secret store.RefreshSecret, peer string) {
	now := s.now().UnixMilli()
	outstanding, err := s.store.SpendRefreshSecretsForSession(secret.SessionID, now, reuseDetectedDetail)
	if err != nil {
		log.Printf("identity: spend refresh family %s: %v", secret.SessionID, err)
	}
	if _, err := s.RevokeSession(secret.SessionID); err != nil {
		log.Printf("identity: revoke session %s after refresh reuse: %v", secret.SessionID, err)
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditRefreshReuseDetected), Outcome: store.AuthAuditOutcomeRefused,
		Reason: ReasonRevokedSession.Code(), SessionID: secret.SessionID, Peer: peer,
		Detail: fmt.Sprintf("spent secret %s presented again; %d outstanding secrets invalidated",
			secret.ID, outstanding),
	})
}

// CheckDeviceProof enforces "refresh binds to the device key on every
// listener" (docs/specs/remote-access.md §4).
//
// Exported because the rule is not about REFRESHING: the app's request
// hook runs it on every request that names a session, so a session whose
// device enrolled a key presents it everywhere — including the ticket
// route, which would otherwise be a way around it. One function, so a
// caller cannot implement a weaker version of the same check.
//
// A `loopback-only` session has no key to bind to — it is the credential
// this backend mints for its own page channel — and it is not renewable at
// all, so it never reaches here through Refresh. Every other class must
// satisfy its device row.
//
// A device-bound session whose device row carries NO thumbprint is refused
// (`key_mismatch`), the same as one whose thumbprint does not match. That
// is the failure a device gets when it was paired before
// proof-of-possession existed, and admitting it would be the downgrade the
// binding rule exists to close.
func (s *Sessions) CheckDeviceProof(session store.Session, proof DeviceProof) Reason {
	if BindingClass(session.BindingClass) == BindingLoopbackOnly {
		return ReasonNone
	}
	if proof.Value == "" {
		return ReasonMissingProof
	}
	device, err := s.store.GetDevice(session.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReasonKeyMismatch
	}
	if err != nil {
		log.Printf("identity: read device %s: %v", session.DeviceID, err)
		return ReasonKeyMismatch
	}
	if device.RevokedAt != 0 {
		// Defensive. Callers check confirmedSession (directly or through Live), which answers
		// the conjunction — so a revoked device is refused there, with this
		// same code, before a proof is ever compared. Kept because this
		// function is exported and a later caller may not have.
		return ReasonRevokedDevice
	}
	if device.KeyThumbprint == "" {
		return ReasonKeyMismatch
	}
	return s.checkProofAgainstDevice(device, proof)
}

// checkProofAgainstDevice compares one presentation against the device row
// that decides what a valid presentation IS.
//
// The row decides, never the presentation. That sentence is the downgrade
// rule: a device whose row records an enrolled key is answered by the
// signed branch whatever it sent, so a bare thumbprint for such a device
// is a refusal and not a fallback. The reverse — a bearer device that
// sends a proof — is also a refusal, because a bearer row's thumbprint is
// an opaque identifier that is not the hash of any key, so there is
// nothing a signature could be checked against.
func (s *Sessions) checkProofAgainstDevice(device store.Device, proof DeviceProof) Reason {
	signed := proof.Signed()
	switch proofKindOf(device) {
	case ProofSignedKey:
		if !signed {
			// Exactly today's wire: the thumbprint string, presented for a
			// device that has since enrolled a real key. Named apart from
			// malformed_proof so this reads as what it is.
			return ReasonProofDowngraded
		}
		return s.verifyDeviceProof(device.KeyThumbprint, proof)
	default:
		if signed {
			return ReasonMalformedProof
		}
		if device.KeyThumbprint != proof.Value {
			return ReasonKeyMismatch
		}
		return ReasonNone
	}
}

// verifyDeviceProof is the whole signed path, in the order the checks have
// to run.
//
// Ordering, and what each position buys:
//
//  1. PARSE. Structure, the pinned `typ` and `alg`, and a key that is
//     actually a point on P-256. Nothing here is trusted yet.
//  2. THUMBPRINT. The embedded key must be the one this device enrolled.
//     Before the signature, because a signature proves possession of
//     whatever key the presentation chose — the thumbprint is what ties
//     that key to this device, and checking it first also declines to
//     spend a P-256 verify on a key we were never going to accept.
//  3. SIGNATURE. The only constructor of verifiedDeviceProof, which is
//     what makes steps 4 and 5 unreachable from an unverified proof.
//  4. BINDING, then FRESHNESS. Both are statements about what the client
//     signed, so both are meaningless before step 3 — and the freshness
//     refusal in particular carries the "check your clock" hint, which
//     must never be shown for a proof this backend's device never signed.
//  5. REPLAY, last. The identifier is spent only by a proof that passed
//     everything else, so a presentation that could never be admitted
//     cannot consume the identifier of one that would be.
func (s *Sessions) verifyDeviceProof(thumbprint string, presented DeviceProof) Reason {
	parsed, reason := parseDeviceProof(presented.Value)
	if reason.Refused() {
		return reason
	}
	if parsed.thumbprint != thumbprint {
		return ReasonKeyMismatch
	}
	return s.admitProof(parsed, presented)
}

// issueFor signs the access credential for a session row and, when the
// policy is renewable, mints the next refresh secret.
//
// The refresh secret is written BEFORE the credential is returned, so a
// device never holds a credential whose renewal path does not exist yet.
//
// It is the ONE function in this package that builds a TokenSet, which is
// what makes it the place the device half of the conjunction is enforced
// at issuance (docs/specs/remote-access.md §2). Two consequences of that
// shape are deliberate:
//
//   - it takes the DEVICE ROW, not a policy. Every caller derived the
//     policy from the same two values anyway, and passing the row means a
//     caller cannot hand this function a policy from one device and a
//     session from another — nor issue without having read a device at all.
//   - the refusal is hygiene, not the enforcement. A revocation can land
//     the instant after this check, so what actually makes such a
//     credential worthless is that every consult re-asks (Sessions.Live).
//     Refusing here is what stops one being HANDED OUT in the first place.
func (s *Sessions) issueFor(session store.Session, device store.Device, now int64) (TokenSet, error) {
	tokens, err := s.accessTokensFor(session, device, now)
	if err != nil {
		return TokenSet{}, err
	}
	policy := PolicyFor(DeviceClass(device.Class), BindingClass(session.BindingClass))
	if !policy.Renewable() {
		return tokens, nil
	}
	secret, digest, err := newRefreshSecret()
	if err != nil {
		return TokenSet{}, err
	}
	refreshExpiry := now + policy.Refresh.Milliseconds()
	if _, err := s.store.CreateRefreshSecret(session.ID, digest[:], now, refreshExpiry); err != nil {
		return TokenSet{}, err
	}
	tokens.RefreshSecret = secret
	tokens.RefreshExpiresAtMillis = refreshExpiry
	return tokens, nil
}

// accessTokensFor signs the access half for already persisted session state.
func (s *Sessions) accessTokensFor(session store.Session, device store.Device, now int64) (TokenSet, error) {
	if device.RevokedAt != 0 {
		return TokenSet{}, fmt.Errorf("identity: issue for session %s: %w",
			session.ID, store.ErrDeviceRevoked)
	}
	key, err := s.signingKeyByID(session.SigningKeyID)
	if err != nil {
		return TokenSet{}, err
	}
	credential, err := signClaims(Claims{
		KeyID:     key.ID,
		SessionID: session.ID,
		IssuedAt:  now,
		ExpiresAt: session.ExpiresAt,
	}, key.Secret, s.backendID)
	if err != nil {
		return TokenSet{}, err
	}
	return TokenSet{
		SessionID:            session.ID,
		Credential:           credential,
		ExpiresAtMillis:      session.ExpiresAt,
		AwaitingConfirmation: session.AwaitingConfirmation(),
		Scopes:               session.Scopes,
	}, nil
}

// signingKeyByID resolves a key row through the same cache Verify uses,
// rather than re-reading it per issuance.
func (s *Sessions) signingKeyByID(keyID string) (store.SigningKey, error) {
	secret, err := s.signingSecret(keyID)
	if err != nil {
		return store.SigningKey{}, fmt.Errorf("identity: read signing key %s: %w", keyID, err)
	}
	return store.SigningKey{ID: keyID, Secret: secret}, nil
}

// generationNow snapshots the revocation generation. Capture it BEFORE
// reading the row you intend to rememberAt: the compare under the write
// lock is what makes the pair safe, and a snapshot taken after the read
// would miss a revocation that landed during it.
func (s *Sessions) generationNow() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

// rememberAt installs a row the caller just read into the fast path,
// declining when any revocation moved the generation since it was
// captured — the same rule Live's slow path applies to its own read.
// Installing unconditionally would let a Revoke that ran between the row
// read and this call resurrect the dead session in the fast path.
func (s *Sessions) rememberAt(generation uint64, session store.Session) {
	if !session.Live(s.now().UnixMilli()) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation != generation {
		return
	}
	s.live[session.ID] = session
}

// newRefreshSecret draws one secret and returns BOTH forms — the string a
// device holds and the digest the row stores.
//
// Both, for the same structural reason newRecoveryCode returns both: a
// single return value would let a caller store a digest of something other
// than the value it handed out, producing a secret that matches no row,
// with no path that would ever notice.
func newRefreshSecret() (secret string, digest [sha256.Size]byte, err error) {
	buf := make([]byte, refreshSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", digest, fmt.Errorf("identity: draw refresh secret: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(buf)
	return secret, hashRefreshSecret(secret), nil
}

// hashRefreshSecret is what the store holds. Domain-separated and a plain
// SHA-256: the input is 256 bits of CSPRNG output, so a slow KDF would buy
// nothing against an offline search and would cost the single indexed
// lookup that makes consumption atomic — the same reasoning recovery codes
// carry.
func hashRefreshSecret(secret string) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(refreshHashDomain))
	h.Write([]byte(secret))
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out
}

// PruneCredentials drops credential rows that can never admit anything
// again: sessions whose access and refresh windows closed before `before`,
// refresh secrets outside that retention window,
// and pairing links that expired without being redeemed.
//
// The only deletion this package performs, and the bound is what makes it
// safe on authoritative rows — every row it touches would already refuse
// every presentation. `keep` is a margin the caller sets so a device list
// can still show recent history; spent refresh secrets INSIDE their window
// are never touched, because they are the reuse detector's evidence.
func (s *Sessions) PruneCredentials(keep time.Duration) {
	before := s.now().Add(-keep).UnixMilli()
	if dropped, err := s.store.DeleteSessionsExpiredBefore(before); err != nil {
		log.Printf("identity: prune expired sessions: %v", err)
	} else if dropped > 0 {
		log.Printf("identity: pruned %d expired sessions", dropped)
	}
	if _, err := s.store.DeleteRefreshSecretsExpiredBefore(before); err != nil {
		log.Printf("identity: prune expired refresh secrets: %v", err)
	}
	if _, err := s.store.DeletePairingLinksExpiredBefore(before); err != nil {
		log.Printf("identity: prune expired pairing links: %v", err)
	}
}
