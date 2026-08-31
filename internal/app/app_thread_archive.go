package app

import (
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
// (threadapp.DeleteTree stops the session before dropping rows).

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
func (a *App) ArchiveThread(id string) error {
	// Stamped BEFORE the lock, because waiting for the lock is exactly
	// the window in which this archive can go stale: a send already
	// holding it can dispatch a whole turn before this call gets its
	// turn. stopArchivedThreadSession re-checks against this instant.
	requestedAt := time.Now().UnixMilli()
	unlock := a.threadLocks().Lock(id)
	defer unlock()

	row, changed, err := a.threadApplication().Archive(id)
	if err != nil {
		return err
	}
	// Broadcast as `unlisted` so a second attached client drops the row
	// from its sidebar too; re-archiving an already-archived thread
	// changes nothing and says nothing.
	a.broadcastThreadRowIfChanged(triage.ThreadActionUnlisted, row, changed)
	a.stopArchivedThreadSession(id, requestedAt)
	return nil
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
func (a *App) UnarchiveThread(id string) (store.Thread, error) {
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	row, changed, err := a.threadApplication().Unarchive(id)
	if err != nil {
		return store.Thread{}, err
	}
	// Broadcast as `listed` so every other attached client puts the row
	// back in its sidebar.
	a.broadcastThreadRowIfChanged(triage.ThreadActionListed, row, changed)
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
func (a *App) stopArchivedThreadSession(threadID string, requestedAtMillis int64) {
	if _, live := a.sessionManager().get(threadID); !live {
		return
	}
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

	sess, ok := a.sessionManager().take(threadID)
	if !ok {
		return
	}
	if err := a.teardownAndCloseSession(threadID, sess); err != nil {
		log.Printf("app: archive thread %s: close provider session: %v", threadID, err)
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
