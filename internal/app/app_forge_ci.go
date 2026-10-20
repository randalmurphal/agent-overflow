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

// The CI phase of the per-PR update pump (app_forge_review.go). The pump
// that polls a pull request's detail and threads also owns its head
// pipeline and the logs of the jobs somebody is watching, so one PR costs
// one poller however many panes show it, pauses with them, and emits each
// observation once. Cadence follows what can still change: nothing while
// every job is terminal (the 45s snapshot re-arms it when the head or the
// check summary moves), prCILiveInterval while a job is queued or running
// (a followed running job's log included, unless its forge streams it),
// prCIFollowInterval while a followed job is live on a forge that streams
// running logs, its final log is still being fetched or a download it
// was cut off in resumes, and prCILogWaitInterval while a completed job's
// log stays unpublished after the quick tries. The logs themselves are in
// app_forge_ci_log.go.
//
// Everything below runs on the pump goroutine, including the work an RPC
// asks for through pump.requests: one writer per PR keeps frame order
// equal to sequence order, which a log's versions depend on.

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

// prPumpRequest is work an RPC hands the pump goroutine: poll the pipeline
// (ci) and fetch these jobs' logs now, pause or not. done closes once the
// poll ran; err is the pipeline poll's failure when ci was asked.
type prPumpRequest struct {
	// snapshot polls the detail, threads and events now (after a merge
	// action); ci the pipeline; jobIDs the logs to force.
	snapshot bool
	ci       bool
	jobIDs   []string
	done     chan struct{}
	err      error
}

func (a *App) fetchPRCI(ctx context.Context, pr gitops.PRReference, prev *gitops.CIPipeline, stepsFor []string) (gitops.CIPipeline, error) {
	if a.prUpdates.ciFetchFn != nil {
		return a.prUpdates.ciFetchFn(ctx, pr, prev, stepsFor)
	}
	read, err := a.gitCore().ReadPR(ctx, pr, gitops.PRReadParts{CI: true}, prev, stepsFor)
	return read.CI, err
}

