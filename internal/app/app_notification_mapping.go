package app

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/serialqueue"
	"agent-overflow/internal/triage"
)

// The event→notification tap. `internal/notify` owns WHAT a moment says and
// when it is taken back; this file owns WHERE each moment is observed and
// how the sentence is finished.
//
// THE TAP IS ON emit, not on the six emitters. Turn completion is announced
// from the triage router, approvals from two places in it plus the App's own
// resolve path, provider status from a discovery service, sign-in from an
// account manager. Subscribing to the one funnel every Go→frontend event
// already crosses closes the class: a seventh emitter of `provider:approval`
// is mapped the day it is written, and nobody has to remember to call a
// notification helper. It also costs one string switch on a path that
// already runs `rememberRateLimitsEvent`'s.
//
// DISPATCH IS OFF THE EMITTING GOROUTINE. `emit` runs on the triage router's
// own goroutine, and finishing a notification needs a SQLite read and a
// platform call that can be a blocking D-Bus round trip. A serial queue —
// not a bare `go` — because ORDER is the retraction contract: a retract that
// overtook its own send would leave the notification it was meant to
// withdraw on screen forever.
//
// WHAT IS PROJECTED SYNCHRONOUSLY, AND WHY. The tap type-asserts and reads
// the two or three fields the mapping needs before enqueuing, so the queue
// never retains a whole wire payload (a TurnCompletedEvent carries its
// turn's raw token-usage JSON). Only the thread TITLE is resolved on the
// queue, because only it needs the database.

// notificationDispatch is the App's notification coordination: the ordered
// queue mapped moments run on, the provider sign-in edges the mapping needs
// supplied, and the log-once ledger.
//
// signedOut and loggedCodes are touched ONLY from queue jobs, which the
// queue runs one at a time with a happens-before edge between them. That is
// why neither needs a mutex — and why nothing outside a job may read them.
type notificationDispatch struct {
	queue serialqueue.Queue
	// harnessSeq numbers the harness RPC's throwaway notification ids. It is
	// the one id in the system that names no moment, so it is the one that
	// still needs a counter.
	harnessSeq atomic.Uint64

	signedOut   map[string]bool
	loggedCodes map[NotificationErrorCode]struct{}
}

// notificationDrainTimeout bounds shutdown's wait for the queue. A mapped
// moment is worth finishing — a turn that completed during teardown is
// exactly the one a user walked away from — but not worth hanging a quit on
// a notification daemon that stopped answering.
const notificationDrainTimeout = 2 * time.Second

// tapNotification projects a wire event onto a notification-worthy moment
// and queues it. It runs on the emitting goroutine, so it does no I/O.
//
// The channel switch comes first and returns for everything else, which is
// almost every call: the hot channels (`provider:item_event`, the streaming
// deltas) fail it on length alone.
func (a *App) tapNotification(name eventchan.Channel, data any) {
	switch name {
	case eventchan.ProviderTurnCompleted:
		event, ok := data.(triage.TurnCompletedEvent)
		if !ok {
			return
		}
		// CountsAsActivity is already "this round was the top-level agent's,
		// not a subagent's" — the same predicate the sidebar bumps on. An
		// error message is what distinguishes a turn that failed from one
		// that finished; the two are one moment with two outcomes, so the
		// mapping picks the kind rather than this tap sending both.
		rest := notify.TurnRest{
			Thread:   notify.ThreadRef{ID: event.ThreadID},
			TopLevel: event.CountsAsActivity,
			Failed:   event.ErrorMessage != "",
			Aborted:  event.Aborted,
		}
		a.queueNotification(func() (notify.Notification, bool) {
			rest.Thread.Title = a.notificationThreadTitle(rest.Thread.ID)
			return notify.MapTurnRest(rest)
		})

	case eventchan.ProviderTurnStarted:
		event, ok := data.(triage.TurnStartedEvent)
		if !ok {
			return
		}
		// The thread going back to work is the "handled elsewhere" that
		// retracts its rest notification. No title read: a retraction says
		// nothing.
		resumed := notify.ThreadResumed{ThreadID: event.ThreadID}
		a.queueNotification(func() (notify.Notification, bool) {
			return notify.MapThreadResumed(resumed)
		})

	case eventchan.ProviderSessionDied:
		event, ok := data.(triage.SessionDiedEvent)
		if !ok {
			return
		}
		// Reason, exit code and the captured stderr tail are all deliberately
		// left behind: they are the provider's own prose about its crash,
		// which is what the redaction line keeps off a lock screen.
		exit := notify.ProviderExit{Thread: notify.ThreadRef{ID: event.ThreadID}}
		a.queueNotification(func() (notify.Notification, bool) {
			exit.Thread.Title = a.notificationThreadTitle(exit.Thread.ID)
			return notify.MapProviderExit(exit)
		})

	case eventchan.ProviderApproval:
		event, ok := data.(provider.ApprovalEvent)
		if !ok {
			return
		}
		moment := approvalMoment(event)
		if moment.RequestID == "" {
			return
		}
		a.queueNotification(func() (notify.Notification, bool) {
			if !moment.Answered {
				moment.Thread.Title = a.notificationThreadTitle(moment.Thread.ID)
			}
			return notify.MapApproval(moment)
		})

	case eventchan.ProviderStatus:
		event, ok := data.(providerstatus.Event)
		if !ok || event.Status != "unauthenticated" {
			return
		}
		// `provider:status` is the credential-checked answer, not a guess:
		// provideraccountapp.emitUnauthenticatedIfNoLogin refuses to raise it
		// while bytes that are not the provider's sign-out husk exist. It is
		// also a LEVEL — re-emitted every time anything asks for provider
		// statuses — which is why the edge, not the event, is what maps.
		name := event.Provider
		a.queueNotification(func() (notify.Notification, bool) {
			return notify.MapProviderAuth(a.observeProviderAuth(name, true))
		})

	case eventchan.ProviderLogin:
		event, ok := data.(provideraccountapp.LoginState)
		if !ok || event.Phase != provideraccountapp.LoginPhaseSucceeded {
			return
		}
		name := event.Provider
		a.queueNotification(func() (notify.Notification, bool) {
			return notify.MapProviderAuth(a.observeProviderAuth(name, false))
		})
	}
}

