package provideraccountapp

import (
	"maps"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/settings"
)

func newTestManager(t *testing.T) (*Manager, *provideraccounts.Store, *provideraccounts.Credentials) {
	t.Helper()
	store, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(t.TempDir(), CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Deps{})
	if err := manager.Attach(store, credentials, ""); err != nil {
		t.Fatal(err)
	}
	return manager, store, credentials
}

func TestManagerSelectionProjectsOwnedStore(t *testing.T) {
	manager, store, _ := newTestManager(t)
	account, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: "selected", Provider: string(provider.Claude), Email: "selected@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	selection := manager.Selection(string(provider.Claude))
	if selection.AccountID != account.ID || selection.Account.Email != account.Email {
		t.Fatalf("Selection() = %+v, want selected account", selection)
	}
	if selection.Generation != store.Generation(string(provider.Claude)) {
		t.Fatalf("generation = %d, want %d", selection.Generation, store.Generation(string(provider.Claude)))
	}
}

func TestManagerSelectionLeaseOrdersActivationWrite(t *testing.T) {
	manager, _, _ := newTestManager(t)
	lease := manager.SelectionLease(string(provider.Codex))
	if manager.mu.TryLock() {
		manager.mu.Unlock()
		t.Fatal("activation write lock acquired while selection lease was held")
	}
	lease.Release()
	lease.Release()
	if !manager.mu.TryLock() {
		t.Fatal("activation write lock remained held after selection lease release")
	}
	manager.mu.Unlock()
}

// A sign-in spawn stacks three layers, and the order is the whole point: the
// user's configuration picks which backend the person authenticates against,
// the boot-mode layer is a contract settings must not be able to reconfigure,
// and the home pin outranks both because the credential must land somewhere
// the adoption epilogue can still decide about.
func TestLoginSpawnEnvStacksConfigurationUnderBootModeUnderThePin(t *testing.T) {
	manager := NewManager(Deps{
		CurrentSettings: func() settings.Settings {
			return settings.Settings{ClaudeCustomEnv: []settings.ProviderEnvVar{
				{Name: "ANTHROPIC_BASE_URL", Value: "https://backend.example.test"},
				{Name: "SHARED", Value: "configured"},
			}}
		},
		LoginSpawnEnv: func() map[string]string {
			return map[string]string{"SHARED": "boot", "AO_HARNESS_CONTROL": "127.0.0.1:1"}
		},
	})

	env := manager.providerLoginEnv(string(provider.Claude), map[string]string{"CLAUDE_CONFIG_DIR": "/pinned", "SHARED": "pin"})
	for name, want := range map[string]string{
		"ANTHROPIC_BASE_URL": "https://backend.example.test",
		"AO_HARNESS_CONTROL": "127.0.0.1:1",
		"CLAUDE_CONFIG_DIR":  "/pinned",
		"SHARED":             "pin",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
}

// With nothing to inject the sign-in environment is the probe's, so the one
// merge rule stays one rule rather than two that agree by coincidence.
func TestLoginSpawnEnvWithoutABootLayerIsTheProbeEnvironment(t *testing.T) {
	manager := NewManager(Deps{
		CurrentSettings: func() settings.Settings {
			return settings.Settings{CodexCustomEnv: []settings.ProviderEnvVar{
				{Name: "OPENAI_BASE_URL", Value: "https://backend.example.test"},
			}}
		},
	})

	pins := map[string]string{"CODEX_HOME": "/pinned"}
	got := manager.providerLoginEnv(string(provider.Codex), pins)
	want := manager.providerProbeEnv(string(provider.Codex), pins)
	if !maps.Equal(got, want) {
		t.Fatalf("providerLoginEnv = %v, want the probe environment %v", got, want)
	}
}
