package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// Archiving a thread is the one row flip that also releases the
// thread's host resources.
//
// The provider process a thread owns is not free: each holds hundreds
// of MB, and it is the parent of everything the model started through
// it — dev servers, file watchers, Monitor tasks. Closing the session
// runs provider.Process.Close, whose cascade is stdin close, then
// SIGTERM to the process GROUP, then SIGKILL to the group, so the whole
// tree goes with it. That is the teardown every other stop path already
// uses; nothing here opens a second one.
//
// On a desktop, app shutdown eventually reaps whatever an archive left
// behind. A backend nobody closes has no such backstop: the archived
// thread's process and its children keep running for the life of the
// host. Archive is therefore where those resources are released.
//
// This does NOT extend the idle reaper. The reaper's rule — never close
// a quiet session that still has running background work — stands
// unchanged (app_session_reaper.go). Archive is different in kind: it
// is a person stating they are done with the thread, and that intent
// outranks the work, exactly as deleting the thread already does
// (threadapp.Destroy stops the session before dropping rows).

// ArchiveThread flips archived to true so the thread leaves the active
// sidebar, and closes the thread's provider session so the host stops
// carrying it. The workers it owns are archived after it, each the same
// way (finishArchive); unarchiving it unarchives none of them.
//
// The row flip commits first and the rest follows. Once the flip has
// committed the call succeeds: the archive itself has succeeded durably,
// and failing the call would tell the caller otherwise. What it could not
// finish afterwards, a worker whose archive was refused or work that may
// still be running or open, is reported in the result.
//
//ao:scope threads:operate
//ao:route thread
func (a *App) ArchiveThread(id string) (ArchiveThreadResult, error) {
	work, err := a.archiveThreadLocked(id, "")
	if err != nil {
		return ArchiveThreadResult{}, err
	}
	return a.finishArchive(work, reachNow).result(a), nil
}

// ArchiveThreadResult is what an archive left undone after the thread's own
// archive committed. The zero value is a complete archive.
type ArchiveThreadResult struct {
	// WorkersNotArchived names each worker under the thread, at any depth,
	// whose archive was refused. It stays open; archiving it again retries
	// it, and so does archiving again the thread that owns it. A worker on
	// another computer named here either has not taken the archive yet
	// (Pending), which is sent again until it does, or runs a version that
	// cannot take it.
	WorkersNotArchived []WorkerFailure `json:"workersNotArchived,omitempty"`
	// CleanupError says, for a person, what of the archived threads may
	// still be running or open (threadCleanupError). Their archives stand.
	CleanupError string `json:"cleanupError,omitempty"`
}

// archiveOutcome is what finishArchive could not do, for each door to
// report in its own terms.
type archiveOutcome struct {
	cleanup threadCleanupError
	workers []workerFailure
}

// add gathers what the archive of a thread under this one could not do.
func (o *archiveOutcome) add(child archiveOutcome) {
	o.cleanup.addBelow(child.cleanup)
	o.workers = append(o.workers, child.workers...)
}

// result renders the outcome for the binding. The host log keeps the
// cleanup's causes.
func (o archiveOutcome) result(a *App) ArchiveThreadResult {
	var result ArchiveThreadResult
	if cause := o.cleanup.Unwrap(); cause != nil {
		log.Printf("app: archive thread: %v", cause)
		result.CleanupError = o.cleanup.message()
	}
	result.WorkersNotArchived = a.workerFailures(o.workers)
	return result
}

// finishArchive stops the work an archive captured, then archives the
// workers the thread owned when it committed
// (docs/specs/standing-agents.md, "Owned workers"), here (walkArchive) and
// on paired computers: the archive's end owes each computer a thread it
// owns spawned on the archive of that spawn's worker there, which reach
// says when to deliver (deliverOwnerOps). A worker it could not archive is
// reported, and the others are still archived.
func (a *App) finishArchive(work restingThreadWork, reach ownerOpReach) archiveOutcome {
	outcome, owed := a.walkArchive(work)
	outcome.workers = append(outcome.workers, a.deliverOwnerOps(owed, nil, reach)...)
	return outcome
}

// walkArchive archives the workers here, each through the binding's own
// path: its locks, its session stop and its disarmed requests, and in turn
// the workers it owns, down the lineage. Each worker's flip is fenced by
// its owner's archive (ArchiveOwnedWorker), so a cleanup that runs late
// leaves alone a worker moved since, and every worker of an owner reopened
// since. Then the archive ends (endArchiveWalk), returning what it owes the
// paired computers. A worker's own walk owes what the owner's end owes
// again, under a newer sequence.
func (a *App) walkArchive(work restingThreadWork) (archiveOutcome, []store.OwnerOp) {
	outcome := archiveOutcome{cleanup: a.stopRestingThreadWork(work)}
	outcome.cleanup.outcome = "archived"
	for _, worker := range work.workers {
		child, err := a.archiveThreadLocked(worker.ThreadID, worker.OwnerID)
		switch {
		case errors.Is(err, sql.ErrNoRows), errors.Is(err, store.ErrThreadGone):
			// Deleted since: there is nothing left to archive.
		case err != nil:
			outcome.workers = append(outcome.workers, workerFailure{threadID: worker.ThreadID, err: err})
		case child.captured:
			childOutcome, _ := a.walkArchive(child)
			outcome.add(childOutcome)
		}
	}
	// A prompt of a worker the archive left running goes back to the user,
	// delivered or not; the archived workers' prompts ended with them.
	a.reroutePromptsFrom(work.threadID)
	owed, err := a.endArchiveWalk(work.threadID)
	outcome.cleanup.remoteWorkers = err
	return outcome, owed
}