func (a *App) prCILogStreams(pr gitops.PRReference) bool {
	if a.prUpdates.ciLogStreams != nil {
		return *a.prUpdates.ciLogStreams
	}
	return a.gitCore().CILogStreams(pr)
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

// prCIStamp is the part of a snapshot a pipeline poll depends on: a new
// head means a new pipeline, a moved check summary means job state moved.
func prCIStamp(snapshot prUpdateSnapshot) string {
	checks, _ := json.Marshal(snapshot.Detail.Checks)
	return snapshot.Detail.HeadSHA + "\n" + string(checks)
}

// prCIInterval is the delay until the next CI poll, 0 for none: the
// shortest cadence anything still changing asks for. retry is the loop's
// doubling delay while the pipeline fetch fails, reset here once it
// succeeds. A rate-limited pipeline or log waits for its failure's
// release instead. A live followed job's log is asked for at the follow
// cadence where the forge streams it, and at the live cadence elsewhere.
func (a *App) prCIInterval(pump *prUpdatePump, retry *time.Duration) time.Duration {
	streams := a.prCILogStreams(pump.pr)
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	if pump.ciFail.rateLimited() {
		*retry = 0
		return heldUntil(pump.ciFail.release)
	}
	if pump.ciFail.failing() {
		if *retry == 0 {
			*retry = a.prCILiveInterval()
		} else {
			*retry = min(*retry*2, a.prUpdatePollInterval())
		}
		return *retry
	}
	*retry = 0
	var next time.Duration
	consider := func(interval time.Duration) {
		if next == 0 || interval < next {
			next = interval
		}
	}
	for _, follow := range pump.follows {
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
	if pump.ciKnown && gitops.CIPipelineLive(pump.ci) {
		consider(a.prCILiveInterval())
	}
	return next
}

func (a *App) prCIWantsPoll(pump *prUpdatePump) bool {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	return pump.ciDirty || !pump.ciKnown
}

// pollPRCI fetches the pipeline and folds it into the pump's CI state,
// the same way pollPRUpdate folds a snapshot: compare and store under the
// lock, emit only on change, dedup identical failures, store nothing on a
// dead pump. A background poll before a rate limit's release asks
// nothing; a person's request (an interactive ctx) still goes to the
// transport, whose gate decides.
func (a *App) pollPRCI(ctx context.Context, pump *prUpdatePump) (PRCIUpdatedEvent, bool) {
	a.prUpdates.mu.Lock()
	if pump.dead || (!forgeapi.IsInteractive(ctx) && time.Now().Before(pump.ciFail.release)) {
		a.prUpdates.mu.Unlock()
		return PRCIUpdatedEvent{}, false
	}
	pump.ciStamp = prCIStamp(pump.lastSnapshot)
	pump.ciDirty = false
	var prev *gitops.CIPipeline
	if pump.ciKnown {
		known := pump.ci
		prev = &known
	}
	// The followed jobs are the ones whose steps anyone shows.
	stepsFor := make([]string, 0, len(pump.follows))
	for jobID := range pump.follows {
		stepsFor = append(stepsFor, jobID)
	}
	a.prUpdates.mu.Unlock()

	pipeline, err := a.fetchPRCI(ctx, pump.pr, prev, stepsFor)
	if err == nil {
		var encoded []byte
		encoded, err = json.Marshal(pipeline)
		if err == nil {
			a.prUpdates.mu.Lock()
			if pump.dead {
				a.prUpdates.mu.Unlock()
				return PRCIUpdatedEvent{}, false
			}
			unchanged := pump.ciKnown && string(encoded) == string(pump.ciLast) && !pump.ciFail.failing()
			var seq uint64
			if !unchanged {
				pump.ci = pipeline
				pump.ciKnown = true
				pump.ciLast = encoded
				pump.ciFail = prForgeFailure{}
				pump.seq = a.nextPRUpdateSeqLocked()
				seq = pump.seq
			}
			a.prUpdates.mu.Unlock()
			if unchanged {
				return PRCIUpdatedEvent{}, false
			}
			return PRCIUpdatedEvent{PRKey: pump.prKey, Pipeline: &pipeline, Seq: seq}, true
		}
	}
	fail := a.newPRForgeFailure(pump.pr, err)
	a.prUpdates.mu.Lock()
	if pump.dead {
		a.prUpdates.mu.Unlock()
		return PRCIUpdatedEvent{}, false
	}
	duplicate := pump.ciFail.key == fail.key
	var seq uint64
	if !duplicate {
		pump.ciFail = fail
		pump.seq = a.nextPRUpdateSeqLocked()
		seq = pump.seq
	}
	a.prUpdates.mu.Unlock()
	if duplicate {
		return PRCIUpdatedEvent{}, false
	}
	log.Printf("pr updates: ci poll failed for pr=%s (id: %s): %v", pump.prKey, fail.correlationID, err)
	return PRCIUpdatedEvent{
		PRKey:     pump.prKey,
		Error:     fail.wire,
		ErrorKind: fail.kind,
		Reserve:   fail.reserve,
		ResumeAt:  fail.resumeAt,
		Seq:       seq,
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
	return a.requestPRPump(ctx, pump, &prPumpRequest{ci: true})
}

// requestPRPump hands a request to the pump goroutine and waits for it to
// run. The pump serves requests whether or not it is paused: this is a
// person's action, not a background tick.
func (a *App) requestPRPump(ctx context.Context, pump *prUpdatePump, req *prPumpRequest) error {
	req.done = make(chan struct{})
	select {
	case pump.requests <- req:
	case <-pump.done:
		return errPRUpdatesUnknownSubscription
	case <-ctx.Done():
		return ctx.Err()
	case <-a.lifeCtx().Done():
		return ErrShuttingDown
	}
	select {
	case <-req.done:
		return req.err
	case <-pump.done:
		return errPRUpdatesUnknownSubscription
	case <-ctx.Done():
		return ctx.Err()
	}
}

// servePRPumpRequest runs on the pump goroutine: polls what the request
// asks for and emits what changed. Returns the pipeline poll's active
// failure when the request asked for one. The polls run under the pump's
// context, marked interactive: a person asked for them.
func (a *App) servePRPumpRequest(pump *prUpdatePump, req *prPumpRequest, runSnapshot func(ctx context.Context), runCI func(ctx context.Context, forced map[string]bool, pollPipeline bool)) {
	ctx := forgeapi.WithInteractive(pump.ctx)
	if req.snapshot {
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
		if pump.ciFail.failing() {
			req.err = errors.New(pump.ciFail.wire)
		}
		a.prUpdates.mu.Unlock()
	}
	close(req.done)
}

// emitPRPumpEvent emits on behalf of a pump unless it was torn down in
// the meantime, the same guard the snapshot emit has.
func (a *App) emitPRPumpEvent(pump *prUpdatePump, channel eventchan.Channel, event any) {
	select {
	case <-pump.done:
		return
	default:
	}
	a.emit(channel, event)
}
