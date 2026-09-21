package identity

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

func TestLocalCredentialLifetimeAndRestart(t *testing.T) {
	s, st, clock, owner, _ := newFixture(t)
	row, first, err := s.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.ProcessBound() || first.RefreshSecret != "" || first.ExpiresAtMillis != 0 {
		t.Fatalf("local lifetime: %+v", row)
	}
	for _, jump := range []time.Duration{366 * 24 * time.Hour, -732 * 24 * time.Hour} {
		clock.advance(jump)
		if _, reason := s.Verify(first.Credential); reason.Refused() {
			t.Fatalf("clock change rejected local credential: %s", reason)
		}
	}
	// A new service over the same durable identity models abrupt restart. No
	// shutdown cleanup is involved, and the session ID remains stable.
	next, err := NewSessions(st, s.backendID)
	if err != nil {
		t.Fatal(err)
	}
	next.now = s.now
	secondRow, second, err := next.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != secondRow.ID || first.Credential == second.Credential {
		t.Fatal("restart must retain attribution and replace authentication")
	}
	if _, reason := next.Verify(first.Credential); !reason.Refused() {
		t.Fatal("previous boot credential admitted")
	}
	if _, reason := next.Verify(second.Credential); reason.Refused() {
		t.Fatal(reason)
	}
	// A pre-migration signed local credential cannot bypass boot binding.
	key, err := next.EnsureSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	old, err := signClaims(Claims{KeyID: key.ID, SessionID: row.ID, IssuedAt: next.Now(), ExpiresAt: next.Now() + time.Hour.Milliseconds()}, key.Secret, next.backendID)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := next.Verify(old); !reason.Refused() {
		t.Fatal("legacy signed local credential admitted")
	}
	if _, err := next.RevokeSession(row.ID); err != nil {
		t.Fatal(err)
	}
	if _, reason := next.Verify(second.Credential); reason != ReasonRevokedSession {
		t.Fatalf("revocation: %s", reason)
	}
	if _, _, err := next.EnsureLocalChannelSession(owner.ID); err == nil {
		t.Fatal("initialization minted around revocation")
	}
}

func TestLocalInitializationIsConcurrentAndIdempotent(t *testing.T) {
	s, st, _, owner, _ := newFixture(t)
	var wg sync.WaitGroup
	tokens := make(chan string, 12)
	for range 12 {
		wg.Go(func() {
			_, token, err := s.EnsureLocalChannelSession(owner.ID)
			if err != nil {
				t.Error(err)
				return
			}
			tokens <- token.Credential
		})
	}
	wg.Wait()
	close(tokens)
	var first string
	for value := range tokens {
		if first == "" {
			first = value
		}
		if value != first {
			t.Fatal("parallel bootstrap replaced credential")
		}
	}
	rows, err := st.ListLiveSessions(s.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("live rows=%d", len(rows))
	}
	if _, _, err := s.Mint(MintRequest{UserID: owner.ID, DeviceID: rows[0].DeviceID, BindingClass: BindingLoopbackOnly, TTL: time.Hour}); err == nil {
		t.Fatal("generic mint issued local credential")
	}
	if _, err := s.MintPairingLink(PairingRequest{UserID: owner.ID, DeviceClass: DeviceBrowser, BindingClass: BindingLoopbackOnly}); err == nil {
		t.Fatal("pairing issued local binding")
	}
}

func TestSessionStoreFailurePreservesRetryableAdmission(t *testing.T) {
	s, st, _, owner, device := newFixture(t)
	local, _, err := s.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	paired, _ := mustMint(t, s, owner, device, time.Hour)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{local.ID, paired.ID} {
		s.forget(id)
		if _, reason := s.Live(id); reason != ReasonTemporarilyUnavailable {
			t.Fatalf("unavailable store ended a session: %s", reason)
		}
	}
}

func TestLocalSessionAdoptsCurrentHostScopesAcrossUpgrade(t *testing.T) {
	s, st, _, owner, _ := newFixture(t)
	device, err := st.EnsureChannelDevice(owner.ID, LocalChannel, "This computer", string(DeviceDesktop), "linux")
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.EnsureSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.EnsureLocalSession(store.Session{
		ID: "old-local", UserID: owner.ID, DeviceID: device.ID,
		BindingClass: string(BindingLoopbackOnly), SigningKeyID: key.ID,
		CreatedAt: s.Now(), ActivatedAt: s.Now(), Scopes: []string{string(ScopeThreadsRead)},
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := NewSessions(st, s.backendID)
	if err != nil {
		t.Fatal(err)
	}
	prior, reason := next.Live(row.ID)
	if reason.Refused() || len(prior.Scopes) != 1 {
		t.Fatalf("old scope fixture: %+v %s", prior.Scopes, reason)
	}
	current, tokens, err := next.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetSession(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != row.ID || len(current.Scopes) != len(Scopes) || len(stored.Scopes) != len(Scopes) || len(tokens.Scopes) != len(Scopes) {
		t.Fatal("upgrade must retain local attribution and adopt the current host scopes")
	}
}
