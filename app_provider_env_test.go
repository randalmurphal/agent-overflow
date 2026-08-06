package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/settings"
)

// The settings layer cannot import internal/provider, so its reserved-name
// table is a copy. This is the cross-check that keeps the copy honest in both
// directions: a pin added to a spawn path that nobody deny-listed would be
// silently overridable, and a name deny-listed here that no spawn path pins
// would refuse a configuration for no reason.
func TestReservedEnvNamesMatchTheProviderPins(t *testing.T) {
	for _, providerName := range []string{
		string(provider.Claude),
		string(provider.ClaudeTUI),
		string(provider.Codex),
	} {
		t.Run(providerName, func(t *testing.T) {
			pinned := upperSet(provider.ReservedEnvNames(providerName))
			denied := upperSet(settings.ReservedProviderEnvNames(providerName))
			for name := range pinned {
				if _, ok := denied[name]; !ok {
					t.Errorf("%s pins %s but the settings deny-list does not reject it", providerName, name)
				}
			}
			for name := range denied {
				if _, ok := pinned[name]; !ok {
					t.Errorf("settings rejects %s for %s but no spawn path pins it", name, providerName)
				}
			}
			// Every pinned name is actually refused end to end, not merely
			// listed in a table.
			for name := range pinned {
				if err := settings.ValidateProviderEnvVarName(providerName, name); err == nil {
					t.Errorf("%s accepted the reserved name %s", providerName, name)
				}
			}
		})
	}
}

// claude-tui pins ANTHROPIC_BASE_URL on the child process, but reserving the
// name would break the feature's primary use case; the app routes it to the
// gateway upstream instead. Pin that the exception stays an exception.
func TestClaudeBaseURLIsConfigurableOnEveryClaudeSurface(t *testing.T) {
	for _, providerName := range []string{string(provider.Claude), string(provider.ClaudeTUI)} {
		if err := settings.ValidateProviderEnvVarName(providerName, claudetui.BaseURLEnv); err != nil {
			t.Fatalf("%s rejected %s: %v", providerName, claudetui.BaseURLEnv, err)
		}
	}
}

// Transition coverage: a variable that is set, then removed, must stop reaching
// the process. A spawn path that read a cached snapshot would keep injecting it.
func TestSessionProcessEnvFollowsCustomEnvAcrossTransitions(t *testing.T) {
	app := newTestAppWithStore(t)

	// Off: nothing injected.
	if got := app.sessionProcessEnv(string(provider.Claude), nil, aoSessionCredential{}); got["ANTHROPIC_BASE_URL"] != "" {
		t.Fatalf("unset custom env leaked %q", got["ANTHROPIC_BASE_URL"])
	}

	// On: reaches Claude and claude-tui, not Codex.
	if _, err := app.SetProviderCustomEnvVar("claude", "ANTHROPIC_BASE_URL", "https://gw.test", false); err != nil {
		t.Fatalf("SetProviderCustomEnvVar() error = %v", err)
	}
	for _, providerName := range []string{string(provider.Claude), string(provider.ClaudeTUI)} {
		got := app.sessionProcessEnv(providerName, nil, aoSessionCredential{})
		if got["ANTHROPIC_BASE_URL"] != "https://gw.test" {
			t.Fatalf("%s env = %#v, want the custom base URL", providerName, got)
		}
	}
	if got := app.sessionProcessEnv(string(provider.Codex), nil, aoSessionCredential{}); got["ANTHROPIC_BASE_URL"] != "" {
		t.Fatalf("Claude's custom env leaked into Codex: %#v", got)
	}

	// Codex keeps its own list.
	if _, err := app.SetProviderCustomEnvVar("codex", "HTTPS_PROXY", "http://proxy.test:8080", false); err != nil {
		t.Fatalf("SetProviderCustomEnvVar() error = %v", err)
	}
	if got := app.sessionProcessEnv(string(provider.Codex), nil, aoSessionCredential{}); got["HTTPS_PROXY"] != "http://proxy.test:8080" {
		t.Fatalf("codex env = %#v, want the proxy", got)
	}
	if got := app.sessionProcessEnv(string(provider.Claude), nil, aoSessionCredential{}); got["HTTPS_PROXY"] != "" {
		t.Fatalf("Codex's custom env leaked into Claude: %#v", got)
	}

	// Off again: the removal has to reach the next spawn.
	if _, err := app.DeleteProviderCustomEnvVar("claude", "ANTHROPIC_BASE_URL"); err != nil {
		t.Fatalf("DeleteProviderCustomEnvVar() error = %v", err)
	}
	for _, providerName := range []string{string(provider.Claude), string(provider.ClaudeTUI)} {
		got := app.sessionProcessEnv(providerName, nil, aoSessionCredential{})
		if _, present := got["ANTHROPIC_BASE_URL"]; present {
			t.Fatalf("%s kept the removed variable: %#v", providerName, got)
		}
	}
	// The other provider is untouched by the removal.
	if got := app.sessionProcessEnv(string(provider.Codex), nil, aoSessionCredential{}); got["HTTPS_PROXY"] == "" {
		t.Fatalf("removing a Claude variable cleared Codex's: %#v", got)
	}
}

