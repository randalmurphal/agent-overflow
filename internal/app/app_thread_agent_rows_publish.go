package app

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"agent-overflow/internal/eventchan"
)

// A batch keeps the changes that must be projected before the owner can acknowledge them.
type threadAgentChanges struct {
	tokens  map[string]struct{}
	callers map[string]struct{}
	targets map[string]struct{}
	// liveness holds the threads whose rest may have changed, true where
	// their durable sources changed too: each is reassessed
	// (reassessWorker) and framed as a target when it is or was a worker,
	// or a request runs on it. A changed request's caller is one, since
	// whether an answer is on its way to it may have changed, and so is the
	// thread a changed workflow run (workflowRuns) reports to.
	liveness     map[string]bool
	workflowRuns map[string]struct{}
	projections  map[string]struct{}
}

func newThreadAgentChanges() threadAgentChanges {
	return threadAgentChanges{
		tokens: map[string]struct{}{}, callers: map[string]struct{}{}, targets: map[string]struct{}{},
		workflowRuns: map[string]struct{}{}, liveness: map[string]bool{},
	}
}

// Keep failed work with any newer events. A durable change requires a fresh
// store read even if a later event changed only in-memory liveness.
func (e *threadAgentChanges) retain(batch threadAgentChanges) {
	maps.Copy(e.tokens, batch.tokens)
	maps.Copy(e.callers, batch.callers)
	maps.Copy(e.targets, batch.targets)
	maps.Copy(e.workflowRuns, batch.workflowRuns)
	for id, durable := range batch.liveness {
		e.liveness[id] = durable || e.liveness[id]
	}
	if e.projections == nil {
		e.projections = batch.projections
	} else {
		maps.Copy(e.projections, batch.projections)
	}
}

// Publish owner activity after every liveness projection used by a peer read is current.
// Peer receipts can follow a thread even when it has no local Agents row.
func (a *App) publishThreadAgentChanges(batch threadAgentChanges) error {
	tokens, callers, targets := batch.tokens, batch.callers, batch.targets
	liveness, runs, projections := batch.liveness, batch.workflowRuns, batch.projections
	var failed error
	for item := range runs {
		origin, err := a.store.WorkItemOriginThread(item)
		if err != nil {
			failed = errors.Join(failed, err)
		} else if origin != "" {
			liveness[origin] = true
		}
	}
	for caller := range callers {
		liveness[caller] = true
	}
	frameTargets := make(map[string]struct{}, len(targets))
	for thread := range targets {
		if a.threadAgentFollows(thread) {
			frameTargets[thread] = struct{}{}
		}
	}
	reassess := func(thread string, durable bool) bool {
		worker, err := a.reassessWorker(thread, durable)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("reassess %s: %w", thread, err))
		}
		return worker || a.threadAgentFollows(thread)
	}
	for thread, durable := range liveness {
		if reassess(thread, durable) {
			frameTargets[thread] = struct{}{}
		}
	}
	changedTokens := slices.Collect(maps.Keys(tokens))
	frameCallers := slices.Collect(maps.Keys(callers))
	frameCallers = append(frameCallers, slices.Collect(maps.Keys(projections))...)
	if len(tokens)+len(frameCallers)+len(frameTargets) > 0 {
		frames, rest, err := a.store.ThreadAgentRowFrames(changedTokens, slices.Collect(maps.Keys(frameTargets)), frameCallers, threadReceiptLocalOwner, threadAgentRowDepth)
		if err != nil {
			failed = errors.Join(failed, err)
			for _, caller := range frameCallers {
				a.emit(eventchan.ThreadAgentRowsChanged, ThreadAgentRowChange{ThreadID: caller})
			}
		} else {
			for _, thread := range rest {
				if !liveness[thread] && reassess(thread, true) {
					// A send from another caller can also be running on this worker.
					a.noteThreadAgentTarget(thread)
				}
				liveness[thread] = true
			}
			for _, frame := range frames {
				a.emit(eventchan.ThreadAgentRowsChanged, ThreadAgentRowChange{ThreadID: frame.ThreadID, ID: frame.Token})
			}
		}
	}
	if len(liveness)+len(tokens)+len(callers) > 0 {
		a.noteRestWaitsMayChange()
	}
	if failed != nil {
		return failed
	}
	if a.ownerActivity.watching() {
		changedThreads := slices.Collect(maps.Keys(targets))
		changedThreads = append(changedThreads, slices.Collect(maps.Keys(liveness))...)
		owners, err := a.store.ActivityOwners(changedTokens, changedThreads)
		if err != nil {
			a.ownerActivity.reset()
			return fmt.Errorf("reset peer cursors after owner lookup failed: %w", err)
		}
		for _, owner := range owners {
			a.ownerActivity.note(owner.Device, owner.Token, false)
		}
	}
	return nil
}
