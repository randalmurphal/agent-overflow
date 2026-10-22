package app

import (
	"context"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// thread_attention: the calling thread's own attention flag
// (docs/specs/agent-thread-tools.md#thread_attention). The flag is a read
// target, so opening the thread lowers it the way it clears a completion,
// and the turn that raised it notifies at its end (notify.MapTurnRest).

// FlagAttention raises or lowers the caller's attention flag. Raising it
// moves the thread up the sidebar as new activity does. Lowering a flag a
// turn already said takes back the notification that said it.
func (t threadToolsApp) FlagAttention(ctx context.Context, caller threadtools.Caller, call threadtools.AttentionCall) (threadtools.AttentionAck, error) {
	if threadtools.Forwarded(ctx) {
		return threadtools.AttentionAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"thread_attention flags the calling thread on its own computer and is never forwarded.", nil)
	}
	thread, err := t.localThread(caller.ThreadID)
	if err != nil {
		return threadtools.AttentionAck{}, err
	}
	if call.Kind == threadtools.AttentionNone {
		said, err := t.app.store.ThreadAttentionSaid(thread.ID)
		if err != nil {
			return threadtools.AttentionAck{}, err
		}
		_, changed, err := t.app.store.ClearThreadAttention(thread.ID)
		if err != nil {
			return threadtools.AttentionAck{}, err
		}
		t.app.announceThreadIfChanged(triage.ThreadActionFull, thread.ID, changed)
		if said {
			t.app.queueNotification(func() (notify.Notification, bool) {
				return notify.MapAttentionLowered(notify.AttentionLowered{ThreadID: thread.ID})
			})
		}
		return threadtools.AttentionAck{}, nil
	}
	// A write that changes nothing found the flag already as asked.
	_, changed, err := t.app.store.SetThreadAttention(thread.ID, call.Kind, call.Reason, time.Now().UnixMilli())
	if err != nil {
		return threadtools.AttentionAck{}, err
	}
	t.app.announceThreadIfChanged(triage.ThreadActionFull, thread.ID, changed)
	return threadtools.AttentionAck{Kind: call.Kind, Reason: call.Reason}, nil
}
