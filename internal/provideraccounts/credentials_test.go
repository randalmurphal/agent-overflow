package provideraccounts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClaudeKeychainIdentityMatchesConfigScopedNativeService(t *testing.T) {
	t.Setenv("USER", "test-user")
	home := filepath.Join(t.TempDir(), ".claude", accountDirectoryName, "account-one")
	service, username, err := claudeKeychainIdentity(home, false)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(home)))
	wantService := "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
	if service != wantService || username != "test-user" {
		t.Fatalf("identity = (%q, %q), want (%q, test-user)", service, username, wantService)
	}
	activeService, _, err := claudeKeychainIdentity(home, true)
	if err != nil {
		t.Fatal(err)
	}
	if activeService != "Claude Code-credentials" {
		t.Fatalf("active service = %q", activeService)
	}
}

func TestEphemeralCodexHomeCopiesConfigWithoutSharedStateOrCredential(t *testing.T) {
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(userHome, ".codex")
	if err := os.MkdirAll(filepath.Join(codexHome, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"x\""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte("canonical-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	home, err := credentials.NewEphemeralHome("codex")
	if err != nil {
		t.Fatal(err)
	}
	path := home.Path
	defer func() {
		if err := home.Cleanup(); err != nil {
			t.Fatal(err)
		}
	}()

	assertFileContents(t, filepath.Join(path, "config.toml"), "model = \"x\"")
	if _, err := os.Lstat(filepath.Join(path, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary sessions path error = %v, want not exist", err)
	}
	if _, err := os.Lstat(filepath.Join(path, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary auth path error = %v, want not exist", err)
	}
}

func TestEphemeralHomeSeedsOnlySavedCredentialAndCleansUp(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(
		"codex",
		"account-one",
		[]byte(`{"account":"one"}`),
	); err != nil {
		t.Fatal(err)
	}
	accountPath, err := credentials.AccountCredentialPath("codex", "account-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(accountPath), "legacy-cache"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := credentials.ReadCredential("codex", "account-one", false)
	if err != nil {
		t.Fatal(err)
	}
	home, err := credentials.NewEphemeralHomeWithCredential("codex", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(home.Path, "auth.json"), `{"account":"one"}`)
	if _, err := os.Lstat(filepath.Join(home.Path, "legacy-cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary legacy cache error = %v, want not exist", err)
	}
	path := home.Path
	if err := home.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary home error after cleanup = %v, want not exist", err)
	}
}

func TestReadEphemeralCredentialCapturesProviderLogin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Claude uses the native Keychain on macOS")
	}
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, err := credentials.NewEphemeralHome("claude")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := home.Cleanup(); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.WriteFile(
		filepath.Join(home.Path, ".credentials.json"),
		[]byte(`{"oauth":"new"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	snapshot, err := credentials.ReadEphemeralCredential(home)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Data) != `{"oauth":"new"}` {
		t.Fatalf("credential = %s", snapshot.Data)
	}
}

func TestActivatePreservesRotatedCurrentCredential(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Claude uses the native Keychain on macOS")
	}
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("claude", "first", []byte("first-old")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("claude", "second", []byte("second-token")); err != nil {
		t.Fatal(err)
	}
	active, _ := credentials.ActiveCredentialPath("claude")
	if err := os.MkdirAll(filepath.Dir(active), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte("first-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.Activate("claude", "first", "second"); err != nil {
		t.Fatal(err)
	}
	first, _ := credentials.AccountCredentialPath("claude", "first")
	assertFileContents(t, first, "first-rotated")
	assertFileContents(t, active, "second-token")
}

func TestActivateWithSnapshotDoesNotMisattributeRacingCredential(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Claude uses the native Keychain on macOS")
	}
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("claude", "first", []byte("first-old")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("claude", "second", []byte("second-token")); err != nil {
		t.Fatal(err)
	}
	active, _ := credentials.ActiveCredentialPath("claude")
	if err := os.MkdirAll(filepath.Dir(active), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte("verified-first"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := credentials.ReadCredentialSnapshot("claude", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte("racing-third-account"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.ActivateWithSnapshot("claude", "first", "second", &snapshot); err != nil {
		t.Fatal(err)
	}
	first, _ := credentials.AccountCredentialPath("claude", "first")
	assertFileContents(t, first, "verified-first")
	assertFileContents(t, active, "second-token")
}

func TestCommitSelectedCredentialPublishesCanonicalBeforeSavedSlot(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("codex", "selected", []byte("old-token")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Activate("codex", "", "selected"); err != nil {
		t.Fatal(err)
	}

	saved, err := credentials.AccountCredentialPath("codex", "selected")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(saved, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := credentials.CommitSelectedCredential("codex", "selected", []byte("new-token")); err == nil {
		t.Fatal("CommitSelectedCredential() succeeded despite an unwritable saved slot")
	}
	active, err := credentials.ActiveCredentialPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, active, "new-token")
	if info, err := os.Stat(saved); err != nil {
		t.Fatal(err)
	} else if !info.IsDir() {
		t.Fatalf("saved credential obstruction mode = %v, want directory", info.Mode())
	}
}

func TestActivateRejectsSavedCredentialSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior is platform-policy dependent on Windows")
	}
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := credentials.AccountCredentialPath("codex", "target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Activate("codex", "", "target"); err == nil {
		t.Fatal("Activate() accepted a symlink credential file")
	}
}

func TestPruneOrphanedAccountsLeavesRegisteredSlotsUntouched(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{"keep", "remove"} {
		if err := credentials.WriteAccountCredential(
			"codex",
			accountID,
			[]byte(`{"account":"`+accountID+`"}`),
		); err != nil {
			t.Fatal(err)
		}
	}
	keptPath, err := credentials.AccountCredentialPath("codex", "keep")
	if err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(filepath.Dir(keptPath), "unrelated-state")
	if err := os.WriteFile(unrelated, []byte("leave untouched"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.PruneOrphanedAccounts(
		"codex",
		map[string]bool{"keep": true},
	); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, keptPath, `{"account":"keep"}`)
	assertFileContents(t, unrelated, "leave untouched")
	if _, err := credentials.ReadCredential("codex", "remove", false); !IsCredentialMissing(err) {
		t.Fatalf("orphaned credential error = %v, want missing", err)
	}
}

func TestManagedAccountMutationsRejectSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior is platform-policy dependent on Windows")
	}
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(userHome, ".codex", accountDirectoryName)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}

	if err := credentials.WriteAccountCredential("codex", "new-account", []byte("secret")); err == nil {
		t.Fatal("WriteAccountCredential() accepted a symlinked managed root")
	}
	if err := credentials.RemoveAccount("codex", "victim"); err == nil {
		t.Fatal("RemoveAccount() accepted a symlinked managed root")
	}
	if err := credentials.PruneOrphanedAccounts("codex", nil); err == nil {
		t.Fatal("PruneOrphanedAccounts() accepted a symlinked managed root")
	}
	assertFileContents(t, sentinel, "keep")
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

// CredentialPresent answers the question the UI asks before offering an
// account: could this be selected right now? Anything the activation path
// would refuse counts as absent, so the two never disagree.
func TestCredentialPresentMatchesWhatActivationWouldAccept(t *testing.T) {
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("codex", "saved", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	slotPath, err := credentials.AccountCredentialPath("codex", "saved")
	if err != nil {
		t.Fatal(err)
	}
	activePath, err := credentials.ActiveCredentialPath("codex")
	if err != nil {
		t.Fatal(err)
	}

	assertCredentialPresent(t, credentials, "saved", false, true)
	// Nothing has been activated yet, so the canonical store is empty.
	assertCredentialPresent(t, credentials, "", true, false)

	if err := credentials.Activate("codex", "", "saved"); err != nil {
		t.Fatal(err)
	}
	assertCredentialPresent(t, credentials, "", true, true)

	// An account with no slot at all: the case metadata outlives.
	assertCredentialPresent(t, credentials, "never-saved", false, false)

	// A truncated credential reads as empty and would fail activation.
	if err := os.WriteFile(slotPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertCredentialPresent(t, credentials, "saved", false, false)

	// A symlink is refused by every read path; presence must agree.
	if err := os.Remove(activePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(slotPath, activePath); err != nil {
		t.Fatal(err)
	}
	assertCredentialPresent(t, credentials, "", true, false)
}

func TestCredentialPresentRejectsAnUnsafeAccountID(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.CredentialPresent("codex", "../escape", false); err == nil {
		t.Fatal("CredentialPresent() accepted an account id that is not a safe path component")
	}
}

func assertCredentialPresent(
	t *testing.T,
	credentials *Credentials,
	accountID string,
	active bool,
	want bool,
) {
	t.Helper()
	got, err := credentials.CredentialPresent("codex", accountID, active)
	if err != nil {
		t.Fatalf("CredentialPresent(%q, active=%v): %v", accountID, active, err)
	}
	if got != want {
		t.Fatalf("CredentialPresent(%q, active=%v) = %v, want %v", accountID, active, got, want)
	}
	if !want {
		return
	}
	if _, readErr := credentials.ReadCredential("codex", accountID, active); readErr != nil {
		t.Fatalf("credential reported present but unreadable: %v", readErr)
	}
}
