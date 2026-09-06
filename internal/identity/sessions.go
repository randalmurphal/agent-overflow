package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// LiveConns is the transport's live-connection registry, as this package
// needs it. Declared here rather than imported so internal/transport stays
// free of this package (and therefore of internal/store); the transport
// side satisfies it structurally.
//
// CloseSession force-closes every connection carrying sessionID and
// returns how many it closed. It must not return until those connections
// have stopped their event streams, because "revoked" that has not reached
// a live socket is not revoked (docs/specs/remote-access.md §4).
type LiveConns interface {
	CloseSession(sessionID string) int
}

// Sessions is the session core: it mints credentials, verifies a
// presentation against both halves of a session, answers the per-RPC
// liveness question, and revokes.
//
// Both halves, always. A presentation is admitted only when the signed
// claims verify AND the database row is live. Neither alone is enough: a
// valid signature over a revoked session admits nothing, and a live row
// nobody can produce a signature for admits nobody.
//
// This coexists with internal/transport's launch Credential and replaces
// none of it. The launch credential is one per-process token for the local
// page; this is the durable, per-device, revocable one. Phase 3 migrates
// the wire onto this core; until then nothing routes through it, which is
// why nothing here reaches into the transport package.
type Sessions struct {
	store     *store.Store
	backendID string
	now       func() time.Time

	mu sync.RWMutex
	// live is the per-RPC fast path: the in-memory session table the spec
	// calls for. Bounded by the number of real sessions a person's devices
	// hold, plus expired entries until the next sweep.
	live map[string]store.Session
	// generation increments on every revocation. A slow-path read that
	// started before a revocation must not install its now-stale row, and
	// comparing the generation it captured is how it finds out. Without it
	// a Live() miss racing a Revoke() could re-cache a dead session.
	generation uint64
	// keys caches signing-key secrets by id. Bounded by the rows in
	// `signing_keys`, which only this package inserts; a presentation
	// naming an unknown key is refused and caches nothing.
	keys map[string][]byte

	// conns is optional. Nil means no transport is attached — a headless
	// boot, a test — and revocation then only has to reach the database and
	// the fast path.
	conns LiveConns

	// proofs is the device-proof replay guard (proofreplay.go). Process
	// local and deliberately not persisted; the file argues both the bound
	// and what a restart costs.
	proofs *proofReplay

	// passkeyMu guards the three passkey fields below. A lock of its own
	// rather than `mu`, because `mu` is held on the per-RPC fast path and
	// must never be waiting behind a ceremony book being scanned.
	passkeyMu sync.Mutex
	// relyingParty resolves what a passkey ceremony runs under, from the
	// boot's live network settings (passkey.go). Nil until the boot calls
	// SetRelyingParty, and a nil resolver means every passkey surface
	// refuses rather than guessing a domain.
	relyingParty func() RelyingParty
	// ceremonies is the outstanding-challenge book: bounded, expiring, and
	// single-use. The library accepts SessionData replay, so this map is
	// the only thing that makes a challenge answerable once.
	ceremonies map[string]passkeyCeremony
	// stepUps holds proven step-ups waiting to be spent, each bound to the
	// session that proved it. Process-local and short-lived on purpose: a
	// step-up that survived a restart would be standing elevation.
	stepUps map[string]stepUpGrant
}

// liveSweepThreshold is when the fast path drops expired entries. Below it
// the map is small enough that scanning would cost more than the rows do.
const liveSweepThreshold = 64

// NewSessions builds the core over a store. backendID comes from
// store.Identity and is captured once: it is mixed into every MAC, so a
// database restored under a new backend identity refuses the sessions it
// imported, and re-pairing — which is the recovery the spec already
// states — is what fixes it.
func NewSessions(st *store.Store, backendID string) (*Sessions, error) {
	if st == nil {
		return nil, errors.New("identity: a store is required")
	}
	if backendID == "" {
		return nil, errors.New("identity: a backend id is required")
	}
	return &Sessions{
		store:      st,
		backendID:  backendID,
		now:        time.Now,
		live:       make(map[string]store.Session),
		keys:       make(map[string][]byte),
		proofs:     newProofReplay(),
		ceremonies: make(map[string]passkeyCeremony),
		stepUps:    make(map[string]stepUpGrant),
	}, nil
}

