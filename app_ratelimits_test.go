package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

func TestMergeRateLimitsSnapshotKeepsAccountAndDynamicBuckets(t *testing.T) {
	incoming := provider.RateLimitsSnapshot{
		Provider:  string(provider.Codex),
		AccountID: "account-one",
		UpdatedAt: 10,
		Limits: []provider.RateLimitEntry{
			{LimitID: "codex", LimitName: "Codex", WindowMins: 300, UsedPercent: 100, ResetsAt: 1000},
			{LimitID: "spark", LimitName: "Spark", WindowMins: 300, UsedPercent: 46, ResetsAt: 1000},
		},
	}
	got, changed := mergeRateLimitsSnapshot(provider.RateLimitsSnapshot{}, incoming)
	if !changed {
		t.Fatal("merge reported unchanged")
	}
	if got.AccountID != "account-one" {
		t.Fatalf("AccountID = %q, want account-one", got.AccountID)
	}
	if len(got.Limits) != 2 {
		t.Fatalf("Limits len = %d, want 2", len(got.Limits))
	}
}

func TestMergeRateLimitsSnapshotCollapsesClaudeLegacyAliases(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: "account-one",
		Limits: []provider.RateLimitEntry{
			{LimitID: "seven_day", LimitName: "seven_day", WindowMins: 10080, UsedPercent: 50, ResetsAt: 1000},
			{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 49, ResetsAt: 1000},
			{LimitID: "weekly_scoped:fable", LimitName: "Fable", WindowMins: 10080, UsedPercent: 99, ResetsAt: 1000},
		},
	}
	incoming := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: "account-one",
		Limits: []provider.RateLimitEntry{
			{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 50, ResetsAt: 1000},
		},
	}

	got, changed := mergeRateLimitsSnapshot(current, incoming)
	if !changed {
		t.Fatal("merge reported unchanged")
	}
	if len(got.Limits) != 2 {
		t.Fatalf("Limits len = %d, want canonical weekly plus Fable: %+v", len(got.Limits), got.Limits)
	}
	if got.Limits[0].LimitID != "weekly_all" || got.Limits[0].LimitName != "All models" {
		t.Fatalf("canonical weekly limit = %+v", got.Limits[0])
	}
	if got.Limits[0].UsedPercent != 50 {
		t.Fatalf("canonical weekly utilization = %v, want 50", got.Limits[0].UsedPercent)
	}
	if got.Limits[1].LimitID != "weekly_scoped:fable" {
		t.Fatalf("scoped limit was changed: %+v", got.Limits[1])
	}
}

func TestMergeRateLimitsSnapshotAcceptsHigherUsageAcrossResetTimestampJitter(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: "account-one",
		UpdatedAt: 10,
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session",
			WindowMins: 300, UsedPercent: 0, ResetsAt: 1784841601,
		}},
	}
	incoming := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: "account-one",
		UpdatedAt: 20,
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session",
			WindowMins: 300, UsedPercent: 8, ResetsAt: 1784841599,
		}},
	}

	got, changed := mergeRateLimitsSnapshot(current, incoming)
	if !changed {
		t.Fatal("merge rejected a higher reading from the same jittered window")
	}
	if got.Limits[0].UsedPercent != 8 {
		t.Fatalf("UsedPercent = %v, want 8", got.Limits[0].UsedPercent)
	}
	if got.Limits[0].ResetsAt != 1784841601 {
		t.Fatalf("ResetsAt = %d, want stable boundary 1784841601", got.Limits[0].ResetsAt)
	}
}

func TestMergeRateLimitsSnapshotIgnoresJitterWithoutFresherUsage(t *testing.T) {
	for _, test := range []struct {
		name            string
		incomingPercent float64
	}{
		{name: "equal usage", incomingPercent: 8},
		{name: "lower usage", incomingPercent: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := provider.RateLimitsSnapshot{
				Provider: string(provider.Claude),
				Limits: []provider.RateLimitEntry{{
					LimitID: "session", LimitName: "Current session",
					WindowMins: 300, UsedPercent: 8, ResetsAt: 1784841601,
				}},
			}
			incoming := provider.RateLimitsSnapshot{
				Provider: string(provider.Claude),
				Limits: []provider.RateLimitEntry{{
					LimitID: "session", LimitName: "Current session",
					WindowMins: 300, UsedPercent: test.incomingPercent, ResetsAt: 1784841599,
				}},
			}

			got, changed := mergeRateLimitsSnapshot(current, incoming)
			if changed {
				t.Fatalf("merge changed on jitter with %s: %+v", test.name, got.Limits)
			}
		})
	}
}

