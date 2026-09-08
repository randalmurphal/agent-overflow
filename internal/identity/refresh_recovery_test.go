package identity

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRecoverableRefreshSurvivesLostReplyAndRestart(t *testing.T) {
	sessions, st, c, owner, _ := newFixture(t)
	device := newSigningDevice(t)
	_, first := keyPairedDevice(t, sessions, owner, device, c.now())
	next, _, err := newRefreshSecret()
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	req := RefreshRequest{Secret: first.RefreshSecret, NextSecret: next,
		Proof: device.proof(t, "POST", "/auth/token", "first", c.now())}
	issued, reason := sessions.Refresh(req)
	if reason.Refused() {
		t.Fatal(reason)
	}
	// Nothing from issued reaches the client. It still holds its two saved
	// secrets after restarting, and the backend has only their digests.
	sessions, err = NewSessions(st, testBackendID)
	if err != nil {
		t.Fatal(err)
	}
	sessions.now = c.now
	c.advance(24 * time.Hour)
	// The predecessor's own window has closed, but the pruner keeps it
	// while its successor is unspent: it is the receipt the retry is
	// proven against.
	sessions.PruneCredentials(0)
	firstDigest := hashRefreshSecret(first.RefreshSecret)
	if _, err := st.GetRefreshSecretByHash(firstDigest[:]); err != nil {
		t.Fatalf("prune dropped the recovery receipt: %v", err)
	}
	req.Proof = device.proof(t, "POST", "/auth/token", "retry", c.now())
	recovered, reason := sessions.Refresh(req)
	if reason.Refused() {
		t.Fatal(reason)
	}
	if recovered.SessionID != first.SessionID || recovered.RefreshSecret != next || recovered.RefreshExpiresAtMillis != issued.RefreshExpiresAtMillis {
		t.Fatal("retry replaced the pairing or created another refresh generation")
	}
	if _, reason := sessions.Verify(recovered.Credential); reason.Refused() {
		t.Fatal("recovered access:", reason)
	}
	chain, err := st.ListRefreshSecretsForSession(first.SessionID)
	if err != nil || len(chain) != 2 {
		t.Fatalf("generation count: %d, %v", len(chain), err)
	}
}

func TestRecoverableRefreshRequiresDeviceProofBeforeReuseCanRevoke(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	device := newSigningDevice(t)
	_, first := keyPairedDevice(t, sessions, owner, device, c.now())
	next, _, _ := newRefreshSecret()
	req := RefreshRequest{Secret: first.RefreshSecret, NextSecret: next, Proof: device.proof(t, "POST", "/auth/token", "first", c.now())}
	issued, reason := sessions.Refresh(req)
	if reason.Refused() {
		t.Fatal(reason)
	}
	req.NextSecret, _, _ = newRefreshSecret()
	req.Proof = bearerProof("not-the-device")
	if _, reason := sessions.Refresh(req); !reason.Refused() {
		t.Fatal("wrong proof admitted")
	}
	if _, reason := sessions.Verify(issued.Credential); reason.Refused() {
		t.Fatal("invalid proof revoked the device:", reason)
	}
	req.Proof = device.proof(t, "POST", "/auth/token", "actual-reuse", c.now())
	if _, reason := sessions.Refresh(req); reason != ReasonRevokedSession {
		t.Fatal("different successor was not reuse:", reason)
	}
}

func TestConcurrentIdenticalRefreshesShareOneSuccessor(t *testing.T) {
	sessions, st, c, owner, _ := newFixture(t)
	device := newSigningDevice(t)
	_, first := keyPairedDevice(t, sessions, owner, device, c.now())
	next, _, _ := newRefreshSecret()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		proof := device.proof(t, "POST", "/auth/token", fmt.Sprintf("concurrent-%d", i), c.now())
		wg.Add(1)
		go func() {
			defer wg.Done()
			issued, reason := sessions.Refresh(RefreshRequest{Secret: first.RefreshSecret, NextSecret: next, Proof: proof})
			if reason.Refused() || issued.RefreshSecret != next {
				t.Errorf("identical request: %s", reason)
			}
		}()
	}
	wg.Wait()
	chain, err := st.ListRefreshSecretsForSession(first.SessionID)
	if err != nil || len(chain) != 2 {
		t.Fatalf("generation count: %d, %v", len(chain), err)
	}
}

