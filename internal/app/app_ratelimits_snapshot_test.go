package app

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
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
		Limits:   []provider.RateLimitEntry{{LimitID: "codex", WindowMins: 300, UsedPercent: 60, ResetsAt: 500}},
	}
	olderReset := provider.RateLimitsSnapshot{
		Provider: "codex",
		Limits:   []provider.RateLimitEntry{{LimitID: "codex", WindowMins: 300, UsedPercent: 90, ResetsAt: 400}},
	}
	olderReading := provider.RateLimitsSnapshot{
		Provider: "codex",
		Limits:   []provider.RateLimitEntry{{LimitID: "codex", WindowMins: 300, UsedPercent: 50, ResetsAt: 500}},
	}
	for _, snapshot := range []*provider.RateLimitsSnapshot{&fresh, &olderReset, &olderReading} {
		app.emit("provider:usage", provider.UsageEvent{Action: "rate_limits", RateLimits: snapshot})
	}

	got := app.GetRateLimitsSnapshots()
	if len(got) != 1 || len(got[0].Limits) != 1 || got[0].Limits[0].UsedPercent != 60 {
		t.Fatalf("stale update regressed snapshot: %+v", got)
	}
}

func TestHydratePersistedAccountRateLimitsRepairsClaudeAliases(t *testing.T) {
	configDir := t.TempDir()
	accounts, err := provideraccounts.NewStore(configDir)
	if err != nil {
		t.Fatal(err)
	}
	account, err := accounts.UpsertAndActivate(provideraccounts.Account{
		ID:       "claude-one",
		Provider: string(provider.Claude),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		AccountID: account.ID,
		Limits: []provider.RateLimitEntry{
			{LimitID: "seven_day", LimitName: "seven_day", WindowMins: 10080, UsedPercent: 50, ResetsAt: time.Now().Add(time.Hour).Unix()},
			{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 50, ResetsAt: time.Now().Add(time.Hour).Unix()},
		},
	}
	if err := accounts.RememberRateLimits(string(provider.Claude), account.ID, snapshot); err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	attachProviderAccountStoresForTest(t, app, accounts, newTestProviderCredentials(t, t.TempDir()))
	app.hydratePersistedAccountRateLimits()

	reloaded, err := provideraccounts.NewStore(configDir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Active(string(provider.Claude), time.Now())
	if !ok || got.RateLimits == nil {
		t.Fatalf("Active() = %+v, %v", got, ok)
	}
	if len(got.RateLimits.Limits) != 1 {
		t.Fatalf("persisted Limits len = %d, want 1: %+v", len(got.RateLimits.Limits), got.RateLimits.Limits)
	}
	if limit := got.RateLimits.Limits[0]; limit.LimitID != "weekly_all" || limit.LimitName != "All models" {
		t.Fatalf("persisted canonical limit = %+v", limit)
	}
}
