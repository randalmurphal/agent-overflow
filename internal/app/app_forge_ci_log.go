package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"time"

	"agent-overflow/internal/cilog"
	gitops "agent-overflow/internal/git"
)

// The log half of the CI phase (app_forge_ci.go). A followed job's log is
// downloaded into the CI log cache (app_ci_logs.go), pinned there while
// anyone follows it, and reported as its index: line and byte counts, the
// job's rows and the version its lines belong to. Its text never rides a
// frame; the frontend reads the lines it shows through GetPRCILogLines.

// maxPRCILogFollowsPerHandle bounds the jobs one subscription follows. A
// follow pins its log in the cache past the cache bound and costs one
// forge request per tick, serially, so an unbounded list is the same
// exhaustion surface maxPRUpdateHandles closes. A pane shows one log;
// this is far above any real UI.
const maxPRCILogFollowsPerHandle = 16

// ErrTooManyPRCILogFollows is returned when one call asks a subscription
// to follow more jobs than maxPRCILogFollowsPerHandle. Typed like
// ErrTooManyPRUpdateSubscriptions: retrying the same call never fixes it.
var ErrTooManyPRCILogFollows = fmt.Errorf("pr updates: too many CI log follows (limit %d)", maxPRCILogFollowsPerHandle)

// PRCILogEvent is the "pr:ci_log" frame for one followed job: its whole
// state, emitted whenever any of it changed.
type PRCILogEvent struct {
	PRKey string `json:"prKey"`
	JobID string `json:"jobId"`
	PRCILogState
}

// prCILogFollow is one followed job's log state, refcounted across the
// handles following it. Guarded by App.prUpdates.mu; store and key are
// fixed at creation.
type prCILogFollow struct {
	refs  int
	store *cilog.Store
	key   cilog.Key
	// fetched is false until the first poll decided something about this
	// job (downloaded its log, found it unpublished, or failed).
	fetched bool
	// available: the forge served the log at the last fetch.
	available bool
	// downloading: a download was cut off after it made progress; the
	// next poll resumes it.
	downloading bool
	index       cilog.Index
	// fail is the active failure, as the pump's fail.
	fail prForgeFailure
	// wasLive is the job's liveness at the last poll; pendingFinal marks
	// the live-to-terminal transition until the final log is in hand.
	wasLive       bool
	pendingFinal  bool
	finalAttempts int
	seq           uint64
}

// prCILogWork is one followed job's work for a tick, decided under the
// lock: whether to ask the forge, and the job its rows are computed for.
type prCILogWork struct {
	jobID string
	store *cilog.Store
	key   cilog.Key
	live  bool
	fetch bool
	job   cilog.Job
}

// prCILogOutcome is what a tick did for one job, folded in under the lock.
type prCILogOutcome struct {
	work     prCILogWork
	fetched  bool
	result   cilog.FetchResult
	err      error
	index    cilog.Index
	indexErr error
}

// pollPRCILogs advances every followed job: decides under the lock what
// each needs, downloads and indexes outside it, and folds the results in.
// forced jobs (an RPC's) are fetched whatever their state; any other job
// held by a rate limit waits for its release. Every follow's index is
// read each tick, since its rows move with the pipeline's step states as
// well as with the log.
func (a *App) pollPRCILogs(ctx context.Context, pump *prUpdatePump, forced map[string]bool) []PRCILogEvent {
	now := time.Now()
	a.prUpdates.mu.Lock()
	if pump.dead || len(pump.follows) == 0 {
		a.prUpdates.mu.Unlock()
		return nil
	}
	work := make([]prCILogWork, 0, len(pump.follows))
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
		held := !forced[jobID] && now.Before(follow.fail.release)
		fetch := !held && (live || forced[jobID] || !follow.fetched || follow.pendingFinal || follow.downloading || follow.fail.rateLimited())
		work = append(work, prCILogWork{jobID: jobID, store: follow.store, key: follow.key, live: live, fetch: fetch, job: ciLogJob(job)})
	}
	a.prUpdates.mu.Unlock()

	outcomes := make([]prCILogOutcome, 0, len(work))
	for _, w := range work {
		outcomes = append(outcomes, a.advancePRCILog(ctx, pump.pr, w))
	}

	var events []PRCILogEvent
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	if pump.dead {
		return nil
	}
	for _, out := range outcomes {
		follow := pump.follows[out.work.jobID]
		if follow == nil {
			// Unfollowed while the fetch ran.
			continue
		}
		if event, ok := a.foldPRCILogLocked(pump, follow, out, forced[out.work.jobID]); ok {
			events = append(events, event)
		}
	}
	return events
}

// advancePRCILog downloads one job's log when the tick asks for it and
// reads its index. A log whose file went from under the cache (a user
// cleared it) was dropped by the read, so it is downloaded again now.
func (a *App) advancePRCILog(ctx context.Context, pr gitops.PRReference, w prCILogWork) prCILogOutcome {
	out := prCILogOutcome{work: w}
	fetch := func() {
		out.fetched = true
		out.result, out.err = w.store.Fetch(ctx, w.key, a.ciLogSource(pr, w.jobID))
	}
	if w.fetch {
		fetch()
	}
	out.index, out.indexErr = w.store.Index(w.key, w.job)
	if errors.Is(out.indexErr, cilog.ErrLogGone) && out.err == nil {
		fetch()
		out.index, out.indexErr = w.store.Index(w.key, w.job)
	}
	return out
}

