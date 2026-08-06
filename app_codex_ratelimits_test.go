package main

import (
	"context"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

func TestProbeCodexRateLimits_EmitsRateLimitsWithoutAccount(t *testing.T) {
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	binary := writeCodexProbeMockBinary(t,
		`{"rateLimits":{"limitId":"codex","planType":"pro","primary":{"usedPercent":39,"windowDurationMins":300,"resetsAt":1775803864},"secondary":{"usedPercent":36,"windowDurationMins":10080,"resetsAt":1776372636}}}`)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	var captured struct {
		mu       sync.Mutex
		usage    []provider.UsageEvent
		accounts int
	}
	app.testEmitHook = func(name string, data any) {
		captured.mu.Lock()
		defer captured.mu.Unlock()
		switch name {
		case "provider:usage":
			if evt, ok := data.(provider.UsageEvent); ok {
				captured.usage = append(captured.usage, evt)
			}
		case "provider:account":
			captured.accounts++
		}
	}

	app.probeCodexRateLimits(context.Background())

	captured.mu.Lock()
	defer captured.mu.Unlock()
	if captured.accounts != 0 {
		t.Fatalf("provider:account emits = %d, want 0 for rate-limit-only refresh", captured.accounts)
	}
	if len(captured.usage) != 1 {
		t.Fatalf("provider:usage emits = %d, want 1", len(captured.usage))
	}
	evt := captured.usage[0]
	if evt.Action != "rate_limits" || evt.RateLimits == nil {
		t.Fatalf("usage event = %+v, want rate_limits with snapshot", evt)
	}
	if evt.RateLimits.Provider != string(provider.Codex) {
		t.Fatalf("snapshot provider = %q, want codex", evt.RateLimits.Provider)
	}
	if len(evt.RateLimits.Limits) != 2 {
		t.Fatalf("limits len = %d, want 2", len(evt.RateLimits.Limits))
	}
	if evt.RateLimits.Limits[0].UsedPercent != 39 || evt.RateLimits.Limits[1].UsedPercent != 36 {
		t.Fatalf("limits = %+v, want 39/36 used", evt.RateLimits.Limits)
	}
}
