package app

import (
	"context"
	"errors"
	"log"
	"time"
)

// The deferred phase of store migration v121: it stamps the subagent
// anchors written before the history triggers kept stamps. It starts once
// the store is open, visits the threads the migration listed in turn, and
// ends when the list is empty; a quit resumes from the list at the next
// start. Until then reads walk those anchors, so nothing waits on it.
//
// Each RecomputeSubagentAggregates call computes on a read snapshot and
// holds the writer only for its short stamp writes, and the loop pauses
// between calls, which keeps every write stall under docs/decisions.md's
// 100 ms rule.
const (
	subagentBackfillBatch = 16
	subagentBackfillPause = 25 * time.Millisecond
	subagentBackfillIdle  = time.Second
)

func (a *App) startSubagentAggregateBackfill() {
	if a.store == nil {
		return
	}
	stop, started := a.subagentBackfill.start()
	if !started {
		return
	}
	go func() {
		defer a.subagentBackfill.done()
		ctx, cancel := context.WithCancel(a.lifeCtx())
		watcherDone := make(chan struct{})
		defer func() {
			cancel()
			<-watcherDone
		}()
		go func() {
			defer close(watcherDone)
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		a.runSubagentAggregateBackfill(ctx)
	}()
}

func (a *App) runSubagentAggregateBackfill(ctx context.Context) {
	pause := func(delay time.Duration) bool {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	after := ""
	progress := false
	for ctx.Err() == nil {
		threadID, err := a.store.NextSubagentAggregateBackfillThread(ctx, after)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("subagent aggregate backfill: %v", err)
			}
			if !pause(subagentBackfillIdle) {
				return
			}
			continue
		}
		if threadID == "" {
			if after == "" {
				return
			}
			// A full pass that stamped nothing is waiting on rows that
			// keep moving or on an error it logged; retry slowly.
			delay := subagentBackfillPause
			if !progress {
				delay = subagentBackfillIdle
			}
			after, progress = "", false
			if !pause(delay) {
				return
			}
			continue
		}
		result, err := a.store.RecomputeSubagentAggregates(ctx, threadID, subagentBackfillBatch)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("subagent aggregate backfill %s: %v", threadID, err)
		}
		progress = progress || result.Stamped > 0 || !result.Remaining
		// A thread with more to do keeps its place while its batches
		// land; the others wait behind it one batch at a time. A batch
		// that landed nothing (its rows moved under it) sends the
		// thread to the next pass, so it cannot hold the loop.
		if err != nil || !result.Remaining || result.Stamped == 0 {
			after = threadID
		}
		if !pause(subagentBackfillPause) {
			return
		}
	}
}
