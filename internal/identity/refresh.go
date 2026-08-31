package identity

import (
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
// access credential exactly once, and whose second presentation is treated
// as evidence rather than as a retry.
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
// Both strings are returned exactly once and neither is stored — the rows
// hold a session id and a digest, and no material that could reconstruct
// either value.
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
}

// RefreshRequest is one renewal presentation.
type RefreshRequest struct {
	// Secret is the refresh secret as the device holds it.
	Secret string
	// KeyThumbprint is the device's proof of possession. Required for
	// every `device-bound` and `public` session on EVERY listener, so a
	// bare bearer copy of a refresh secret cannot renew itself even from
	// loopback (§4: "Refresh binds to the device key on every listener").
	//
	// Phase 5 replaces the thumbprint with a signed DPoP proof over the
	// request. The token endpoint already accepts thumbprints for exactly
	// that reason (§16 phase 5), so the wire shape does not move.
	KeyThumbprint string
	// Peer is the request's source address, for audit attribution only.
	Peer string
}

// Refresh exchanges a refresh secret for a fresh credential pair.
//
// The order is the contract, and the two halves of it decide different
// things:
//
//  1. Resolve the presented secret. Unknown → refused and nothing else.
//     Already spent → the real device exchanged this secret and moved on,
//     so a second presentation is a copy in circulation: the whole family
//     is revoked (see revokeFamilyForReuse). Past its window → expired.
//  2. Judge the SESSION and the device proof, both BEFORE the secret is
//     consumed. A device whose proof is momentarily wrong keeps its
//     secret and can present it again with a correct one; consuming
//     first would sign out a device for a recoverable mistake.
//  3. Only then spend it, through the store's one-statement CAS, which is
//     what makes rotation single-use against every other connection. A
//     lost CAS means another presentation of the same secret won between
//     steps 1 and 3 — the same evidence step 1 looks for, answered the
//     same way.
//
// A device that loses the RESPONSE to a successful renewal and retries with
// the same secret is signed out by step 1. That is the spec's rule as
// written, and it is what makes the detector meaningful: a copy renewing
// alongside the real device is indistinguishable from a device renewing
// twice. A client must treat a renewal whose answer it never read as spent
// and re-authenticate rather than retry.
func (s *Sessions) Refresh(req RefreshRequest) (TokenSet, Reason) {
	if req.Secret == "" {
		return TokenSet{}, ReasonMissingProof
	}
	digest := hashRefreshSecret(req.Secret)
	now := s.now().UnixMilli()

	held, reason := s.resolveRefreshSecret(digest[:], now, req.Peer)
	if reason.Refused() {
		return TokenSet{}, reason
	}
	session, reason := s.Live(held.SessionID)
	if reason.Refused() {
		s.RecordRefusal(reason, req.Peer, held.SessionID)
		return TokenSet{}, reason
	}
	if reason := s.checkDeviceProof(session, req.KeyThumbprint); reason.Refused() {
		s.RecordRefusal(reason, req.Peer, session.ID)
		return TokenSet{}, reason
	}

	if _, err := s.store.ConsumeRefreshSecret(digest[:], now, req.KeyThumbprint); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("identity: consume refresh secret: %v", err)
			return TokenSet{}, ReasonUnknownCredential
		}
		// Another presentation of this same secret won the race while we
		// were checking. Exactly the evidence step 1 looks for.
		s.revokeFamilyForReuse(held, req.Peer)
		return TokenSet{}, ReasonRevokedSession
	}

	tokens, err := s.reissue(session, now)
	if err != nil {
		log.Printf("identity: reissue session %s: %v", session.ID, err)
		return TokenSet{}, ReasonUnknownCredential
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditSessionRefreshed), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: session.UserID, DeviceID: session.DeviceID, SessionID: session.ID,
		Peer: req.Peer,
	})
	return tokens, ReasonNone
}

// resolveRefreshSecret reads the presented secret and classifies it. The
// three refusals it can produce are the three things a bare secret can be
// wrong about, and each carries a different consequence.
func (s *Sessions) resolveRefreshSecret(digest []byte, now int64, peer string) (store.RefreshSecret, Reason) {
	held, err := s.store.GetRefreshSecretByHash(digest)
	if errors.Is(err, sql.ErrNoRows) {
		s.audit(store.AuthAuditEntry{
			Event: string(AuditRefreshRefused), Outcome: store.AuthAuditOutcomeRefused,
			Reason: ReasonUnknownCredential.Code(), Peer: peer,
		})
		return store.RefreshSecret{}, ReasonUnknownCredential
	}
	if err != nil {
		log.Printf("identity: read refresh secret: %v", err)
		return store.RefreshSecret{}, ReasonUnknownCredential
	}
	if held.Spent() {
		s.revokeFamilyForReuse(held, peer)
		return store.RefreshSecret{}, ReasonRevokedSession
	}
	if held.ExpiresAt <= now {
		s.audit(store.AuthAuditEntry{
			Event: string(AuditRefreshRefused), Outcome: store.AuthAuditOutcomeRefused,
			Reason: ReasonExpiredSession.Code(), SessionID: held.SessionID, Peer: peer,
		})
		return store.RefreshSecret{}, ReasonExpiredSession
	}
	return held, ReasonNone
}