// AttachConns wires the live-connection registry. Called once during boot,
// after the transport server exists. A revocation before this point still
// closes nothing, which is correct: there is nothing open.
func (s *Sessions) AttachConns(conns LiveConns) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns = conns
}

// EnsureSigningKey returns the active signing key, minting one on first
// boot. Idempotent and safe against a concurrent minter: the loser's
// insert is ignored and both re-read the same active key, since "active"
// is a deterministic ordering rather than a flag someone sets.
func (s *Sessions) EnsureSigningKey() (store.SigningKey, error) {
	key, err := s.store.ActiveSigningKey()
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return store.SigningKey{}, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return store.SigningKey{}, fmt.Errorf("identity: mint signing key: %w", err)
	}
	digest := sha256.Sum256(secret)
	minted := store.SigningKey{
		ID:        encodeKeyID(digest[:claimsKeyIDLen]),
		Secret:    secret,
		CreatedAt: s.now().UnixMilli(),
	}
	if err := s.store.InsertSigningKey(minted); err != nil {
		return store.SigningKey{}, err
	}
	return s.store.ActiveSigningKey()
}

// MintRequest describes a session to issue.
type MintRequest struct {
	UserID       string
	DeviceID     string
	BindingClass BindingClass
	Scopes       []Scope
	// TTL bounds the session. Required: a credential with no expiry is one
	// nothing but an explicit revocation can ever end, and this is the
	// short-lived half of the access-token/refresh pair (§4).
	TTL time.Duration
	// AwaitConfirmation mints the session UNACTIVATED: the row and the
	// credential both exist, and neither admits anything until Confirm
	// stamps `sessions.activated_at`.
	//
	// Set only by pairing redemption. The credential is handed to the new
	// device immediately so it can simply keep trying while the owner
	// checks the verification number, rather than holding a second secret
	// to poll with — the confirmation gate lives on the row, where one
	// predicate covers every presentation path.
	AwaitConfirmation bool
}

// Mint issues a session and returns the row plus the credential string.
// The credential is returned exactly once and never stored — the row holds
// no material that could reconstruct it.
//
// The session id is minted here rather than by the store because the same
// id is signed into the claims; two independent mints could not agree.
//
// It is the only caller of store.CreateSession, which is what makes it the
// single mint chokepoint for a session ROW — and the device gate lives
// inside that statement rather than as a read here (see CreateSession).
// A revoked device therefore fails the write itself, so a mint path added
// later inherits the refusal instead of having to restate it.
func (s *Sessions) Mint(req MintRequest) (store.Session, string, error) {
	if !req.BindingClass.Valid() {
		return store.Session{}, "", fmt.Errorf("identity: %q is not a declared binding class", string(req.BindingClass))
	}
	if req.TTL <= 0 {
		return store.Session{}, "", fmt.Errorf("identity: session ttl %s is not positive", req.TTL)
	}
	scopes, err := ValidateScopes(req.Scopes)
	if err != nil {
		return store.Session{}, "", err
	}
	key, err := s.EnsureSigningKey()
	if err != nil {
		return store.Session{}, "", err
	}
	now := s.now().UnixMilli()
	activatedAt := now
	if req.AwaitConfirmation {
		activatedAt = 0
	}
	session := store.Session{
		ID:           uuid.NewString(),
		UserID:       req.UserID,
		DeviceID:     req.DeviceID,
		BindingClass: string(req.BindingClass),
		Scopes:       scopes,
		SigningKeyID: key.ID,
		CreatedAt:    now,
		ExpiresAt:    now + req.TTL.Milliseconds(),
		ActivatedAt:  activatedAt,
	}
	credential, err := signClaims(Claims{
		KeyID:     key.ID,
		SessionID: session.ID,
		IssuedAt:  session.CreatedAt,
		ExpiresAt: session.ExpiresAt,
	}, key.Secret, s.backendID)
	if err != nil {
		return store.Session{}, "", err
	}
	if err := s.store.CreateSession(session); err != nil {
		return store.Session{}, "", fmt.Errorf("identity: mint session: %w", err)
	}
	s.rememberKey(key)
	s.audit(store.AuthAuditEntry{
		Event: string(AuditSessionMinted), Outcome: store.AuthAuditOutcomeAllowed,
		UserID: session.UserID, DeviceID: session.DeviceID, SessionID: session.ID,
		Detail: session.BindingClass,
	})
	return session, credential, nil
}

