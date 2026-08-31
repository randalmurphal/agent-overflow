package provideraccountapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provideraccounts"
)

func rotationTestCredentials(t *testing.T) (*provideraccounts.Credentials, string) {
	t.Helper()
	userHome := t.TempDir()
	credentials, err := provideraccounts.NewCredentials(userHome, CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return credentials, userHome
}

// The rotation watch is opt-in per probe, which makes forgetting it silent:
// the probe still works, it just goes back to killing the CLI mid-refresh and
// destroying the login. Nothing about a nil reader fails a build or a test, so
// this is the enforcement.
func TestClaudeProbeConfigAlwaysWiresTheRotationReader(t *testing.T) {
	credentials, userHome := rotationTestCredentials(t)
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Deps{})
	if err := manager.Attach(accounts, credentials, ""); err != nil {
		t.Fatal(err)
	}

	canonical := []byte(`{"claudeAiOauth":{"accessToken":"canonical","refreshToken":"r","expiresAt":1}}`)
	claudeHome := filepath.Join(userHome, ".claude")
	if err := os.MkdirAll(claudeHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeHome, ".credentials.json"), canonical, 0o600); err != nil {
		t.Fatal(err)
	}

	unpinned := manager.claudeProbeConfig("claude", nil)
	if unpinned.ReadCredential == nil {
		t.Fatal("an unpinned Claude probe was built with no rotation reader")
	}
	data, err := unpinned.ReadCredential()
	if err != nil {
		t.Fatalf("unpinned reader: %v", err)
	}
	if string(data) != string(canonical) {
		t.Fatalf("unpinned reader read %q, want the canonical credential", data)
	}

	// A pinned probe runs the CLI against a different home, and the pin beats
	// ProbeAccount's UnsetEnv — so the reader has to follow the pin or it
	// would watch a file the spawned CLI never touches.
	pinnedHome := t.TempDir()
	pinned := []byte(`{"claudeAiOauth":{"accessToken":"pinned","refreshToken":"r","expiresAt":2}}`)
	if err := os.WriteFile(filepath.Join(pinnedHome, ".credentials.json"), pinned, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := manager.claudeProbeConfig("claude", map[string]string{"CLAUDE_CONFIG_DIR": pinnedHome})
	if cfg.ReadCredential == nil {
		t.Fatal("a pinned Claude probe was built with no rotation reader")
	}
	data, err = cfg.ReadCredential()
	if err != nil {
		t.Fatalf("pinned reader: %v", err)
	}
	if string(data) != string(pinned) {
		t.Fatalf("pinned reader read %q, want the pinned home's credential", data)
	}
}

// claudeProbeConfig is the only constructor allowed to build one, because it
// is what guarantees the reader above is wired. A literal anywhere else opts
// that probe out of the protection with nothing to notice.
func TestClaudeProbeConfigIsTheOnlyProbeConstructor(t *testing.T) {
	const allowed = "env.go"
	// Assembled rather than written out, so this file does not match itself.
	needle := "claude.ProbeConfig" + "{"
	skipDirs := map[string]bool{
		".git": true, ".claude": true, "node_modules": true,
		"frontend": true, "docs": true, "dist": true, "build": true,
	}
	walkErr := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || filepath.Base(path) == allowed {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(source), needle) {
			t.Errorf(
				"%s builds a %s literal; use (*Manager).claudeProbeConfig so the probe carries "+
					"the rotation reader that keeps it from destroying the login",
				path,
				needle,
			)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

func TestProviderCredentialChainPositionReadsOnlyClaudeOAuth(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		data     string
		want     int64
		wantOK   bool
	}{
		{"claude oauth", "claude", `{"claudeAiOauth":{"expiresAt":1750000000000}}`, 1750000000000, true},
		{"codex has no single-use chain", "codex", `{"claudeAiOauth":{"expiresAt":1750000000000}}`, 0, false},
		{"husk", "claude", `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`, 0, false},
		{"api key shape", "claude", `{"other":true}`, 0, false},
		{"unparseable", "claude", "not json", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CredentialChainPosition(tc.provider, []byte(tc.data))
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("= (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
