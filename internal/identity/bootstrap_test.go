package identity

import (
	"testing"

	"agent-overflow/internal/store/storetest"
)

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