// archiveThreadLocked archives id under its action lock. ownerID names the
// owner whose archive this one carries out and fences the flip by it
// (commitArchive); an archive the fence excludes changes nothing and stops
// nothing, because the thread is no longer that archive's to stop.
func (a *App) archiveThreadLocked(id, ownerID string) (restingThreadWork, error) {
	return a.runArchive(id, archiveRequest{ownerID: ownerID})
}

// archiveRequest is which archive runArchive carries out: a person's or an
// agent's (the zero value), a worker's for its owner's archive (ownerID),
// or a worker's for its owner on a paired computer (link).
type archiveRequest struct {
	ownerID string
	link    *linkedArchive
}

// linkedArchive is the archive a paired computer's owner sent through the
// spawn link token from device, as its operation seq.
type linkedArchive struct {
	token, device string
	seq           int64
}

// errOwnerOpTaken is a linked archive whose operation the link already
// took: it was admitted before, or a newer one superseded it.
var errOwnerOpTaken = errors.New("that owner operation was already taken")

// runArchive is the one archive: commit, capture, session stop and the
// read of the workers its walk goes on to archive. A worker's archive for
// its owner here that captured nothing stops nothing and walks nothing:
// the thread is no longer that archive's to stop.
func (a *App) runArchive(id string, req archiveRequest) (restingThreadWork, error) {
	// Stamped BEFORE the lock, because waiting for the lock is exactly
	// the window in which this archive can go stale: a send already
	// holding it can dispatch a whole turn before this call gets its
	// turn. stopArchivedThreadSession re-checks against this instant.
	requestedAt := time.Now().UnixMilli()
	// The thread starts no work from before its capture until its session
	// is stopped, so the capture holds all it started (threadRestFence).
	reopen := a.resting.close(id)
	defer reopen()
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	work, changed, err := a.commitArchive(id, req)
	if err != nil {
		return restingThreadWork{}, err
	}
	// Broadcast as `unlisted` so a second attached client drops the row
	// from its sidebar too; re-archiving an already-archived thread
	// changes nothing and says nothing.
	a.announceThreadIfChanged(triage.ThreadActionUnlisted, id, changed)
	if !work.captured && req.ownerID != "" {
		return work, nil
	}
	a.beginArchiveWalk(id)
	a.stopArchivedThreadSession(&work, requestedAt)
	a.readArchivedWorkers(&work)
	return work, nil
}

// readArchivedWorkers names the workers an archive goes on to archive: the
// unarchived ones the thread owns, at any depth through workers already
// archived, whose own archives are not replayed. Its caller holds the
// thread's action lock, which unarchive takes, after the archive committed
// and the session stopped, so a thread the owner spawns once reopened is
// never among them.
func (a *App) readArchivedWorkers(work *restingThreadWork) {
	workers, err := a.store.ListOwnedWorkersToArchive(work.threadID)
	if err != nil {
		work.cleanup.workers = fmt.Errorf("read the workers of %s: %w", work.threadID, err)
		return
	}
	work.workers = workers
}

// commitArchive flips the row and, when that changed it, captures the work
// the archive stops, in one critical section under the mutation lock. A
// queue write that holds app messages from an archived thread
// (registerQueueItem) takes the same lock, so it sees the flip either way
// round, never between its check and its write. The lock is released
// before the session stops: the teardown's queue drop takes it, and it is
// not reentrant. A worker's archive, ownerID set, flips the row only while
// its owner is archived and still owns it (store.ArchiveOwnedWorker); a
// linked one only while the link owns it, and once per operation
// (store.ArchiveLinkedWorker), walking again a worker already archived.
func (a *App) commitArchive(id string, req archiveRequest) (restingThreadWork, bool, error) {
	unlock, err := a.threadApplication().LockMutable(a.lifeCtx(), id)
	if err != nil {
		return restingThreadWork{}, false, err
	}
	defer unlock()
	var changed bool
	switch {
	case req.link != nil:
		var applied bool
		_, changed, applied, err = a.threadApplication().ArchiveLinkedWorker(id, req.link.token, req.link.device, req.link.seq)
		if err == nil && !applied {
			return restingThreadWork{}, false, errOwnerOpTaken
		}
	case req.ownerID == "":
		_, changed, err = a.threadApplication().Archive(id)
	default:
		_, changed, err = a.threadApplication().ArchiveWorker(id, req.ownerID)
	}
	if err != nil || !changed {
		return restingThreadWork{threadID: id}, changed, err
	}
	return a.captureRestingThreadWork(id, archiveRest), true, nil
}

