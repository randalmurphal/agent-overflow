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

// newTestCore is a core under lifetime. The tests are its owner and call
// the ...Locked methods from one goroutine, which stands in for the lock.
func newTestCore(t *testing.T, lifetime context.Context) *Core[testRequest] {
	t.Helper()
	core := NewCore[testRequest](lifetime, Refusals{Stopped: errStopped, LifetimeEnded: errEnded})
	t.Cleanup(core.StopLocked)
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

func stopped(core *Core[testRequest]) bool {
	select {
	case <-core.Done():
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
				core.StopLocked()
			}
			if refusals.Stopped != nil && refusals.LifetimeEnded != nil {
				build()
				return
			}
			assertPanics(t, "NewCore without a refusal", build)
		})
	}
}

func TestZeroCoreReadsLiveAndRefusesUse(t *testing.T) {
	var core Core[testRequest]
	if core.DeadLocked() || core.Paused() {
		t.Fatal("the zero core reads dead or paused")
	}
	if refs, active := core.HandlesLocked(); refs != 0 || active != 0 {
		t.Fatalf("the zero core holds %d/%d handles", refs, active)
	}
	for name, use := range map[string]func(){
		"Context":    func() { core.Context() },
		"Done":       func() { core.Done() },
		"Wake":       func() { core.Wake() },
		"Requests":   func() { core.Requests() },
		"TakeLocked": func() { core.TakeLocked() },
		"StopLocked": core.StopLocked,
		"Request":    func() { _ = core.Request(t.Context(), testRequest{}) },
	} {
		assertPanics(t, name+" on the zero core", use)
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
	core.TakeLocked().VoteLocked(false)
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

func TestTakeOnAStoppedCorePanics(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.StopLocked()
	assertPanics(t, "TakeLocked on a stopped core", func() { core.TakeLocked() })
}

func TestVotePausesOnlyWhenEveryHandleIsHidden(t *testing.T) {
	core := newTestCore(t, t.Context())
	a, b := core.TakeLocked(), core.TakeLocked()
	a.VoteLocked(false)
	assertHandles(t, core, 2, 1, false)
	b.VoteLocked(false)
	assertHandles(t, core, 2, 0, true)
	if woken(core) {
		t.Fatal("a hiding vote woke the loop")
	}
	a.VoteLocked(true)
	assertHandles(t, core, 2, 1, false)
	if !woken(core) {
		t.Fatal("the vote that resumed the core did not wake its loop")
	}
	b.VoteLocked(true)
	assertHandles(t, core, 2, 2, false)
	if woken(core) {
		t.Fatal("a vote on a running core woke its loop")
	}
}

func TestARepeatedVoteChangesNothing(t *testing.T) {
	core := newTestCore(t, t.Context())
	a, b := core.TakeLocked(), core.TakeLocked()
	a.VoteLocked(true)
	assertHandles(t, core, 2, 2, false)
	a.VoteLocked(false)
	a.VoteLocked(false)
	assertHandles(t, core, 2, 1, false)
	b.VoteLocked(false)
	assertHandles(t, core, 2, 0, true)
}

func TestRepeatedResumesQueueOneWake(t *testing.T) {
	core := newTestCore(t, t.Context())
	h := core.TakeLocked()
	h.VoteLocked(false)
	h.VoteLocked(true)
	h.VoteLocked(false)
	h.VoteLocked(true)
	if !woken(core) {
		t.Fatal("no wake queued")
	}
	if woken(core) {
		t.Fatal("two resumes queued two wakes")
	}
}

func TestVoteOnAStoppedCoreCountsNothing(t *testing.T) {
	core := newTestCore(t, t.Context())
	a, b := core.TakeLocked(), core.TakeLocked()
	core.StopLocked()
	a.VoteLocked(false)
	b.VoteLocked(false)
	assertHandles(t, core, 2, 2, false)
}

func TestVoteOnAReleasedHandlePanics(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	h := core.TakeLocked()
	h.ReleaseLocked()
	assertPanics(t, "a vote on a released handle", func() { h.VoteLocked(false) })
}

func TestReleaseNotLastRecountsThePause(t *testing.T) {
	core := newTestCore(t, t.Context())
	hidden, visible := core.TakeLocked(), core.TakeLocked()
	hidden.VoteLocked(false)
	if visible.ReleaseLocked() {
		t.Fatal("releasing one of two handles reported the last")
	}
	assertHandles(t, core, 1, 0, true)
	if core.DeadLocked() || stopped(core) {
		t.Fatal("a release that was not the last stopped the core")
	}
}

func TestReleaseLastStopsTheCore(t *testing.T) {
	core := newTestCore(t, t.Context())
	h := core.TakeLocked()
	h.VoteLocked(false)
	if !h.ReleaseLocked() {
		t.Fatal("releasing the only handle did not report the last")
	}
	assertHandles(t, core, 0, 0, true)
	if !core.DeadLocked() || !stopped(core) || core.Context().Err() == nil {
		t.Fatal("the last release left the core running")
	}
	if err := core.Request(t.Context(), testRequest{}); err != errStopped {
		t.Fatalf("Request after the last release = %v, want the Stopped refusal", err)
	}
}

func TestReleaseTwicePanics(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.TakeLocked()
	h := core.TakeLocked()
	h.ReleaseLocked()
	assertPanics(t, "a second release of one handle", func() { h.ReleaseLocked() })
	assertHandles(t, core, 1, 1, false)
}

func TestStopEndsTheCoreAndRepeatsHarmlessly(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.StopLocked()
	core.StopLocked()
	if !core.DeadLocked() || !stopped(core) {
		t.Fatal("StopLocked left the core live")
	}
	if core.Context().Err() == nil {
		t.Fatal("the core's context outlived StopLocked")
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
		core.TakeLocked().VoteLocked(false)
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

func TestFinishTwicePanics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newTestCore(t, t.Context())
		taken := make(chan *Request[testRequest], 1)
		go func() { taken <- <-core.Requests() }()
		result := make(chan error, 1)
		go func() { result <- core.Request(t.Context(), testRequest{}) }()
		req := <-taken
		req.Finish(nil)
		func() {
			defer func() {
				if got := recover(); got != "pump: request finished twice" {
					t.Errorf("a second Finish recovered %v, want the refusal", got)
				}
			}()
			req.Finish(errStopped)
		}()
		if err := <-result; err != nil {
			t.Fatalf("Request = %v, want the first Finish's answer", err)
		}
	})
}

func TestRequestOnAStoppedCoreIsRefused(t *testing.T) {
	core := newTestCore(t, t.Context())
	core.StopLocked()
	if err := core.Request(t.Context(), testRequest{}); err != errStopped {
		t.Fatalf("Request = %v, want the Stopped refusal", err)
	}
}

// A goroutine that took the request and exits without finishing it (a
// panic in its handler) stops the core on its way out; the caller is
// refused rather than left waiting.
func TestRequestRefusedWhenTheGoroutineExitsAfterTakingIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newTestCore(t, t.Context())
		go func() {
			defer core.StopLocked()
			<-core.Requests()
		}()
		if err := core.Request(t.Context(), testRequest{}); err != errStopped {
			t.Fatalf("Request = %v, want the Stopped refusal", err)
		}
	})
}

