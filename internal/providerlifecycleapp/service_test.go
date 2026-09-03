package providerlifecycleapp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
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

// A whole answer whose entry loses the boundary check is still an entry the
// server LISTED. Pruning it would delete a live quota over a stale reading.
func TestMergeSnapshotKeepsAWholeAnswerEntryRejectedForAnOlderBoundary(t *testing.T) {
	current := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude),
		Limits: []provider.RateLimitEntry{
			// Already canonical, so normalization alone reports no change and
			// the assertion below is about the merge and nothing else.
			{LimitID: "session", LimitName: "Current session", WindowMins: 300, UsedPercent: 30, ResetsAt: 1_788_316_200},
		},
	}
	stale := provider.RateLimitsSnapshot{
		Provider: string(provider.Claude), Complete: true,
		Limits: []provider.RateLimitEntry{
			{LimitID: "session", LimitName: "Current session", WindowMins: 300, UsedPercent: 90, ResetsAt: 1_788_298_200},
		},
	}
	merged, changed := MergeSnapshot(current, stale)
	if changed {
		t.Fatalf("older-boundary reading changed the cache: %+v", merged.Limits)
	}
	if len(merged.Limits) != 1 || merged.Limits[0].UsedPercent != 30 {
		t.Fatalf("merged = %+v, want the cached window untouched", merged.Limits)
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

func TestUsageProbeGateCollapsesConcurrentTriggerIntoOneTrailingRun(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	var nowMu sync.Mutex
	now := time.Unix(1000, 0)
	var timer func()
	gate := newUsageProbeGate(func(context.Context) error {
		started <- struct{}{}
		<-release
		return nil
	}, context.Background)
	gate.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	gate.afterFunc = func(_ time.Duration, callback func()) { timer = callback }

	done := make(chan struct{})
	go func() {
		gate.Request()
		close(done)
	}()
	<-started
	gate.Request()
	release <- struct{}{}
	<-done
	if timer == nil {
		t.Fatal("trailing trigger did not arm a timer")
	}
	nowMu.Lock()
	now = now.Add(probeMinInterval)
	nowMu.Unlock()
	timerDone := make(chan struct{})
	go func() {
		timer()
		close(timerDone)
	}()
	<-started
	release <- struct{}{}
	<-timerDone
}

func TestConcurrentCacheAndActivityAccess(t *testing.T) {
	service := New(Deps{})
	const workers = 16
	var group sync.WaitGroup
	for worker := range workers {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			providerName := string(provider.Claude)
			service.NoteTurnActivity(providerName)
			snapshot := provider.RateLimitsSnapshot{
				Provider: providerName, AccountID: "account",
				Limits: []provider.RateLimitEntry{{
					LimitID: "session", WindowMins: 300, UsedPercent: float64(worker),
				}},
			}
			service.RememberEvent(eventchan.ProviderUsage, provider.UsageEvent{
				Action: "rate_limits", RateLimits: &snapshot,
			})
			_ = service.Snapshots()
			_ = service.TurnCompletedSince(providerName, time.Time{})
		}(worker)
	}
	group.Wait()
}
