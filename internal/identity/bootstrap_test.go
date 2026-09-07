package identity

import (
	"fmt"
	"testing"

	"agent-overflow/internal/store/storetest"
)

func TestBootstrapRetiresEveryUnfinishedInvitationButPreservesConfirmedDevice(t *testing.T) {
	st := storetest.Clone(t)
	s, boot, err := Bootstrap(st, testBackendID, "Owner")
	if err != nil {
		t.Fatal(err)
	}
	confirmed := mustMintLink(t, s, boot.Owner)
	active := mustRedeem(t, s, confirmed.Token, "confirmed-device")
	if _, err := s.ConfirmPairing(confirmed.Link.ID); err != nil {
		t.Fatal(err)
	}
	pending := mustMintLink(t, s, boot.Owner)
	inactive := mustRedeem(t, s, pending.Token, "pending-device")
	// More than the access overview's row limit: boot is a store sweep, not
	// a loop over whichever rows the current settings page happened to show.
	links := make([]PairingLink, 61)
	for i := range links {
		links[i] = mustMintLink(t, s, boot.Owner)
	}
	next, _, err := Bootstrap(st, testBackendID, "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := next.Verify(active.Tokens.Credential); reason.Refused() {
		t.Fatal("confirmed credential lost on restart", reason)
	}
	if _, reason := next.Verify(inactive.Tokens.Credential); !reason.Refused() {
		t.Fatal("pending credential admitted after restart")
	}
	if _, err := next.ConfirmPairing(pending.Link.ID); err == nil {
		t.Fatal("old comparison could confirm after restart")
	}
	for i, link := range links {
		if _, reason := next.RedeemPairing(RedemptionRequest{Token: link.Token, Proof: bearerProof(fmt.Sprintf("late-%d", i))}); !reason.Refused() {
			t.Fatal("unredeemed invitation survived restart", i)
		}
	}
}

func TestBootstrapMintsOnceAndIsSafeToRepeat(t *testing.T) {
	st := storetest.Clone(t)

	sessions, first, err := Bootstrap(st, testBackendID, "Owner")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if first.Owner.ID == "" || first.SigningKey.ID == "" {
		t.Fatalf("first boot produced no identity: %+v", first)
	}
	if len(first.RecoveryCodes) != RecoveryCodeCount {
		t.Fatalf("first boot returned %d recovery codes, want %d",
			len(first.RecoveryCodes), RecoveryCodeCount)
	}
	if sessions == nil {
		t.Fatal("Bootstrap returned no session core")
	}

	_, second, err := Bootstrap(st, testBackendID, "Owner")
	if err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if second.Owner.ID != first.Owner.ID {
		t.Fatalf("owner moved between boots: %q -> %q", first.Owner.ID, second.Owner.ID)
	}
	if second.SigningKey.ID != first.SigningKey.ID {
		t.Fatalf("signing key moved between boots: %q -> %q",
			first.SigningKey.ID, second.SigningKey.ID)
	}
	if second.RecoveryCodes != nil {
		t.Fatal("a later boot re-minted recovery codes, invalidating the set somebody saved")
	}
	// The first boot's codes still work.
	if _, err := sessions.ConsumeRecoveryCode(first.RecoveryCodes[0], "", ""); err != nil {
		t.Fatalf("a code from first boot stopped working: %v", err)
	}
}

// TestBootstrapDoesNotReMintAfterEveryCodeIsSpent — keying the mint on
// "no UNSPENT codes" would hand someone who used their last code a fresh
// set they were never shown, replacing the one they still believe in.
func TestBootstrapDoesNotReMintAfterEveryCodeIsSpent(t *testing.T) {
	st := storetest.Clone(t)
	sessions, first, err := Bootstrap(st, testBackendID, "Owner")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, code := range first.RecoveryCodes {
		if _, err := sessions.ConsumeRecoveryCode(code, "", ""); err != nil {
			t.Fatalf("ConsumeRecoveryCode: %v", err)
		}
	}
	if count, err := st.CountUnspentRecoveryCodes(first.Owner.ID); err != nil || count != 0 {
		t.Fatalf("CountUnspentRecoveryCodes = %d (err %v), want 0", count, err)
	}
	_, second, err := Bootstrap(st, testBackendID, "Owner")
	if err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if second.RecoveryCodes != nil {
		t.Fatal("Bootstrap re-minted codes for an account whose set was merely spent")
	}
}

func TestBootstrapRefusesAnIncompleteWiring(t *testing.T) {
	st := storetest.Clone(t)
	if _, _, err := Bootstrap(st, "", "Owner"); err == nil {
		t.Fatal("Bootstrap accepted an empty backend id")
	}
	if _, _, err := Bootstrap(st, testBackendID, ""); err == nil {
		t.Fatal("Bootstrap accepted an empty owner name")
	}
}
