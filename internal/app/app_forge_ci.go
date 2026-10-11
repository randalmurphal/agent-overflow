package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

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
// check summary moves), prCILiveInterval while a job is queued or running,
// prCIFollowInterval while a followed job is live on a forge that serves
// running logs or its final log is still being fetched, and
// prCILogWaitInterval while a completed job's log stays unpublished after
// the quick tries.
//
// Everything below runs on the pump goroutine, including the work an RPC
// asks for through pump.requests: one writer per PR keeps frame order
// equal to sequence order, which the log deltas depend on.

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

// maxPRCILogFollowsPerHandle bounds the jobs one subscription follows. A
// follow holds up to ciLogDisplayTailBytes of text and costs one forge
// fetch per tick, serially, so an unbounded list is the same exhaustion
// surface maxPRUpdateHandles closes. A pane shows one log; this is far
// above any real UI.
const maxPRCILogFollowsPerHandle = 16

// ErrTooManyPRCILogFollows is returned when one call asks a subscription
// to follow more jobs than maxPRCILogFollowsPerHandle. Typed like
// ErrTooManyPRUpdateSubscriptions: retrying the same call never fixes it.
var ErrTooManyPRCILogFollows = fmt.Errorf("pr updates: too many CI log follows (limit %d)", maxPRCILogFollowsPerHandle)

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

// PRCILogEvent is the "pr:ci_log" frame for one followed job. A delta
// frame says: keep the first Base UTF-16 units of the text you hold (which
// must be PrevLen units long) and append Append. A receiver holding
// anything else missed a frame and asks for the whole text again through
// SetPRCILogFollows. Available false means the forge cannot serve the log
// yet: it answered 404 (the log is not published yet, which on GitHub can
// last a running job's whole run), or the job is live on a forge that
// serves no running logs; the text is unchanged. Error is a fetch failure with its kind fields (see
// PRUpdatedEvent), text unchanged.
type PRCILogEvent struct {
	PRKey      string `json:"prKey"`
	JobID      string `json:"jobId"`
	Seq        uint64 `json:"seq"`
	PrevLen    int    `json:"prevLen"`
	Base       int    `json:"base"`
	Append     string `json:"append"`
	Truncated  bool   `json:"truncated"`
	TotalBytes int    `json:"totalBytes"`
	Available  bool   `json:"available"`
	Error      string `json:"error,omitempty"`
	ErrorKind  string `json:"errorKind,omitempty"`
	Reserve    bool   `json:"reserve,omitempty"`
	ResumeAt   string `json:"resumeAt,omitempty"`
}

// prCILogFollow is one followed job's log state, refcounted across the
// handles following it. Guarded by App.prUpdates.mu.
type prCILogFollow struct {
	refs int
	// fetched is false until the first poll decided something about this
	// job (fetched its text, or found it unavailable while running).
	fetched    bool
	available  bool
	text       string
	truncated  bool
	totalBytes int
	// etag validates text: the next fetch sends it, and a 304 keeps text.
	etag string
	// fail is the active fetch failure, as the pump's fail.
	fail prForgeFailure
	// wasLive is the job's liveness at the last poll; pendingFinal marks
	// the live-to-terminal transition until the final log is in hand.
	wasLive       bool
	pendingFinal  bool
	finalAttempts int
	seq           uint64
}

// prPumpRequest is work an RPC hands the pump goroutine: poll the pipeline
// (ci) and fetch these jobs' logs now, pause or not. done closes once the
// poll ran; err is the pipeline poll's failure when ci was asked.
type prPumpRequest struct {
	ci     bool
	jobIDs []string
	done   chan struct{}
	err    error
}

func (a *App) fetchPRCI(ctx context.Context, pr gitops.PRReference, prev *gitops.CIPipeline, stepsFor []string) (gitops.CIPipeline, error) {
	if a.prUpdates.ciFetchFn != nil {
		return a.prUpdates.ciFetchFn(ctx, pr, prev, stepsFor)
	}
	read, err := a.gitCore().ReadPR(ctx, pr, gitops.PRReadParts{CI: true}, prev, stepsFor)
	return read.CI, err
}

