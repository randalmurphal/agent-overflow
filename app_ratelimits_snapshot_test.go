package main

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestGetRateLimitsSnapshotsHydratesMissedUsageEvents(t *testing.T) {
	app := &App{}
	snapshot := provider.RateLimitsSnapshot{
		Provider: "codex",
		Limits: []provider.RateLimitEntry{
			{LimitID: "codex", WindowMins: 300, UsedPercent: 31, ResetsAt: 1783643306},
			{LimitID: "codex", WindowMins: 10080, UsedPercent: 30, ResetsAt: 1784009268},
		},
		UpdatedAt: 1783629000000,
	}

	// No event bus and no test hook: the event itself has no live consumer,
	// but the backend must still retain the latest account snapshot.
	app.emit("provider:usage", provider.UsageEvent{
		Action:     "rate_limits",
		RateLimits: &snapshot,
	})

	got := app.GetRateLimitsSnapshots()
	if len(got) != 1 || got[0].Provider != "codex" || len(got[0].Limits) != 2 {
		t.Fatalf("snapshots = %+v", got)
	}
	got[0].Limits[0].UsedPercent = 99
	again := app.GetRateLimitsSnapshots()
	if again[0].Limits[0].UsedPercent != 31 {
		t.Fatalf("caller mutated retained snapshot: %+v", again[0])
	}
}

func TestGetRateLimitsSnapshotsIgnoresNonQuotaUsage(t *testing.T) {
	app := &App{}
	app.emit("provider:usage", provider.UsageEvent{Action: "usage", UsedTokens: 10})
	app.emit("other", provider.UsageEvent{
		Action: "rate_limits",
		RateLimits: &provider.RateLimitsSnapshot{
			Provider: "codex",
			Limits:   []provider.RateLimitEntry{{WindowMins: 300, ResetsAt: 1}},
		},
	})
	if got := app.GetRateLimitsSnapshots(); len(got) != 0 {
		t.Fatalf("unexpected snapshots: %+v", got)
	}
}

func TestGetRateLimitsSnapshotsMergesProviderWindows(t *testing.T) {
	app := &App{}
	for _, snapshot := range []provider.RateLimitsSnapshot{
		{
			Provider:  "claude",
			Limits:    []provider.RateLimitEntry{{LimitID: "five_hour", WindowMins: 300, UsedPercent: 40, ResetsAt: 200}},
			UpdatedAt: 10,
		},
		{
			Provider:  "claude",
			Limits:    []provider.RateLimitEntry{{LimitID: "seven_day", WindowMins: 10080, UsedPercent: 55, ResetsAt: 500}},
			UpdatedAt: 20,
		},
	} {
		app.emit("provider:usage", provider.UsageEvent{Action: "rate_limits", RateLimits: &snapshot})
	}

	got := app.GetRateLimitsSnapshots()
	if len(got) != 1 || len(got[0].Limits) != 2 {
		t.Fatalf("snapshots = %+v, want both Claude windows", got)
	}
	if got[0].Limits[0].WindowMins != 300 || got[0].Limits[1].WindowMins != 10080 {
		t.Fatalf("limits = %+v, want stable window order", got[0].Limits)
	}
}

func TestGetRateLimitsSnapshotsRejectsStaleWindowUpdates(t *testing.T) {
	app := &App{}
	fresh := provider.RateLimitsSnapshot{
		Provider: "codex",
		Limits:   []provider.RateLimitEntry{{WindowMins: 300, UsedPercent: 60, ResetsAt: 500}},
	}
	olderReset := provider.RateLimitsSnapshot{
		Provider: "codex",
		Limits:   []provider.RateLimitEntry{{WindowMins: 300, UsedPercent: 90, ResetsAt: 400}},
	}
	olderReading := provider.RateLimitsSnapshot{
		Provider: "codex",
		Limits:   []provider.RateLimitEntry{{WindowMins: 300, UsedPercent: 50, ResetsAt: 500}},
	}
	for _, snapshot := range []*provider.RateLimitsSnapshot{&fresh, &olderReset, &olderReading} {
		app.emit("provider:usage", provider.UsageEvent{Action: "rate_limits", RateLimits: snapshot})
	}

	got := app.GetRateLimitsSnapshots()
	if len(got) != 1 || len(got[0].Limits) != 1 || got[0].Limits[0].UsedPercent != 60 {
		t.Fatalf("stale update regressed snapshot: %+v", got)
	}
}
