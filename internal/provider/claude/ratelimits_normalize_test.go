package claude

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestNormalizeRateLimitsSnapshotCollapsesKnownAliasesOnly(t *testing.T) {
	snapshot := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{
			{LimitID: "five_hour", LimitName: "five_hour", WindowMins: 300, UsedPercent: 42, ResetsAt: 100},
			{LimitID: "session", LimitName: "Current session", WindowMins: 300, UsedPercent: 40, ResetsAt: 100},
			{LimitID: "seven_day", LimitName: "seven_day", WindowMins: 10080, UsedPercent: 51, ResetsAt: 200},
			{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 50, ResetsAt: 200},
			{LimitID: "weekly_scoped:fable", LimitName: "Fable", WindowMins: 10080, UsedPercent: 51, ResetsAt: 200},
			{LimitID: "future_limit", LimitName: "Future limit", WindowMins: 10080, UsedPercent: 51, ResetsAt: 200},
		},
	}

	got, changed := NormalizeRateLimitsSnapshot(snapshot)
	if !changed {
		t.Fatal("NormalizeRateLimitsSnapshot reported unchanged")
	}
	if len(got.Limits) != 4 {
		t.Fatalf("Limits len = %d, want 4: %+v", len(got.Limits), got.Limits)
	}

	assertLimit := func(index int, id, name string, used float64) {
		t.Helper()
		entry := got.Limits[index]
		if entry.LimitID != id || entry.LimitName != name || entry.UsedPercent != used {
			t.Fatalf("Limits[%d] = %+v, want id=%q name=%q used=%v", index, entry, id, name, used)
		}
	}
	assertLimit(0, "session", "Current session", 42)
	assertLimit(1, "weekly_all", "All models", 51)
	assertLimit(2, "weekly_scoped:fable", "Fable", 51)
	assertLimit(3, "future_limit", "Future limit", 51)
}
