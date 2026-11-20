package pump

import "time"

// Retry is a pump's retry timer after a failed poll: a doubling delay from
// Base up to Interval, or for a rate limit a wait for its release, during
// which Held says no background poll runs. The zero value with Base and
// Interval set is a healthy schedule. It is owned by the pump goroutine.
type Retry struct {
	// Base is the first delay of the doubling curve and Interval its
	// ceiling, the pump's tick. Both are positive and set before the first
	// Arm.
	Base, Interval time.Duration

	timer <-chan time.Time
	delay time.Duration
	hold  time.Time
}

// C fires when the retry is due; it is nil while none is armed.
func (r *Retry) C() <-chan time.Time { return r.timer }

// Arm re-arms the schedule after a poll: nothing while healthy, the
// release when one is given (a rate limit, which Held then reports until
// it passes), else the next doubling step. It panics when Base or Interval
// is not positive.
func (r *Retry) Arm(retrying bool, release time.Time) {
	if r.Base <= 0 || r.Interval <= 0 {
		panic("pump: Retry needs a positive Base and Interval")
	}
	switch {
	case !retrying:
		r.timer, r.delay, r.hold = nil, 0, time.Time{}
	case !release.IsZero():
		r.delay, r.hold = 0, release
		r.timer = time.After(HeldUntil(release))
	default:
		r.hold = time.Time{}
		if r.delay == 0 {
			r.delay = min(r.Base, r.Interval)
		} else {
			r.delay = min(r.delay*2, r.Interval)
		}
		r.timer = time.After(r.delay)
	}
}

// FireBy brings the timer forward to a release that holds only part of
// the pump's work, so the rest keeps its curve: it is armed for release
// when nothing is armed or the current step ends later. Held does not
// report it. A zero release changes nothing.
func (r *Retry) FireBy(release time.Time) {
	if release.IsZero() {
		return
	}
	if wait := HeldUntil(release); r.timer == nil || wait < r.delay {
		r.timer = time.After(wait)
	}
}

// FiredWhilePaused records a retry that fired while the pump was paused:
// the resume polls once and the curve restarts from there. A rate limit's
// hold stays.
func (r *Retry) FiredWhilePaused() {
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
