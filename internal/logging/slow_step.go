package logging

import (
	"context"
	"errors"
	"log"
	"net"
	"sync/atomic"
	"time"
)

// defaultSlowStepThreshold is how long one step on the path between two
// computers (a credential exchange, a dial, a handler, a lock wait) may
// take before it is logged. Each is a small round trip or a short local
// wait, so a step past it names where a pairing or reconnect spent its time.
const defaultSlowStepThreshold = 2 * time.Second

var slowStepThreshold atomic.Int64

func init() { slowStepThreshold.Store(int64(defaultSlowStepThreshold)) }

// SetSlowStepThreshold replaces the threshold and returns the restore. A
// function so a test can drive a step past it; nothing else calls it.
func SetSlowStepThreshold(threshold time.Duration) (restore func()) {
	previous := slowStepThreshold.Swap(int64(threshold))
	return func() { slowStepThreshold.Store(previous) }
}

// LogSlowStep writes one line for a step that outlasted the threshold:
//
//	<component>: slow step=<step> duration=<d> outcome=<outcome>
//
// step is a constant name and outcome comes from ErrorOutcome,
// StatusOutcome or HTTPOutcome. The line never carries an endpoint, a credential, a link
// token, a verification number or a label; callers pass nothing else.
func LogSlowStep(component, step string, started time.Time, outcome string) {
	elapsed := time.Since(started)
	if elapsed < time.Duration(slowStepThreshold.Load()) {
		return
	}
	log.Printf("%s: slow step=%s duration=%s outcome=%s", component, step, elapsed.Round(time.Millisecond), outcome)
}

// ErrorOutcome is how a step that returned err ended: ok, refused (an
// error that reports Refused), canceled, timeout or error.
func ErrorOutcome(err error) string {
	var refusal interface{ Refused() bool }
	var netErr net.Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &refusal) && refusal.Refused():
		return "refused"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "error"
	}
}

// StatusOutcome is how a step that answered or received HTTP status ended:
// ok below 400, refused for a 4xx, error for a 5xx.
func StatusOutcome(status int) string {
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "refused"
	default:
		return "ok"
	}
}

// HTTPOutcome is how an HTTP exchange that returned status and err ended:
// err's classification when the exchange failed, otherwise the status's. A
// step that returns a fully read 4xx or 5xx with a nil err logs refused or
// error, not ok.
func HTTPOutcome(status int, err error) string {
	if err != nil {
		return ErrorOutcome(err)
	}
	return StatusOutcome(status)
}
