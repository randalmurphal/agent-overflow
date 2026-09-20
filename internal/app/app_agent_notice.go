package app

import (
	"context"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// A notice is a message the app writes into a thread on something else's
// behalf: a remote job's completion, a thread request's answer. Both arrive
// the same way, and the way is the part that must not drift between them:
// the thread's action lock, the ordinary durable queue with the caller's own
// persist hook so the queue row and the caller's record commit together, and
// a lazy session start for a thread with nobody to read it.
//
// What each notice decides for itself stays its own: who may receive it,
// what it says, and what to do when the session will not start.

// agentNotice is one such message.
type agentNotice struct {
	threadID string
	// sendID is the notice's identity for send admission, so a repeated
	// observation of the same event cannot queue it twice.
	sendID string
	// origin and originThread attribute the message in the timeline. Empty
	// for a notice the app writes in its own voice.
	origin       string
	originThread *usermessage.OriginThread
	// prepare runs under the thread lock and returns the message body. A
	// false result abandons the notice without an error: the reason for it
	// went away while the lock was being taken.
	prepare func() (string, bool, error)
	// persist writes the caller's durable record in the queue insert's own
	// transaction.
	persist func(store.FlushQueueItem) error
	// startFailed renders what the thread is told when its session could
	// not be started. The message is already queued by then.
	startFailed func(error) string
}

// queueAgentNotice queues one notice and makes sure something is running to
// read it.
//
// ctx bounds the wait for the thread lock alone. The lock is held across the
// queue write and the lazy start, which is the same action then mutation
// order an ordinary send takes, so a notice cannot interleave with an
// archive, a transfer or a stop.
func (a *App) queueAgentNotice(ctx context.Context, notice agentNotice) error {
	unlock, err := a.threadLocks().LockCtx(ctx, notice.threadID)
	if err != nil {
		return err
	}
	defer unlock()
	body, proceed, err := notice.prepare()
	if err != nil || !proceed {
		return err
	}
	if _, err := a.registerQueueItem(notice.threadID, body,
		SendMessageOptions{SendID: notice.sendID},
		injectedQueueOptions{
			preserveDraft: true,
			origin:        notice.origin,
			originThread:  notice.originThread,
			persist:       notice.persist,
		}); err != nil {
		return err
	}
	if _, live := a.sessionManager().get(notice.threadID); live {
		return nil
	}
	// The message is already in the ordinary durable queue; startup's flush
	// trigger dispatches it. A failure here leaves it queued for the next
	// start rather than losing it.
	if err := a.startSession(a.lifeCtx(), notice.threadID); err != nil {
		a.emitWireErrorToThread(notice.threadID, notice.startFailed(err))
	}
	return nil
}
