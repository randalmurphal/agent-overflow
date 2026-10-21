package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// The CI phase every pump that shows a pipeline owns: the PR pump's head
// pipeline (app_forge_ci.go) and a pipeline pump's one pipeline
// (app_pipeline_pump.go). It holds the pipeline last fetched and the logs
// of the jobs somebody is watching, so one pipeline costs one poller
// however many panes show it, pauses with them, and emits each
// observation once. Cadence follows what can still change: nothing while
// every job is terminal (the owner re-arms it), prCILiveInterval while a
// job is queued or running (a followed running job's log included, unless
// its forge streams it), prCIFollowInterval while a followed job is live
// on a forge that streams running logs, its final log is still being
// fetched or a download it was cut off in resumes, and prCILogWaitInterval
// while a completed job's log stays unpublished after the quick tries. The
// logs themselves are in app_forge_ci_log.go.
//
// Everything below runs on the owning pump's goroutine, including the work
// an RPC asks for through its requests: one writer per pump keeps frame
// order equal to sequence order, which a log's versions depend on.

const (
	defaultPRCILiveInterval   = 10 * time.Second
	defaultPRCIFollowInterval = 5 * time.Second
	// prCILogFinalAttempts bounds the fast phase of the wait for a
	// completed job's log: that many tries at prCIFollowInterval. A forge
	// that still answers 404 (GitHub publishes the log some time after the
	// job reports complete) is then asked at prCILogWaitInterval for as long
	// as the follow stands. Any other failure is shown once the fast phase
	// ran out.
	prCILogFinalAttempts = 6
)

// ciPhase is one pump's pipeline and followed logs, guarded by
// App.prUpdates.mu but for retryDelay, which only the pump goroutine
// touches. Its configuration is fixed at creation: the repository the
// pipeline and its job logs belong to, the keys its log frames carry (a
// PR pump's PR key, or a pipeline pump's key), how to fetch the pipeline
// and how to word a failure.
type ciPhase struct {
	core        *pumpCore
	repo        gitops.RepoReference
	prKey       string
	pipelineKey string
	fetch       func(ctx context.Context, prev *gitops.CIPipeline, stepsFor []string) (gitops.CIPipeline, error)
	failure     func(err error) prForgeFailure

	// ci is the last pipeline fetched (meaningful once ciKnown), ciLast its
	// encoding for change detection, ciFail the active fetch failure.
	// ciDirty is set by an owner that saw the pipeline move and by a new
	// follow (whose steps the next pipeline poll fills). follows holds the
	// logs being watched, by job id.
	ci      gitops.CIPipeline
	ciKnown bool
	ciLast  []byte
	ciFail  prForgeFailure
	ciDirty bool
	follows map[string]*prCILogFollow
	// retryDelay is the doubling delay while the pipeline fetch fails.
	retryDelay time.Duration
}

// name is the phase in log lines.
func (p *ciPhase) name() string {
	if p.pipelineKey != "" {
		return "pipeline=" + p.pipelineKey
	}
	return "pr=" + p.prKey
}

// prPumpRequest is work an RPC hands a pump goroutine: poll the snapshot
// (a PR pump after a merge action), the pipeline (ci) and fetch these
// jobs' logs now, paused or not. done closes once the poll ran; err is the
// pipeline poll's failure when ci was asked.
type prPumpRequest struct {
	snapshot bool
	ci       bool
	jobIDs   []string
	done     chan struct{}
	err      error
}

func (a *App) ciLogStreams(repo gitops.RepoReference) bool {
	if a.prUpdates.ciLogStreams != nil {
		return *a.prUpdates.ciLogStreams
	}
	return a.gitCore().CILogStreams(repo)
}

func (a *App) prCILiveInterval() time.Duration {
	if a.prUpdates.ciLiveInterval > 0 {
		return a.prUpdates.ciLiveInterval
	}
	return defaultPRCILiveInterval
}

func (a *App) prCIFollowInterval() time.Duration {
	if a.prUpdates.ciFollowInterval > 0 {
		return a.prUpdates.ciFollowInterval
	}
	return defaultPRCIFollowInterval
}

// prCILogWaitInterval paces a followed job's log the forge has still not
// published after prCILogFinalAttempts tries: the snapshot cadence.
func (a *App) prCILogWaitInterval() time.Duration {
	if a.prUpdates.ciLogWaitInterval > 0 {
		return a.prUpdates.ciLogWaitInterval
	}
	return a.prUpdatePollInterval()
}

// ciInterval is the delay until the phase's next poll, 0 for none: the
// shortest cadence anything still changing asks for. While the pipeline
// fetch fails the phase retries on a doubling delay from the live cadence
// up to the snapshot interval, reset once it succeeds. A rate-limited
// pipeline or log waits for its failure's release instead. A live
// followed job's log is asked for at the follow cadence where the forge
// streams it, and at the live cadence elsewhere.
func (a *App) ciInterval(phase *ciPhase) time.Duration {
	streams := a.ciLogStreams(phase.repo)
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	if phase.ciFail.rateLimited() {
		phase.retryDelay = 0
		return heldUntil(phase.ciFail.release)
	}
	if phase.ciFail.failing() {
		if phase.retryDelay == 0 {
			phase.retryDelay = a.prCILiveInterval()
		} else {
			phase.retryDelay = min(phase.retryDelay*2, a.prUpdatePollInterval())
		}
		return phase.retryDelay
	}
	phase.retryDelay = 0
	var next time.Duration
	consider := func(interval time.Duration) {
		if next == 0 || interval < next {
			next = interval
		}
	}
	for _, follow := range phase.follows {
		switch {
		case follow.fail.rateLimited():
			consider(heldUntil(follow.fail.release))
		case follow.downloading:
			consider(a.prCIFollowInterval())
		case follow.pendingFinal && follow.finalAttempts < prCILogFinalAttempts:
			consider(a.prCIFollowInterval())
		case follow.pendingFinal:
			consider(a.prCILogWaitInterval())
		case follow.wasLive && streams:
			consider(a.prCIFollowInterval())
		case follow.wasLive:
			consider(a.prCILiveInterval())
		}
	}
	if phase.ciKnown && gitops.CIPipelineLive(phase.ci) {
		consider(a.prCILiveInterval())
	}
	return next
}