// A request still waiting for the hand-off when the goroutine exits is
// refused too.
func TestRequestRefusedWhenTheGoroutineExitsBeforeTakingIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newTestCore(t, t.Context())
		result := make(chan error, 1)
		go func() { result <- core.Request(t.Context(), testRequest{}) }()
		synctest.Wait()
		core.StopLocked()
		if err := <-result; err != errStopped {
			t.Fatalf("Request = %v, want the Stopped refusal", err)
		}
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
		if err := <-result; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrNotTaken) {
			t.Fatalf("Request = %v, want the caller's cancellation of a request not taken", err)
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
		if err := <-result; !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotTaken) {
			t.Fatalf("Request = %v, want the caller's cancellation of a request taken", err)
		}
		req.Finish(nil)
	})
}

// A goroutine that exits because its lifetime ended stops the core; a
// request is told the lifetime ended, not that the pump stopped.
func TestRequestOnACoreStoppedByItsLifetimeSaysTheLifetimeEnded(t *testing.T) {
	lifetime, end := context.WithCancel(t.Context())
	core := newTestCore(t, lifetime)
	end()
	core.StopLocked()
	for range 50 {
		if err := core.Request(t.Context(), testRequest{}); err != errEnded {
			t.Fatalf("Request = %v, want the LifetimeEnded refusal", err)
		}
	}
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
