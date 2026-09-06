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