func (a *App) ciWantsPoll(phase *ciPhase) bool {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	return phase.ciDirty || !phase.ciKnown
}

// ciPollResult is a pipeline poll that changed what the phase shows: the
// new pipeline, or the new failure, and the sequence it was stamped with.
type ciPollResult struct {
	pipeline *gitops.CIPipeline
	fail     prForgeFailure
	seq      uint64
}

// pollCIPhase fetches the pipeline and folds it into the phase: compare
// and store under the lock, report only a change, dedup identical
// failures, store nothing on a dead pump. A background poll before a rate
// limit's release asks nothing; a person's request (an interactive ctx)
// still goes to the transport, whose gate decides. before runs in the
// critical section the poll starts in, once it is going ahead.
func (a *App) pollCIPhase(ctx context.Context, phase *ciPhase, before func()) (ciPollResult, bool) {
	a.prUpdates.mu.Lock()
	if phase.core.dead || (!forgeapi.IsInteractive(ctx) && time.Now().Before(phase.ciFail.release)) {
		a.prUpdates.mu.Unlock()
		return ciPollResult{}, false
	}
	if before != nil {
		before()
	}
	phase.ciDirty = false
	var prev *gitops.CIPipeline
	if phase.ciKnown {
		known := phase.ci
		prev = &known
	}
	// The followed jobs are the ones whose steps anyone shows.
	stepsFor := make([]string, 0, len(phase.follows))
	for jobID := range phase.follows {
		stepsFor = append(stepsFor, jobID)
	}
	a.prUpdates.mu.Unlock()

	pipeline, err := phase.fetch(ctx, prev, stepsFor)
	if err == nil {
		var encoded []byte
		encoded, err = json.Marshal(pipeline)
		if err == nil {
			a.prUpdates.mu.Lock()
			if phase.core.dead {
				a.prUpdates.mu.Unlock()
				return ciPollResult{}, false
			}
			unchanged := phase.ciKnown && string(encoded) == string(phase.ciLast) && !phase.ciFail.failing()
			var seq uint64
			if !unchanged {
				phase.ci = pipeline
				phase.ciKnown = true
				phase.ciLast = encoded
				phase.ciFail = prForgeFailure{}
				phase.core.seq = a.nextPRUpdateSeqLocked()
				seq = phase.core.seq
			}
			a.prUpdates.mu.Unlock()
			if unchanged {
				return ciPollResult{}, false
			}
			return ciPollResult{pipeline: &pipeline, seq: seq}, true
		}
	}
	fail := phase.failure(err)
	a.prUpdates.mu.Lock()
	if phase.core.dead {
		a.prUpdates.mu.Unlock()
		return ciPollResult{}, false
	}
	duplicate := phase.ciFail.key == fail.key
	var seq uint64
	if !duplicate {
		phase.ciFail = fail
		phase.core.seq = a.nextPRUpdateSeqLocked()
		seq = phase.core.seq
	}
	a.prUpdates.mu.Unlock()
	if duplicate {
		return ciPollResult{}, false
	}
	log.Printf("pr updates: ci poll failed for %s (id: %s): %v", phase.name(), fail.correlationID, err)
	return ciPollResult{fail: fail, seq: seq}, true
}

// runCIPhase is one turn of the phase on its pump goroutine: the pipeline
// when asked or dirty (pollPipeline emits it in the owner's frame), then
// every followed log, each change emitted on pr:ci_log. It returns the
// delay until the phase wants its next turn, 0 for none.
func (a *App) runCIPhase(ctx context.Context, phase *ciPhase, forced map[string]bool, poll bool, pollPipeline func(context.Context)) time.Duration {
	if poll || a.ciWantsPoll(phase) {
		pollPipeline(ctx)
	}
	for _, event := range a.pollCILogs(ctx, phase, forced) {
		a.emitPumpEvent(phase.core, eventchan.PRCILog, event)
	}
	return a.ciInterval(phase)
}

// servePumpRequest runs on the pump goroutine: polls what the request asks
// for and emits what changed, then reports the pipeline poll's active
// failure when the request asked for one. runSnapshot is nil for a pump
// with no snapshot. The polls run under the pump's context, marked
// interactive: a person asked for them.
func (a *App) servePumpRequest(phase *ciPhase, req *prPumpRequest, runSnapshot func(ctx context.Context), runCI func(ctx context.Context, forced map[string]bool, pollPipeline bool)) {
	ctx := forgeapi.WithInteractive(phase.core.ctx)
	if req.snapshot && runSnapshot != nil {
		runSnapshot(ctx)
	}
	forced := make(map[string]bool, len(req.jobIDs))
	for _, jobID := range req.jobIDs {
		forced[jobID] = true
	}
	// A moved snapshot marks the pipeline dirty; runCI polls it then.
	runCI(ctx, forced, req.ci)
	if req.ci {
		a.prUpdates.mu.Lock()
		if phase.ciFail.failing() {
			req.err = errors.New(phase.ciFail.wire)
		}
		a.prUpdates.mu.Unlock()
	}
	close(req.done)
}