// Verify checks a presented credential and returns the session it admits.
//
// The order is the contract:
//
//  1. Something was presented.
//  2. It parses.
//  3. It names a signing key this backend holds.
//  4. Its MAC verifies under that key.
//  5. ONLY THEN its time window is judged.
//  6. The session row is live.
//
// Steps 4 and 5 cannot be swapped by accident: withinWindow is a method on
// verifiedClaims, and the only way to hold one is to have passed step 4.
//
// This is the per-PRESENTATION path — an HTTP request, a WebSocket
// upgrade, a ticket redemption. The per-RPC path is Live, which re-reads
// liveness without re-verifying a MAC.
func (s *Sessions) Verify(credential string) (store.Session, Reason) {
	if credential == "" {
		return store.Session{}, ReasonMissingProof
	}
	claims, payload, mac, reason := parseClaims(credential)
	if reason.Refused() {
		return store.Session{}, reason
	}
	secret, err := s.signingSecret(claims.KeyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Session{}, ReasonKeyMismatch
		}
		log.Printf("identity: read signing key %s: %v", claims.KeyID, err)
		return store.Session{}, ReasonKeyMismatch
	}
	want := claimsMAC(payload[:], secret, s.backendID)
	if !hmac.Equal(mac[:], want[:]) {
		return store.Session{}, ReasonInvalidSignature
	}
	verified := verifiedClaims{claims: claims}
	if reason := verified.withinWindow(s.now().UnixMilli()); reason.Refused() {
		return store.Session{}, reason
	}
	return s.Live(verified.claims.SessionID)
}

// Live answers the per-RPC question: does this session id still admit a
// call, right now?
//
// The fast path is one read-locked map lookup and an integer comparison —
// no allocation, no signature work, no database round trip. A miss falls
// through to the row, which is what makes a restart or an eviction correct
// rather than merely fast.
//
// Nothing here reads state captured when a connection was upgraded. That
// is the point: a revocation must reach a connection that is already open,
// and it can only do so if every call re-asks.
//
// # The conjunction, and why it costs nothing per call
//
// A session is live only while its own row AND its device's row are both
// unrevoked (docs/specs/remote-access.md §2, "Revocation is absolute").
// Both halves are answered from ONE row: `sessionSelect` joins the device
// and `store.Session.Live` folds its revocation stamp in, so the fast path
// gained a second integer comparison and no second lookup, no second round
// trip, and no device cache to keep coherent.
//
// The entry a hit reads carries the device stamp as it stood when the
// entry was installed, and three things keep that from going stale:
//
//   - a device revocation sweeps every un-revoked session the device holds
//     and forgets each one, so no entry for it survives (RevokeDevice);
//   - a device revocation moves the generation UNCONDITIONALLY, so a slow
//     path already in flight — one that read the rows before the
//     revocation committed and would install a stamp of zero — declines to
//     install;
//   - a session row that appears after the sweep has no entry at all, so
//     its first consult is a slow path, and the row it reads carries the
//     revocation.
//
// That last case is the one the incident turned on. A device-liveness bit
// resolved separately, or a set of revoked device ids consulted per call,
// would both have to answer it with extra state; joining the stamp onto
// the row answers it with the read that was already happening.
func (s *Sessions) Live(sessionID string) (store.Session, Reason) {
	if sessionID == "" {
		return store.Session{}, ReasonUnknownSession
	}
	now := s.now().UnixMilli()

	s.mu.RLock()
	cached, hit := s.live[sessionID]
	generation := s.generation
	s.mu.RUnlock()
	if hit {
		if cached.Live(now) {
			return cached, ReasonNone
		}
		// Expired in place. Drop it and fall through to the row, which may
		// have been extended.
		s.forget(sessionID)
	}

	session, reason := s.confirmedSession(sessionID, now)
	if reason.Refused() {
		return store.Session{}, reason
	}
	if session.ExpiresAt <= now {
		return store.Session{}, ReasonExpiredSession
	}

	s.mu.Lock()
	// Only cache what was still current for the whole read. A revocation
	// that landed while this read was in flight moved the generation, and
	// installing the row we fetched before it would resurrect a dead
	// session in the fast path.
	if s.generation == generation {
		if len(s.live) >= liveSweepThreshold {
			s.sweepExpiredLocked(now)
		}
		s.live[sessionID] = session
	}
	s.mu.Unlock()
	return session, ReasonNone
}

