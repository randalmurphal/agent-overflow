package provideraccounts

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestClaudeKeychainIdentityMatchesConfigScopedNativeService(t *testing.T) {
	t.Setenv("USER", "test-user")
	home := filepath.Join(t.TempDir(), ".claude", profileDirectoryName, "account-one")
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

func TestPrepareLoginHomeSharesStateButNotCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior is platform-policy dependent on Windows")
	}
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	sharedHome := filepath.Join(userHome, ".codex")
	if err := os.MkdirAll(filepath.Join(sharedHome, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedHome, "config.toml"), []byte("model = \"x\""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedHome, "auth.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	profileHome, err := credentials.PrepareLoginHome("codex", "account-one")
	if err != nil {
		t.Fatalf("PrepareLoginHome() error = %v", err)
	}
	for _, name := range []string{"sessions", "config.toml"} {
		info, err := os.Lstat(filepath.Join(profileHome, name))
		if err != nil {
			t.Fatalf("Lstat(%s): %v", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not shared by symlink", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(profileHome, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("profile auth.json exists before login: %v", err)
	}
}

func TestActivatePreservesRotatedCurrentCredentials(t *testing.T) {
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if _, err := credentials.PrepareLoginHome("claude", id); err != nil {
			t.Fatal(err)
		}
	}
	active, _ := credentials.ActiveCredentialPath("claude")
	first, _ := credentials.ProfileCredentialPath("claude", "first")
	second, _ := credentials.ProfileCredentialPath("claude", "second")
	if err := os.WriteFile(active, []byte("first-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("first-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.Activate("claude", "first", "second"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	assertFileContents(t, first, "first-rotated")
	assertFileContents(t, active, "second-token")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(active)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("active credential mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestActivateRejectsCredentialSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior is platform-policy dependent on Windows")
	}
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.PrepareLoginHome("codex", "target"); err != nil {
		t.Fatal(err)
	}
	target, _ := credentials.ProfileCredentialPath("codex", "target")
	other := filepath.Join(userHome, "outside-secret")
	if err := os.WriteFile(other, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, target); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Activate("codex", "", "target"); err == nil {
		t.Fatal("Activate() accepted a symlink credential file")
	}
}

func TestPrepareLoginHomeRejectsProfileSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior is platform-policy dependent on Windows")
	}
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	profileHome, err := credentials.ProfileHome("codex", "target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(profileHome), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(userHome, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, profileHome); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.PrepareLoginHome("codex", "target"); err == nil {
		t.Fatal("PrepareLoginHome() accepted a profile-home symlink")
	}
}

func TestReconcileProfileMovesNewStateIntoSharedHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior is platform-policy dependent on Windows")
	}
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	profileHome, err := credentials.PrepareLoginHome("codex", "account-one")
	if err != nil {
		t.Fatal(err)
	}
	privateState := filepath.Join(profileHome, "new-provider-state")
	if err := os.WriteFile(privateState, []byte("shared-value"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.ReconcileProfile("codex", "account-one"); err != nil {
		t.Fatalf("ReconcileProfile() error = %v", err)
	}
	info, err := os.Lstat(privateState)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink after reconciliation", privateState)
	}
	assertFileContents(t, filepath.Join(userHome, ".codex", "new-provider-state"), "shared-value")
}

func TestCodexActivatePreservesNewestCredentialCopy(t *testing.T) {
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if _, err := credentials.PrepareLoginHome("codex", id); err != nil {
			t.Fatal(err)
		}
	}
	active, _ := credentials.ActiveCredentialPath("codex")
	first, _ := credentials.ProfileCredentialPath("codex", "first")
	second, _ := credentials.ProfileCredentialPath("codex", "second")
	if err := os.WriteFile(active, []byte("canonical-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("profile-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(active, old, old); err != nil {
		t.Fatal(err)
	}

	if err := credentials.Activate("codex", "first", "second"); err != nil {
		t.Fatalf("Activate(profile newer) error = %v", err)
	}
	assertFileContents(t, first, "profile-rotated")

	if err := os.WriteFile(active, []byte("canonical-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(first, old, old); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Activate("codex", "first", "second"); err != nil {
		t.Fatalf("Activate(canonical newer) error = %v", err)
	}
	assertFileContents(t, first, "canonical-rotated")
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
