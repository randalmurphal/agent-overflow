package providerlifecycleapp

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	claudeprovider "agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provideraccounts"
)

func TestMergeSnapshotPreservesFresherWindowAndAcceptsReset(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Codex),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", WindowMins: 300,
			UsedPercent: 95, ResetsAt: 1_784_823_600,
		}},
	}
	older := provider.RateLimitsSnapshot{
		Provider: string(provider.Codex),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", WindowMins: 300,
			UsedPercent: 99, ResetsAt: 1_784_820_000,
		}},
	}
	if _, changed := MergeSnapshot(current, older); changed {
		t.Fatal("older quota window replaced current snapshot")
	}
	reset := provider.RateLimitsSnapshot{
		Provider: string(provider.Codex),
		Limits: []provider.RateLimitEntry{{
			LimitID: "session", WindowMins: 300,
			UsedPercent: 5, ResetsAt: 1_784_841_600,
		}},
	}
	merged, changed := MergeSnapshot(current, reset)
	if !changed || merged.Limits[0].UsedPercent != 5 {
		t.Fatalf("new quota window merge = %+v, changed = %v", merged, changed)
	}
}

// A same-window reading with LOWER used-percent is the server's current
// answer, not staleness: the server can raise the limit or outright reset
// usage mid-window (2026-09-01: a manual server-side weekly reset under an
// unchanged boundary). The newest reading must win or the meter freezes at
// the stale maximum until the window rolls.
func TestMergeSnapshotAcceptsSameWindowUsageDrop(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "seven_day", WindowMins: 10080,
			UsedPercent: 46, ResetsAt: 1_788_000_000,
		}},
	}
	fresh := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{{
			LimitID: "seven_day", WindowMins: 10080,
			UsedPercent: 4, ResetsAt: 1_788_000_000,
		}},
	}
	merged, changed := MergeSnapshot(current, fresh)
	if !changed || merged.Limits[0].UsedPercent != 4 {
		t.Fatalf("same-window usage drop merge = %+v, changed = %v", merged, changed)
	}
	if merged.Limits[0].ResetsAt != 1_788_000_000 {
		t.Fatalf("reset boundary moved: %+v", merged.Limits[0])
	}
}

// A provider drops a bucket from its answer once the bucket has no usage,
// which is what a mid-window reset produces. Merging alone kept the pre-reset
// figure for the rest of the window (2026-09-01: a Fable weekly row frozen at
// 90% while session and all-models correctly read 0%).
func TestMergeSnapshotDropsLimitsAWholeAnswerOmits(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{
			{LimitID: "weekly_all", WindowMins: 10080, UsedPercent: 46, ResetsAt: 1_788_000_000},
			{LimitID: "weekly_scoped:fable", LimitName: "Fable", WindowMins: 10080, UsedPercent: 90, ResetsAt: 1_788_000_000},
		},
	}
	whole := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude), Complete: true,
		Limits: []provider.RateLimitEntry{
			{LimitID: "weekly_all", WindowMins: 10080, UsedPercent: 0, ResetsAt: 1_788_000_000},
		},
	}
	merged, changed := MergeSnapshot(current, whole)
	if !changed {
		t.Fatal("dropping a limit is a change")
	}
	if len(merged.Limits) != 1 || merged.Limits[0].LimitID != "weekly_all" {
		t.Fatalf("merged = %+v, want the scoped row gone", merged.Limits)
	}
	if merged.Complete {
		t.Fatal("the cached union must never claim to be a reading")
	}
}

// The same omission from a PARTIAL reading proves nothing: a wire event
// carries one window and Claude's header fallback can never see a scoped
// bucket, so both must leave the rest of the cache alone.
func TestMergeSnapshotKeepsLimitsAPartialReadingOmits(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{
			{LimitID: "weekly_all", WindowMins: 10080, UsedPercent: 46, ResetsAt: 1_788_000_000},
			{LimitID: "weekly_scoped:fable", LimitName: "Fable", WindowMins: 10080, UsedPercent: 90, ResetsAt: 1_788_000_000},
		},
	}
	partial := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{
			{LimitID: "weekly_all", WindowMins: 10080, UsedPercent: 0, ResetsAt: 1_788_000_000},
		},
	}
	merged, changed := MergeSnapshot(current, partial)
	if !changed || len(merged.Limits) != 2 {
		t.Fatalf("merged = %+v (changed=%v), want both rows kept", merged.Limits, changed)
	}
}

// A whole answer replaces every limit it names even when the cached boundary
// is LATER. A cached boundary can be wrong (a reading filed under the wrong
// account, a boundary projected past an expired window), and honouring it
// would reject every true reading until the wrong boundary passed: the
// account would read 28% for days after the server reported 100%.
func TestMergeSnapshotWholeAnswerReplacesALaterCachedBoundary(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{
			// Already canonical, so normalization alone reports no change and
			// the assertions below are about the merge and nothing else.
			{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 28, ResetsAt: 1_792_537_200},
		},
	}
	whole := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude), Complete: true,
		Limits: []provider.RateLimitEntry{
			{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 100, ResetsAt: 1_792_411_200},
		},
	}
	merged, changed := MergeSnapshot(current, whole)
	if !changed {
		t.Fatal("a whole answer with an earlier boundary was rejected")
	}
	want := provider.RateLimitEntry{LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080, UsedPercent: 100, ResetsAt: 1_792_411_200}
	if len(merged.Limits) != 1 || merged.Limits[0] != want {
		t.Fatalf("merged = %+v, want %+v", merged.Limits, want)
	}

	// The same reading without Complete is partial and still loses to the
	// later cached boundary.
	partial := whole
	partial.Complete = false
	if merged, changed := MergeSnapshot(current, partial); changed {
		t.Fatalf("partial older-boundary reading changed the cache: %+v", merged.Limits)
	}
}

