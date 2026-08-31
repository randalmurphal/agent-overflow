package providerdiscoveryapp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/codexmodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/settings"
)

func testCaches() *Caches {
	return &Caches{
		Claude:      provider.NewProbeCache(time.Minute),
		Codex:       provider.NewProbeCache(time.Minute),
		CodexModels: codexmodels.New(),
	}
}

func testProbeKey(_ string, binary, accountID string) provider.ProbeCacheKey {
	return provider.ProbeCacheKey{Binary: binary, AccountID: accountID, WorkDir: "/tmp"}
}

func cachedProbeRunner(request AccountProbeRequest) (provider.AccountInfo, error) {
	if cached, ok := request.Cache.Get(request.Key); ok {
		return cached, nil
	}
	info, err := request.Probe(context.Background())
	if err != nil {
		return provider.AccountInfo{}, err
	}
	request.Cache.Set(request.Key, info)
	if request.AfterAdopt != nil {
		request.AfterAdopt(provideraccounts.Account{ID: "adopted-account"})
	}
	return info, nil
}

func TestClaudeProbeCachesAndRecheckInvalidates(t *testing.T) {
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	var calls atomic.Int32
	service := New(Deps{
		ProviderBinary:  func(string) string { return "/mock/claude" },
		Selection:       func(string) AccountSelection { return AccountSelection{AccountID: "account"} },
		ProbeKey:        testProbeKey,
		RunAccountProbe: cachedProbeRunner,
		ClaudeConfig:    func(string) claude.ProbeConfig { return claude.ProbeConfig{} },
		ProbeClaude: func(context.Context, claude.ProbeConfig) (provider.AccountInfo, error) {
			call := calls.Add(1)
			return provider.AccountInfo{SubscriptionType: string(rune('0' + call))}, nil
		},
	}, testCaches())

	first, err := service.ProbeClaudeAccount()
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ProbeClaudeAccount()
	if err != nil {
		t.Fatal(err)
	}
	if first != second || calls.Load() != 1 {
		t.Fatalf("cached probes = (%+v, %+v), calls = %d", first, second, calls.Load())
	}
	refreshed, err := service.RecheckClaudeAccount()
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.SubscriptionType != "2" || calls.Load() != 2 {
		t.Fatalf("recheck = %+v, calls = %d", refreshed, calls.Load())
	}
}

func TestCodexProbePublishesAdoptedSnapshotOnlyOnMiss(t *testing.T) {
	var calls atomic.Int32
	var published []provider.RateLimitsSnapshot
	service := New(Deps{
		ProviderBinary:  func(string) string { return "/mock/codex" },
		Selection:       func(string) AccountSelection { return AccountSelection{} },
		ProbeKey:        testProbeKey,
		RunAccountProbe: cachedProbeRunner,
		CodexConfig:     func(string) codex.ProbeConfig { return codex.ProbeConfig{} },
		ProbeCodex: func(_ context.Context, cfg codex.ProbeConfig) (provider.AccountInfo, error) {
			calls.Add(1)
			cfg.OnSnapshot(provider.RateLimitsSnapshot{
				Provider: string(provider.Codex),
				Limits:   []provider.RateLimitEntry{{WindowMins: 300, UsedPercent: 42}},
			})
			return provider.AccountInfo{SubscriptionType: "pro"}, nil
		},
		EmitRateLimits: func(snapshot provider.RateLimitsSnapshot) {
			published = append(published, snapshot)
		},
	}, testCaches())

	if _, err := service.ProbeCodexAccount(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ProbeCodexAccount(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(published) != 1 {
		t.Fatalf("calls = %d, published = %+v", calls.Load(), published)
	}
	if published[0].AccountID != "adopted-account" || published[0].Limits[0].UsedPercent != 42 {
		t.Fatalf("published snapshot = %+v", published[0])
	}
}

func TestCustomEnvChangeInvalidatesOldAndNewIdentities(t *testing.T) {
	settingsService := settings.NewService(t.TempDir())
	caches := testCaches()
	keyForEnv := func(binary, accountID string, env map[string]string) provider.ProbeCacheKey {
		return provider.ProbeCacheKey{
			Binary: binary, AccountID: accountID, WorkDir: "/tmp",
			EnvFingerprint: provider.EnvFingerprint(env),
		}
	}
	service := New(Deps{
		CurrentSettings: settingsService.Get,
		SettingsService: func() *settings.Service { return settingsService },
		ProviderBinary:  func(string) string { return "/mock/claude" },
		Selection:       func(string) AccountSelection { return AccountSelection{AccountID: "account"} },
		ProbeKeyForEnv:  keyForEnv,
		ProbeKey: func(_ string, binary, accountID string) provider.ProbeCacheKey {
			return keyForEnv(binary, accountID, settingsService.Get().ProviderEnvMap(string(provider.Claude)))
		},
	}, caches)

	oldKey := keyForEnv("/mock/claude", "account", nil)
	newKey := keyForEnv("/mock/claude", "account", map[string]string{"HTTPS_PROXY": "http://proxy.test"})
	caches.Claude.Set(oldKey, provider.AccountInfo{Email: "old@example.test"})
	caches.Claude.Set(newKey, provider.AccountInfo{Email: "new@example.test"})
	if _, err := service.SetProviderCustomEnvVar("claude", "HTTPS_PROXY", "http://proxy.test", false); err != nil {
		t.Fatal(err)
	}
	if _, hit := caches.Claude.Get(oldKey); hit {
		t.Fatal("old environment cache entry survived")
	}
	if _, hit := caches.Claude.Get(newKey); hit {
		t.Fatal("new environment cache entry survived")
	}
}

func TestProviderStatusesEmitsOnlyFailures(t *testing.T) {
	var emitted []providerstatus.Event
	service := New(Deps{
		CurrentSettings: func() settings.Settings {
			return settings.Settings{ClaudeBinaryPath: "claude", CodexBinaryPath: "codex"}
		},
		DetectProvider: func(name, _ string) provider.ProviderStatus {
			status := "ready"
			if name == string(provider.Codex) {
				status = "missing"
			}
			return provider.ProviderStatus{Provider: name, Status: status}
		},
		EmitStatus: func(event providerstatus.Event) { emitted = append(emitted, event) },
	}, testCaches())

	statuses := service.ProviderStatuses()
	if len(statuses) != 2 || len(emitted) != 1 || emitted[0].Provider != string(provider.Codex) {
		t.Fatalf("statuses = %+v, emitted = %+v", statuses, emitted)
	}
}

func TestDefaultCachesConcurrentAccessReturnsOneSet(t *testing.T) {
	ResetDefaultCachesForTest()
	const callers = 32
	results := make(chan *Caches, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- DefaultCaches()
		}()
	}
	group.Wait()
	close(results)
	var first *Caches
	for result := range results {
		if first == nil {
			first = result
		}
		if result != first {
			t.Fatal("DefaultCaches returned more than one cache set")
		}
	}
}