// UnarchiveThread flips archived back to false so the thread reappears in the
// active sidebar. Returns the refreshed row so the caller can re-render
// without a follow-up GetThread round-trip.
//
// It deliberately starts NO session. The thread comes back exactly as
// cold as any other thread whose session was closed — by the reaper, by
// StopSession, by an app restart — and the next send lazy-starts a
// process through the ordinary runSessionStart path. Restarting here
// would spawn a provider process for a thread the reader may only want
// to look at.
//
// Takes the per-thread action lock so the archive/unarchive pair is
// serialized against each other and against every send/start entry
// point. An unarchive can no longer land between an archive's row flip
// and its session close, which is the one ordering that would leave a
// visibly-active thread with a session dying underneath it.
//
//ao:scope threads:operate
//ao:route thread
func (a *App) UnarchiveThread(id string) (store.Thread, error) {
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	return a.unarchiveThreadLocked(id)
}

// unarchiveThreadLocked is UnarchiveThread for a caller that holds the
// thread's action lock.
func (a *App) unarchiveThreadLocked(id string) (store.Thread, error) {
	if err := a.threadApplication().CheckMutable(id); err != nil {
		return store.Thread{}, err
	}
	row, changed, err := a.threadApplication().Unarchive(id)
	if err != nil {
		return store.Thread{}, err
	}
	// Broadcast as `listed` so every other attached client puts the row
	// back in its sidebar, unless it is a never-sent thread the list does
	// not count yet.
	a.announceThreadIfChanged(a.listedAction(row.ID), row.ID, changed)
	return row, nil
}

// stopArchivedThreadSession closes the just-archived thread's provider
// session unless the thread was re-engaged after the archive was
// requested.
//
// The re-check is the point, not a formality. requestedAt is stamped
// before ArchiveThread queues on the per-thread action lock, and a send
// already holding that lock can dispatch a turn while this call waits.
// Killing that turn would be a stale archive stopping work the reader
// asked for AFTER they archived.
//
// The discriminator is when the newest turn began, from its durable rows
// (threadTurnStartedAfter), not the session's activity stamp. A session that was mid-turn when the reader
// archived keeps streaming events, so its activity stamp advances past
// requestedAt within milliseconds — reading that as "re-engaged" would
// make archive unable to stop the very sessions it most needs to. A
// turn's start time does not move: one that began before the request is
// what archive is for, one that began after it belongs to a newer
// engagement.
//
// A thread with no live session costs one map lookup and no query,
// which is what archiving an idle thread does.
//
// The thread's browser pages close with the session, suspended ones and
// their saved copies included, and stay open exactly when the session is
// kept: a turn that started after the request may be using them.
//
// What it could not close goes into work.cleanup. A re-engaged session is
// kept on purpose and is not a failure.
func (a *App) stopArchivedThreadSession(work *restingThreadWork, requestedAtMillis int64) {
	threadID := work.threadID
	if _, live := a.sessionManager().get(threadID); live {
		reengaged, err := a.threadTurnStartedAfter(threadID, requestedAtMillis)
		if err != nil {
			// Fail in the same direction the idle reaper does on its own
			// probe error: an unreadable turn history is not evidence that
			// nothing started, so leave the session alone, and its pages
			// with it.
			work.cleanup.session = fmt.Errorf("read turn history: %w", err)
			return
		}
		if reengaged {
			log.Printf("app: archive thread %s: session kept, a turn started after the archive was requested", threadID)
			return
		}
		if sess, ok := a.sessionManager().take(threadID); ok {
			if err := a.teardownAndCloseSession(threadID, sess); errors.Is(err, errBackgroundNotRetired) {
				work.cleanup.background = err
			} else if err != nil {
				work.cleanup.session = fmt.Errorf("close provider session: %w", err)
			}
		}
	}
	if err := a.closeThreadBrowserPages(threadID); err != nil {
		work.cleanup.browser = fmt.Errorf("close browser pages: %w", err)
	}
}

// threadTurnStartedAfter reports whether the thread's newest turn began
// after the given instant: was asked for then, by the first message written
// into it, or, with none (a turn the agent started on its own), reported
// by the provider then. The provider reports a turn some time after its
// message was written (Claude's init, Codex's turn/started), so a turn the
// reader asked for before archiving can be reported after; it is not a
// newer engagement. Two indexed reads; turns are ordered by turn_index, so
// the newest turn is also the newest start.
func (a *App) threadTurnStartedAfter(threadID string, sinceMillis int64) (bool, error) {
	turns, err := a.store.ListRecentTurns(threadID, 1)
	if err != nil {
		return false, err
	}
	if len(turns) == 0 || turns[0].StartedAt <= sinceMillis {
		return false, nil
	}
	asked, found, err := a.store.FirstUserRowOfTurn(threadID, turns[0].TurnIndex)
	if err != nil {
		return false, err
	}
	return !found || asked > sinceMillis, nil
}