// confirmedSession checks durable admission state, independently of the access
// window. Refresh has already checked its own longer-lived secret; Live must
// additionally check access expiry before admitting a request or caching a row.
func (s *Sessions) confirmedSession(sessionID string, now int64) (store.Session, Reason) {
	session, err := s.store.GetSession(sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, ReasonUnknownSession
	}
	if err != nil {
		log.Printf("identity: read session %s: %v", sessionID, err)
		return store.Session{}, ReasonUnknownSession
	}
	if session.RevokedAt != 0 {
		return store.Session{}, ReasonRevokedSession
	}
	// Before expiry and before the confirmation gate: a revoked device is
	// the most specific true thing about this credential, and it is the one
	// with a different remedy (restore the device, then redeem a fresh
	// link) than either of them.
	if session.DeviceRevokedAt != 0 {
		return store.Session{}, ReasonRevokedDevice
	}
	// Checked after expiry on purpose: a pairing whose window lapsed
	// before anyone confirmed it needs a fresh link, which is what
	// "expired" says. Only a session still inside its window has a
	// confirmation worth waiting for.
	if session.AwaitingConfirmation() {
		if session.ExpiresAt <= now {
			return store.Session{}, ReasonExpiredSession
		}
		return store.Session{}, ReasonPendingConfirmation
	}

	return session, ReasonNone
}

// Now is the session core's clock, in Unix milliseconds.
//
// Exported so a surface that presents session and pairing rows reads the
// SAME clock the core writes them with: two clocks would let a list say
// "expired" about a row the core would still admit, and would leave a test
// that moves time moving only half the answer.
func (s *Sessions) Now() int64 { return s.now().UnixMilli() }

// RevokeSession ends one session everywhere, in the order that makes
// revocation real:
//
//  1. the database row, so every later read — including one already in
//     flight — sees it dead;
//  2. the in-memory fast path, at the same instant;
//  3. the live connections carrying it, synchronously.
//
// Reversing 1 and 2 would let a concurrent Live() re-populate the fast
// path from a row that had not been written yet. Doing 3 first would close
// a socket that could immediately reconnect on a credential still valid.
//
// Reports whether the session moved. A second revocation reports false and
// still closes connections, because a connection surviving a first
// revocation is exactly the case worth closing again.
func (s *Sessions) RevokeSession(sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	moved, err := s.store.RevokeSession(sessionID, s.now().UnixMilli())
	if err != nil {
		return false, err
	}
	s.forget(sessionID)
	closed := s.closeConns(sessionID)
	if moved {
		s.audit(store.AuthAuditEntry{
			Event: string(AuditSessionRevoked), Outcome: store.AuthAuditOutcomeAllowed,
			SessionID: sessionID, Detail: fmt.Sprintf("closed %d connections", closed),
		})
	}
	return moved, nil
}

// DeviceRevocation is what one RevokeDevice did, for a surface that has to
// report it honestly: "revoked, 2 sessions ended, 1 connection closed" and
// "already revoked, nothing was live" are different answers and a person
// acting on a lost device needs to be told which one they got.
type DeviceRevocation struct {
	// DeviceMoved is false when the device row was already revoked, or
	// names no row at all.
	DeviceMoved bool
	// SessionsEnded is how many un-revoked sessions this call swept.
	SessionsEnded int
	// ConnectionsClosed is how many live sockets it force-closed.
	ConnectionsClosed int
}

