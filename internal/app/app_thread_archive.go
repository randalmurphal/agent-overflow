package app

import (
	"errors"
	"fmt"
	"log"
	"slices"
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
// (threadapp.DeleteThread stops the session before dropping rows).

// ArchiveThread flips archived to true so the thread leaves the active
// sidebar, and closes the thread's provider session so the host stops
// carrying it.
//
// The row flip commits first and the close follows. A failed close is
// logged rather than returned: the archive itself has already succeeded
// durably, and failing the call would tell the caller otherwise. That
// is also the policy the desktop client encoded before this moved
// server-side — it called StopSession, swallowed any failure, and
// archived anyway. Moving it here is what makes the stop true for every
// client instead of only the one that remembers to ask.
//
//ao:scope threads:operate
//ao:route thread
func (a *App) ArchiveThread(id string) error {
	work, err := a.archiveThreadLocked(id)
	if err != nil {
		return err
	}
	return a.stopArchivedThreadWork(work)
}

// archivedThreadWork is what a thread was waiting on at the moment its
// archive committed: the asks it owns and the remote commands it started.
type archivedThreadWork struct {
	threadID string
	// archived is set when this archive changed the row; a repeated one
	// captures and stops nothing.
	archived bool
	asks     []store.ThreadRequest
	commands []store.RemoteWatch
	// queued are the send ids of the messages in the thread's queue.
	queued []string
	err    error
}

// held reports whether sendID was in the thread's queue when it was
// archived.
func (w archivedThreadWork) held(sendID string) bool {
	return slices.Contains(w.queued, sendID)
}

// captureArchivedThreadWork runs in the critical section that commits the
// archive, under the thread's action and mutation locks, which unarchive,
// remote command admission and every queue write also take. It ends the
// thread's parked calls, disarms its wakes, cancels its loops and watches
// and stops their runs, and names the work open and the messages queued at
// that moment. A later wait or thread_status re-arms the wakes, and
// delivering one unarchives the thread.
//
// Nothing keeps new work from starting on an archived thread afterwards: a
// wake unarchives it, a remote command is admitted to it whatever its
// archive state, and a loop or watch registered after an unarchive reuses
// its label's row. So only the archive that changes the row captures, the
// schedule is cancelled here rather than after the locks are released, and
// the stop acts on these identities alone, never on what the thread is
// running or holds queued by the time it gets there.
func (a *App) captureArchivedThreadWork(id string) archivedThreadWork {
	work := archivedThreadWork{threadID: id, archived: true}
	asks, askErr := a.disarmThreadRequests(id)
	commands, commandErr := a.disarmRemoteCommands(id)
	if askErr != nil {
		log.Printf("thread tools: archive %s: cancel requests: %v", id, askErr)
	}
	wakesErr := a.cancelAgentWakesForThread(id, "cancelled: the thread was archived")
	rows, queueErr := a.store.ListFlushQueueItems(id)
	if queueErr != nil {
		queueErr = fmt.Errorf("archive %s: read its queue: %w", id, queueErr)
	}
	for _, row := range rows {
		work.queued = append(work.queued, row.SendID)
	}
	work.asks, work.commands, work.err = asks, commands, errors.Join(commandErr, wakesErr, queueErr)
	return work
}

// stopArchivedThreadWork stops exactly the work its archive captured. Both
// doors that archive (this binding and the agents' thread_update patch)
// run it, so a thread archived from either one is left in the same state.
// It runs on the app's lifetime, not its caller's, because the archive is
// already durable, and outside the locks because reaching the other
// computer may take a while.
func (a *App) stopArchivedThreadWork(work archivedThreadWork) error {
	if !work.archived {
		return work.err
	}
	ctx, id := a.lifeCtx(), work.threadID
	// What the app had queued for the thread goes, live session or not, so
	// nothing of it reaches the agent when the thread is next started. Its
	// loops and watches were cancelled with the capture.
	withdrawErr := a.withdrawQueuedAgentMessages(id, work.held, "the thread was archived before the agent took it in")
	// A request's answer or reminder goes back to its request, disarmed
	// with it, so the retry cannot bring the thread back.
	wakesErr := a.withdrawHeldThreadWakes(id, work.held)
	a.cancelOwnedAsks(ctx, id, work.asks)
	remoteErr := a.cancelRemoteCommands(ctx, id, work.commands, true)
	// A finished job's completion is kept for the thread: the cancel keeps
	// its watch, and one already held in the queue goes back to its watch
	// after the cancel has run. It arrives once the thread is unarchived.
	return errors.Join(work.err, withdrawErr, wakesErr, remoteErr, a.withdrawHeldRemoteCompletions(id, work.held))
}

func (a *App) archiveThreadLocked(id string) (archivedThreadWork, error) {
	// Stamped BEFORE the lock, because waiting for the lock is exactly
	// the window in which this archive can go stale: a send already
	// holding it can dispatch a whole turn before this call gets its
	// turn. stopArchivedThreadSession re-checks against this instant.
	requestedAt := time.Now().UnixMilli()
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	work, row, changed, err := a.commitArchive(id)
	if err != nil {
		return archivedThreadWork{}, err
	}
	// Broadcast as `unlisted` so a second attached client drops the row
	// from its sidebar too; re-archiving an already-archived thread
	// changes nothing and says nothing.
	a.announceThreadIfChanged(triage.ThreadActionUnlisted, id, changed)
	if changed {
		a.broadcastSpawnerRow(row)
	}
	a.stopArchivedThreadSession(id, requestedAt)
	return work, nil
}

// commitArchive flips the row and, when that changed it, captures the work
// the archive stops, in one critical section under the mutation lock. A
// queue write that holds app messages from an archived thread
// (registerQueueItem) takes the same lock, so it sees the flip either way
// round, never between its check and its write. The lock is released
// before the session stops: the teardown's queue drop takes it, and it is
// not reentrant.
func (a *App) commitArchive(id string) (archivedThreadWork, store.Thread, bool, error) {
	unlock, err := a.threadApplication().LockMutable(a.lifeCtx(), id)
	if err != nil {
		return archivedThreadWork{}, store.Thread{}, false, err
	}
	defer unlock()
	row, changed, err := a.threadApplication().Archive(id)
	if err != nil || !changed {
		return archivedThreadWork{threadID: id}, row, changed, err
	}
	return a.captureArchivedThreadWork(id), row, true, nil
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
	if err := a.threadApplication().CheckMutable(id); err != nil {
		return store.Thread{}, err
	}
	row, changed, err := a.threadApplication().Unarchive(id)
	if err != nil {
		return store.Thread{}, err
	}
	// Broadcast as `listed` so every other attached client puts the row
	// back in its sidebar.
	a.announceThreadIfChanged(triage.ThreadActionListed, row.ID, changed)
	if changed {
		a.broadcastSpawnerRow(row)
	}
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
// The discriminator is the durable turn row's started_at, not the
// session's activity stamp. A session that was mid-turn when the reader
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
func (a *App) stopArchivedThreadSession(threadID string, requestedAtMillis int64) {
	if _, live := a.sessionManager().get(threadID); live {
		reengaged, err := a.threadTurnStartedAfter(threadID, requestedAtMillis)
		if err != nil {
			// Fail in the same direction the idle reaper does on its own
			// probe error: an unreadable turn history is not evidence that
			// nothing started, so leave the session alone. Shutdown and the
			// next archive still reach it.
			log.Printf("app: archive thread %s: read turn history: %v", threadID, err)
			return
		}
		if reengaged {
			log.Printf("app: archive thread %s: session kept, a turn started after the archive was requested", threadID)
			return
		}
		if sess, ok := a.sessionManager().take(threadID); ok {
			if err := a.teardownAndCloseSession(threadID, sess); err != nil {
				log.Printf("app: archive thread %s: close provider session: %v", threadID, err)
			}
		}
	}
	if err := a.closeThreadBrowserPages(threadID); err != nil {
		log.Printf("app: archive thread %s: close browser pages: %v", threadID, err)
	}
}

// threadTurnStartedAfter reports whether the thread's newest turn began
// after the given instant. One indexed row read; turns are ordered by
// turn_index, so the newest turn is also the newest start.
func (a *App) threadTurnStartedAfter(threadID string, sinceMillis int64) (bool, error) {
	turns, err := a.store.ListRecentTurns(threadID, 1)
	if err != nil {
		return false, err
	}
	for _, turn := range turns {
		if turn.StartedAt > sinceMillis {
			return true, nil
		}
	}
	return false, nil
}
