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
	// NotificationScreenAttended is the ATTENDED-SCREEN refusal: the kind was
	// allowed, and the screen this send would have interrupted is already
	// looking at the thing it was about to say.
	//
	// Its own code rather than a second NotificationSuppressed, because the
	// two are different facts about a user and a log line that cannot tell
	// them apart is useless for the one question anybody asks here — "why
	// did I not get a notification". "You turned it off" and "you were
	// watching" have different answers.
	NotificationScreenAttended NotificationErrorCode = "screen_attended"
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
	case NotificationScreenAttended:
		return "the screen is already looking"
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
// TWO QUESTIONS, ONE GATE. The per-kind toggles answer "is this moment worth
// an interruption"; the attended-screen rules answer "is this screen already
// looking". Both live here for the same reason: a sender that could reach a
// presenter without passing them is a sender that can forget one.
//
// A RETRACTION IS NEVER GATED, by either half. The gate answers "may I
// interrupt you", and withdrawing something already on screen is the opposite
// of an interruption. Gating it would mean a user who turns a kind off — or
// who walks back to their desk — between a send and its retraction keeps the
// notification forever, and the toggle would strand the very alerts it was
// meant to stop.
func (a *App) notifyOS(send notify.Send) error {
	if err := notify.ValidateSend(send); err != nil {
		return err
	}
	if !send.Retract {
		if !a.notificationKindEnabled(send.Kind) {
			return &NotificationError{Code: NotificationSuppressed}
		}
		if a.screenIsAlreadyLooking(send) {
			return &NotificationError{Code: NotificationScreenAttended}
		}
	}
	return a.notifyOSUngated(send)
}

// notifyOSUngated is the presentation half of notifyOS with the two gates
// above already answered — by the caller, and by nothing else.
//
// It exists for ONE caller, and that caller is not a production sender: the
// harness host's Notify RPC (app_harness.go), whose send exercises the pipe
// rather than reporting a moment. Every gate here reads a preference or a
// screen the e2e rig cannot see, so a harness notification that went through
// them would depend on whether a Playwright page happened to hold focus.
//
// It is not a second pipe: it validates, and it is the only thing that ever
// reaches the presenter, so notifyOS is still the one place a real send is
// judged. TestOnlyTheHarnessBypassesTheNotificationGate keeps the caller list
// at one — the bypass is only safe while nothing that describes real state
// can reach it.
func (a *App) notifyOSUngated(send notify.Send) error {
	if err := notify.ValidateSend(send); err != nil {
		return err
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

// screenIsAlreadyLooking answers the ATTENDED-SCREEN half of the gate for one
// presentation: is the backend machine's own screen showing what this send
// was about to say?
//
// The preferences come from that screen (Service.BackendScreen) for the same
// reason the per-kind ones do — it is the screen this process interrupts — and
// the facts come from the transport's per-connection presence
// (internal/transport/presence.go), ORed over the connections on a loopback
// origin.
//
// READING FOCUS HERE CHANGES NOTHING ABOUT DELIVERY. It decides whether an OS
// notification is RAISED and nothing else: no client is sent fewer frames, no
// surface renders differently, and no work is skipped because something is
// off-view. Off-view work shedding is a rejected design in this codebase
// (internal/transport/lease.go, event_entity.go) and this is not it — the
// alternative to a toast is no toast, not a stale pane.
//
// Two rules the shape encodes:
//
//   - The two preferences are INDEPENDENT, so a person who wants only the
//     stronger one gets only the stronger one.
//   - The thread-visible rule applies only to a send whose Target NAMES a
//     thread. A workflow-attention target names a work item and the
//     signed-out / update kinds name nothing, so there is no thread for a
//     pane to be showing; those remain subject to the focus rule alone.
func (a *App) screenIsAlreadyLooking(send notify.Send) bool {
	current := settings.DefaultSettings
	if a.settings != nil {
		current = a.settings.BackendScreen().Get()
	}
	if !current.NotifyMuteWhenFocused && !current.NotifyMuteWhenThreadVisible {
		return false
	}
	bus := a.eventBus.Load()
	if bus == nil {
		// No transport, so no screen has told us anything. "Not attended" is
		// the answer that raises the notification, which is the behavior
		// before this gate existed.
		return false
	}
	threadID := ""
	if send.Target.Kind == notify.TargetThread {
		threadID = send.Target.ThreadID
	}
	focused, threadVisible := bus.LocalScreenPresence(threadID)
	if current.NotifyMuteWhenFocused && focused {
		return true
	}
	return current.NotifyMuteWhenThreadVisible && threadVisible
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
	return notificationKindEnabledIn(current, kind)
}

// notificationKindEnabledIn is that question with the screen taken out of
// it: given ONE screen's settings, may this kind interrupt it?
//
// ONE COPY, TWO SCREENS. The desktop asks it of the backend machine's own
// settings; the push fan-out asks it of each registered phone's device-tier
// bucket (app_push.go). They are the same question about different screens,
// and two copies of this switch would eventually disagree about a kind — at
// which point a phone would buzz for something the person had turned off, or
// stay silent for something they had not.
//
// TOTAL, with no permissive default. Every notify.Kind has a toggle, so an
// unknown kind is one this build has no preference for at all and the honest
// answer is no: raising it would be interrupting somebody with something they
// were never offered a way to silence. Unreachable in practice — ValidateSend
// refuses an undeclared kind before any of this — which is exactly why the
// arm has to be the safe one rather than the convenient one.
func notificationKindEnabledIn(current settings.Settings, kind notify.Kind) bool {
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
	case notify.KindWorkflowAttention:
		return current.NotifyWorkflowAttention
	case notify.KindAppUpdate:
		return current.NotifyAppUpdate
	default:
		return false
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
