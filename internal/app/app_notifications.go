package app

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/settings"
)

type NotificationErrorCode string

const (
	NotificationUnavailable          NotificationErrorCode = "unavailable"
	NotificationAuthorizationPending NotificationErrorCode = "authorization_pending"
	NotificationAuthorizationDenied  NotificationErrorCode = "authorization_denied"
	NotificationDeliveryFailed       NotificationErrorCode = "delivery_failed"
	// NotificationSuppressed is the user's own preference refusing the
	// send. It is a typed answer rather than a silent nil so a caller can
	// tell "you asked me not to" from "I tried and failed" — the first is
	// not a fault and must not read as one in a log.
	NotificationSuppressed NotificationErrorCode = "suppressed"
)

// NotificationError is the visible typed failure returned by notifyOS when
// the current runtime cannot present a notification. Callers can use
// errors.As and Code without parsing user-facing prose.
type NotificationError struct {
	Code  NotificationErrorCode
	Cause error
}

func (e *NotificationError) Error() string {
	if e == nil {
		return "OS notification failed"
	}
	switch e.Code {
	case NotificationAuthorizationPending:
		return "OS notification authorization is pending"
	case NotificationAuthorizationDenied:
		return "OS notification authorization was not granted"
	case NotificationDeliveryFailed:
		if e.Cause != nil {
			return fmt.Sprintf("send OS notification: %v", e.Cause)
		}
		return "send OS notification failed"
	case NotificationSuppressed:
		return "OS notifications for this kind are turned off"
	case NotificationUnavailable:
		if e.Cause != nil {
			return fmt.Sprintf("OS notifications are unavailable: %v", e.Cause)
		}
		return "OS notifications are unavailable in this application mode"
	default:
		if e.Cause != nil {
			return e.Cause.Error()
		}
		return "OS notification failed"
	}
}

func (e *NotificationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type osNotificationSender interface {
	send(notify.Send) error
}

// notifyOS is the one internal notification send pipe, for presentations and
// retractions alike. Callers do not know whether presentation is in-process
// or bridged to the Windows launcher.
//
// It is also the one PREFERENCE GATE, and that placement is the point: a
// send that reaches a presenter without passing through here does not exist,
// so no sender — the event mapping, the workflow attention notice, the WSL
// update notice, or the next one — can ship having forgotten to ask. The
// alternative, checking at each call site, is a class of bug rather than a
// bug: every new sender is a fresh chance to miss it.
//
// The preferences are read from the BACKEND MACHINE's own screen, because
// that is the screen this process interrupts (in-process on macOS and Linux,
// through the Windows launcher on WSL — one machine either way).
//
// A RETRACTION IS NEVER SUPPRESSED. The gate answers "may I interrupt you",
// and withdrawing something already on screen is the opposite of an
// interruption. Gating it would mean a user who turns a kind off between a
// send and its retraction keeps the notification forever — the toggle would
// strand the very alerts it was meant to stop.
func (a *App) notifyOS(send notify.Send) error {
	if err := notify.ValidateSend(send); err != nil {
		return err
	}
	if !send.Retract && !a.notificationKindEnabled(send.Kind) {
		return &NotificationError{Code: NotificationSuppressed}
	}
	if a.osNotifications == nil {
		return &NotificationError{Code: NotificationUnavailable}
	}
	if err := a.osNotifications.send(send); err != nil {
		var notificationErr *NotificationError
		if errors.As(err, &notificationErr) {
			return err
		}
		return &NotificationError{Code: NotificationDeliveryFailed, Cause: err}
	}
	return nil
}

// notificationKindEnabled answers the user's preference for one kind.
//
// A settings service that is not wired yet answers from DefaultSettings,
// which has every notification preference ON: an App that has not finished
// booting must not silently start swallowing the notices it does raise
// during boot (the WSL "update didn't apply" notice is exactly one).
func (a *App) notificationKindEnabled(kind notify.Kind) bool {
	current := settings.DefaultSettings
	if a.settings != nil {
		current = a.settings.BackendScreen().Get()
	}
	if !current.NotificationsEnabled {
		return false
	}
	switch kind {
	case notify.KindTurnComplete:
		return current.NotifyTurnComplete
	case notify.KindApprovalNeeded:
		return current.NotifyApprovalNeeded
	case notify.KindError:
		return current.NotifyError
	case notify.KindProviderSignedOut:
		return current.NotifyProviderSignedOut
	default:
		// The kinds that predate the mapping (workflow attention, the
		// update notice) have no toggle of their own by design; the master
		// switch above is what silences them. ValidateSend has already
		// refused anything that is not a declared kind.
		return true
	}
}

func (a *App) activateNotificationTarget(target notify.Target) error {
	if err := notify.ValidateTarget(target); err != nil {
		return err
	}
	a.emit(eventchan.NotificationActivated, target)
	return nil
}

// NotificationActivated is called by the native Windows launcher after a
// bridged notification click. It validates the launcher-provided target and
// emits the same frontend event as the in-process desktop callback.
//
//ao:scope host
//ao:route home
func (a *App) NotificationActivated(target notify.Target) error {
	return a.activateNotificationTarget(target)
}

type transportNotificationSender struct {
	app              *App
	noSubscriberOnce sync.Once
}

func newTransportNotificationSender(app *App) *transportNotificationSender {
	return &transportNotificationSender{app: app}
}

func (s *transportNotificationSender) send(payload notify.Send) error {
	bus := s.app.eventBus.Load()
	if bus == nil {
		return &NotificationError{
			Code:  NotificationUnavailable,
			Cause: errors.New("notification transport is not configured"),
		}
	}
	if bus.ChannelSubscriberCount(notify.SendChannel) == 0 {
		s.noSubscriberOnce.Do(func() {
			log.Printf("notifications: bridge accepted notification with no connected launcher subscriber")
		})
	}
	if _, err := bus.Emit(eventchan.NotificationSend, payload); err != nil {
		return fmt.Errorf("publish notification to launcher: %w", err)
	}
	return nil
}

// unavailableNotificationSender refuses every send with a typed
// NotificationUnavailable. It has no production installer any more: the
// isolated boot modes that used to take it now install the real transport
// sender (`newIsolatedProviderApp`), because a stub there made the one
// pipe an isolated boot exists to exercise the one pipe it did not run.
// The platform senders in app_notifications_desktop.go still return the
// same typed code when the OS itself refuses, which is what the frontend
// branches on; this type survives as the shape those tests assert against.
type unavailableNotificationSender struct {
	reason error
}

func (s unavailableNotificationSender) send(notify.Send) error {
	return &NotificationError{Code: NotificationUnavailable, Cause: s.reason}
}

var _ osNotificationSender = (*transportNotificationSender)(nil)