// RevokeDevice ends a device and every session it holds. The store writes
// both in one transaction and hands back the sessions that moved, so this
// cannot leave a revoked device holding a live credential.
//
// The three steps are RevokeSession's three, for the same reasons: rows,
// then fast path, then sockets. Two rules are specific to devices:
//
//   - **Re-revoking re-sweeps.** The store no longer returns early on an
//     already-revoked device, so a session that appeared after an earlier
//     sweep is ended by the next revoke rather than being unreachable
//     through this surface forever (incident 2026-08-31).
//   - **The generation moves even when nothing was swept.** It is the
//     device row that changed, and a Live() slow path already in flight may
//     be holding a joined row whose device stamp is now stale. Moving the
//     generation is what makes that read decline to install it. Bumping
//     only per swept session — which is what a loop over `forget` does —
//     would leave exactly the zero-session case, the straggler case,
//     unguarded.
func (s *Sessions) RevokeDevice(deviceID string) (DeviceRevocation, error) {
	if deviceID == "" {
		return DeviceRevocation{}, nil
	}
	revoked, err := s.store.RevokeDevice(deviceID, s.now().UnixMilli())
	if err != nil {
		return DeviceRevocation{}, err
	}
	s.forgetAll(revoked.SessionIDs)
	closed := 0
	for _, id := range revoked.SessionIDs {
		closed += s.closeConns(id)
	}
	result := DeviceRevocation{
		DeviceMoved:       revoked.DeviceMoved,
		SessionsEnded:     len(revoked.SessionIDs),
		ConnectionsClosed: closed,
	}
	if revoked.DeviceMoved || result.SessionsEnded > 0 {
		s.audit(store.AuthAuditEntry{
			Event: string(AuditDeviceRevoked), Outcome: store.AuthAuditOutcomeAllowed,
			DeviceID: deviceID,
			Detail: fmt.Sprintf("device moved: %t, revoked %d sessions, closed %d connections",
				revoked.DeviceMoved, result.SessionsEnded, closed),
		})
	}
	return result, nil
}

// RestoreDevice re-admits a revoked device's key to pairing. Its sessions
// stay revoked — restoring answers "I want that device back" (the refusal
// RedeemPairing gives a revoked key names this call as the remedy), and
// the way back to a credential is still a fresh owner-minted link plus
// the verification number. Reports whether a row moved; restoring a
// device that is not revoked is a no-op, not an error.
func (s *Sessions) RestoreDevice(deviceID string) (bool, error) {
	if deviceID == "" {
		return false, nil
	}
	moved, err := s.store.RestoreDevice(deviceID)
	if err != nil {
		return false, err
	}
	if moved {
		s.audit(store.AuthAuditEntry{
			Event: string(AuditDeviceRestored), Outcome: store.AuthAuditOutcomeAllowed,
			DeviceID: deviceID,
		})
	}
	return moved, nil
}

// ErrDeviceNotRevoked refuses forgetting a device that still holds
// standing. Revoke is what withdraws access; forgetting only removes the
// row a revocation already emptied, so the two are ordered rather than
// alternatives.
var ErrDeviceNotRevoked = errors.New("identity: the device is not revoked")