// The custom environment sits above the provider config and below the two
// contracts. The reserved names make a real collision impossible, so this pins
// the layering itself rather than a reachable conflict.
func TestSessionProcessEnvPrecedenceAroundCustomEnv(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.SetProviderCustomEnvVar("claude", "SHARED", "user", false); err != nil {
		t.Fatal(err)
	}
	got := app.sessionProcessEnv(
		string(provider.Claude),
		map[string]string{"SHARED": "provider-config"},
		aoSessionCredential{},
	)
	if got["SHARED"] != "user" {
		t.Fatalf("SHARED = %q, want the user's value to outrank the provider config", got["SHARED"])
	}

	app.providerExtraEnv = map[string]string{"SHARED": "harness"}
	got = app.sessionProcessEnv(string(provider.Claude), nil, aoSessionCredential{
		env: map[string]string{aocli.EnvToken: "tok"},
	})
	if got["SHARED"] != "harness" {
		t.Fatalf("SHARED = %q, want the boot-mode override to outrank user settings", got["SHARED"])
	}
	if got[aocli.EnvToken] != "tok" {
		t.Fatalf("the ao credential did not survive: %#v", got)
	}
}

// An endpoint that changes which backend answers must change which backend the
// PROBE asks — otherwise the account banner reports one identity while every
// turn runs against another.
func TestProbeConfigsCarryCustomEnvWithPinsWinning(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.SetProviderCustomEnvVar("claude", "ANTHROPIC_BASE_URL", "https://gw.test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetProviderCustomEnvVar("codex", "HTTPS_PROXY", "http://proxy.test:8080", false); err != nil {
		t.Fatal(err)
	}

	claudeCfg := app.claudeProbeConfig("claude", nil)
	if claudeCfg.Env["ANTHROPIC_BASE_URL"] != "https://gw.test" {
		t.Fatalf("claude probe env = %#v, want the custom base URL", claudeCfg.Env)
	}
	if claudeCfg.WorkDir != providerProbeWorkDir() {
		t.Fatalf("claude probe workdir = %q, want the pinned probe home", claudeCfg.WorkDir)
	}

	codexCfg := app.codexProbeConfig("codex", nil)
	if codexCfg.Env["HTTPS_PROXY"] != "http://proxy.test:8080" {
		t.Fatalf("codex probe env = %#v, want the custom proxy", codexCfg.Env)
	}

	// A probe that pins a temporary credential home keeps it: the pin names
	// the account the probe exists to interrogate.
	pinned := app.codexProbeConfig("codex", map[string]string{"CODEX_HOME": "/tmp/ephemeral"})
	if pinned.Env["CODEX_HOME"] != "/tmp/ephemeral" {
		t.Fatalf("codex probe env = %#v, want the pinned home", pinned.Env)
	}
	if pinned.Env["HTTPS_PROXY"] != "http://proxy.test:8080" {
		t.Fatalf("codex probe env = %#v, want the custom proxy alongside the pin", pinned.Env)
	}

	// With nothing configured and nothing pinned, probes keep inheriting the
	// process environment rather than being handed an empty override map.
	clean := newTestAppWithStore(t)
	if env := clean.claudeProbeConfig("claude", nil).Env; env != nil {
		t.Fatalf("probe env = %#v, want nil when nothing is configured", env)
	}
}

func TestProbeCacheKeyIncludesCustomEnv(t *testing.T) {
	app := newTestAppWithStore(t)
	before := app.providerProbeCacheKey(string(provider.Claude), "claude")
	if _, err := app.SetProviderCustomEnvVar("claude", "ANTHROPIC_BASE_URL", "https://gw.test", false); err != nil {
		t.Fatal(err)
	}
	after := app.providerProbeCacheKey(string(provider.Claude), "claude")
	if before.String() == after.String() {
		t.Fatalf("probe cache key did not change with the custom environment: %q", after.String())
	}
	// The digest, not the value: a cache key outlives the call that built it.
	if strings.Contains(after.String(), "gw.test") {
		t.Fatalf("probe cache key embeds the environment verbatim: %q", after.String())
	}
}

