package deviceclient

import (
	"time"

	"agent-overflow/internal/logging"
)

// Credential steps, as logged.
const (
	stepPair           = "pair"
	stepRenew          = "renew"
	stepTicket         = "ticket"
	stepRenewalSupport = "renewal_support"
	stepRouteProbe     = "route_probe"
	stepProfileLock    = "profile_lock"
)

// logSlowStep logs a credential step that outlasted the threshold
// (logging.LogSlowStep).
func logSlowStep(step string, started time.Time, err error) {
	logging.LogSlowStep("deviceclient", step, started, logging.ErrorOutcome(err))
}