func TestMergeRateLimitsSnapshotAcceptsLowerUsageInNewQuotaWindow(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session",
			WindowMins: 300, UsedPercent: 95, ResetsAt: 1784823600,
		}},
	}
	incoming := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session",
			WindowMins: 300, UsedPercent: 5, ResetsAt: 1784841600,
		}},
	}

	got, changed := mergeRateLimitsSnapshot(current, incoming)
	if !changed {
		t.Fatal("merge rejected a lower reading from a new quota window")
	}
	if got.Limits[0].UsedPercent != 5 || got.Limits[0].ResetsAt != 1784841600 {
		t.Fatalf("new-window limit = %+v, want 5%% at 1784841600", got.Limits[0])
	}
}

func TestMergeRateLimitsSnapshotStillRejectsOlderQuotaWindow(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session", WindowMins: 300,
			UsedPercent: 5, ResetsAt: 1784841600,
		}},
	}
	incoming := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", LimitName: "Current session", WindowMins: 300,
			UsedPercent: 95, ResetsAt: 1784823600,
		}},
	}

	got, changed := mergeRateLimitsSnapshot(current, incoming)
	if changed {
		t.Fatalf("merge accepted an older quota window: %+v", got.Limits)
	}
}

// TestStartRateLimitProbeLoop_ExitsOnAppCtxCancel pins Step 1b's wiring
// into the probe loop: a regression that swapped a.lifeCtx() back to
// context.Background() (or dropped the <-ctx.Done() select arm) would
// keep the goroutine alive after Shutdown returned. Drive the loop
// with a probe stub that signals on each call, cancel appCtx after the
// startup probe lands, and confirm no further probe fires.
func TestStartRateLimitProbeLoop_ExitsOnAppCtxCancel(t *testing.T) {
	app := newTestAppWithStore(t)

	probeFired := make(chan struct{}, 4)
	hasActive := func() bool { return true }
	probe := func(ctx context.Context) {
		select {
		case probeFired <- struct{}{}:
		default:
		}
	}

	app.startRateLimitProbeLoop(rateLimitProbeLoop{
		probeImmediately: true,
		hasActiveSession: hasActive,
		probe:            probe,
	})

	// Wait for the startup probe so we know the goroutine reached the
	// select-loop body, not still mid-spawn.
	select {
	case <-probeFired:
	case <-time.After(time.Second):
		t.Fatal("startup probe never fired")
	}

	// Cancel appCtx the same way Shutdown step 1b does.
	app.appCancel()

	// The select arm on ctx.Done() must win — no further probe calls.
	// rateLimitProbeInterval is 2 minutes in production, so any probe
	// inside this window is the spurious post-cancel call we're
	// guarding against.
	select {
	case <-probeFired:
		t.Fatal("probe fired after appCancel — loop did not honour ctx.Done()")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSessionEventHandlerTurnCompleteFiresProviderRateLimitProbe verifies
// the turn-complete trigger dispatches to the matching provider probe. The
// test injects a fake Claude HTTP server and a fake Codex app-server binary;
// the goroutines launched from sessionEventHandler have their own cadence so
// we await up to 1s for each provider signal.
func TestSessionEventHandlerTurnCompleteFiresProviderRateLimitProbe(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	credsDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credsDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"bearer-x"}}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	hits := atomic.Int32{}
	hitCh := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.10")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
		select {
		case hitCh <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	app.rateLimitProbeClientOverride = &http.Client{
		Transport: redirectRoundTripper{target: srvURL, inner: http.DefaultTransport},
	}
	codexBinary := writeCodexProbeMockBinary(t,
		`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1775803864},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":1776372636}}}`)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": codexBinary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	codexUsageCh := make(chan struct{}, 1)
	app.testEmitHook = func(name string, data any) {
		if name != "provider:usage" {
			return
		}
		evt, ok := data.(provider.UsageEvent)
		if !ok || evt.RateLimits == nil || evt.RateLimits.Provider != string(provider.Codex) {
			return
		}
		select {
		case codexUsageCh <- struct{}{}:
		default:
		}
	}

	// Codex turn-complete: should trigger the Codex probe, not the
	// Claude HTTP probe.
	codexHandler := app.sessionEventHandler("thread-codex", "tok-codex", string(provider.Codex))
	codexHandler(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "thread-codex",
		Timestamp: time.Now(),
	})

	select {
	case <-codexUsageCh:
	case <-time.After(1 * time.Second):
		t.Fatalf("Codex turn-complete did not emit a rate-limit snapshot")
	}
	// Give any incorrectly-fired Claude goroutine a moment to make an
	// HTTP call.
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("Codex turn-complete fired the Claude probe (hits=%d)", hits.Load())
	}

	// Claude turn-complete: should trigger the Claude probe.
	claudeHandler := app.sessionEventHandler("thread-claude", "tok-claude", string(provider.Claude))
	claudeHandler(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "thread-claude",
		Timestamp: time.Now(),
	})

	select {
	case <-hitCh:
		// Got the expected hit.
	case <-time.After(1 * time.Second):
		t.Fatalf("Claude turn-complete did not fire the probe within 1s (hits=%d)", hits.Load())
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
}