// foldPRCILogLocked applies one job's outcome to its follow and returns
// the frame to emit when its state changed, or on the first decision.
func (a *App) foldPRCILogLocked(pump *prUpdatePump, follow *prCILogFollow, out prCILogOutcome, forced bool) (PRCILogEvent, bool) {
	before := prCILogStateLocked(follow)
	first := !follow.fetched
	var failure error
	if out.fetched {
		switch {
		case out.err == nil:
			follow.fetched = true
			follow.downloading = out.result.Partial
			if !out.result.Partial {
				follow.available = true
				follow.pendingFinal = false
				follow.finalAttempts = 0
			}
		case errors.Is(out.err, gitops.ErrCIJobLogNotFound):
			// The forge has not published the job's log yet: GitHub answers
			// 404 until the log blob exists, which in practice is once the
			// job completed, while the API can still report it running.
			// That is a wait, not a failure: the follow keeps asking at the
			// pace prCIInterval sets and shows the job as waiting, over the
			// log the cache holds from before, if any.
			follow.fetched = true
			follow.available = false
			follow.downloading = false
			if !out.work.live {
				follow.pendingFinal = true
				follow.finalAttempts++
			}
		default:
			failure = out.err
		}
	}
	switch {
	case out.indexErr == nil:
		follow.index = out.index
	case errors.Is(out.indexErr, cilog.ErrNoLog):
		follow.index = cilog.Index{Sections: cilog.SectionsWithoutLog(follow.key, out.work.job)}
	case failure == nil:
		failure = fmt.Errorf("read CI log: %w", out.indexErr)
	}
	switch {
	case failure != nil:
		fail := a.newPRForgeFailure(pump.pr, failure)
		if follow.pendingFinal && !fail.rateLimited() {
			follow.finalAttempts++
			if follow.finalAttempts >= prCILogFinalAttempts {
				follow.pendingFinal = false
			} else if !forced {
				// A failure right after completion may pass; the follow
				// cadence tries again quietly.
				return PRCILogEvent{}, false
			}
		}
		// A failed fetch is an answer: a terminal job is not asked again
		// until someone re-sends the follow (the Refresh button), or a
		// rate limit's release.
		follow.fetched = true
		follow.downloading = false
		if follow.fail.key != fail.key {
			follow.fail = fail
			log.Printf("pr updates: ci log failed for pr=%s job=%s (id: %s): %v", pump.prKey, out.work.jobID, fail.correlationID, failure)
		}
	case out.fetched:
		follow.fail = prForgeFailure{}
	}
	after := prCILogStateLocked(follow)
	if (!first || !follow.fetched) && reflect.DeepEqual(before, after) {
		return PRCILogEvent{}, false
	}
	follow.seq = a.nextPRUpdateSeqLocked()
	after.Seq = follow.seq
	return PRCILogEvent{PRKey: pump.prKey, JobID: out.work.jobID, PRCILogState: after}, true
}

// prCILogStateLocked is a follow as the wire reports it.
func prCILogStateLocked(follow *prCILogFollow) PRCILogState {
	sections := follow.index.Sections
	if sections == nil {
		sections = []cilog.Section{}
	}
	return PRCILogState{
		Version:     follow.index.Version,
		TotalLines:  follow.index.TotalLines,
		TotalBytes:  follow.index.TotalBytes,
		StableLines: follow.index.StableLines,
		Sections:    sections,
		Available:   follow.available,
		Downloading: follow.downloading,
		Error:       follow.fail.wire,
		ErrorKind:   follow.fail.kind,
		Reserve:     follow.fail.reserve,
		ResumeAt:    follow.fail.resumeAt,
		Seq:         follow.seq,
	}
}

// releaseFollowsLocked drops every follow a handle holds.
func releaseFollowsLocked(pump *prUpdatePump, handle *prUpdateHandle) {
	for jobID := range handle.follows {
		dropFollowLocked(pump, jobID)
	}
	handle.follows = nil
}

// dropFollowLocked releases one handle's ref on a follow; a job nobody
// follows any more is unpinned, so the cache bound applies to its log.
func dropFollowLocked(pump *prUpdatePump, jobID string) {
	follow := pump.follows[jobID]
	if follow == nil {
		return
	}
	follow.refs--
	if follow.refs <= 0 {
		delete(pump.follows, jobID)
		follow.store.Unpin(follow.key)
	}
}

// setPRCILogFollows replaces one handle's follow set and has the pump
// fetch the requested jobs now, so the reply is the current state.
func (a *App) setPRCILogFollows(ctx context.Context, subscriptionID string, jobIDs []string) (PRCILogFollowResult, error) {
	if len(jobIDs) > maxPRCILogFollowsPerHandle {
		return PRCILogFollowResult{}, ErrTooManyPRCILogFollows
	}
	var store *cilog.Store
	if len(jobIDs) > 0 {
		var err error
		if store, err = a.ciLogs(); err != nil {
			return PRCILogFollowResult{}, err
		}
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
			follow = &prCILogFollow{store: store, key: ciLogKey(pump.pr, jobID)}
			store.Pin(follow.key)
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
