package provideraccounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func rollbackFixture(t *testing.T) (*Credentials, string) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("darwin stores Claude credentials in the Keychain, not the config home")
	}
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	return credentials, userHome
}

func accountSlotDir(t *testing.T, userHome, accountID string) string {
	t.Helper()
	return filepath.Join(userHome, ".claude", accountDirectoryName, accountID)
}

// The regression this whole type exists for, in both directions: a slot that
// already held a credential must survive a rollback, and the bytes in it must
// NOT be rewound. Rewinding is what enshrines a refresh token the provider has
// already retired — the operation being rolled back is frequently what rotated
// it — and that is an unrecoverable login, not a stale copy.
func TestRestoreLeavesAPreexistingCredentialAlone(t *testing.T) {
	credentials, userHome := rollbackFixture(t)
	if err := credentials.WriteAccountCredential("claude", "acct", []byte("original")); err != nil {
		t.Fatal(err)
	}

	saved, err := credentials.CaptureAccountCredential("claude", "acct")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !saved.HadCredential() {
		t.Fatal("capture missed an existing credential")
	}
	if err := credentials.WriteAccountCredential("claude", "acct", []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.RestoreAccountCredential(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}

	data, err := credentials.ReadCredential("claude", "acct", false)
	if err != nil {
		t.Fatalf("read credential after rollback: %v", err)
	}
	if string(data) != "rotated" {
		t.Fatalf("rollback rewound the credential to %q; the chain had moved to %q", data, "rotated")
	}
	if _, err := os.Stat(accountSlotDir(t, userHome, "acct")); err != nil {
		t.Fatalf("rollback removed a slot it did not create: %v", err)
	}
}

// A slot whose directory exists but holds no credential is NOT a slot
// this operation created. Rolling it back must clear only the
// credential introduced, leaving the directory (and any unrecognized
// contents AO deliberately ignores) in place.
func TestRestoreOfIntroducedCredentialKeepsThePreexistingSlot(t *testing.T) {
	credentials, userHome := rollbackFixture(t)
	slot := accountSlotDir(t, userHome, "acct")
	if err := os.MkdirAll(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	bystander := filepath.Join(slot, "left-by-an-older-layout")
	if err := os.WriteFile(bystander, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	saved, err := credentials.CaptureAccountCredential("claude", "acct")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if saved.HadCredential() {
		t.Fatal("capture invented a credential")
	}
	if err := credentials.WriteAccountCredential("claude", "acct", []byte("introduced")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.RestoreAccountCredential(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if _, err := credentials.ReadCredential("claude", "acct", false); !IsCredentialMissing(err) {
		t.Fatalf("introduced credential survived rollback: %v", err)
	}
	if _, err := os.Stat(slot); err != nil {
		t.Fatalf("rollback removed a slot directory it did not create: %v", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatalf("rollback removed contents it does not own: %v", err)
	}
}

// A slot that did not exist at all IS this operation's to clean up.
func TestRestoreRemovesASlotThisOperationCreated(t *testing.T) {
	credentials, userHome := rollbackFixture(t)

	saved, err := credentials.CaptureAccountCredential("claude", "acct")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := credentials.WriteAccountCredential("claude", "acct", []byte("introduced")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.RestoreAccountCredential(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if _, err := os.Stat(accountSlotDir(t, userHome, "acct")); !os.IsNotExist(err) {
		t.Fatalf("rollback left behind a slot it created: %v", err)
	}
}

// Rollback runs on the failure path. An uncaptured value must do
// nothing rather than delete something or mask the original error.
func TestRestoreOfAnUncapturedValueIsANoOp(t *testing.T) {
	credentials, userHome := rollbackFixture(t)
	if err := credentials.WriteAccountCredential("claude", "acct", []byte("untouched")); err != nil {
		t.Fatal(err)
	}

	if err := credentials.RestoreAccountCredential(SavedCredential{}); err != nil {
		t.Fatalf("restore of zero value: %v", err)
	}

	data, err := credentials.ReadCredential("claude", "acct", false)
	if err != nil {
		t.Fatalf("read after no-op restore: %v", err)
	}
	if string(data) != "untouched" {
		t.Fatalf("no-op restore changed the slot: %q", data)
	}
	if _, err := os.Stat(accountSlotDir(t, userHome, "acct")); err != nil {
		t.Fatalf("no-op restore removed a slot: %v", err)
	}
}

func TestCaptureRejectsAnInvalidAccountID(t *testing.T) {
	credentials, _ := rollbackFixture(t)
	if _, err := credentials.CaptureAccountCredential("claude", "../escape"); err == nil {
		t.Fatal("capture accepted an account id that is not a safe path component")
	}
}

// Every slot write must LAND, including one that moves the slot backwards
// along its rotation chain. The chain position is diagnostic only, and it has
// to stay that way: callers re-read the slot after writing it — Activate
// installs whatever the slot holds, not the bytes it was handed — so a write
// that is quietly skipped makes the next activation publish the stale
// credential it was trying to replace. That is the bug the observation exists
// to warn about, not a licence to introduce it.
func TestSlotWriteAlwaysLandsEvenMovingBackwardsAlongTheChain(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin stores Claude credentials in the Keychain, not the config home")
	}
	position := func(providerName string, data []byte) (int64, bool) {
		if providerName != "claude" {
			return 0, false
		}
		var parsed struct {
			At int64 `json:"at"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil || parsed.At <= 0 {
			return 0, false
		}
		return parsed.At, true
	}
	credentials, err := NewCredentials(t.TempDir(), Policy{ChainPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []string{`{"at":200}`, `{"at":100}`, `{"at":100}`, `{"at":300}`, `{"other":true}`} {
		if err := credentials.WriteAccountCredential("claude", "acct", []byte(next)); err != nil {
			t.Fatalf("write %s: %v", next, err)
		}
		data, err := credentials.ReadCredential("claude", "acct", false)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != next {
			t.Fatalf("write %s did not land: slot holds %q", next, data)
		}
	}
}
