package providerlifecycleapp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

const testTick = 5 * time.Millisecond

// startTestPoll runs the production poll predicate for providerName on a
// short tick and counts its probes. The service is closed at cleanup.
func startTestPoll(t *testing.T, service *Service, cancel context.CancelFunc, providerName string) *atomic.Int32 {
	t.Helper()
	var probes atomic.Int32
	loop := service.pollLoop(providerName, func() { probes.Add(1) })
	loop.Interval = testTick
	service.StartProbeLoop(loop)
	t.Cleanup(func() {
		cancel()
		service.Close()
	})
	return &probes
}

func newPollService(inFlight *atomic.Bool) (*Service, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return New(Deps{
		Context: func() context.Context { return ctx },
		Sessions: SessionDeps{TurnInFlight: func(providerName string) bool {
			return providerName == stringProviderClaude && inFlight.Load()
		}},
	}), cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPollIdleProviderNeverProbes(t *testing.T) {
	t.Parallel()
	var inFlight atomic.Bool
	service, cancel := newPollService(&inFlight)
	probes := startTestPoll(t, service, cancel, stringProviderClaude)
	time.Sleep(20 * testTick)
	if got := probes.Load(); got != 0 {
		t.Fatalf("idle provider probed %d times", got)
	}
}

func TestPollProbesEveryTickWhileATurnIsInFlight(t *testing.T) {
	t.Parallel()
	var inFlight atomic.Bool
	inFlight.Store(true)
	service, cancel := newPollService(&inFlight)
	probes := startTestPoll(t, service, cancel, stringProviderClaude)
	waitFor(t, "three probes with no completed turn", func() bool { return probes.Load() >= 3 })

	inFlight.Store(false)
	time.Sleep(2 * testTick)
	settled := probes.Load()
	time.Sleep(20 * testTick)
	if got := probes.Load(); got != settled {
		t.Fatalf("probes after the turn ended = %d, want %d", got, settled)
	}
}

func TestPollProbesOnceAfterACompletedTurn(t *testing.T) {
	t.Parallel()
	var inFlight atomic.Bool
	service, cancel := newPollService(&inFlight)
	probes := startTestPoll(t, service, cancel, stringProviderClaude)
	service.NoteTurnActivity(stringProviderClaude)
	waitFor(t, "a probe after the completed turn", func() bool { return probes.Load() == 1 })
	time.Sleep(20 * testTick)
	if got := probes.Load(); got != 1 {
		t.Fatalf("probes after one completed turn = %d, want 1", got)
	}
}

// claude-tui runs the claude binary on the same account, so its turns drive
// the Claude poll and never a poll of their own.
func TestClaudeTUIActivityDrivesTheClaudePoll(t *testing.T) {
	t.Parallel()
	var inFlight atomic.Bool
	service, cancel := newPollService(&inFlight)
	probes := startTestPoll(t, service, cancel, stringProviderClaude)
	service.NoteTurnActivity(string(provider.ClaudeTUI))
	if !service.TurnCompletedSince(stringProviderClaude, time.Time{}) {
		t.Fatal("a claude-tui turn did not record Claude activity")
	}
	waitFor(t, "a Claude probe after a claude-tui turn", func() bool { return probes.Load() == 1 })
}

func TestOnReadingFiresOncePerMergedReadingOutsideTheLock(t *testing.T) {
	t.Parallel()
	type call struct {
		providerName, accountID string
		percent                 float64
	}
	var service *Service
	var calls []call
	service = New(Deps{OnReading: func(providerName, accountID string) {
		// Reading the cache from the listener deadlocks if the listener
		// runs under the cache lock.
		snapshot, ok := service.Snapshot(providerName, accountID)
		if !ok {
			t.Errorf("listener for %s/%s found no cached snapshot", providerName, accountID)
			return
		}
		calls = append(calls, call{providerName, accountID, snapshot.Limits[0].UsedPercent})
	}})
	reading := func(accountID string, percent float64) provider.UsageEvent {
		return provider.UsageEvent{Action: "rate_limits", RateLimits: &provider.RateLimitsSnapshot{
			Provider: stringProviderClaude, AccountID: accountID,
			Limits: []provider.RateLimitEntry{{
				LimitID: "weekly_all", LimitName: "All models", WindowMins: 10080,
				UsedPercent: percent, ResetsAt: 1_788_000_000,
			}},
		}}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RememberEvent(eventchan.ProviderUsage, reading("selected", 20))
		service.RememberEvent(eventchan.ProviderUsage, reading("other", 70))
		service.RememberEvent(eventchan.ProviderUsage, reading("selected", 30))
		// Not a reading: no listener call.
		service.RememberEvent(eventchan.ProviderUsage, provider.UsageEvent{Action: "rate_limits_removed"})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("OnReading ran under the cache lock")
	}
	want := []call{
		{stringProviderClaude, "selected", 20},
		{stringProviderClaude, "other", 70},
		{stringProviderClaude, "selected", 30},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %+v, want %+v", calls, want)
		}
	}
	if _, ok := service.Snapshot(string(provider.ClaudeTUI), "other"); !ok {
		t.Fatal("Snapshot does not read claude-tui as the Claude family")
	}
	if _, ok := service.Snapshot(stringProviderClaude, "missing"); ok {
		t.Fatal("Snapshot found an account that has no reading")
	}
}

