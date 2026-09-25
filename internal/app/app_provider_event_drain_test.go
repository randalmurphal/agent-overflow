package app

import (
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// A drain waits for the whole backlog while its worker keeps handling it
// (providerEventDrainStall): a Stop of many agents reads a kill frame for
// each agent and shell, and a drain that gave up on the clock would leave
// the rest for CleanupThread to drop.

const (
	drainTestStall   = 80 * time.Millisecond
	drainTestEvents  = 12
	drainTestHandler = drainTestStall / 4
)

func slowHandler(rec *seqRecorder) func(provider.ProviderEvent) {
	return func(evt provider.ProviderEvent) {
		time.Sleep(drainTestHandler)
		rec.record(eventSeq(evt))
	}
}

func TestProviderEventDrainWaitsForABacklogThatKeepsMoving(t *testing.T) {
	const threadID = "thread-drain-backlog"
	qs := providerEventQueues{drainStall: drainTestStall}
	rec := &seqRecorder{}
	for seq := range drainTestEvents {
		enqueueWithin(t, &qs, threadID, seqEvent(seq, 0), slowHandler(rec))
	}
	start := time.Now()
	if err := qs.drain(threadID); err != nil {
		t.Fatalf("drain after %s: %v", time.Since(start), err)
	}
	if elapsed := time.Since(start); elapsed < 2*drainTestStall {
		t.Fatalf("the backlog drained in %s; the case needs one longer than the stall bound", elapsed)
	}
	rec.assertInOrder(t, threadID, drainTestEvents)
}

func TestProviderEventDrainAllWaitsForEveryBacklogThatKeepsMoving(t *testing.T) {
	qs := providerEventQueues{drainStall: drainTestStall}
	recs := map[string]*seqRecorder{}
	for n := range 3 {
		threadID := fmt.Sprintf("thread-drain-all-%d", n)
		recs[threadID] = &seqRecorder{}
		for seq := range drainTestEvents {
			enqueueWithin(t, &qs, threadID, seqEvent(seq, 0), slowHandler(recs[threadID]))
		}
	}
	if err := qs.drainAll(); err != nil {
		t.Fatalf("drainAll: %v", err)
	}
	for threadID, rec := range recs {
		rec.assertInOrder(t, threadID, drainTestEvents)
	}
}
