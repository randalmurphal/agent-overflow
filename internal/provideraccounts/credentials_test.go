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
	service, username := claudeKeychainIdentity(home, false)
	sum := sha256.Sum256([]byte(home))
	wantService := "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
	if service != wantService || username != "test-user" {
		t.Fatalf("identity = (%q, %q), want (%q, test-user)", service, username, wantService)
	}
	activeService, _ := claudeKeychainIdentity(home, true)
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
	home, err := credentials.NewEphemeralHomeWithCredential("codex", snapshot, "codex-account")
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

// A canonical credential that moves between the caller's verified snapshot
// and the activation write is, in practice, the provider rotating the
// outgoing account's single-use chain (a live session, the user's own CLI in
// a terminal). The pre-rotation snapshot is worthless the moment that
// happens, so activation must preserve the NEWER bytes into the outgoing
// slot — preserving the snapshot instead was a guaranteed bricked login. The
// theoretical alternative (an external login as a different account landing
// inside this same instant) mislabels one slot and self-corrects through the
// identity reconcile on that slot's next activation; a lost rotation never
// self-heals.
func TestActivateWithSnapshotPreservesRotationRacingActivation(t *testing.T) {
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
	if err := os.WriteFile(active, []byte("first-rotated-mid-switch"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.ActivateWithSnapshot("claude", "first", "second", &snapshot); err != nil {
		t.Fatal(err)
	}
	first, _ := credentials.AccountCredentialPath("claude", "first")
	assertFileContents(t, first, "first-rotated-mid-switch")
	assertFileContents(t, active, "second-token")
}

// The provider's sign-out husk is the one racing write that must NOT be
// preserved: it is not a credential, and overwriting the outgoing slot with
// it would destroy the slot's last saved pair for nothing.
func TestActivateWithSnapshotDoesNotPreserveRacingSignedOutHusk(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials.SetSignedOutDetector(func(providerName string, data []byte) bool {
		return providerName == "claude" && string(data) == "husk"
	})
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
	if err := os.WriteFile(active, []byte("husk"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.ActivateWithSnapshot("claude", "first", "second", &snapshot); err != nil {
		t.Fatal(err)
	}
	first, _ := credentials.AccountCredentialPath("claude", "first")
	assertFileContents(t, first, "verified-first")
	assertFileContents(t, active, "second-token")
}

// Activate called with a canonical-holder ID (the rollback shape) treats a
// husk canonical as "nothing to preserve" and still completes the
// reinstatement, rather than stamping the husk into the holder's slot.
func TestActivateTreatsSignedOutCanonicalAsNothingToPreserve(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials.SetSignedOutDetector(func(providerName string, data []byte) bool {
		return providerName == "claude" && string(data) == "husk"
	})
	if err := credentials.WriteAccountCredential("claude", "holder", []byte("holder-saved")); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("claude", "target", []byte("target-token")); err != nil {
		t.Fatal(err)
	}
	active, _ := credentials.ActiveCredentialPath("claude")
	if err := os.MkdirAll(filepath.Dir(active), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte("husk"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.Activate("claude", "holder", "target"); err != nil {
		t.Fatal(err)
	}
	holder, _ := credentials.AccountCredentialPath("claude", "holder")
	assertFileContents(t, holder, "holder-saved")
	assertFileContents(t, active, "target-token")
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

	pruned, err := credentials.PruneOrphanedAccounts(
		"codex",
		map[string]bool{"keep": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != "remove" {
		t.Fatalf("pruned = %v, want the orphaned slot reported", pruned)
	}
	assertFileContents(t, keptPath, `{"account":"keep"}`)
	assertFileContents(t, unrelated, "leave untouched")
	if _, err := credentials.ReadCredential("codex", "remove", false); !IsCredentialMissing(err) {
		t.Fatalf("orphaned credential error = %v, want missing", err)
	}
}

// An empty keep-set must prune nothing: zero registered accounts is
// indistinguishable from a process reading a metadata store that these slots
// do not belong to (a fresh --data-dir, a test overriding the config root but
// not the home), and the slots hold single-use refresh tokens that cannot be
// recovered once deleted. This is the guard against the 2026-07-29 incident,
// where every `go test ./...` run wiped the developer's real saved logins.
func TestPruneOrphanedAccountsRefusesEmptyKeepSet(t *testing.T) {
	credentials, err := NewCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{"first", "second"} {
		if err := credentials.WriteAccountCredential(
			"claude",
			accountID,
			[]byte(`{"account":"`+accountID+`"}`),
		); err != nil {
			t.Fatal(err)
		}
	}

	for _, keep := range []map[string]bool{nil, {}} {
		pruned, err := credentials.PruneOrphanedAccounts("claude", keep)
		if err != nil {
			t.Fatalf("PruneOrphanedAccounts(%v) error = %v", keep, err)
		}
		if len(pruned) != 0 {
			t.Fatalf("PruneOrphanedAccounts(%v) pruned %v, want nothing", keep, pruned)
		}
	}
	for _, accountID := range []string{"first", "second"} {
		if _, err := credentials.ReadCredential("claude", accountID, false); err != nil {
			t.Fatalf("slot %s no longer readable after empty-keep-set prune: %v", accountID, err)
		}
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
	if _, err := credentials.PruneOrphanedAccounts("codex", map[string]bool{"keep": true}); err == nil {
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
