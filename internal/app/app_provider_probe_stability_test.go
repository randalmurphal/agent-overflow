package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// probeTestWorkDir stands in for the pinned probe directory. Absolute via
// os.TempDir because filepath.IsAbs is OS-specific.
var probeTestWorkDir = os.TempDir()

func TestRunAccountProbeRetriesOneCredentialRotationAndAdoptsMatchingIdentity(t *testing.T) {
	app, activePath := newStableProbeTestApp(t, []byte("credential-one"))
	cache := provider.NewProbeCache(time.Minute)
	var calls atomic.Int32

	info, err := app.runAccountProbe(providerProbeRunner{
		ProviderName: string(provider.Codex),
		Cache:        cache,
		Key:          provider.ProbeCacheKey{Binary: "codex-test", WorkDir: probeTestWorkDir},
		Probe: func(context.Context) (provider.AccountInfo, error) {
			call := calls.Add(1)
			if call == 1 {
				if err := os.WriteFile(activePath, []byte("credential-two"), 0o600); err != nil {
					return provider.AccountInfo{}, err
				}
				return provider.AccountInfo{Email: "discarded@example.com"}, nil
			}
			return provider.AccountInfo{Email: "matched@example.com"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || info.Email != "matched@example.com" {
		t.Fatalf("probe result = %+v after %d calls", info, calls.Load())
	}
	account, ok := app.providerAccounts.Active(string(provider.Codex), time.Now())
	if !ok || account.Email != "matched@example.com" {
		t.Fatalf("adopted account = %+v, ok=%v", account, ok)
	}
	saved, err := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Codex),
		account.ID,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != "credential-two" {
		t.Fatalf("saved credential = %q, want credential-two", saved)
	}
}

func TestRunAccountProbeRejectsRepeatedCredentialChangesWithoutCachingOrAdopting(t *testing.T) {
	app, activePath := newStableProbeTestApp(t, []byte("credential-one"))
	cache := provider.NewProbeCache(time.Minute)
	var (
		calls atomic.Int32
		emits atomic.Int32
	)
	app.testEmitHook = func(string, any) {
		emits.Add(1)
	}

	_, err := app.runAccountProbe(providerProbeRunner{
		ProviderName: string(provider.Codex),
		Cache:        cache,
		Key:          provider.ProbeCacheKey{Binary: "codex-test", WorkDir: probeTestWorkDir},
		Probe: func(context.Context) (provider.AccountInfo, error) {
			next := "credential-two"
			if calls.Add(1) == 2 {
				next = "credential-three"
			}
			if err := os.WriteFile(activePath, []byte(next), 0o600); err != nil {
				return provider.AccountInfo{}, err
			}
			return provider.AccountInfo{Email: next + "@example.com"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("probe error = %v, want repeated-change rejection", err)
	}
	if _, hit := cache.Get(provider.ProbeCacheKey{Binary: "codex-test", WorkDir: probeTestWorkDir}); hit {
		t.Fatal("failed identity probe populated the cache")
	}
	if accounts := app.providerAccounts.List(string(provider.Codex), time.Now()); len(accounts) != 0 {
		t.Fatalf("failed identity probe adopted accounts: %+v", accounts)
	}
	if emits.Load() != 0 {
		t.Fatalf("failed identity probe emitted %d events", emits.Load())
	}
}

func newStableProbeTestApp(
	t *testing.T,
	initialCredential []byte,
) (*App, string) {
	t.Helper()
	app := newTestAppWithStore(t)
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials := newTestProviderCredentials(t, t.TempDir())
	activePath, err := credentials.ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(activePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activePath, initialCredential, 0o600); err != nil {
		t.Fatal(err)
	}
	attachProviderAccountStoresForTest(t, app, accounts, credentials)
	return app, activePath
}
