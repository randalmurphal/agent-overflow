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
