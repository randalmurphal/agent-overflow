package app

import (
	"fmt"
	"time"

	"agent-overflow/internal/provider"
)

// providerEventStallAcquire bounds how long stall waits for the worker to
// reach it: under the harness client's 15 s RPC timeout
// (e2e/src/harness-app.ts RPC_TIMEOUT_MS), so the spec reads this error
// rather than the client's.
const providerEventStallAcquire = 10 * time.Second

// stall queues a handler that holds threadID's worker until releaseStall,
// as a handler stuck on a slow write would, and returns once the worker is
// holding it. The worker is serial, so every event queued before the stall
// has been handled by then: from then on the thread's pending count is the
// stall and the events queued behind it, and it only grows until the
// release. Events queued after it wait, and a drain gives up on them at its
// stall bound. When the worker does not reach the stall within its bound, or
// the stall is released first, stall returns an error and holds nothing.
// Harness only (HarnessStallProviderEvents).
func (qs *providerEventQueues) stall(threadID string) error {
	held, release, bound, err := qs.beginStall(threadID)
	if err != nil {
		return err
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-held:
		return nil
	case <-release:
	case <-timer.C:
	}
	qs.mu.Lock()
	defer qs.mu.Unlock()
	select {
	case <-held:
		// The worker reached the stall before the release or the bound.
		return nil
	default:
	}
	if qs.stalls[threadID] != release {
		return fmt.Errorf("provider events for thread %s were released before the worker reached the stall", threadID)
	}
	// Give the reservation up and let the marker through, so it cannot hold
	// the worker later.
	delete(qs.stalls, threadID)
	close(release)
	return fmt.Errorf("provider events for thread %s: the worker did not reach the stall within %s; nothing is stalled", threadID, bound)
}

// beginStall reserves threadID's stall and queues its marker. The worker
// closes held when it reaches the marker with the reservation still in
// place; a marker whose reservation was released or given up first passes
// with held open. Both sides decide under qs.mu, so held closed means the
// worker was held.
func (qs *providerEventQueues) beginStall(threadID string) (held, release chan struct{}, bound time.Duration, err error) {
	release = make(chan struct{})
	held = make(chan struct{})
	qs.mu.Lock()
	if _, taken := qs.stalls[threadID]; taken {
		qs.mu.Unlock()
		return nil, nil, 0, fmt.Errorf("provider events for thread %s are already stalled", threadID)
	}
	if qs.stalls == nil {
		qs.stalls = make(map[string]chan struct{})
	}
	qs.stalls[threadID] = release
	bound = qs.stallAcquire
	if bound <= 0 {
		bound = providerEventStallAcquire
	}
	qs.mu.Unlock()
	qs.enqueue(threadID, provider.ProviderEvent{ThreadID: threadID}, func(provider.ProviderEvent) {
		qs.mu.Lock()
		if qs.stalls[threadID] == release {
			close(held)
		}
		qs.mu.Unlock()
		<-release
	})
	return held, release, bound, nil
}

// releaseStall lets the worker held by stall go on.
func (qs *providerEventQueues) releaseStall(threadID string) error {
	qs.mu.Lock()
	release, held := qs.stalls[threadID]
	delete(qs.stalls, threadID)
	qs.mu.Unlock()
	if !held {
		return fmt.Errorf("provider events for thread %s are not stalled", threadID)
	}
	close(release)
	return nil
}

// pending counts the events queued for threadID that are not handled yet,
// the one being handled included.
func (qs *providerEventQueues) pending(threadID string) int {
	q := qs.lookup(threadID)
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return int(q.enqueued - q.handled)
}