// Two requests in a burst probe once, then once more as one trailing run, the
// same coalescing the poll gets.
func TestRequestReadingGoesThroughTheGate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 4)
	release := make(chan struct{}, 4)
	var probes atomic.Int32
	service := New(Deps{
		Context: func() context.Context { return ctx },
		Accounts: AccountDeps{
			Selection: func(string) Selection { return Selection{AccountID: "selected"} },
			RefreshUsage: func(_ context.Context, providerName, accountID string) error {
				if providerName != stringProviderClaude || accountID != "selected" {
					t.Errorf("refreshed %s/%s, want the selected Claude account", providerName, accountID)
				}
				probes.Add(1)
				started <- struct{}{}
				<-release
				return nil
			},
		},
	})
	gate := service.claudeUsageGate()
	var timerMu sync.Mutex
	var timers []func()
	gate.afterFunc = func(_ time.Duration, callback func()) {
		timerMu.Lock()
		timers = append(timers, func() { service.runMember(callback) })
		timerMu.Unlock()
	}

	service.RequestReading(string(provider.ClaudeTUI))
	<-started
	service.RequestReading(stringProviderClaude)
	waitFor(t, "the second request to queue a trailing run", func() bool {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		return gate.trailing
	})
	release <- struct{}{}
	waitFor(t, "the trailing run's timer", func() bool {
		timerMu.Lock()
		defer timerMu.Unlock()
		return len(timers) == 1
	})
	if got := probes.Load(); got != 1 {
		t.Fatalf("probes before the trailing run = %d, want 1", got)
	}
	gate.now = func() time.Time { return time.Now().Add(probeMinInterval) }
	release <- struct{}{}
	timers[0]()
	if got := probes.Load(); got != 2 {
		t.Fatalf("probes after the trailing run = %d, want 2", got)
	}

	service.RequestReading("unknown")
	cancel()
	service.Close()
	service.RequestReading(stringProviderClaude)
	if got := probes.Load(); got != 2 {
		t.Fatalf("probes after Close = %d, want 2", got)
	}
}

func TestProbeMinIntervalReplacesTheGateGapOnlyWhenSet(t *testing.T) {
	t.Parallel()
	if got := New(Deps{}).claudeUsageGate().minInterval; got != probeMinInterval {
		t.Fatalf("default gate gap = %s, want %s", got, probeMinInterval)
	}
	service := New(Deps{ProbeMinInterval: func() time.Duration { return time.Millisecond }})
	for name, gate := range map[string]*usageProbeGate{"claude": service.claudeUsageGate(), "codex": service.codexUsageGate()} {
		if gate.minInterval != time.Millisecond {
			t.Fatalf("%s gate gap = %s, want the configured 1ms", name, gate.minInterval)
		}
	}
}
