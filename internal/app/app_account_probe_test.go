package app

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// collectProviderAccountEmissions captures every `provider:account`
// payload via the testEmitHook seam. Mirrors the helper in
// app_provider_status_test.go so the two startup channels share an
// observation pattern.
func collectProviderAccountEmissions(a *App) (events *[]ProviderAccountEvent, mu *sync.Mutex) {
	events = &[]ProviderAccountEvent{}
	mu = &sync.Mutex{}
	a.testEmitHook = func(name string, data any) {
		if name != "provider:account" {
			return
		}
		evt, ok := data.(ProviderAccountEvent)
		if !ok {
			return
		}
		mu.Lock()
		*events = append(*events, evt)
		mu.Unlock()
	}
	return events, mu
}

// waitForAccountEmissions polls the captured slice until it has at
// least `want` entries or the deadline elapses. The probes run in
// background goroutines spawned by probeStartupAccountInfo, so
// observers must wait rather than read once.
func waitForAccountEmissions(t *testing.T, events *[]ProviderAccountEvent, mu *sync.Mutex, want int, timeout time.Duration) []ProviderAccountEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(*events)
		if got >= want {
			out := append([]ProviderAccountEvent(nil), (*events)...)
			mu.Unlock()
			return out
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	out := append([]ProviderAccountEvent(nil), (*events)...)
	mu.Unlock()
	return out
}

func TestProbeStartupAccountInfoEmitsPerProvider(t *testing.T) {
	resetClaudeProbeCacheForTest()
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	claudeBin := writeProbeMockBinary(t, `{"subscriptionType":"Claude Max","tokenSource":"oauth"}`)
	codexBin := writeCodexProbeMockBinary(t, `{"rateLimits":{"planType":"pro"}}`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": claudeBin,
		"codexBinaryPath":  codexBin,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	events, mu := collectProviderAccountEmissions(app)
	app.probeStartupAccountInfo()
	got := waitForAccountEmissions(t, events, mu, 2, 5*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 emissions, got %d (%+v)", len(got), got)
	}

	byProvider := map[string]ProviderAccountEvent{}
	for _, e := range got {
		byProvider[e.Provider] = e
	}
	if claude := byProvider[string(provider.Claude)]; claude.Account.SubscriptionType != "Claude Max" {
		t.Errorf("claude SubscriptionType: got %q, want Claude Max", claude.Account.SubscriptionType)
	}
	if codex := byProvider[string(provider.Codex)]; codex.Account.SubscriptionType != "pro" {
		t.Errorf("codex SubscriptionType: got %q, want pro", codex.Account.SubscriptionType)
	}
}

func TestProbeStartupAccountInfoSwallowsFailure(t *testing.T) {
	// One probe succeeds, the other fails (binary missing). The startup
	// hook must NOT emit for the failed provider — duplicating the
	// provider:status banner channel would produce noise. Verifying
	// "no emit" is what locks the behavior down.
	resetClaudeProbeCacheForTest()
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	claudeBin := writeProbeMockBinary(t, `{"subscriptionType":"Claude Max"}`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": claudeBin,
		"codexBinaryPath":  "/nonexistent/codex-missing-xyz-12345",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	events, mu := collectProviderAccountEmissions(app)
	app.probeStartupAccountInfo()
	// Wait long enough for the missing-binary spawn to fail and the
	// claude probe to land.
	got := waitForAccountEmissions(t, events, mu, 1, 5*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 emission, got %d (%+v)", len(got), got)
	}
	if got[0].Provider != string(provider.Claude) {
		t.Errorf("emission Provider: got %q, want claude", got[0].Provider)
	}
}

func TestProbeStartupAccountInfoNoSettingsShortCircuits(t *testing.T) {
	// Tests / pre-init paths can construct an App without settings.
	// The startup hook must not panic in that case — same defensive
	// guard the sibling probeStartupProviderStatuses uses.
	app := &App{}
	// Should return immediately without panicking. No emit hook needed —
	// nothing should fire.
	app.probeStartupAccountInfo()
}
