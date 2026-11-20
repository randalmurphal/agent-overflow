package pump

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

var (
	errStopped = errors.New("test: stopped")
	errEnded   = errors.New("test: lifetime ended")
)

type testRequest struct{ name string }

func newTestCore(t *testing.T, lifetime context.Context) *Core[testRequest] {
	t.Helper()
	core := NewCore[testRequest](lifetime, Refusals{Stopped: errStopped, LifetimeEnded: errEnded})
	t.Cleanup(core.Stop)
	return &core
}

func woken(core *Core[testRequest]) bool {
	select {
	case <-core.Wake():
		return true
	default:
		return false
	}
}

func assertHandles(t *testing.T, core *Core[testRequest], refs, active int, paused bool) {
	t.Helper()
	gotRefs, gotActive := core.HandlesLocked()
	if gotRefs != refs || gotActive != active || core.Paused() != paused {
		t.Fatalf("refs=%d active=%d paused=%v, want %d/%d/%v", gotRefs, gotActive, core.Paused(), refs, active, paused)
	}
}

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	fn()
}

func TestNewCoreNeedsBothRefusals(t *testing.T) {
	for name, refusals := range map[string]Refusals{
		"none":           {},
		"stopped only":   {Stopped: errStopped},
		"lifetime only":  {LifetimeEnded: errEnded},
		"both are given": {Stopped: errStopped, LifetimeEnded: errEnded},
	} {
		t.Run(name, func(t *testing.T) {
			build := func() {
				core := NewCore[testRequest](t.Context(), refusals)
				core.Stop()
			}
			if refusals.Stopped != nil && refusals.LifetimeEnded != nil {
				build()
				return
			}
			assertPanics(t, "NewCore without a refusal", build)
		})
	}
}

func TestZeroCoreIsNotDead(t *testing.T) {
	var core Core[testRequest]
	if core.DeadLocked() {
		t.Fatal("the zero core reads dead")
	}
}

func TestTakeCountsAVisibleHandleWithoutWaking(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	assertHandles(t, core, 1, 1, false)
	if woken(core) {
		t.Fatal("the first handle woke a loop that was never paused")
	}
}

func TestTakeResumesAPausedCoreAndWakesIt(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	core.VoteLocked(false)
	assertHandles(t, core, 1, 0, true)
	core.TakeLocked()
	assertHandles(t, core, 2, 1, false)
	if !woken(core) {
		t.Fatal("a handle that resumed a paused core did not wake its loop")
	}
	core.TakeLocked()
	if woken(core) {
		t.Fatal("a handle on a running core woke its loop")
	}
}

func TestVotePausesOnlyWhenEveryHandleIsHidden(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	core.TakeLocked()
	core.VoteLocked(false)
	assertHandles(t, core, 2, 1, false)
	core.VoteLocked(false)
	assertHandles(t, core, 2, 0, true)
	if woken(core) {
		t.Fatal("a hiding vote woke the loop")
	}
	core.VoteLocked(true)
	assertHandles(t, core, 2, 1, false)
	if !woken(core) {
		t.Fatal("the vote that resumed the core did not wake its loop")
	}
	core.VoteLocked(true)
	assertHandles(t, core, 2, 2, false)
	if woken(core) {
		t.Fatal("a vote on a running core woke its loop")
	}
}

func TestRepeatedResumesQueueOneWake(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	core.VoteLocked(false)
	core.VoteLocked(true)
	core.VoteLocked(false)
	core.VoteLocked(true)
	if !woken(core) {
		t.Fatal("no wake queued")
	}
	if woken(core) {
		t.Fatal("two resumes queued two wakes")
	}
}

func TestVoteOnADeadCoreCountsNothing(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	core.TakeLocked()
	core.MarkDeadLocked()
	core.VoteLocked(false)
	core.VoteLocked(false)
	assertHandles(t, core, 2, 2, false)
}

func TestTakeOnADeadCorePanics(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.MarkDeadLocked()
	assertPanics(t, "TakeLocked on a dead core", core.TakeLocked)
}

func TestReleaseNotLastRecountsThePause(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	core.TakeLocked()
	core.VoteLocked(false)
	if core.ReleaseLocked(true) {
		t.Fatal("releasing one of two handles reported the last")
	}
	assertHandles(t, core, 1, 0, true)
	if core.DeadLocked() {
		t.Fatal("a release that was not the last marked the core dead")
	}
}

