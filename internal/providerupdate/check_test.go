package providerupdate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Lookups of one provider never overlap, and one slow provider does not
// hold the other's result.
func TestLookupsOfOneProviderAreSerialAndProvidersConcurrent(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	release, unblock := gate(t)
	entered := make(chan struct{}, 4)
	var inFlight, peak atomic.Int32
	r.runner.on("doctor", func(ctx context.Context) (Output, error) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
		}
		entered <- struct{}{}
		<-release
		return doctorSays("latest")(ctx)
	})
	done := make(chan []Status, 2)
	go func() { done <- r.svc.Check() }()
	<-entered
	// Claude's lookup is held; Codex's is applied anyway.
	r.waitFrame(0, func(statuses []Status) bool {
		return statusOf(statuses, ProviderCodex).Available && statusOf(statuses, ProviderClaude).CheckedAt == 0
	})

	// A second Check: its Codex lookup applies while its Claude lookup
	// waits for the first instead of starting beside it.
	mark := r.mark()
	go func() { done <- r.svc.Check() }()
	r.waitFrame(mark, func(statuses []Status) bool { return statusOf(statuses, ProviderCodex).Available })
	select {
	case <-entered:
		t.Fatal("a second claude lookup started while the first ran")
	default:
	}
	unblock()
	for range 2 {
		if claude := statusOf(<-done, ProviderClaude); !claude.Available {
			t.Errorf("claude = %+v", claude)
		}
	}
	if got := r.runner.ran("doctor"); got != 2 {
		t.Errorf("doctor ran %d times, want one per Check", got)
	}
	if got := peak.Load(); got != 1 {
		t.Errorf("%d claude lookups ran at once", got)
	}
}

// A check that began while the update command ran read the old binary; the
// run's own check waits for it and is applied after it.
func TestTheRunsCheckIsAppliedAfterALookupThatBeganDuringIt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.check()
	updating, finishUpdate := gate(t)
	updateStarted := make(chan struct{})
	r.runner.on("update", func(context.Context) (Output, error) {
		close(updateStarted)
		<-updating
		r.setInstalled(ProviderClaude, "2.1.325")
		return Output{}, nil
	})
	afterRun := make(chan struct{})
	r.svc.deps.AfterRun = func() { close(afterRun) }

	mark := r.mark()
	if err := r.svc.Run(ProviderClaude); err != nil {
		t.Fatal(err)
	}
	<-updateStarted
	staleLookup, finishStale := gate(t)
	staleEntered := make(chan struct{})
	r.runner.on("doctor", func(ctx context.Context) (Output, error) {
		// The lookup begun during the update reports the stable channel's
		// 2.1.311, which with the old installed 2.1.300 reads as an update.
		close(staleEntered)
		<-staleLookup
		r.runner.on("doctor", doctorSays("latest"))
		return doctorSays("stable")(ctx)
	})
	staleDone := make(chan struct{})
	go func() {
		r.svc.Check()
		close(staleDone)
	}()
	<-staleEntered
	finishUpdate()
	<-afterRun
	finishStale()
	<-staleDone

	status := r.waitRunSettled(mark, ProviderClaude)
	if status.Run.Phase != PhaseSucceeded || status.InstalledVersion != "2.1.325" ||
		status.LatestVersion != "2.1.325" || status.Available {
		t.Fatalf("final = %+v, want the run's own check of 2.1.325 as the last word", status)
	}
	// The offer, the run's confirmation, the one begun during the update,
	// the run's own afterwards.
	if got := r.runner.ran("doctor"); got != 4 {
		t.Errorf("doctor ran %d times", got)
	}
}

// A lookup Stop cancels applies nothing and emits nothing.
func TestALookupEndedByStopAppliesNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	entered := make(chan struct{})
	r.runner.on("doctor", func(ctx context.Context) (Output, error) {
		close(entered)
		<-ctx.Done()
		return Output{}, ctx.Err()
	})
	done := make(chan struct{})
	go func() {
		r.svc.Check()
		close(done)
	}()
	<-entered
	r.svc.Stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Check did not return after Stop")
	}
	if claude := statusOf(r.svc.Snapshot(), ProviderClaude); claude.CheckedAt != 0 || claude.CheckError != "" {
		t.Fatalf("claude = %+v, want the cancelled lookup unapplied", claude)
	}
	for _, frame := range r.allFrames() {
		if statusOf(frame, ProviderClaude).CheckedAt != 0 {
			t.Fatalf("a frame carried the cancelled lookup: %+v", frame)
		}
	}
}
