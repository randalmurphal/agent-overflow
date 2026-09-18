package providerdiscoveryapp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/claudemodels"
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

// The runner emits `provider:account` right after AfterAdopt, and clients
// refresh the model catalog on that event; the wire catalog must be readable
// by then, and not before the identity is accepted.
func TestClaudeProbeCommitsWireCatalogInAfterAdopt(t *testing.T) {
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	key := testProbeKey("", "/mock/claude", "account")
	var enrichedBeforeAdopt, enrichedAfterAdopt bool
	service := New(Deps{
		ProviderBinary: func(string) string { return "/mock/claude" },
		Selection:      func(string) AccountSelection { return AccountSelection{AccountID: "account"} },
		ProbeKey:       testProbeKey,
		RunAccountProbe: func(request AccountProbeRequest) (provider.AccountInfo, error) {
			info, err := request.Probe(context.Background())
			if err != nil {
				return provider.AccountInfo{}, err
			}
			enrichedBeforeAdopt = hasModel(catalogModels(key), "claude-newthing-1")
			request.AfterAdopt(provideraccounts.Account{ID: "account"})
			enrichedAfterAdopt = hasModel(catalogModels(key), "claude-newthing-1")
			return info, nil
		},
		ClaudeConfig: func(string) claude.ProbeConfig { return claude.ProbeConfig{} },
		ProbeClaude: func(_ context.Context, cfg claude.ProbeConfig) (provider.AccountInfo, error) {
			cfg.OnModels([]claude.WireModel{{Value: "claude-newthing-1", DisplayName: "Newthing"}}, nil)
			return provider.AccountInfo{SubscriptionType: "max"}, nil
		},
	}, testCaches())

	if _, err := service.ProbeClaudeAccount(); err != nil {
		t.Fatal(err)
	}
	if enrichedBeforeAdopt {
		t.Error("wire catalog was committed before the identity was adopted")
	}
	if !enrichedAfterAdopt {
		t.Error("wire catalog was not committed by the time AfterAdopt returned")
	}
}

func catalogModels(key provider.ProbeCacheKey) []provider.ModelInfo {
	models, _ := claudecatalog.Models(key, string(provider.Claude))
	return models
}