// ForgetDevice deletes a REVOKED device row and everything the schema
// cascades from it (store.DeleteDevice names the set). Reports whether a
// row was there to delete; forgetting a device that is already gone is a
// no-op, not an error, so a double click on the surface answers the same
// thing twice.
//
// It refuses an un-revoked device. Revoking is what ENDS access, and it
// is the step that closes live sockets and drops the device's persisted
// UI state; deleting the row first would remove the only handle the
// person has on a device that still holds credentials. Revoke, then
// forget.
//
// The device's key becomes free to enroll again, which is intended and
// is the whole difference from RestoreDevice: restoring says "that is
// still my device", forgetting says "that device is no longer anything
// to me". Either way the way back to a credential is an owner-minted
// link and the verification number, so a re-enrollment is still one the
// owner confirms.
//
// The audit rows naming the device STAY. They are the record of what
// this backend admitted and withdrew, and the row being gone is exactly
// when that record matters.
func (s *Sessions) ForgetDevice(deviceID string) (bool, error) {
	if deviceID == "" {
		return false, nil
	}
	device, err := s.store.GetDevice(deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("identity: forget device: %w", err)
	}
	if device.RevokedAt == 0 {
		return false, ErrDeviceNotRevoked
	}
	forgotten, err := s.store.DeleteDevice(deviceID)
	if err != nil {
		return false, err
	}
	if forgotten {
		s.audit(store.AuthAuditEntry{
			Event: string(AuditDeviceForgotten), Outcome: store.AuthAuditOutcomeAllowed,
			DeviceID: deviceID,
			Detail:   fmt.Sprintf("label: %s, class: %s", device.Label, device.Class),
		})
	}
	return forgotten, nil
}

// RecordRefusal writes one refused presentation to the credential log.
// Kept separate from Verify so the caller — which knows the peer address
// and the surface that was reached — supplies the attribution this package
// cannot see.
func (s *Sessions) RecordRefusal(reason Reason, peer, sessionID string) {
	if !reason.Refused() {
		return
	}
	s.audit(store.AuthAuditEntry{
		Event: string(AuditVerificationRefused), Outcome: store.AuthAuditOutcomeRefused,
		Reason: reason.Code(), Peer: peer, SessionID: sessionID,
	})
}

// forget drops one session from the fast path and moves the generation, so
// a slow-path read already in flight declines to install what it fetched.
func (s *Sessions) forget(sessionID string) {
	s.mu.Lock()
	delete(s.live, sessionID)
	s.generation++
	s.mu.Unlock()
}

// forgetAll drops a set of sessions in ONE lock hold and moves the
// generation ONCE — including for an empty set, which is the case that
// matters. A device revocation with nothing to sweep still invalidates
// every slow-path read in flight, because what changed is the device row
// those reads joined.
func (s *Sessions) forgetAll(sessionIDs []string) {
	s.mu.Lock()
	for _, id := range sessionIDs {
		delete(s.live, id)
	}
	s.generation++
	s.mu.Unlock()
}

// closeConns force-closes the live connections for a session. Reads the
// registry under the lock and calls it outside, so a teardown that blocks
// on a connection cannot stall an unrelated verification.
func (s *Sessions) closeConns(sessionID string) int {
	s.mu.RLock()
	conns := s.conns
	s.mu.RUnlock()
	if conns == nil {
		return 0
	}
	return conns.CloseSession(sessionID)
}

// sweepExpiredLocked drops entries whose window has passed. Called only
// when the map has grown past liveSweepThreshold, so the ordinary case
// pays nothing for it.
func (s *Sessions) sweepExpiredLocked(now int64) {
	for id, session := range s.live {
		if !session.Live(now) {
			delete(s.live, id)
		}
	}
}

// signingSecret resolves a key id to its bytes, caching the read. A
// presentation naming a key this backend does not hold returns
// sql.ErrNoRows and caches nothing, so an unknown id cannot grow the map.
func (s *Sessions) signingSecret(keyID string) ([]byte, error) {
	s.mu.RLock()
	secret, hit := s.keys[keyID]
	s.mu.RUnlock()
	if hit {
		return secret, nil
	}
	key, err := s.store.SigningKeyByID(keyID)
	if err != nil {
		return nil, err
	}
	s.rememberKey(key)
	return key.Secret, nil
}

func (s *Sessions) rememberKey(key store.SigningKey) {
	s.mu.Lock()
	s.keys[key.ID] = key.Secret
	s.mu.Unlock()
}

// audit appends a credential event, logging rather than returning a
// failure. A log write that fails must not turn a successful revocation
// into a reported error: the revocation already happened, and telling the
// caller it did not would invite a retry that finds nothing to do.
func (s *Sessions) audit(entry store.AuthAuditEntry) {
	if _, err := s.store.AppendAuthAudit(entry); err != nil {
		log.Printf("identity: append auth audit (%s): %v", entry.Event, err)
	}
}
