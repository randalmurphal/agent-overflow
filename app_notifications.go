package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
)

type NotificationErrorCode string

const (
	NotificationUnavailable          NotificationErrorCode = "unavailable"
	NotificationAuthorizationPending NotificationErrorCode = "authorization_pending"
	NotificationAuthorizationDenied  NotificationErrorCode = "authorization_denied"
	NotificationDeliveryFailed       NotificationErrorCode = "delivery_failed"
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
	send(title, body string, target notify.Target) error
}

// notifyOS is the one internal notification send pipe. Callers do not know
// whether presentation is in-process or bridged to the Windows launcher.
func (a *App) notifyOS(title, body string, target notify.Target) error {
	if title == "" {
		return errors.New("notification title must be non-empty")
	}
	if len(title) > notify.MaxTitleBytes {
		return fmt.Errorf("notification title exceeds %d bytes", notify.MaxTitleBytes)
	}
	if len(body) > notify.MaxBodyBytes {
		return fmt.Errorf("notification body exceeds %d bytes", notify.MaxBodyBytes)
	}
	if err := notify.ValidateTarget(target); err != nil {
		return err
	}
	if a.osNotifications == nil {
		return &NotificationError{Code: NotificationUnavailable}
	}
	if err := a.osNotifications.send(title, body, target); err != nil {
		var notificationErr *NotificationError
		if errors.As(err, &notificationErr) {
			return err
		}
		return &NotificationError{Code: NotificationDeliveryFailed, Cause: err}
	}
	return nil
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
func (a *App) NotificationActivated(target notify.Target) error {
	return a.activateNotificationTarget(target)
}

type transportNotificationSender struct {
	app              *App
	nextID           atomic.Uint64
	noSubscriberOnce sync.Once
}

func newTransportNotificationSender(app *App) *transportNotificationSender {
	return &transportNotificationSender{app: app}
}

func (s *transportNotificationSender) send(title, body string, target notify.Target) error {
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
	payload := notify.Send{
		ID:     notify.NewID(s.nextID.Add(1)),
		Title:  title,
		Body:   body,
		Target: target,
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

func (s unavailableNotificationSender) send(string, string, notify.Target) error {
	return &NotificationError{Code: NotificationUnavailable, Cause: s.reason}
}

var _ osNotificationSender = (*transportNotificationSender)(nil)
