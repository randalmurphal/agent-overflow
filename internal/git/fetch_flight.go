package git

import (
	"context"
	"sync"
)

// fetchFlights runs one fetch per repository for every caller waiting on
// it. A caller leaving ends only its own wait; the fetch's context ends
// when its last waiter leaves, so one caller's bound or cancellation never
// ends a fetch another caller still waits for, and a cadence shutting down
// still kills a fetch hanging on a dead network.
type fetchFlights struct {
	mu      sync.Mutex
	flights map[string]*fetchFlight
}

type fetchFlight struct {
	done    chan struct{}
	fetched bool
	err     error
	waiters int
	cancel  context.CancelFunc
}

// do joins key's fetch or starts fn as it. The fetch keeps the starting
// caller's values, not its cancellation.
func (f *fetchFlights) do(ctx context.Context, key string, fn func(context.Context) (bool, error)) (bool, error) {
	f.mu.Lock()
	flight := f.flights[key]
	if flight == nil {
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &fetchFlight{done: make(chan struct{}), cancel: cancel}
		if f.flights == nil {
			f.flights = make(map[string]*fetchFlight)
		}
		f.flights[key] = flight
		go func() {
			fetched, err := fn(runCtx)
			cancel()
			f.mu.Lock()
			f.forget(key, flight)
			flight.fetched, flight.err = fetched, err
			f.mu.Unlock()
			close(flight.done)
		}()
	}
	flight.waiters++
	f.mu.Unlock()

	select {
	case <-flight.done:
		return flight.fetched, flight.err
	case <-ctx.Done():
		f.mu.Lock()
		flight.waiters--
		if flight.waiters == 0 {
			// Nobody waits: end the fetch, and let the next caller start
			// a fresh one rather than join it.
			flight.cancel()
			f.forget(key, flight)
		}
		f.mu.Unlock()
		return false, ctx.Err()
	}
}

// forget drops flight from the table if it is still key's. Callers hold mu.
func (f *fetchFlights) forget(key string, flight *fetchFlight) {
	if f.flights[key] == flight {
		delete(f.flights, key)
	}
}
