package identity

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

func TestMintThenVerifyAdmitsTheSession(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	minted, credential := mustMint(t, sessions, owner, device, time.Hour)

	got, reason := sessions.Verify(credential)
	if reason.Refused() {
		t.Fatalf("Verify refused a credential it just minted: %s", reason)
	}
	if got.ID != minted.ID || got.UserID != owner.ID || got.DeviceID != device.ID {
		t.Fatalf("Verify returned the wrong session: %+v", got)
	}
	if got.BindingClass != string(BindingDeviceBound) {
		t.Fatalf("binding class = %q", got.BindingClass)
	}
	if len(got.Scopes) != 2 {
		t.Fatalf("scopes = %v", got.Scopes)
	}
}

func TestMintRefusesAnUndeclaredGrant(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	if _, _, err := sessions.Mint(MintRequest{
		UserID: owner.ID, DeviceID: device.ID,
		BindingClass: BindingDeviceBound,
		Scopes:       []Scope{"threads:reed"},
		TTL:          time.Hour,
	}); err == nil {
		t.Fatal("Mint accepted a scope that is not declared")
	}
	if _, _, err := sessions.Mint(MintRequest{
		UserID: owner.ID, DeviceID: device.ID,
		BindingClass: "sort-of-bound", TTL: time.Hour,
	}); err == nil {
		t.Fatal("Mint accepted an undeclared binding class")
	}
	if _, _, err := sessions.Mint(MintRequest{
		UserID: owner.ID, DeviceID: device.ID,
		BindingClass: BindingDeviceBound, TTL: 0,
	}); err == nil {
		t.Fatal("Mint accepted a session with no expiry")
	}
}

