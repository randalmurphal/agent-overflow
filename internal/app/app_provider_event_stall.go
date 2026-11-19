package app

import (
	"fmt"

	"agent-overflow/internal/provider"
)

// stall queues a handler that holds threadID's worker until releaseStall,
// as a handler stuck on a slow write would. Events queued after it wait,
// and a drain gives up on them at its stall bound. Harness only
// (HarnessStallProviderEvents).
func (qs *providerEventQueues) stall(threadID string) error {
	release := make(chan struct{})
	qs.mu.Lock()
	if _, held := qs.stalls[threadID]; held {
		qs.mu.Unlock()
		return fmt.Errorf("provider events for thread %s are already stalled", threadID)
	}
	if qs.stalls == nil {
		qs.stalls = make(map[string]chan struct{})
	}
	qs.stalls[threadID] = release
	qs.mu.Unlock()
	qs.enqueue(threadID, provider.ProviderEvent{ThreadID: threadID}, func(provider.ProviderEvent) { <-release })
	return nil
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
