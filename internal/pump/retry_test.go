package pump

import (
	"testing"
	"testing/synctest"
	"time"
)

// waitFired reports how long the armed retry took to fire.
func waitFired(t *testing.T, r *Retry) time.Duration {
	t.Helper()
	if r.C() == nil {
		t.Fatal("no retry armed")
	}
	start := time.Now()
	<-r.C()
	return time.Since(start)
}

func TestRetryHealthyArmsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: time.Second, Interval: 10 * time.Second}
		r.Arm(true, time.Time{})
		r.Arm(false, time.Time{})
		if r.C() != nil {
			t.Fatal("a healthy poll left a retry armed")
		}
		if r.Held() {
			t.Fatal("a healthy schedule reports a hold")
		}
		// The curve restarts after a healthy poll.
		r.Arm(true, time.Time{})
		if got := waitFired(t, &r); got != time.Second {
			t.Fatalf("first retry after recovery fired after %v, want the base", got)
		}
	})
}

func TestRetryDoublesUpToTheInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: time.Second, Interval: 5 * time.Second}
		for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
			r.Arm(true, time.Time{})
			if got := waitFired(t, &r); got != want {
				t.Fatalf("retry %d fired after %v, want %v", i, got, want)
			}
			if r.Held() {
				t.Fatalf("retry %d reports a hold without a rate limit", i)
			}
		}
	})
}

func TestRetryBaseAboveTheIntervalStartsAtTheInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: time.Minute, Interval: 5 * time.Second}
		r.Arm(true, time.Time{})
		if got := waitFired(t, &r); got != 5*time.Second {
			t.Fatalf("retry fired after %v, want the interval", got)
		}
	})
}

func TestRetryRateLimitHoldsUntilItsRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: time.Second, Interval: 5 * time.Second}
		r.Arm(true, time.Time{})
		r.Arm(true, time.Time{})
		release := time.Now().Add(30 * time.Second)
		r.Arm(true, release)
		if !r.Held() {
			t.Fatal("a rate limit does not hold the poll")
		}
		if got := waitFired(t, &r); got != 30*time.Second {
			t.Fatalf("held retry fired after %v, want the release", got)
		}
		if r.Held() {
			t.Fatal("the hold outlived its release")
		}
		// The doubling curve restarts after a rate limit.
		r.Arm(true, time.Time{})
		if got := waitFired(t, &r); got != time.Second {
			t.Fatalf("retry after a rate limit fired after %v, want the base", got)
		}
	})
}

func TestRetryReleaseInThePastFiresAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: time.Second, Interval: 5 * time.Second}
		r.Arm(true, time.Now().Add(-time.Minute))
		if got := waitFired(t, &r); got != time.Millisecond {
			t.Fatalf("past release fired after %v, want a millisecond", got)
		}
	})
}

func TestRetryFiredWhilePausedRestartsTheCurve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: time.Second, Interval: 10 * time.Second}
		r.Arm(true, time.Time{})
		r.Arm(true, time.Time{})
		waitFired(t, &r)
		r.FiredWhilePaused()
		if r.C() != nil {
			t.Fatal("a retry fired while paused stays armed")
		}
		r.Arm(true, time.Time{})
		if got := waitFired(t, &r); got != time.Second {
			t.Fatalf("retry after a pause fired after %v, want the base", got)
		}
	})
}

func TestRetryHeldAfterResumeRearmsForTheRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: time.Second, Interval: 5 * time.Second}
		release := time.Now().Add(time.Minute)
		r.Arm(true, release)
		time.Sleep(10 * time.Second)
		r.FiredWhilePaused()
		if !r.Held() {
			t.Fatal("the hold did not survive the pause")
		}
		if got := waitFired(t, &r); got != 50*time.Second {
			t.Fatalf("re-armed hold fired after %v, want the rest of the wait", got)
		}
	})
}

// ArmBy is the PR list pump's answer to each tick: failing projects keep
// the doubling curve, a rate-limited one brings the retry forward to its
// release, and nothing is held.
func TestRetryArmByFollowsEachPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: 10 * time.Second, Interval: time.Minute}
		for i, poll := range []struct {
			failing bool
			by      time.Duration
			want    time.Duration
		}{
			{failing: true, by: 3 * time.Second, want: 3 * time.Second},
			{failing: true, by: 30 * time.Second, want: 20 * time.Second},
			{failing: true, want: 40 * time.Second},
			{failing: false, by: 5 * time.Second, want: 5 * time.Second},
			{failing: true, want: 10 * time.Second},
		} {
			var by time.Time
			if poll.by > 0 {
				by = time.Now().Add(poll.by)
			}
			r.ArmBy(poll.failing, by)
			if r.Held() {
				t.Fatalf("poll %d: ArmBy reported a hold", i)
			}
			if got := waitFired(t, &r); got != poll.want {
				t.Fatalf("poll %d fired after %v, want %v", i, got, poll.want)
			}
		}
		r.ArmBy(false, time.Time{})
		if r.C() != nil {
			t.Fatal("a healthy poll with no release armed a retry")
		}
	})
}

// Each arm is one poll's whole answer and replaces the last, so an
// earlier release is never lost to a later one already armed, and a later
// poll's release is what the timer waits for.
func TestRetryArmByReplacesThePreviousArm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := Retry{Base: 10 * time.Second, Interval: time.Minute}
		r.ArmBy(false, time.Now().Add(20*time.Second))
		r.ArmBy(false, time.Now().Add(3*time.Second))
		if got := waitFired(t, &r); got != 3*time.Second {
			t.Fatalf("earlier release fired after %v, want 3s", got)
		}

		r = Retry{Base: 10 * time.Second, Interval: time.Minute}
		r.ArmBy(true, time.Now().Add(3*time.Second))
		r.ArmBy(true, time.Now().Add(8*time.Second))
		if got := waitFired(t, &r); got != 8*time.Second {
			t.Fatalf("second poll's release fired after %v, want 8s", got)
		}

		r.Arm(true, time.Now().Add(30*time.Second))
		r.ArmBy(true, time.Time{})
		if r.Held() {
			t.Fatal("ArmBy kept the hold an earlier Arm set")
		}
	})
}

func TestRetryFiredWhilePausedNeedsAnArmedRetry(t *testing.T) {
	r := Retry{Base: time.Second, Interval: time.Minute}
	assertPanics(t, "FiredWhilePaused before any arm", r.FiredWhilePaused)
	r.Arm(true, time.Time{})
	r.FiredWhilePaused()
	assertPanics(t, "a second FiredWhilePaused", r.FiredWhilePaused)
}

func TestRetryArmNeedsItsCurve(t *testing.T) {
	for name, r := range map[string]Retry{
		"no base":     {Interval: time.Second},
		"no interval": {Base: time.Second},
		"negative":    {Base: -time.Second, Interval: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			assertPanics(t, "Arm without a curve", func() { r.Arm(false, time.Time{}) })
			assertPanics(t, "ArmBy without a curve", func() { r.ArmBy(false, time.Time{}) })
		})
	}
}

func TestHeldUntilIsAtLeastAMillisecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		if got := HeldUntil(time.Now().Add(-time.Hour)); got != time.Millisecond {
			t.Fatalf("HeldUntil(past) = %v, want a millisecond", got)
		}
		if got := HeldUntil(time.Now().Add(time.Minute)); got != time.Minute {
			t.Fatalf("HeldUntil(+1m) = %v, want a minute", got)
		}
	})
}
