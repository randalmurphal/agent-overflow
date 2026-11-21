package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/accountswitch"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/claudefake"
)

// switchWorkerHold is the app's auto-switch coordinator built with its
// InFlight dependency wrapped: once armed, the next call, the first step of
// a decision off the send path, waits for release.
type switchWorkerHold struct {
	armed    atomic.Bool
	entered  chan struct{}
	released chan struct{}
	open     sync.Once
}

// holdSwitchWorker installs the held coordinator. Call it before anything
// reaches app.accountSwitch().
func holdSwitchWorker(t *testing.T, app *App) *switchWorkerHold {
	t.Helper()
	h := &switchWorkerHold{entered: make(chan struct{}), released: make(chan struct{})}
	deps := app.accountSwitchDeps()
	inFlight := deps.InFlight
	deps.InFlight = func(providerName string) []accountswitch.ThreadModel {
		if h.armed.CompareAndSwap(true, false) {
			close(h.entered)
			<-h.released
		}
		return inFlight(providerName)
	}
	installed := false
	app.accountSwitcherOnce.Do(func() {
		app.accountSwitcher = accountswitch.New(deps)
		installed = true
	})
	if !installed {
		t.Fatal("the auto-switch coordinator was built before the hold was installed")
	}
	return h
}

// arm holds the next decision. A failing test releases it before the
// worker's Stop, which a rig.start before arm registered.
func (h *switchWorkerHold) arm(t *testing.T) {
	h.armed.Store(true)
	t.Cleanup(h.release)
}

func (h *switchWorkerHold) release() { h.open.Do(func() { close(h.released) }) }

// The order behind a spec that flaked: a reading of the selected account,
// past the threshold with a turn running and the account outside the pool,
// waits behind a decision the worker is still making, and the account is
// ticked back into the pool before the worker reaches it. Settling before
// the pool changes decides the reading on the pool it arrived under:
// nothing moves, and the next reading still switches.
func TestAutoSwitchSettleDecidesAReadingBeforeAPoolChange(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	hold := holdSwitchWorker(t, app)
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "a", "b")
	rig.setPool(t, "b", false)
	thread := autoSwitchThread("running", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "b")
	openClaudeTurn(t, app, thread.ID)
	rig.reading("a", usageLimit("session", 10, time.Hour))
	rig.start(t)
	rig.settle(t)

	hold.arm(t)
	rig.reading("b", usageLimit("session", 20, time.Hour))
	<-hold.entered
	rig.reading("b", usageLimit("session", 99, time.Hour))

	// The barrier: the worker decides on the readings before the pool
	// changes.
	hold.release()
	rig.settle(t)

	rig.setPool(t, "b", true)
	hold.release()
	rig.settle(t)
	if reports := rig.reported(); len(reports) != 0 || rig.selected() != "b" {
		t.Fatalf("reports = %+v selected = %q; want no switch for readings taken while b was outside the pool",
			reports, rig.selected())
	}

	rig.reading("b", usageLimit("session", 100, time.Hour))
	rig.settle(t)
	reports := rig.reported()
	if len(reports) != 1 || reports[0].FromAccountID != "b" || reports[0].ToAccountID != "a" || reports[0].Reason != accountswitch.ReasonExhausted {
		t.Fatalf("reports = %+v, want one exhausted switch b -> a", reports)
	}
	if got := rig.selected(); got != "a" {
		t.Fatalf("selected = %q, want a", got)
	}
}

// A harness usage read whose decision cannot be made by its deadline ends
// at the deadline with what auto-switch was still doing.
func TestHarnessUsageReadNamesAPassThatCannotSettle(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	isolated := &App{}
	ConfigureIsolation(isolated, IsolationConfig{})
	app.rateLimitProbeClientOverride = isolated.rateLimitProbeClientOverride
	app.claudeUsageFake = isolated.claudeUsageFake
	hold := holdSwitchWorker(t, app)
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	thread := autoSwitchThread("running", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "second")
	openClaudeTurn(t, app, thread.ID)
	rig.start(t)
	if err := app.claudeUsageFake.Set("second@example.com", []claudefake.UsageLimit{
		{Kind: "session", Group: "session", Percent: 20, ResetsAt: time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}
	hold.arm(t)
	rig.reading("second", usageLimit("session", 20, time.Hour))
	<-hold.entered

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := (&harnessHost{app: app}).ReadClaudeUsage(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), "auto-switch did not settle: the worker is still deciding on the work queued before the wait") {
		t.Fatalf("read with the worker held = %v, want the unsettled pass named at the deadline", err)
	}
}

// The harness's usage read returns only once auto-switch has decided on the
// reading it took, so a spec asserting right after it sees the decision.
func TestHarnessUsageReadReturnsAfterTheSwitchDecision(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	isolated := &App{}
	ConfigureIsolation(isolated, IsolationConfig{})
	app.rateLimitProbeClientOverride = isolated.rateLimitProbeClientOverride
	app.claudeUsageFake = isolated.claudeUsageFake
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	thread := autoSwitchThread("running", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "second")
	openClaudeTurn(t, app, thread.ID)
	rig.reading("first", usageLimit("session", 10, time.Hour))
	resetsAt := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	if err := app.claudeUsageFake.Set("second@example.com", []claudefake.UsageLimit{
		{Kind: "session", Group: "session", Percent: 99, ResetsAt: resetsAt},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&harnessHost{app: app}).ReadClaudeUsage(ctx) }()
	waitForCondition(t, "the reading of second in the cache", func() bool {
		snapshot, ok := app.providerLifecycleService().Snapshot(string(provider.Claude), "second")
		return ok && len(snapshot.Limits) == 1 && snapshot.Limits[0].UsedPercent == 99
	})
	select {
	case err := <-done:
		t.Fatalf("the read returned (%v) before auto-switch decided on its reading", err)
	default:
	}

	rig.start(t)
	if err := <-done; err != nil {
		t.Fatalf("read = %v", err)
	}
	reports := rig.reported()
	if len(reports) != 1 || reports[0].FromAccountID != "second" || reports[0].ToAccountID != "first" || reports[0].Reason != accountswitch.ReasonThreshold {
		t.Fatalf("reports when the read returned = %+v, want one threshold switch second -> first", reports)
	}
	if got := rig.selected(); got != "first" {
		t.Fatalf("selected when the read returned = %q, want first", got)
	}
}