func TestVerifyRefusalsAreTypedAndSpecific(t *testing.T) {
	sessions, _, c, owner, device := newFixture(t)
	_, credential := mustMint(t, sessions, owner, device, time.Hour)

	if _, reason := sessions.Verify(""); reason != ReasonMissingProof {
		t.Fatalf("empty credential = %s, want missing_proof", reason)
	}
	if _, reason := sessions.Verify("not a credential"); reason != ReasonMalformedProof {
		t.Fatalf("garbage credential = %s, want malformed_proof", reason)
	}

	// A key this backend does not hold: well-formed, signed by someone,
	// simply not ours.
	foreign, err := signClaims(Claims{
		KeyID: "ffffffffffffffff", SessionID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		IssuedAt: c.now().UnixMilli(), ExpiresAt: c.now().UnixMilli() + 1000,
	}, []byte("someone else's secret"), testBackendID)
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	if _, reason := sessions.Verify(foreign); reason != ReasonKeyMismatch {
		t.Fatalf("unknown signing key = %s, want key_mismatch", reason)
	}

	if _, reason := sessions.Verify(flipMACBit(credential)); reason != ReasonInvalidSignature {
		t.Fatalf("altered mac = %s, want invalid_signature", reason)
	}

	// A verified credential whose session row was revoked.
	revoked, revokedCredential := mustMint(t, sessions, owner, device, time.Hour)
	if _, err := sessions.RevokeSession(revoked.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, reason := sessions.Verify(revokedCredential); reason != ReasonRevokedSession {
		t.Fatalf("revoked session = %s, want revoked_session", reason)
	}

	// A verified credential whose session row was never written. Signed
	// with this backend's real key, so only the row half can refuse it.
	key, err := sessions.EnsureSigningKey()
	if err != nil {
		t.Fatalf("EnsureSigningKey: %v", err)
	}
	orphan, err := signClaims(Claims{
		KeyID: key.ID, SessionID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		IssuedAt: c.now().UnixMilli(), ExpiresAt: c.now().UnixMilli() + 1000,
	}, key.Secret, testBackendID)
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	if _, reason := sessions.Verify(orphan); reason != ReasonUnknownSession {
		t.Fatalf("session with no row = %s, want unknown_session", reason)
	}

	// Expiry, from the claims' own window.
	_, shortLived := mustMint(t, sessions, owner, device, time.Minute)
	c.advance(2 * time.Minute)
	if _, reason := sessions.Verify(shortLived); reason != ReasonExpiredSession {
		t.Fatalf("expired credential = %s, want expired_session", reason)
	}
}

// TestSignatureIsCheckedBeforeTheTimeWindow is the ordering the spec
// requires: a proof that does not verify must never come back as a clock
// problem, because "check your date and time" is the wrong instruction for
// a credential that was not signed by this backend.
//
// The fixture is a credential that is BOTH future-dated and unsigned-by-us,
// so the two checks disagree about what to say and only the order decides.
func TestSignatureIsCheckedBeforeTheTimeWindow(t *testing.T) {
	sessions, _, c, owner, device := newFixture(t)
	key, err := sessions.EnsureSigningKey()
	if err != nil {
		t.Fatalf("EnsureSigningKey: %v", err)
	}
	session, _ := mustMint(t, sessions, owner, device, time.Hour)

	future := c.now().UnixMilli() + maxFutureSkewMillis + time.Hour.Milliseconds()
	futureClaims := Claims{
		KeyID: key.ID, SessionID: session.ID,
		IssuedAt: future, ExpiresAt: future + time.Hour.Milliseconds(),
	}

	// Signed properly, it is a clock problem and says so.
	signed, err := signClaims(futureClaims, key.Secret, testBackendID)
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	if _, reason := sessions.Verify(signed); reason != ReasonOutsideTimeWindow {
		t.Fatalf("future-dated credential = %s, want outside_time_window", reason)
	}

	// The same claims with a mac that does not verify must report the
	// signature, not the clock.
	if _, reason := sessions.Verify(flipMACBit(signed)); reason != ReasonInvalidSignature {
		t.Fatalf("future-dated credential with a bad mac = %s, want invalid_signature", reason)
	}

	// And the same claims signed by a key we do not hold reports the key,
	// not the clock either.
	foreign, err := signClaims(
		Claims{KeyID: "ffffffffffffffff", SessionID: session.ID, IssuedAt: future, ExpiresAt: future + 1000},
		key.Secret, testBackendID)
	if err != nil {
		t.Fatalf("signClaims: %v", err)
	}
	if _, reason := sessions.Verify(foreign); reason != ReasonKeyMismatch {
		t.Fatalf("future-dated credential under an unknown key = %s, want key_mismatch", reason)
	}
}

// flipMACBit alters one bit of a credential's mac while leaving the
// payload — and therefore the claims — exactly as they were.
func flipMACBit(credential string) string {
	parts := strings.Split(credential, ".")
	mac := []byte(parts[2])
	switch mac[0] {
	case 'A':
		mac[0] = 'B'
	default:
		mac[0] = 'A'
	}
	return parts[0] + "." + parts[1] + "." + string(mac)
}

// TestLiveIsTheAuthorityForEveryCall — nothing may authorize from state
// captured when a connection was established, so the per-call answer has
// to change the instant the row does.
func TestLiveIsTheAuthorityForEveryCall(t *testing.T) {
	sessions, _, c, owner, device := newFixture(t)
	session, credential := mustMint(t, sessions, owner, device, time.Hour)

	// Warm the fast path the way a presentation would.
	if _, reason := sessions.Verify(credential); reason.Refused() {
		t.Fatalf("Verify: %s", reason)
	}
	if _, reason := sessions.Live(session.ID); reason.Refused() {
		t.Fatalf("Live on a warm session: %s", reason)
	}
	if got := sessions.cachedCount(); got != 1 {
		t.Fatalf("fast path holds %d entries, want 1", got)
	}

	if _, err := sessions.RevokeSession(session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if got := sessions.cachedCount(); got != 0 {
		t.Fatalf("fast path still holds %d entries after a revoke", got)
	}
	if _, reason := sessions.Live(session.ID); reason != ReasonRevokedSession {
		t.Fatalf("Live after revoke = %s, want revoked_session", reason)
	}
	if _, reason := sessions.Verify(credential); reason != ReasonRevokedSession {
		t.Fatalf("Verify after revoke = %s, want revoked_session", reason)
	}

	// A row whose expiry passed answers from the row, even though the
	// claims are not consulted on this path.
	fresh, _ := mustMint(t, sessions, owner, device, time.Minute)
	if _, reason := sessions.Live(fresh.ID); reason.Refused() {
		t.Fatalf("Live on a fresh session: %s", reason)
	}
	c.advance(2 * time.Minute)
	if _, reason := sessions.Live(fresh.ID); reason != ReasonExpiredSession {
		t.Fatalf("Live past expiry = %s, want expired_session", reason)
	}
	if _, reason := sessions.Live(""); reason != ReasonUnknownSession {
		t.Fatalf("Live on an empty id = %s, want unknown_session", reason)
	}
	if _, reason := sessions.Live("6ba7b810-9dad-11d1-80b4-00c04fd430c8"); reason != ReasonUnknownSession {
		t.Fatalf("Live on an unknown id = %s, want unknown_session", reason)
	}
}

// TestRevokeUnderConcurrentLiveNeverResurrects — a Live() miss that reads
// the row just before a revocation lands must not install what it read.
// The generation counter is the mechanism; this is the property.
func TestRevokeUnderConcurrentLiveNeverResurrects(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	session, _ := mustMint(t, sessions, owner, device, time.Hour)

	var revoked atomic.Bool
	var admittedAfterRevoke atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Read the flag BEFORE the call. A read admitted before the
				// revoke landed, then descheduled past the store, is not an
				// admission after revocation; only a call that BEGAN after the
				// revoke returned may be counted against the session core.
				wasRevoked := revoked.Load()
				_, reason := sessions.Live(session.ID)
				if !reason.Refused() && wasRevoked {
					admittedAfterRevoke.Add(1)
				}
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	if _, err := sessions.RevokeSession(session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	revoked.Store(true)
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()

	if got := admittedAfterRevoke.Load(); got != 0 {
		t.Fatalf("%d calls were admitted after the revocation returned", got)
	}
	if got := sessions.cachedCount(); got != 0 {
		t.Fatalf("fast path holds %d entries for a revoked session", got)
	}
}

// TestRevokeDeviceEndsEverySessionItHeld — revoking the device is the
// user-facing action ("this laptop is gone"), and it has to reach every
// credential that device holds, not just the one in front of someone.
func TestRevokeDeviceEndsEverySessionItHeld(t *testing.T) {
	sessions, st, _, owner, device := newFixture(t)
	first, firstCredential := mustMint(t, sessions, owner, device, time.Hour)
	second, secondCredential := mustMint(t, sessions, owner, device, time.Hour)

	other, err := st.CreateDevice(owner.ID, "Phone", string(DevicePhone), "ios")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	untouched, untouchedCredential := mustMint(t, sessions, owner, other, time.Hour)

	closer := &recordingConns{}
	sessions.AttachConns(closer)

	revoked, err := sessions.RevokeDevice(device.ID)
	if err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if !revoked.DeviceMoved || revoked.SessionsEnded != 2 {
		t.Fatalf("RevokeDevice returned %+v, want the device and both sessions", revoked)
	}
	for _, credential := range []string{firstCredential, secondCredential} {
		if _, reason := sessions.Verify(credential); reason != ReasonRevokedSession {
			t.Fatalf("a revoked device's credential = %s, want revoked_session", reason)
		}
	}
	if _, reason := sessions.Verify(untouchedCredential); reason.Refused() {
		t.Fatalf("another device's session was caught in the revocation: %s", reason)
	}
	closer.mustHaveClosed(t, first.ID, second.ID)
	if closer.closed(untouched.ID) {
		t.Fatal("another device's connections were closed")
	}
}

// TestRevocationReachesLiveConnectionsSynchronously — a revocation that
// has not reached an open socket is not a revocation. RevokeSession must
// have closed the connections by the time it returns, not scheduled the
// close for later.
func TestRevocationReachesLiveConnectionsSynchronously(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	session, _ := mustMint(t, sessions, owner, device, time.Hour)

	closer := &recordingConns{}
	sessions.AttachConns(closer)

	if _, err := sessions.RevokeSession(session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	closer.mustHaveClosed(t, session.ID)

	// A second revocation still closes: a connection that survived the
	// first one is exactly the case worth closing again.
	closer.reset()
	moved, err := sessions.RevokeSession(session.ID)
	if err != nil {
		t.Fatalf("second RevokeSession: %v", err)
	}
	if moved {
		t.Fatal("a second revocation reported the row as having moved")
	}
	closer.mustHaveClosed(t, session.ID)
}

// TestRevocationWorksWithNoTransportAttached — a headless boot or a test
// has no connection registry, and revocation must still reach the row and
// the fast path rather than depending on one being wired.
func TestRevocationWorksWithNoTransportAttached(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	session, credential := mustMint(t, sessions, owner, device, time.Hour)
	if _, err := sessions.RevokeSession(session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, reason := sessions.Verify(credential); reason != ReasonRevokedSession {
		t.Fatalf("Verify = %s, want revoked_session", reason)
	}
}

func TestRefusalsAreRecordedWithPeerAttribution(t *testing.T) {
	sessions, st, _, _, _ := newFixture(t)
	sessions.RecordRefusal(ReasonInvalidSignature, "192.168.1.9:51000", "sess-x")
	// A non-refusal writes nothing: the log is for events, not for every
	// successful call.
	sessions.RecordRefusal(ReasonNone, "192.168.1.9:51000", "sess-x")

	entries, err := st.ListRecentAuthAudit(10)
	if err != nil {
		t.Fatalf("ListRecentAuthAudit: %v", err)
	}
	refusals := 0
	for _, entry := range entries {
		if entry.Event != string(AuditVerificationRefused) {
			continue
		}
		refusals++
		if entry.Reason != ReasonInvalidSignature.Code() {
			t.Fatalf("reason = %q", entry.Reason)
		}
		if entry.Peer != "192.168.1.9:51000" || entry.SessionID != "sess-x" {
			t.Fatalf("attribution missing: %+v", entry)
		}
		if entry.Outcome != store.AuthAuditOutcomeRefused {
			t.Fatalf("outcome = %q", entry.Outcome)
		}
	}
	if refusals != 1 {
		t.Fatalf("%d refusals recorded, want exactly 1", refusals)
	}
}

func TestMintAndRevokeAreAudited(t *testing.T) {
	sessions, st, _, owner, device := newFixture(t)
	session, _ := mustMint(t, sessions, owner, device, time.Hour)
	if _, err := sessions.RevokeSession(session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	entries, err := st.ListRecentAuthAudit(50)
	if err != nil {
		t.Fatalf("ListRecentAuthAudit: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Event] = true
	}
	for _, want := range []AuditEvent{AuditSessionMinted, AuditSessionRevoked} {
		if !seen[string(want)] {
			t.Fatalf("no %q entry in the credential log", want)
		}
	}
}

func TestEnsureSigningKeyIsIdempotent(t *testing.T) {
	sessions, _, _, _, _ := newFixture(t)
	first, err := sessions.EnsureSigningKey()
	if err != nil {
		t.Fatalf("EnsureSigningKey: %v", err)
	}
	second, err := sessions.EnsureSigningKey()
	if err != nil {
		t.Fatalf("second EnsureSigningKey: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("signing key moved: %q -> %q", first.ID, second.ID)
	}
	if len(first.Secret) != 32 {
		t.Fatalf("signing key secret is %d bytes, want 32", len(first.Secret))
	}
}

// TestFastPathSweepsExpiredEntries — the in-memory table is bounded by
// real sessions, so expired entries must not accumulate behind them.
func TestFastPathSweepsExpiredEntries(t *testing.T) {
	sessions, _, c, owner, device := newFixture(t)
	for range liveSweepThreshold + 4 {
		session, _ := mustMint(t, sessions, owner, device, time.Minute)
		if _, reason := sessions.Live(session.ID); reason.Refused() {
			t.Fatalf("Live: %s", reason)
		}
	}
	before := sessions.cachedCount()
	if before == 0 {
		t.Fatal("nothing was cached")
	}
	c.advance(2 * time.Minute)
	// One more live session past the sweep threshold triggers the sweep,
	// which drops every expired entry it finds.
	fresh, _ := mustMint(t, sessions, owner, device, time.Hour)
	if _, reason := sessions.Live(fresh.ID); reason.Refused() {
		t.Fatalf("Live on a fresh session: %s", reason)
	}
	if got := sessions.cachedCount(); got != 1 {
		t.Fatalf("fast path holds %d entries after the sweep, want only the live one", got)
	}
}

func TestNewSessionsRefusesAnIncompleteWiring(t *testing.T) {
	if _, err := NewSessions(nil, testBackendID); err == nil {
		t.Fatal("NewSessions accepted a nil store")
	}
	st := storetest.Clone(t)
	if _, err := NewSessions(st, ""); err == nil {
		t.Fatal("NewSessions accepted an empty backend id")
	}
}

// recordingConns is a LiveConns that remembers what it was asked to close.
type recordingConns struct {
	mu   sync.Mutex
	sids []string
}

func (r *recordingConns) CloseSession(sessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sids = append(r.sids, sessionID)
	return 1
}

func (r *recordingConns) closed(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.sids {
		if id == sessionID {
			return true
		}
	}
	return false
}

func (r *recordingConns) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sids = nil
}

func (r *recordingConns) mustHaveClosed(t *testing.T, want ...string) {
	t.Helper()
	for _, id := range want {
		if !r.closed(id) {
			t.Fatalf("connections for session %s were never closed", id)
		}
	}
}

// cachedCount reports the fast path's size, for tests that assert on
// invalidation and bounding.
func (s *Sessions) cachedCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.live)
}
