package app

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/pump"
	"agent-overflow/internal/transport"
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

// pumpHandle is a forge pump subscriber's handle id with its connection's
// tie, made before the handle or a pump for it exists (bindPumpHandle).
type pumpHandle struct {
	id  string
	tie transport.Tie
}

// bindPumpHandle mints a handle id and ties its release to conn, so the
// caller is admitted before anything is made or published for it: a
// connection that is closing gets an error and costs nothing, not even a
// pump core's context. Callers hold App.prUpdates.mu and publish the handle
// in the same critical section, so a release that runs at once (it takes
// that lock) finds it.
func bindPumpHandle(conn *transport.ConnState, what string, key func(id string) string, release func(id string)) (pumpHandle, error) {
	id := uuid.NewString()
	tie, err := conn.Bind(key(id), func() { release(id) })
	if err != nil {
		return pumpHandle{}, fmt.Errorf("%s: %w", what, err)
	}
	return pumpHandle{id: id, tie: tie}, nil
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