// The new environment misses the cache by key. The entry cached under the OLD
// one has to go too, or flipping a variable back would serve a pre-change
// identity for the rest of the TTL.
func TestCustomEnvChangeEvictsBothProbeAnswers(t *testing.T) {
	resetClaudeProbeCacheForTest()
	app := newTestAppWithStore(t)

	beforeKey := app.providerProbeCacheKey(string(provider.Claude), app.providerBinaryPath(string(provider.Claude)))
	claudeAccountProbeCache().Set(beforeKey, provider.AccountInfo{Email: "old@example.test"})

	if _, err := app.SetProviderCustomEnvVar("claude", "ANTHROPIC_BASE_URL", "https://gw.test", false); err != nil {
		t.Fatal(err)
	}
	if _, hit := claudeAccountProbeCache().Get(beforeKey); hit {
		t.Fatal("the pre-change probe answer survived the environment change")
	}
	afterKey := app.providerProbeCacheKey(string(provider.Claude), app.providerBinaryPath(string(provider.Claude)))
	if _, hit := claudeAccountProbeCache().Get(afterKey); hit {
		t.Fatal("the new environment started from a cached answer")
	}

	// Same on the way back out.
	claudeAccountProbeCache().Set(afterKey, provider.AccountInfo{Email: "gw@example.test"})
	if _, err := app.DeleteProviderCustomEnvVar("claude", "ANTHROPIC_BASE_URL"); err != nil {
		t.Fatal(err)
	}
	if _, hit := claudeAccountProbeCache().Get(afterKey); hit {
		t.Fatal("removing the variable left its probe answer cached")
	}
	if _, hit := claudeAccountProbeCache().Get(beforeKey); hit {
		t.Fatal("removing the variable resurrected the pre-change probe answer")
	}
}

// The probe caches only hold entries keyed by the CANONICAL provider —
// claude-tui shares Claude's binary, account store, and custom environment,
// and never probes under its own name. A mutation named "claude-tui" (the
// bound method accepts it even though the settings UI happens to send
// "claude") must evict under Claude's active account: Store.Active is a raw
// map lookup, so resolving the account under "claude-tui" would come back
// empty and both evictions would miss the entries the probes actually use.
func TestClaudeTUINamedEnvChangeEvictsClaudeProbeAnswers(t *testing.T) {
	resetClaudeProbeCacheForTest()
	app := newTestAppWithStore(t)
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
		ID:       "acct-1",
		Provider: string(provider.Claude),
		Email:    "acct-1@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	app.providerAccounts = accounts

	beforeKey := app.providerProbeCacheKey(string(provider.Claude), app.providerBinaryPath(string(provider.Claude)))
	claudeAccountProbeCache().Set(beforeKey, provider.AccountInfo{Email: "old@example.test"})

	if _, err := app.SetProviderCustomEnvVar(string(provider.ClaudeTUI), "HTTPS_PROXY", "http://proxy.test:8080", false); err != nil {
		t.Fatal(err)
	}
	if _, hit := claudeAccountProbeCache().Get(beforeKey); hit {
		t.Fatal("a claude-tui-named environment change left Claude's probe answer cached")
	}
}

// claude-tui owns the child's ANTHROPIC_BASE_URL (it must point at the
// per-session gateway), so a user endpoint reaches the gateway's upstream.
func TestClaudeTUIRoutesCustomBaseURLToTheGatewayUpstream(t *testing.T) {
	app := newTestAppWithStore(t)
	if got := app.claudetuiUpstream(); got != "" {
		t.Fatalf("claudetuiUpstream() = %q, want the real API by default", got)
	}
	if _, err := app.SetProviderCustomEnvVar("claude", claudetui.BaseURLEnv, "https://gw.test", false); err != nil {
		t.Fatal(err)
	}
	if got := app.claudetuiUpstream(); got != "https://gw.test" {
		t.Fatalf("claudetuiUpstream() = %q, want the configured endpoint", got)
	}
	if _, err := app.DeleteProviderCustomEnvVar("claude", claudetui.BaseURLEnv); err != nil {
		t.Fatal(err)
	}
	if got := app.claudetuiUpstream(); got != "" {
		t.Fatalf("claudetuiUpstream() = %q after removal, want the real API", got)
	}
}

