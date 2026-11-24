package identity

import (
	"testing"
	"time"
)

// A renewal that cannot sign its new credential moves nothing durable: the
// presented secret stays unspent, so the device's next renewal with it
// succeeds instead of being read as reuse. This holds for a client that
// lets the backend choose the next secret, which cannot replay a rotation.
func TestARenewalThatCannotSignSpendsNothing(t *testing.T) {
	sessions, st, raw, owner, _ := stragglerFixture(t)
	_, first := pairedDevice(t, sessions, owner, "thumb-renew")
	// A restarted backend holds no signing key in memory, so signing reads
	// the key's row, which is withheld until the first renewal has failed.
	restarted, err := NewSessions(st, testBackendID)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = sessions.now
	var id string
	var secret []byte
	var created int64
	if err := raw.QueryRow(`SELECT id, secret, created_at FROM signing_keys`).Scan(&id, &secret, &created); err != nil {
		t.Fatalf("read the signing key: %v", err)
	}
	if _, err := raw.Exec(`DELETE FROM signing_keys`); err != nil {
		t.Fatalf("withhold the signing key: %v", err)
	}
	if _, err := st.GetSession(first.SessionID); err != nil {
		t.Fatalf("withholding the key must leave the session: %v", err)
	}

	renewal := RefreshRequest{Secret: first.RefreshSecret, Proof: bearerProof("thumb-renew")}
	if _, reason := restarted.Refresh(renewal); reason != ReasonTemporarilyUnavailable {
		t.Fatalf("a renewal that cannot sign = %s, want %s", reason, ReasonTemporarilyUnavailable)
	}
	held, err := st.GetRefreshSecretByHash(func() []byte { d := hashRefreshSecret(first.RefreshSecret); return d[:] }())
	if err != nil || held.Spent() {
		t.Fatalf("the presented secret after the failure = %+v (%v), want unspent", held, err)
	}

	if _, err := raw.Exec(`INSERT INTO signing_keys (id, secret, created_at) VALUES (?, ?, ?)`, id, secret, created); err != nil {
		t.Fatalf("restore the signing key: %v", err)
	}
	restarted.now = func() time.Time { return sessions.now().Add(time.Minute) }
	renewed, reason := restarted.Refresh(renewal)
	if reason.Refused() {
		t.Fatalf("the renewal after the key returned = %s, want renewed", reason)
	}
	if _, reason := restarted.Verify(renewed.Credential); reason.Refused() {
		t.Fatalf("the renewed credential = %s, want admitted", reason)
	}
}