func (a *App) fetchPRCILog(ctx context.Context, pr gitops.PRReference, req gitops.CIJobLogRequest) (gitops.CIJobLog, error) {
	if a.prUpdates.ciLogFetchFn != nil {
		return a.prUpdates.ciLogFetchFn(ctx, pr, req)
	}
	return a.gitCore().GetCIJobLog(ctx, pr, req)
}

func (a *App) prCILogWhileRunning(pr gitops.PRReference) bool {
	if a.prUpdates.ciLogWhileRunning != nil {
		return *a.prUpdates.ciLogWhileRunning
	}
	return a.gitCore().CILogWhileRunning(pr)
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
// release instead. A live followed job on a forge that serves no running
// logs asks nothing of its own: the pipeline's live cadence carries its
// steps.
func (a *App) prCIInterval(pump *prUpdatePump, retry *time.Duration) time.Duration {
	whileRunning := a.prCILogWhileRunning(pump.pr)
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
		case follow.pendingFinal && follow.finalAttempts < prCILogFinalAttempts:
			consider(a.prCIFollowInterval())
		case follow.pendingFinal:
			consider(a.prCILogWaitInterval())
		case follow.wasLive && whileRunning:
			consider(a.prCIFollowInterval())
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
	fail := a.newPRForgeFailure(err)
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

// prCILogPlan is one job's decision for this tick, taken under the lock
// from the pipeline the pump holds.
type prCILogPlan struct {
	jobID string
	// fetch asks for the trace; unavailable records that the job is live
	// on a forge without running logs (nothing to fetch, state to emit).
	fetch       bool
	unavailable bool
	// live is the job's liveness the plan was made from.
	live bool
	// etag is the validator of the text the follow holds, sent with the
	// fetch so an unchanged log answers 304.
	etag string
}

// pollPRCILogs advances every followed job: decides under the lock what
// each needs, fetches outside it, and folds the results in. forced jobs
// (an RPC's) are fetched whatever their state; any other job held by a
// rate limit waits for its release.
func (a *App) pollPRCILogs(ctx context.Context, pump *prUpdatePump, forced map[string]bool) []PRCILogEvent {
	whileRunning := a.prCILogWhileRunning(pump.pr)
	now := time.Now()
	a.prUpdates.mu.Lock()
	if pump.dead || len(pump.follows) == 0 {
		a.prUpdates.mu.Unlock()
		return nil
	}
	plans := make([]prCILogPlan, 0, len(pump.follows))
	for jobID, follow := range pump.follows {
		job := gitops.FindCIJob(pump.ci, jobID)
		// A job the current pipeline no longer lists (the head moved on)
		// is terminal for following purposes: one final fetch, then idle.
		live := pump.ciKnown && job != nil && gitops.CIJobLive(job.Status)
		if follow.wasLive && !live {
			follow.pendingFinal = true
			follow.finalAttempts = 0
		}
		follow.wasLive = live
		etag := ""
		if follow.fetched && follow.available {
			etag = follow.etag
		}
		held := !forced[jobID] && now.Before(follow.fail.release)
		switch {
		case live && !whileRunning:
			// Nothing to fetch; record the state once (and over an error
			// a fetch made before the pipeline said the job was live).
			if !follow.fetched || follow.available || follow.fail.failing() {
				plans = append(plans, prCILogPlan{jobID: jobID, unavailable: true, live: true})
			}
		case held:
		case live:
			plans = append(plans, prCILogPlan{jobID: jobID, fetch: true, live: true, etag: etag})
		default:
			if forced[jobID] || !follow.fetched || follow.pendingFinal || follow.fail.rateLimited() {
				plans = append(plans, prCILogPlan{jobID: jobID, fetch: true, etag: etag})
			}
		}
	}
	a.prUpdates.mu.Unlock()

	type fetched struct {
		plan prCILogPlan
		log  gitops.CIJobLog
		err  error
	}
	results := make([]fetched, 0, len(plans))
	for _, plan := range plans {
		result := fetched{plan: plan}
		if plan.fetch {
			result.log, result.err = a.fetchPRCILog(ctx, pump.pr, gitops.CIJobLogRequest{JobID: plan.jobID, ETag: plan.etag})
		}
		results = append(results, result)
	}

	var events []PRCILogEvent
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	if pump.dead {
		return nil
	}
	for _, result := range results {
		jobID := result.plan.jobID
		follow := pump.follows[jobID]
		if follow == nil {
			// Unfollowed while the fetch ran.
			continue
		}
		frame := PRCILogEvent{PRKey: pump.prKey, JobID: jobID}
		switch {
		case result.plan.unavailable:
			follow.fetched = true
			follow.available = false
			follow.fail = prForgeFailure{}
			follow.seq = a.nextPRUpdateSeqLocked()
			frame.PrevLen = utf16Len(follow.text)
			frame.Base = frame.PrevLen
		case result.err == nil && result.log.NotModified:
			// The log is the text the follow holds; only a failure or an
			// unavailable state it showed over that text changes.
			changed := !follow.available || follow.fail.failing()
			follow.fetched = true
			follow.available = true
			follow.pendingFinal = false
			follow.finalAttempts = 0
			follow.fail = prForgeFailure{}
			if !changed {
				continue
			}
			follow.seq = a.nextPRUpdateSeqLocked()
			frame.PrevLen = utf16Len(follow.text)
			frame.Base = frame.PrevLen
			frame.Available = true
		case result.err == nil:
			tail, truncated := tailCapLog(result.log.Text, ciLogDisplayTailBytes)
			changed := !follow.fetched || !follow.available || tail != follow.text || follow.fail.failing()
			prevLen, base, appended := ciLogDelta(follow.text, tail)
			follow.fetched = true
			follow.available = true
			follow.pendingFinal = false
			follow.finalAttempts = 0
			follow.fail = prForgeFailure{}
			follow.text = tail
			follow.etag = result.log.ETag
			follow.truncated = truncated
			follow.totalBytes = len(result.log.Text)
			if !changed {
				continue
			}
			follow.seq = a.nextPRUpdateSeqLocked()
			frame.PrevLen, frame.Base, frame.Append = prevLen, base, appended
			frame.Available = true
		case errors.Is(result.err, gitops.ErrCIJobLogNotFound):
			// The forge has not published the job's log yet: GitHub answers
			// 404 until the log blob exists, for a running job too. That is
			// a wait, not a failure: the follow keeps asking at the pace
			// prCIInterval sets (the follow cadence while the job is live,
			// the final tries and then the wait cadence once it completed),
			// a forced fetch included, and shows the job as waiting.
			shown := follow.fetched && !follow.available && !follow.fail.failing()
			follow.fetched = true
			follow.available = false
			follow.fail = prForgeFailure{}
			if !result.plan.live {
				follow.pendingFinal = true
				follow.finalAttempts++
			}
			if shown {
				continue
			}
			follow.seq = a.nextPRUpdateSeqLocked()
			frame.PrevLen = utf16Len(follow.text)
			frame.Base = frame.PrevLen
		default:
			fail := a.newPRForgeFailure(result.err)
			if follow.pendingFinal && !fail.rateLimited() {
				follow.finalAttempts++
				if follow.finalAttempts >= prCILogFinalAttempts {
					follow.pendingFinal = false
				} else if !forced[jobID] {
					// A failure right after completion may pass; the follow
					// cadence tries again quietly.
					continue
				}
			}
			// A failed fetch is an answer: a terminal job is not asked again
			// until someone re-sends the follow (the Refresh button), or a
			// rate limit's release.
			follow.fetched = true
			if follow.fail.key == fail.key {
				continue
			}
			follow.fail = fail
			follow.seq = a.nextPRUpdateSeqLocked()
			log.Printf("pr updates: ci log fetch failed for pr=%s job=%s (id: %s): %v", pump.prKey, jobID, fail.correlationID, result.err)
			frame.PrevLen = utf16Len(follow.text)
			frame.Base = frame.PrevLen
			frame.Available = follow.available
			frame.Error = fail.wire
			frame.ErrorKind = fail.kind
			frame.Reserve = fail.reserve
			frame.ResumeAt = fail.resumeAt
		}
		frame.Seq = follow.seq
		frame.Truncated = follow.truncated
		frame.TotalBytes = follow.totalBytes
		events = append(events, frame)
	}
	return events
}

// prCILogStateLocked is a follow as the wire reports it.
func prCILogStateLocked(follow *prCILogFollow) PRCILogState {
	return PRCILogState{
		Text:       follow.text,
		Truncated:  follow.truncated,
		TotalBytes: follow.totalBytes,
		Available:  follow.available,
		Error:      follow.fail.wire,
		ErrorKind:  follow.fail.kind,
		Reserve:    follow.fail.reserve,
		ResumeAt:   follow.fail.resumeAt,
		Seq:        follow.seq,
	}
}

// releaseFollowsLocked drops every follow a handle holds; a job nobody
// follows any more loses its text.
func releaseFollowsLocked(pump *prUpdatePump, handle *prUpdateHandle) {
	for jobID := range handle.follows {
		dropFollowLocked(pump, jobID)
	}
	handle.follows = nil
}

func dropFollowLocked(pump *prUpdatePump, jobID string) {
	follow := pump.follows[jobID]
	if follow == nil {
		return
	}
	follow.refs--
	if follow.refs <= 0 {
		delete(pump.follows, jobID)
	}
}

// setPRCILogFollows replaces one handle's follow set and has the pump
// fetch the requested jobs now, so the reply is the current text.
func (a *App) setPRCILogFollows(ctx context.Context, subscriptionID string, jobIDs []string) (PRCILogFollowResult, error) {
	if len(jobIDs) > maxPRCILogFollowsPerHandle {
		return PRCILogFollowResult{}, ErrTooManyPRCILogFollows
	}
	a.prUpdates.mu.Lock()
	handle, ok := a.prUpdates.handles[subscriptionID]
	if !ok || handle.pump.dead {
		a.prUpdates.mu.Unlock()
		return PRCILogFollowResult{}, errPRUpdatesUnknownSubscription
	}
	pump := handle.pump
	want := make(map[string]bool, len(jobIDs))
	for _, jobID := range jobIDs {
		want[jobID] = true
	}
	for jobID := range handle.follows {
		if !want[jobID] {
			dropFollowLocked(pump, jobID)
			delete(handle.follows, jobID)
		}
	}
	if handle.follows == nil {
		handle.follows = make(map[string]bool)
	}
	if pump.follows == nil {
		pump.follows = make(map[string]*prCILogFollow)
	}
	for jobID := range want {
		if handle.follows[jobID] {
			continue
		}
		handle.follows[jobID] = true
		follow := pump.follows[jobID]
		if follow == nil {
			follow = &prCILogFollow{}
			pump.follows[jobID] = follow
			// A followed job's steps ride the pipeline, which the forge
			// fills only for followed jobs: poll it with this one in.
			pump.ciDirty = true
		}
		follow.refs++
	}
	a.prUpdates.mu.Unlock()

	result := PRCILogFollowResult{Logs: make(map[string]PRCILogState, len(jobIDs))}
	if len(jobIDs) == 0 {
		return result, nil
	}
	if err := a.requestPRPump(ctx, pump, &prPumpRequest{jobIDs: jobIDs}); err != nil {
		return PRCILogFollowResult{}, err
	}
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	for _, jobID := range jobIDs {
		if follow := pump.follows[jobID]; follow != nil {
			result.Logs[jobID] = prCILogStateLocked(follow)
		}
	}
	return result, nil
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
func (a *App) servePRPumpRequest(pump *prUpdatePump, req *prPumpRequest, runCI func(ctx context.Context, forced map[string]bool, pollPipeline bool)) {
	forced := make(map[string]bool, len(req.jobIDs))
	for _, jobID := range req.jobIDs {
		forced[jobID] = true
	}
	runCI(forgeapi.WithInteractive(pump.ctx), forced, req.ci)
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

// ciLogDelta describes new against old as the frontend applies it: keep
// the first base UTF-16 units of the prevLen it holds, append the rest.
// The split lands on a rune boundary so neither side holds half a
// character.
func ciLogDelta(old, new string) (prevLen, base int, appended string) {
	n := min(len(old), len(new))
	i := 0
	for i < n && old[i] == new[i] {
		i++
	}
	for i > 0 && i < len(new) && !utf8.RuneStart(new[i]) {
		i--
	}
	return utf16Len(old), utf16Len(new[:i]), new[i:]
}

// utf16Len is the length JavaScript reports for s once it has crossed the
// wire as JSON: one unit per BMP rune, two per astral rune, one per
// invalid byte (encoding/json writes U+FFFD for each).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}