// Text generation drives the same CLI against the same backend, so it takes the
// same environment — including after the availability fallback substitutes the
// other provider, where it must take the OTHER provider's list.
func TestTextGenerationCarriesTheChosenProvidersCustomEnv(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	if _, err := app.SetProviderCustomEnvVar("claude", "ANTHROPIC_BASE_URL", "https://claude.test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetProviderCustomEnvVar("codex", "HTTPS_PROXY", "http://proxy.test:8080", false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.settings.Update(map[string]any{"textGenerationProvider": "claude"}); err != nil {
		t.Fatal(err)
	}

	app.lookPathFn = func(string) error { return nil }
	cfg := app.resolveTextGenerationConfig()
	if cfg.Provider != string(provider.Claude) {
		t.Fatalf("provider = %q, want claude", cfg.Provider)
	}
	if cfg.Env["ANTHROPIC_BASE_URL"] != "https://claude.test" {
		t.Fatalf("textgen env = %#v, want Claude's custom environment", cfg.Env)
	}

	// Claude unavailable: the run substitutes Codex, and must carry Codex's
	// environment rather than the endpoint of a backend it isn't calling.
	app.lookPathFn = func(binary string) error {
		if strings.Contains(binary, "claude") {
			return os.ErrNotExist
		}
		return nil
	}
	cfg = app.resolveTextGenerationConfig()
	if cfg.Provider != string(provider.Codex) {
		t.Fatalf("provider = %q, want the codex substitution", cfg.Provider)
	}
	if cfg.Env["HTTPS_PROXY"] != "http://proxy.test:8080" {
		t.Fatalf("textgen env = %#v, want Codex's custom environment", cfg.Env)
	}
	if _, leaked := cfg.Env["ANTHROPIC_BASE_URL"]; leaked {
		t.Fatalf("textgen env = %#v, want Claude's endpoint left behind", cfg.Env)
	}

	// The explicit-provider builder (the layer-2 retry) agrees.
	explicit, ok := app.resolveTextGenerationConfigFor(string(provider.Codex))
	if !ok {
		t.Fatal("resolveTextGenerationConfigFor(codex) reported unavailable")
	}
	if explicit.Env["HTTPS_PROXY"] != "http://proxy.test:8080" {
		t.Fatalf("textgen env = %#v, want Codex's custom environment", explicit.Env)
	}
}

// GetSettings is reachable from a LAN-attached client, so a sensitive value
// must not ride out on it — the same rule the remote-endpoint tokens follow.
// Non-sensitive values stay readable; the UI needs them to render.
func TestGetSettingsRedactsSensitiveCustomEnvValues(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.SetProviderCustomEnvVar("claude", "OPEN_VALUE", "visible", false); err != nil {
		t.Fatal(err)
	}
	returned, err := app.SetProviderCustomEnvVar("claude", "SECRET_TOKEN", "hunter2", true)
	if err != nil {
		t.Fatal(err)
	}
	assertRedacted := func(t *testing.T, where string, vars []settings.ProviderEnvVar) {
		t.Helper()
		if len(vars) != 2 {
			t.Fatalf("%s: env vars = %+v, want 2", where, vars)
		}
		if vars[0].Value != "visible" {
			t.Errorf("%s: non-sensitive value = %q, want it readable", where, vars[0].Value)
		}
		if vars[1].Value != "" || !vars[1].Sensitive {
			t.Errorf("%s: sensitive entry = %+v, want an empty value and the flag kept", where, vars[1])
		}
	}
	// The mutator's own return value is the frontend's re-seed source, so it
	// has to be redacted too.
	assertRedacted(t, "SetProviderCustomEnvVar", returned.ClaudeCustomEnv)

	got, err := app.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	assertRedacted(t, "GetSettings", got.ClaudeCustomEnv)

	// Redaction is a wire projection, not a write: the spawn paths still see
	// the real value, and a second read is not served a redacted cache.
	if env := app.providerCustomEnv(string(provider.Claude)); env["SECRET_TOKEN"] != "hunter2" {
		t.Fatalf("spawn env = %#v, want the real secret", env)
	}
	again, err := app.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	assertRedacted(t, "GetSettings (second call)", again.ClaudeCustomEnv)
}

// The settings file is the persistence boundary; a sensitive value has to
// survive a restart or "re-enter to change" becomes "re-enter every launch".
func TestSensitiveCustomEnvSurvivesAReload(t *testing.T) {
	dir := t.TempDir()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(dir)
	if _, err := app.SetProviderCustomEnvVar("codex", "PROXY_TOKEN", "hunter2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatalf("settings file not written: %v", err)
	}

	reloaded := settings.NewService(dir).Get()
	if got := reloaded.ProviderEnvMap(string(provider.Codex))["PROXY_TOKEN"]; got != "hunter2" {
		t.Fatalf("reloaded value = %q, want the stored secret", got)
	}
}

func upperSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[strings.ToUpper(name)] = struct{}{}
	}
	return out
}