func TestReleaseLastMarksTheCoreDead(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	core.VoteLocked(false)
	if !core.ReleaseLocked(false) {
		t.Fatal("releasing the only handle did not report the last")
	}
	assertHandles(t, core, 0, 0, true)
	if !core.DeadLocked() {
		t.Fatal("the last release left the core live")
	}
}

func TestReleaseWithoutAHandlePanics(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	core.ReleaseLocked(true)
	assertPanics(t, "a second release of one handle", func() { core.ReleaseLocked(true) })
}

func TestStopEndsTheCoreAndRepeatsHarmlessly(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.Stop()
	core.Stop()
	select {
	case <-core.Done():
	default:
		t.Fatal("Done is open after Stop")
	}
	if core.Context().Err() == nil {
		t.Fatal("the core's context outlived Stop")
	}
}

func TestCancelEndsTheContextOnly(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.Cancel()
	if core.Context().Err() == nil {
		t.Fatal("Cancel left the context running")
	}
	select {
	case <-core.Done():
		t.Fatal("Cancel stopped the core")
	default:
	}
}

func TestContextEndsWithTheLifetime(t *testing.T) {
	lifetime, end := context.WithCancel(t.Context())
	core := newTestCore(t, lifetime)
	end()
	if core.Context().Err() == nil {
		t.Fatal("the core's context outlived its lifetime")
	}
}

// serveOne answers the next request with err and reports its body.
func serveOne(core *Core[testRequest], err error) <-chan testRequest {
	served := make(chan testRequest, 1)
	go func() {
		req := <-core.Requests()
		served <- req.Body()
		req.Finish(err)
	}()
	return served
}

func TestRequestIsServedWhilePaused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newTestCore(t, t.Context())
		core.TakeLocked()
		core.VoteLocked(false)
		if !core.Paused() {
			t.Fatal("the core is not paused")
		}
		answer := errors.New("test: the poll failed")
		served := serveOne(core, answer)
		if err := core.Request(t.Context(), testRequest{name: "refresh"}); err != answer {
			t.Fatalf("Request = %v, want the goroutine's answer %v", err, answer)
		}
		if got := <-served; got.name != "refresh" {
			t.Fatalf("served %+v, want the caller's body", got)
		}
	})
}

func TestRequestAnswersNilWhenTheWorkSucceeded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newTestCore(t, t.Context())
		serveOne(core, nil)
		if err := core.Request(t.Context(), testRequest{}); err != nil {
			t.Fatalf("Request = %v, want nil", err)
		}
	})
}

func TestRequestOnAStoppedCoreIsRefused(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.Stop()
	if err := core.Request(t.Context(), testRequest{}); err != errStopped {
		t.Fatalf("Request = %v, want the Stopped refusal", err)
	}
}

func TestRequestRefusedWhenStoppedBeforeTheAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newTestCore(t, t.Context())
		taken := make(chan *Request[testRequest], 1)
		go func() { taken <- <-core.Requests() }()
		result := make(chan error, 1)
		go func() { result <- core.Request(t.Context(), testRequest{}) }()
		req := <-taken
		synctest.Wait()
		core.Stop()
		if err := <-result; err != errStopped {
			t.Fatalf("Request = %v, want the Stopped refusal", err)
		}
		req.Finish(nil)
	})
}

func TestRequestEndsWithACancelledCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newTestCore(t, t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- core.Request(ctx, testRequest{}) }()
		synctest.Wait()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Request = %v, want the caller's cancellation", err)
		}
	})
}

func TestRequestEndsWithACallerCancelledBeforeTheAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newTestCore(t, t.Context())
		taken := make(chan *Request[testRequest], 1)
		go func() { taken <- <-core.Requests() }()
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- core.Request(ctx, testRequest{}) }()
		req := <-taken
		synctest.Wait()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Request = %v, want the caller's cancellation", err)
		}
		req.Finish(nil)
	})
}

func TestRequestRefusedOnceTheLifetimeEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lifetime, end := context.WithCancel(t.Context())
		core := newTestCore(t, lifetime)
		result := make(chan error, 1)
		go func() { result <- core.Request(t.Context(), testRequest{}) }()
		synctest.Wait()
		end()
		if err := <-result; err != errEnded {
			t.Fatalf("Request = %v, want the LifetimeEnded refusal", err)
		}
	})
}
