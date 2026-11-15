package deviceclient

import (
	"context"
	"errors"
	"log"
	"net"
	"time"
)

// slowStepThreshold is how long one credential step may take before it is
// logged. Every step is a small round trip or a short local lock wait, so a
// step past it names where a pairing or reconnect spent its time. A variable
// so a test can drive a step past it; nothing else assigns it.
var slowStepThreshold = 2 * time.Second

// Credential steps, as logged.
const (
	stepPair           = "pair"
	stepRenew          = "renew"
	stepTicket         = "ticket"
	stepRenewalSupport = "renewal_support"
	stepRouteProbe     = "route_probe"
	stepProfileLock    = "profile_lock"
)

// logSlowStep writes one line for a step that outlasted slowStepThreshold:
// the step, its duration and how it ended. The line never carries the
// endpoint, a credential, a link token, a verification number or a label.
func logSlowStep(step string, started time.Time, err error) {
	elapsed := time.Since(started)
	if elapsed < slowStepThreshold {
		return
	}
	log.Printf("deviceclient: slow step=%s duration=%s outcome=%s", step, elapsed.Round(time.Millisecond), stepOutcome(err))
}

func stepOutcome(err error) string {
	var refusal *Refusal
	var netErr net.Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &refusal):
		return "refused"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "error"
	}
}