func TestRememberEventMergesBeforePersisting(t *testing.T) {
	var persisted []provider.RateLimitsSnapshot
	service := New(Deps{Accounts: AccountDeps{
		RememberRateLimit: func(_, _ string, snapshot provider.RateLimitsSnapshot) error {
			persisted = append(persisted, CloneSnapshot(snapshot))
			return nil
		},
	}})
	first := provider.RateLimitsSnapshot{
		Provider: string(provider.Codex), AccountID: "account",
		Limits: []provider.RateLimitEntry{{LimitID: "session", WindowMins: 300, UsedPercent: 20, ResetsAt: 1_784_823_600}},
	}
	service.RememberEvent(eventchan.ProviderUsage, provider.UsageEvent{Action: "rate_limits", RateLimits: &first})
	second := CloneSnapshot(first)
	second.Limits[0].UsedPercent = 10
	second.Limits[0].ResetsAt = 1_784_820_000 // older window: merge rejects, nothing re-persists
	service.RememberEvent(eventchan.ProviderUsage, provider.UsageEvent{Action: "rate_limits", RateLimits: &second})
	if len(persisted) != 1 || persisted[0].Limits[0].UsedPercent != 20 {
		t.Fatalf("persisted = %+v", persisted)
	}
	if snapshots := service.Snapshots(); len(snapshots) != 1 || snapshots[0].Limits[0].UsedPercent != 20 {
		t.Fatalf("cache = %+v", snapshots)
	}
}

// Reset forgets every reading, the turn activity that paces polling and the
// server holds, and tells clients each reading is gone.
func TestResetForgetsReadingsActivityAndHolds(t *testing.T) {
	var removed []provider.RateLimitsSnapshot
	service := New(Deps{Emit: func(channel eventchan.Channel, data any) {
		if event, ok := data.(provider.UsageEvent); ok && channel == eventchan.ProviderUsage && event.Action == "rate_limits_removed" {
			removed = append(removed, *event.RateLimits)
		}
	}})
	path := filepath.Join(t.TempDir(), "backoff.json")
	service.LoadBackoff(path)
	reading := provider.RateLimitsSnapshot{
		Provider: string(provider.Codex),
		Limits:   []provider.RateLimitEntry{{LimitID: "session", WindowMins: 300, UsedPercent: 64, ResetsAt: 1_784_823_600}},
	}
	service.RememberEvent(eventchan.ProviderUsage, provider.UsageEvent{Action: "rate_limits", RateLimits: &reading})
	mark := time.Now().Add(-time.Second)
	service.NoteTurnActivity(string(provider.Codex))
	service.Note(string(provider.Claude), "", &claudeprovider.RateLimitedError{RetryAfter: time.Hour})
	if service.Remaining(string(provider.Claude), "") == 0 {
		t.Fatal("precondition: a 429 with Retry-After holds the account")
	}

	service.Reset()

	if snapshots := service.Snapshots(); len(snapshots) != 0 {
		t.Fatalf("readings survived reset: %+v", snapshots)
	}
	if len(removed) != 1 || removed[0].Provider != string(provider.Codex) || removed[0].AccountID != "" {
		t.Fatalf("removal events = %+v, want the one cleared reading", removed)
	}
	if service.TurnCompletedSince(string(provider.Codex), mark) {
		t.Fatal("turn activity survived reset")
	}
	if remaining := service.Remaining(string(provider.Claude), ""); remaining != 0 {
		t.Fatalf("server hold survived reset: %v", remaining)
	}
	reloaded := New(Deps{})
	reloaded.LoadBackoff(path)
	if remaining := reloaded.Remaining(string(provider.Claude), ""); remaining != 0 {
		t.Fatalf("persisted hold survived reset: %v", remaining)
	}
}

func TestPrepareEventAttributesAccountBeforeRecordingActivity(t *testing.T) {
	var recorded provider.ProviderEvent
	service := New(Deps{Sessions: SessionDeps{
		Account: func(string) (RuntimeAccount, bool) {
			return RuntimeAccount{SessionToken: "token", CredentialAccountID: "account"}, true
		},
		RecordActivity: func(_ string, _ string, kind provider.EventKind, content string, _ time.Time) {
			recorded = provider.ProviderEvent{Kind: kind, Content: content}
		},
	}})
	meta, _ := json.Marshal(provider.RateLimitsSnapshot{Provider: string(provider.Codex)})
	event := service.PrepareEvent("thread", "token", provider.ProviderEvent{
		Kind: provider.EventRateLimits, Content: "quota", Meta: meta,
	})
	var snapshot provider.RateLimitsSnapshot
	if err := json.Unmarshal(event.Meta, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.AccountID != "account" || recorded.Kind != provider.EventRateLimits || recorded.Content != "quota" {
		t.Fatalf("snapshot = %+v, recorded = %+v", snapshot, recorded)
	}
}

func TestSessionAccountFallsBackToManagedMetadata(t *testing.T) {
	service := New(Deps{
		Sessions: SessionDeps{Account: func(string) (RuntimeAccount, bool) {
			return RuntimeAccount{
				Provider: string(provider.Claude), SessionToken: "token",
				CredentialGeneration: 4, CredentialAccountID: "account",
			}, true
		}},
		Accounts: AccountDeps{Account: func(_, _ string) (provideraccounts.Account, bool) {
			return provideraccounts.Account{Email: "person@example.test", SubscriptionType: "max"}, true
		}},
	})
	account, ok := service.SessionAccount("thread")
	if !ok || account.Account.Email != "person@example.test" || account.Generation != 4 {
		t.Fatalf("session account = %+v, ok = %v", account, ok)
	}
}