// approvalMoment projects an ApprovalEvent onto the mapping's input.
//
// A request carries its whole ApprovalRequest and a resolve carries only the
// id, which is why the thread id is read from two places. `fail` counts as
// answered alongside `resolve`: the prompt is gone either way, and a
// notification pointing at a prompt that no longer exists is the stale alert
// retraction exists to prevent.
func approvalMoment(event provider.ApprovalEvent) notify.ApprovalMoment {
	moment := notify.ApprovalMoment{
		Thread:    notify.ThreadRef{ID: event.ThreadID},
		RequestID: event.RequestID,
		Answered:  event.Action != "request",
	}
	if event.Request != nil {
		if moment.RequestID == "" {
			moment.RequestID = event.Request.RequestID
		}
		if moment.Thread.ID == "" {
			moment.Thread.ID = event.Request.ThreadID
		}
		moment.ToolName = event.Request.ToolName
	}
	return moment
}

// observeProviderAuth records one edge of a provider's sign-in state and
// answers the transition the mapping needs. Queue-only, like the map it
// reads.
//
// An unknown provider starts SIGNED IN, so the first unauthenticated status
// after a boot is a real transition and does notify, while the first
// successful sign-in of a session — the ordinary case, where nothing was
// wrong — retracts nothing.
func (a *App) observeProviderAuth(providerName string, signedOut bool) notify.ProviderAuthChange {
	if a.notifications.signedOut == nil {
		a.notifications.signedOut = make(map[string]bool, 2)
	}
	change := notify.ProviderAuthChange{
		Provider:     providerName,
		WasSignedOut: a.notifications.signedOut[providerName],
		IsSignedOut:  signedOut,
	}
	a.notifications.signedOut[providerName] = signedOut
	return change
}

// queueNotification runs one mapping on the notification queue and sends
// whatever it produces.
func (a *App) queueNotification(build func() (notify.Notification, bool)) {
	a.notifications.queue.Go(func() {
		notification, ok := build()
		if !ok {
			return
		}
		send := notification.Send
		// Only a presentation carries a route to attribute; a retraction
		// carries an id and a kind and nothing else, by contract.
		if !send.Retract {
			send.Target.BackendID = a.notificationBackendID()
		}
		a.logNotificationFailure(a.notifyOS(send))
	})
}

// notificationThreadTitle reads the one thing a notification may say about a
// thread. A missing row or an unreadable one answers "", which the mapping
// renders as its untitled fallback: a notification with a generic heading is
// better than no notification about a turn the user is waiting on.
func (a *App) notificationThreadTitle(threadID string) string {
	if a.store == nil || threadID == "" {
		return ""
	}
	title, err := a.store.GetThreadTitle(threadID)
	if err != nil {
		log.Printf("notifications: read title for thread %s: %v", threadID, err)
		return ""
	}
	return title
}

// notificationBackendID is this backend's UUID, the attribution half of §9's
// deep-link scheme. Empty before the store identity is loaded, which is
// exactly what an optional field is for.
func (a *App) notificationBackendID() string {
	backendID, _ := a.backendIdentity()
	return backendID
}

// logNotificationFailure reports the first failure of each kind and stays
// quiet afterwards.
//
// A suppressed send is not a failure at all — it is the user's preference
// being honoured — and logging it would put a line in the log every time a
// turn completed on a machine where turn notifications are off. Everything
// else logs ONCE per code: a notification daemon that is not running, or an
// authorization the user denied, is a standing condition, and one line says
// it as well as a thousand.
func (a *App) logNotificationFailure(err error) {
	if err == nil {
		return
	}
	code := NotificationDeliveryFailed
	var notificationErr *NotificationError
	if errors.As(err, &notificationErr) {
		code = notificationErr.Code
	}
	if code == NotificationSuppressed {
		return
	}
	if a.notifications.loggedCodes == nil {
		a.notifications.loggedCodes = make(map[NotificationErrorCode]struct{}, 4)
	}
	if _, logged := a.notifications.loggedCodes[code]; logged {
		return
	}
	a.notifications.loggedCodes[code] = struct{}{}
	log.Printf("notifications: %v (further %s failures are not logged)", err, code)
}

// drainNotifications lets queued moments finish before SQLite closes.
func (a *App) drainNotifications(ctx context.Context, timeout time.Duration) error {
	drainCtx, cancel := contextWithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		a.notifications.queue.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-drainCtx.Done():
		return drainCtx.Err()
	}
}