func TestSupersededRecoveryDoesNotRevokeNewerState(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	device := newSigningDevice(t)
	_, first := keyPairedDevice(t, sessions, owner, device, c.now())
	next, _, _ := newRefreshSecret()
	req := RefreshRequest{Secret: first.RefreshSecret, NextSecret: next, Proof: device.proof(t, "POST", "/auth/token/recover", "first", c.now())}
	second, reason := sessions.Refresh(req)
	if reason.Refused() {
		t.Fatal(reason)
	}
	thirdSecret, _, _ := newRefreshSecret()
	third, reason := sessions.Refresh(RefreshRequest{Secret: second.RefreshSecret, NextSecret: thirdSecret, Proof: device.proof(t, "POST", "/auth/token/recover", "second", c.now())})
	if reason.Refused() {
		t.Fatal(reason)
	}
	req.Proof = device.proof(t, "POST", "/auth/token/recover", "late", c.now())
	if _, reason := sessions.Refresh(req); reason != ReasonRefreshSuperseded {
		t.Fatal(reason)
	}
	if _, reason := sessions.Verify(third.Credential); reason.Refused() {
		t.Fatal("late retry revoked newer state:", reason)
	}
}

// A copy of the live head cannot mint access by presenting itself as the
// SUCCESSOR of a secret nothing issued. Before this held, the recover path
// looked the proposed successor up when the presented secret was unknown,
// so any leak of the current head plus a bearer thumbprint renewed without
// ever spending the head — and reuse detection, which only fires on a
// spent secret, never saw it.
func TestRecoveryNeverAdmitsTheLiveHeadAsAProposedSuccessor(t *testing.T) {
	sessions, st, c, owner, _ := newFixture(t)
	_, first := pairedDevice(t, sessions, owner, "thumb-phone")
	next, _, _ := newRefreshSecret()
	c.advance(time.Minute)
	second, reason := sessions.Refresh(RefreshRequest{Secret: first.RefreshSecret, NextSecret: next, Proof: bearerProof("thumb-phone")})
	if reason.Refused() {
		t.Fatal(reason)
	}
	junk, _, _ := newRefreshSecret()
	if _, reason := sessions.Refresh(RefreshRequest{Secret: junk, NextSecret: second.RefreshSecret, Proof: bearerProof("thumb-phone")}); reason != ReasonUnknownCredential {
		t.Fatalf("forged recovery = %s, want unknown_credential", reason)
	}
	headDigest := hashRefreshSecret(second.RefreshSecret)
	head, err := st.GetRefreshSecretByHash(headDigest[:])
	if err != nil || head.Spent() {
		t.Fatalf("forged recovery touched the head: %+v, %v", head, err)
	}
	// The real device still renews, and the copy is then caught the
	// ordinary way.
	third, reason := sessions.Refresh(RefreshRequest{Secret: second.RefreshSecret, Proof: bearerProof("thumb-phone")})
	if reason.Refused() {
		t.Fatal(reason)
	}
	if _, reason := sessions.Refresh(RefreshRequest{Secret: second.RefreshSecret, Proof: bearerProof("thumb-phone")}); reason != ReasonRevokedSession {
		t.Fatalf("reuse of the spent head = %s, want revoked_session", reason)
	}
	if _, reason := sessions.Verify(third.Credential); reason != ReasonRevokedSession {
		t.Fatalf("family survived reuse: %s", reason)
	}
}

// A proposed successor that already names a secret can never succeed on a
// retry of the same pair, so it is refused terminally rather than as a
// temporary failure the client would retry forever.
func TestRefreshRefusesATakenSuccessorTerminally(t *testing.T) {
	sessions, _, c, owner, _ := newFixture(t)
	_, first := pairedDevice(t, sessions, owner, "thumb-phone")
	c.advance(time.Minute)
	if _, reason := sessions.Refresh(RefreshRequest{Secret: first.RefreshSecret, NextSecret: first.RefreshSecret + "x", Proof: bearerProof("thumb-phone")}); reason != ReasonMalformedProof {
		// Not a valid successor at all; the shape check answers first.
		t.Fatalf("malformed successor = %s", reason)
	}
	next, _, _ := newRefreshSecret()
	second, reason := sessions.Refresh(RefreshRequest{Secret: first.RefreshSecret, NextSecret: next, Proof: bearerProof("thumb-phone")})
	if reason.Refused() {
		t.Fatal(reason)
	}
	taken, _, _ := newRefreshSecret()
	if _, reason := sessions.Refresh(RefreshRequest{Secret: second.RefreshSecret, NextSecret: taken, Proof: bearerProof("thumb-phone")}); reason.Refused() {
		t.Fatal(reason)
	}
	// The head is now `taken`; proposing it again beside itself is caught by
	// the equality check, so propose it beside a fresh legitimate head.
	fresh, _, _ := newRefreshSecret()
	fourth, reason := sessions.Refresh(RefreshRequest{Secret: taken, NextSecret: fresh, Proof: bearerProof("thumb-phone")})
	if reason.Refused() {
		t.Fatal(reason)
	}
	if _, reason := sessions.Refresh(RefreshRequest{Secret: fourth.RefreshSecret, NextSecret: next, Proof: bearerProof("thumb-phone")}); reason != ReasonMalformedProof {
		t.Fatalf("taken successor = %s, want malformed_proof", reason)
	}
}
