package provideraccounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const claudeIdentityConfig = `{
  "numStartups": 7,
  "oauthAccount": {"accountUuid": "acct-1", "emailAddress": "one@example.com"},
  "userID": "user-hash"
}`

// claudeIdentityFixture builds a canonical Claude home holding an
// identity, an active credential, and one saved account slot.
func claudeIdentityFixture(t *testing.T, savedAccountID string) (*Credentials, string) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("darwin stores Claude credentials in the Keychain, not the config home")
	}
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	claudeHome := filepath.Join(userHome, ".claude")
	if err := os.MkdirAll(claudeHome, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(userHome, ".claude.json")
	if err := os.WriteFile(configPath, []byte(claudeIdentityConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(claudeHome, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"current"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if savedAccountID != "" {
		if err := credentials.WriteAccountCredential(
			"claude",
			savedAccountID,
			[]byte(`{"claudeAiOauth":{"accessToken":"saved"}}`),
		); err != nil {
			t.Fatal(err)
		}
	}
	return credentials, configPath
}

func hasIdentity(t *testing.T, configPath string) bool {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	_, present := config["oauthAccount"]
	return present
}

// Switching accounts must retire the identity along with the
// credential. Leaving it behind is the split state that makes the CLI —
// and every identity probe run against it — report the previous account
// while running on the new account's tokens.
func TestActivateRetiresTheOutgoingIdentity(t *testing.T) {
	credentials, configPath := claudeIdentityFixture(t, "target")

	if err := credentials.Activate("claude", "current", "target"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if hasIdentity(t, configPath) {
		t.Fatal("identity survived an account switch")
	}
	active, err := credentials.ReadCredential("claude", "", true)
	if err != nil {
		t.Fatalf("read active credential: %v", err)
	}
	if string(active) != `{"claudeAiOauth":{"accessToken":"saved"}}` {
		t.Fatalf("active credential = %s", active)
	}
	// The outgoing account's rotated bytes still had to be preserved.
	preserved, err := credentials.ReadCredential("claude", "current", false)
	if err != nil {
		t.Fatalf("read preserved credential: %v", err)
	}
	if string(preserved) != `{"claudeAiOauth":{"accessToken":"current"}}` {
		t.Fatalf("preserved credential = %s", preserved)
	}
}

// Re-activating the account that is already selected changes no
// identity, so it must not force the provider into a needless profile
// refetch. Same for a same-account credential rotation.
func TestSameAccountActivationKeepsTheIdentity(t *testing.T) {
	credentials, configPath := claudeIdentityFixture(t, "same")

	if err := credentials.Activate("claude", "same", "same"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !hasIdentity(t, configPath) {
		t.Fatal("a same-account activation retired the identity")
	}

	if err := credentials.CommitSelectedCredential(
		"claude",
		"same",
		[]byte(`{"claudeAiOauth":{"accessToken":"rotated"}}`),
	); err != nil {
		t.Fatalf("CommitSelectedCredential: %v", err)
	}
	if !hasIdentity(t, configPath) {
		t.Fatal("a same-account credential rotation retired the identity")
	}
}

// Adopting a credential when nothing is selected yet still installs an
// account whose identity we cannot vouch for, so it retires too.
func TestActivationFromNoSelectionRetiresTheIdentity(t *testing.T) {
	credentials, configPath := claudeIdentityFixture(t, "target")

	if err := credentials.Activate("claude", "", "target"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if hasIdentity(t, configPath) {
		t.Fatal("identity survived activation from an empty selection")
	}
}

// A switch that cannot read the selected credential must leave both
// halves alone: aborting with the identity already retired would push
// the provider into a profile refetch for a switch that never happened.
func TestFailedActivationLeavesTheIdentityAlone(t *testing.T) {
	credentials, configPath := claudeIdentityFixture(t, "")

	if err := credentials.Activate("claude", "current", "missing"); err == nil {
		t.Fatal("expected activation to fail for an account with no credential")
	}
	if !hasIdentity(t, configPath) {
		t.Fatal("a failed activation retired the identity")
	}
}

// Signing out the last account removes the credential; the identity
// describes a login that no longer exists and goes with it.
func TestRemoveActiveRetiresTheIdentity(t *testing.T) {
	credentials, configPath := claudeIdentityFixture(t, "")

	if err := credentials.RemoveActive("claude"); err != nil {
		t.Fatalf("RemoveActive: %v", err)
	}
	if hasIdentity(t, configPath) {
		t.Fatal("identity survived sign-out")
	}
	if _, err := credentials.ReadCredential("claude", "", true); !IsCredentialMissing(err) {
		t.Fatalf("active credential survived sign-out: %v", err)
	}
}

// Codex carries its account claims inside auth.json, so replacing the
// credential replaces the identity. There is no second file to retire
// and no Claude config to touch.
func TestCodexActivationTouchesNoConfig(t *testing.T) {
	userHome := t.TempDir()
	credentials, err := NewCredentials(userHome, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(userHome, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential("codex", "target", []byte("saved")); err != nil {
		t.Fatal(err)
	}
	claudeConfig := filepath.Join(userHome, ".claude.json")
	if err := os.WriteFile(claudeConfig, []byte(claudeIdentityConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := credentials.Activate("codex", "current", "target"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !hasIdentity(t, claudeConfig) {
		t.Fatal("a Codex switch retired Claude's identity")
	}
}

// A temporary home is seeded with one specific account's credential.
// Copying the canonical identity in would tell the CLI running there it
// already knows who it is, so it would never derive the identity of the
// credential it actually holds — and a login there would be recorded
// against the canonical home's account.
func TestEphemeralClaudeHomeCopiesConfigWithoutTheIdentity(t *testing.T) {
	credentials, _ := claudeIdentityFixture(t, "")

	home, err := credentials.NewEphemeralHomeWithCredential(
		"claude",
		[]byte(`{"claudeAiOauth":{"accessToken":"probe"}}`),
		"probe-account",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := home.Cleanup(); err != nil {
			t.Fatal(err)
		}
	}()

	copiedPath := filepath.Join(home.Path, ".claude.json")
	if hasIdentity(t, copiedPath) {
		t.Fatal("temporary home inherited the canonical identity")
	}
	data, err := os.ReadFile(copiedPath)
	if err != nil {
		t.Fatalf("read copied config: %v", err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode copied config: %v", err)
	}
	// The rest of the config is what makes the temporary home behave
	// like the user's own — onboarding state, preferences.
	for _, key := range []string{"numStartups", "userID"} {
		if _, present := config[key]; !present {
			t.Fatalf("temporary home lost config key %q", key)
		}
	}
}
