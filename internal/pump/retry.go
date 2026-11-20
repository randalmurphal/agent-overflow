package pump

import "time"

// Retry is a pump's retry timer after a failed poll: a doubling delay from
// Base up to Interval, or for a rate limit a wait for its release, during
// which Held says no background poll runs. The zero value with Base and
// Interval set is a healthy schedule. It is owned by the pump goroutine,
// which arms it once after each poll (Arm or ArmBy) and reads C.
type Retry struct {
	// Base is the first delay of the doubling curve and Interval its
	// ceiling, the pump's tick. Both are positive and set before the first
	// arm.
	Base, Interval time.Duration

	timer <-chan time.Time
	delay time.Duration
	hold  time.Time
}

// C fires when the retry is due; it is nil while none is armed.
func (r *Retry) C() <-chan time.Time { return r.timer }

// Arm re-arms the schedule after a poll whose failure holds the whole
// pump: nothing while healthy, the release when one is given (a rate
// limit, which Held then reports until it passes), else the next doubling
// step. It is the schedule's answer to one poll and replaces whatever was
// armed. It panics when Base or Interval is not positive.
func (r *Retry) Arm(retrying bool, release time.Time) {
	r.mustHaveCurve()
	switch {
	case !retrying:
		r.timer, r.delay, r.hold = nil, 0, time.Time{}
	case !release.IsZero():
		r.delay, r.hold = 0, release
		r.timer = time.After(HeldUntil(release))
	default:
		r.hold = time.Time{}
		r.timer = time.After(r.step())
	}
}

// ArmBy re-arms the schedule after a poll whose rate limit, released at
// by, holds only part of the pump's work, so the rest keeps its curve: the
// next doubling step while retrying, and no later than by when by is set.
// Held reports nothing for it. Like Arm, it is the answer to one poll and
// replaces whatever was armed.
func (r *Retry) ArmBy(retrying bool, by time.Time) {
	r.mustHaveCurve()
	r.hold = time.Time{}
	var wait time.Duration
	if retrying {
		wait = r.step()
	} else {
		r.delay = 0
	}
	if !by.IsZero() && (wait == 0 || HeldUntil(by) < wait) {
		wait = HeldUntil(by)
	}
	if wait == 0 {
		r.timer = nil
		return
	}
	r.timer = time.After(wait)
}

func (r *Retry) mustHaveCurve() {
	if r.Base <= 0 || r.Interval <= 0 {
		panic("pump: Retry needs a positive Base and Interval")
	}
}

// step advances the doubling curve and returns its delay.
func (r *Retry) step() time.Duration {
	if r.delay == 0 {
		r.delay = min(r.Base, r.Interval)
	} else {
		r.delay = min(r.delay*2, r.Interval)
	}
	return r.delay
}

// FiredWhilePaused records that C fired while the pump was paused: the
// resume polls once and the curve restarts from there. A rate limit's
// hold stays. It panics when no retry is armed.
func (r *Retry) FiredWhilePaused() {
	if r.timer == nil {
		panic("pump: FiredWhilePaused with no retry armed")
	}
	r.timer = nil
	r.delay = 0
}

// Held reports whether a rate limit holds the poll, re-arming the timer
// for its release when a resume after a pause dropped it.
func (r *Retry) Held() bool {
	if !time.Now().Before(r.hold) {
		return false
	}
	if r.timer == nil {
		r.timer = time.After(HeldUntil(r.hold))
	}
	return true
}

// HeldUntil is how long from now a poller waits for a rate limit's
// release, at least a millisecond so a timer armed with it fires.
func HeldUntil(release time.Time) time.Duration {
	return max(time.Until(release), time.Millisecond)
}
