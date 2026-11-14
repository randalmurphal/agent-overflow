package app

import (
	"context"
	"encoding/json"
	"errors"

	gitops "agent-overflow/internal/git"
)

// The PR pump's CI phase (app_ci_phase.go) is its head pipeline: polled on
// start, again whenever a snapshot poll moves the head or the check
// summary (prCIStamp), and on the phase's own cadence while anything is
// live.

var errPRUpdatesUnknownSubscription = errors.New("pr updates: unknown subscription")

// PRCIUpdatedEvent is the "pr:ci_updated" frame: the PR's pipeline when
// it changed, or a fetch failure when that changed, with the same kind
// fields PRUpdatedEvent carries. Seq is the same per-PR pump sequence
// pr:updated frames carry, so the frontend ranks both against one
// watermark.
type PRCIUpdatedEvent struct {
	PRKey     string             `json:"prKey"`
	Pipeline  *gitops.CIPipeline `json:"pipeline,omitempty"`
	Error     string             `json:"error,omitempty"`
	ErrorKind string             `json:"errorKind,omitempty"`
	Reserve   bool               `json:"reserve,omitempty"`
	ResumeAt  string             `json:"resumeAt,omitempty"`
	Seq       uint64             `json:"seq"`
}

func (a *App) fetchPRCI(ctx context.Context, pr gitops.PRReference, prev *gitops.CIPipeline, stepsFor []string) (gitops.CIPipeline, error) {
	if a.prUpdates.ciFetchFn != nil {
		return a.prUpdates.ciFetchFn(ctx, pr, prev, stepsFor)
	}
	read, err := a.gitCore().ReadPR(ctx, pr, gitops.PRReadParts{CI: true}, prev, stepsFor)
	return read.CI, err
}

// prCIStamp is the part of a snapshot a pipeline poll depends on: a new
// head means a new pipeline, a moved check summary means job state moved.
func prCIStamp(snapshot prUpdateSnapshot) string {
	checks, _ := json.Marshal(snapshot.Detail.Checks)
	return snapshot.Detail.HeadSHA + "\n" + string(checks)
}

// pollPRCI polls the head pipeline (pollCIPhase), recording the snapshot
// it was polled under, and words a change as the PR's frame.
func (a *App) pollPRCI(ctx context.Context, pump *prUpdatePump) (PRCIUpdatedEvent, bool) {
	result, changed := a.pollCIPhase(ctx, &pump.ciPhase, func() {
		pump.ciStamp = prCIStamp(pump.lastSnapshot)
	})
	if !changed {
		return PRCIUpdatedEvent{}, false
	}
	return PRCIUpdatedEvent{
		PRKey:     pump.prKey,
		Pipeline:  result.pipeline,
		Error:     result.fail.wire,
		ErrorKind: result.fail.kind,
		Reserve:   result.fail.reserve,
		ResumeAt:  result.fail.resumeAt,
		Seq:       result.seq,
	}, true
}

// refreshPRCI has the pump poll the pipeline now and reports that poll's
// failure, if any; the pipeline itself travels on pr:ci_updated.
func (a *App) refreshPRCI(ctx context.Context, subscriptionID string) error {
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.handles[subscriptionID]
	if !ok || handle.pump.dead {
		a.prUpdates.mu.Unlock()
		return errPRUpdatesUnknownSubscription
	}
	pump := handle.pump
	a.prUpdates.mu.Unlock()
	return a.requestPump(ctx, &pump.pumpCore, &prPumpRequest{ci: true})
}