// revokeFamilyForReuse ends a session because a spent secret was presented
// again, and records why.
//
// Order matters the same way it does in RevokeSession, and for the same
// reason: the outstanding secrets are spent FIRST, so a renewal already in
// flight on another connection cannot slip through between the revocation
// and the chain teardown. RevokeSession then writes the row, drops the fast
// path, and force-closes the live sockets.
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

// checkDeviceProof enforces "refresh binds to the device key on every
// listener".
//
// A `loopback-only` session has no key to bind to — it is the credential
// this backend mints for its own page channel — and it is not renewable at
// all, so it never reaches here through Refresh. Every other class must
// present the thumbprint its device row holds.
//
// A device-bound session whose device row carries NO thumbprint is refused
// with the same missing-proof answer as one that presented nothing. That is
// the failure a device gets when it was paired before proof-of-possession
// existed, and admitting it would be the downgrade the binding rule exists
// to close.
func (s *Sessions) checkDeviceProof(session store.Session, thumbprint string) Reason {
	if BindingClass(session.BindingClass) == BindingLoopbackOnly {
		return ReasonNone
	}
	if thumbprint == "" {
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
		return ReasonRevokedSession
	}
	if device.KeyThumbprint == "" || device.KeyThumbprint != thumbprint {
		return ReasonKeyMismatch
	}
	return ReasonNone
}

// reissue extends a live session's window and issues the next credential
// pair in its chain.
//
// The session row's expiry IS the access window — the claims carry the same
// two stamps, and a claim outliving its row would be admitted by nothing —
// so renewing one means moving the other, in that order: extend the row
// first, sign second, so a signature can never name a window the row does
// not hold.
func (s *Sessions) reissue(session store.Session, now int64) (TokenSet, error) {
	device, err := s.store.GetDevice(session.DeviceID)
	if err != nil {
		return TokenSet{}, fmt.Errorf("identity: reissue: read device: %w", err)
	}
	policy := PolicyFor(DeviceClass(device.Class), BindingClass(session.BindingClass))
	expiresAt := now + policy.Access.Milliseconds()
	if _, err := s.store.ExtendSession(session.ID, expiresAt, now); err != nil {
		return TokenSet{}, err
	}
	// The extend is conditional (it never shortens a window), so read back
	// what the row actually holds rather than assuming ours won. A session
	// whose window already reached further keeps it, and the claims say so.
	extended, err := s.store.GetSession(session.ID)
	if err != nil {
		return TokenSet{}, fmt.Errorf("identity: reissue: read session: %w", err)
	}
	s.remember(extended)
	return s.issueFor(extended, policy, now)
}

// issueFor signs the access credential for a session row and, when the
// policy is renewable, mints the next refresh secret.
//
// The refresh secret is written BEFORE the credential is returned, so a
// device never holds a credential whose renewal path does not exist yet.
func (s *Sessions) issueFor(session store.Session, policy TokenPolicy, now int64) (TokenSet, error) {
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
	tokens := TokenSet{
		SessionID:            session.ID,
		Credential:           credential,
		ExpiresAtMillis:      session.ExpiresAt,
		AwaitingConfirmation: session.AwaitingConfirmation(),
	}
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

// signingKeyByID resolves a key row through the same cache Verify uses,
// rather than re-reading it per issuance.
func (s *Sessions) signingKeyByID(keyID string) (store.SigningKey, error) {
	secret, err := s.signingSecret(keyID)
	if err != nil {
		return store.SigningKey{}, fmt.Errorf("identity: read signing key %s: %w", keyID, err)
	}
	return store.SigningKey{ID: keyID, Secret: secret}, nil
}

// remember installs a row the caller just wrote into the fast path. Safe
// against a concurrent revocation for the same reason Live's install is:
// the generation is captured and compared, so a revocation that landed
// while this call was in flight declines the write.
func (s *Sessions) remember(session store.Session) {
	if !session.Live(s.now().UnixMilli()) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
// again: sessions and refresh secrets whose windows closed before `before`,
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