func hasModel(models []provider.ModelInfo, slug string) bool {
	for _, model := range models {
		if model.Slug == slug {
			return true
		}
	}
	return false
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

// TestModelsForProviderStampsProvenance: a cold catalog and a probed one look
// identical on the wire without this, so a client cannot tell "nobody has
// asked the binary yet" from "the binary answered".
func TestModelsForProviderStampsProvenance(t *testing.T) {
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	key := testProbeKey("", "/mock/claude", "account")
	service := New(Deps{
		ProviderBinary: func(string) string { return "/mock/claude" },
		Selection:      func(string) AccountSelection { return AccountSelection{AccountID: "account"} },
		ProbeKey:       testProbeKey,
	}, testCaches())

	before, err := service.ModelsForProvider(context.Background(), string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if before.Provenance != provider.CatalogShipped {
		t.Errorf("provenance before any probe = %q, want shipped", before.Provenance)
	}
	if len(before.Models) == 0 {
		t.Error("the shipped answer must still carry the shipped models")
	}

	var capture claudecatalog.ModelCapture
	capture.Capture([]claude.WireModel{{Value: "claude-newthing-1"}}, nil)
	capture.Store(key)

	after, err := service.ModelsForProvider(context.Background(), string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	if after.Provenance != provider.CatalogProbed {
		t.Errorf("provenance after a probe = %q, want probed", after.Provenance)
	}
	if !hasModel(after.Models, "claude-newthing-1") {
		t.Errorf("models = %v, want the probe's wire-only model", after.Models)
	}

	// claude-tui shares Claude's binary and login, so it shares the answer
	// and its provenance.
	static, err := service.ModelsForProvider(context.Background(), string(provider.ClaudeTUI))
	if err != nil {
		t.Fatal(err)
	}
	if static.Provenance != provider.CatalogProbed {
		t.Errorf("claude-tui provenance = %q, want probed", static.Provenance)
	}
	// A provider with neither a live nor a probe-enriched catalog is the
	// shipped list and says so.
	unknown, err := service.ModelsForProvider(context.Background(), "unknown")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Provenance != provider.CatalogShipped || unknown.Models != nil {
		t.Errorf("unknown provider = %+v, want a shipped, empty answer", unknown)
	}
}

// Codex's list is the app-server's own answer, so it is live — and an error
// stays an error rather than degrading into a shipped-looking catalog.
func TestModelsForProviderReportsCodexLive(t *testing.T) {
	caches := testCaches()
	caches.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{{Slug: "gpt-5.5", Name: "GPT-5.5", Provider: "codex"}}, nil
	}, time.Now)
	service := New(Deps{
		ProviderBinary: func(string) string { return "/mock/codex" },
	}, caches)

	catalog, err := service.ModelsForProvider(context.Background(), string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Provenance != provider.CatalogLive {
		t.Errorf("codex provenance = %q, want live", catalog.Provenance)
	}
	if !hasModel(catalog.Models, "gpt-5.5") {
		t.Errorf("models = %v, want the live answer", catalog.Models)
	}

	// A failed `model/list` is a failure, not a shipped-looking catalog: the
	// picker must show the error rather than a list nobody vouched for.
	failing := testCaches()
	failing.CodexModels = codexmodels.NewWith(time.Minute, func(context.Context, string) ([]provider.ModelInfo, error) {
		return nil, errors.New("app-server refused")
	}, time.Now)
	broken := New(Deps{ProviderBinary: func(string) string { return "/mock/codex" }}, failing)
	answer, err := broken.ModelsForProvider(context.Background(), string(provider.Codex))
	if err == nil {
		t.Fatalf("ModelsForProvider = %+v, want the lister's error", answer)
	}
	if answer.Provenance != "" || answer.Models != nil {
		t.Errorf("failed answer = %+v, want the zero catalog", answer)
	}
}

// The probe is what fills the catalog, so it is also what has something worth
// persisting. AfterAdopt hands the committed entry to the persistence dep,
// including the models earlier probes of this binary learned.
func TestClaudeProbePersistsTheCommittedCatalog(t *testing.T) {
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	key := testProbeKey("", "/mock/claude", "account")
	// A model an earlier probe of this binary taught the catalog, which this
	// probe's wire omits.
	var earlier claudecatalog.ModelCapture
	earlier.Capture([]claude.WireModel{{Value: "claude-oldthing-1"}}, nil)
	earlier.Store(key)

	var gotAccountID string
	var got claudemodels.Snapshot
	var calls int
	service := New(Deps{
		ProviderBinary:  func(string) string { return "/mock/claude" },
		Selection:       func(string) AccountSelection { return AccountSelection{AccountID: "account"} },
		ProbeKey:        testProbeKey,
		RunAccountProbe: cachedProbeRunner,
		ClaudeConfig:    func(string) claude.ProbeConfig { return claude.ProbeConfig{} },
		ProbeClaude: func(_ context.Context, cfg claude.ProbeConfig) (provider.AccountInfo, error) {
			cfg.OnModels([]claude.WireModel{{Value: "claude-newthing-1"}}, nil)
			return provider.AccountInfo{SubscriptionType: "max"}, nil
		},
		RememberClaudeCatalog: func(accountID string, snapshot claudemodels.Snapshot) {
			calls++
			gotAccountID = accountID
			got = snapshot
		},
	}, testCaches())

	if _, err := service.ProbeClaudeAccount(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("RememberClaudeCatalog called %d times, want 1", calls)
	}
	// The ADOPTED account, not the selection the key was built from: a first
	// probe runs with no selection and the account it adopts is the one the
	// next boot probes as.
	if gotAccountID != "adopted-account" {
		t.Errorf("account id = %q, want the adopted account", gotAccountID)
	}
	if len(got.Wire) != 1 || got.Wire[0].Value != "claude-newthing-1" {
		t.Errorf("persisted wire = %+v, want this probe's rows", got.Wire)
	}
	if !hasModel(got.Learned, "claude-newthing-1") || !hasModel(got.Learned, "claude-oldthing-1") {
		t.Errorf("persisted learned = %v, want both the new and the retained model", got.Learned)
	}
}

// A nil dep is a supported wiring (focused tests, and any boot where account
// metadata is unavailable), and must not turn a working probe into a panic.
func TestClaudeProbeWithoutPersistenceDepStillProbes(t *testing.T) {
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	service := New(Deps{
		ProviderBinary:  func(string) string { return "/mock/claude" },
		Selection:       func(string) AccountSelection { return AccountSelection{AccountID: "account"} },
		ProbeKey:        testProbeKey,
		RunAccountProbe: cachedProbeRunner,
		ClaudeConfig:    func(string) claude.ProbeConfig { return claude.ProbeConfig{} },
		ProbeClaude: func(_ context.Context, cfg claude.ProbeConfig) (provider.AccountInfo, error) {
			cfg.OnModels([]claude.WireModel{{Value: "claude-newthing-1"}}, nil)
			return provider.AccountInfo{SubscriptionType: "max"}, nil
		},
	}, testCaches())

	if _, err := service.ProbeClaudeAccount(); err != nil {
		t.Fatal(err)
	}
	if !hasModel(catalogModels(testProbeKey("", "/mock/claude", "account")), "claude-newthing-1") {
		t.Error("the probe must still commit its catalog without a persistence dep")
	}
}

// A probe that adopts nothing (no credential to attribute the answer to) still
// belongs to the selected account: that is the identity the key was built from
// and the one the next boot will probe as.
func TestClaudeProbePersistsUnderTheSelectionWhenAdoptionIsEmpty(t *testing.T) {
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
	var gotAccountID string
	service := New(Deps{
		ProviderBinary: func(string) string { return "/mock/claude" },
		Selection:      func(string) AccountSelection { return AccountSelection{AccountID: "selected"} },
		ProbeKey:       testProbeKey,
		RunAccountProbe: func(request AccountProbeRequest) (provider.AccountInfo, error) {
			info, err := request.Probe(context.Background())
			if err != nil {
				return provider.AccountInfo{}, err
			}
			request.AfterAdopt(provideraccounts.Account{})
			return info, nil
		},
		ClaudeConfig: func(string) claude.ProbeConfig { return claude.ProbeConfig{} },
		ProbeClaude: func(_ context.Context, cfg claude.ProbeConfig) (provider.AccountInfo, error) {
			cfg.OnModels([]claude.WireModel{{Value: "claude-newthing-1"}}, nil)
			return provider.AccountInfo{SubscriptionType: "max"}, nil
		},
		RememberClaudeCatalog: func(accountID string, _ claudemodels.Snapshot) {
			gotAccountID = accountID
		},
	}, testCaches())

	if _, err := service.ProbeClaudeAccount(); err != nil {
		t.Fatal(err)
	}
	if gotAccountID != "selected" {
		t.Errorf("account id = %q, want the selected account", gotAccountID)
	}
}
