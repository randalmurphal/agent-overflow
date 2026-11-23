package app

import (
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/pump"
)

// pumpCore is what every forge pump shares (the PR pump, the pipeline
// pump, the PR list pump): the pump package's core, whose ...Locked
// methods run under App.prUpdates.mu, and the sequence the pump's state is
// stamped with. Every pump shares that lock and stamps from
// App.prUpdates.seq, so one global sequence orders all their frames.
type pumpCore struct {
	pump.Core[prPumpRequest]
	// seq stamps the state the pump holds, under App.prUpdates.mu.
	seq uint64
}

// newPumpCore is a live core under the app's lifetime. A request it
// cannot hand over fails as an unknown subscription once the pump
// stopped, and as ErrShuttingDown once the app's lifetime ended.
func (a *App) newPumpCore() pumpCore {
	return pumpCore{Core: pump.NewCore[prPumpRequest](a.lifeCtx(), pump.Refusals{
		Stopped:       errPRUpdatesUnknownSubscription,
		LifetimeEnded: ErrShuttingDown,
	})}
}

// newPumpRetry is a forge pump's retry schedule: doubling from the PR
// update retry base up to the pump's interval.
func (a *App) newPumpRetry(interval time.Duration) pump.Retry {
	return pump.Retry{Base: a.prUpdateRetryBase(), Interval: interval}
}

// emitPumpEvent emits on behalf of a pump unless it was stopped in the
// meantime.
func (a *App) emitPumpEvent(core *pumpCore, channel eventchan.Channel, event any) {
	select {
	case <-core.Done():
		return
	default:
	}
	a.emit(channel, event)
}
