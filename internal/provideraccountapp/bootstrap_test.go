package provideraccountapp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

const crashHomeEnv = "AO_TEST_CRASHED_CODEX_PROBE_HOME"

// codexAuthFile is a ChatGPT-mode auth.json for one login at one point on
// its rotation chain.
func codexAuthFile(t *testing.T, user, refreshToken string, lastRefresh time.Time) []byte {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{
		"chatgpt_account_id": "ws-1",
		"chatgpt_user_id":    user,
	}})
	if err != nil {
		t.Fatal(err)
	}
	segment := base64.RawURLEncoding.EncodeToString
	data, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      segment([]byte(`{"alg":"none"}`)) + "." + segment(claims) + "." + segment([]byte("sig")),
			"access_token":  "access-" + refreshToken,
			"refresh_token": refreshToken,
		},
		"last_refresh": lastRefresh.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestCrashedCodexProbeHelper is the process a crash kills: it opens a
// selected-account probe home, rotates the credential in it the way the
// app-server does, and exits without cleaning up. It runs only when
// TestBootRecoversTheSelectedCodexAccountsRotation starts it.
func TestCrashedCodexProbeHelper(t *testing.T) {
	userHome := os.Getenv(crashHomeEnv)
	if userHome == "" {
		t.Skip("helper process only")
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(userHome, CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := credentials.ReadCredentialSnapshot(string(provider.Codex), "", true)
	if err != nil {
		t.Fatal(err)
	}
	home, err := credentials.NewEphemeralHomeWithCredential(string(provider.Codex), seed.Bytes(), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	rotated := codexAuthFile(t, "user-1", "rt-2", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	if err := os.WriteFile(filepath.Join(home.Path, "auth.json"), rotated, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("home=" + home.Path + "\n")
	os.Exit(0)
}

// A crash right after the selected Codex account's probe rotated its
// single-use refresh token leaves the only live token in the probe's home,
// with the canonical credential and the slot both on the consumed one. The
// next boot, however soon, puts the rotation back in both: the owner's
// death released its lease, and the canonical store still held that login
// behind the orphan.
func TestBootRecoversTheSelectedCodexAccountsRotation(t *testing.T) {
	userHome := t.TempDir()
	dbDir := t.TempDir()
	store, err := provideraccounts.NewStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(userHome, CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Deps{})
	if err := manager.Attach(store, credentials, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: "acct-1", Provider: string(provider.Codex), Email: "user@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	consumed := codexAuthFile(t, "user-1", "rt-1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err := credentials.WriteAccountCredential(string(provider.Codex), "acct-1", consumed); err != nil {
		t.Fatal(err)
	}
	if err := credentials.CommitSelectedCredential(string(provider.Codex), "acct-1", consumed, consumed); err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestCrashedCodexProbeHelper$", "-test.count=1")
	helper.Env = append(os.Environ(), crashHomeEnv+"="+userHome)
	var stdout, stderr bytes.Buffer
	helper.Stdout, helper.Stderr = &stdout, &stderr
	if err := helper.Run(); err != nil {
		t.Fatalf("crashing helper: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	_, after, found := strings.Cut(stdout.String(), "home=")
	homePath, _, _ := strings.Cut(after, "\n")
	if !found || homePath == "" {
		t.Fatalf("helper reported no home:\n%s", stdout.String())
	}
	t.Cleanup(func() { _ = os.RemoveAll(homePath) })

	if err := manager.ReconcileProviderHome(dbDir, userHome); err != nil {
		t.Fatal(err)
	}
	rotated := codexAuthFile(t, "user-1", "rt-2", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	canonical, err := credentials.ReadCredential(string(provider.Codex), "", true)
	if err != nil || !bytes.Equal(canonical, rotated) {
		t.Fatalf("canonical credential not restored to the rotation (err %v)", err)
	}
	slot, err := credentials.ReadCredential(string(provider.Codex), "acct-1", false)
	if err != nil || !bytes.Equal(slot, rotated) {
		t.Fatalf("slot not restored to the rotation (err %v)", err)
	}
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Fatalf("crashed probe home survived the boot sweep: %v", err)
	}
}
