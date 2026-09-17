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
	// NotificationHiddenThread is the third preference answer: the kind was
	// allowed, but the thread is one the sidebar does not list and this
	// screen has not opted into hearing about those
	// (settings.NotifyHiddenThreads). Its own code for the same reason
	// NotificationScreenAttended has one: "you turned the kind off" and
	// "that thread is not on your sidebar" are different answers to "why did
	// I not get a notification".
	NotificationHiddenThread NotificationErrorCode = "hidden_thread"
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
	case NotificationHiddenThread:
		return "OS notifications for threads not in the sidebar are turned off"
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
// an interruption" (narrowed, for a thread the sidebar does not list, by the
// hidden-thread opt-in); the attended-screen rules answer "is this screen
// already looking". Both live here for the same reason: a sender that could
// reach a presenter without passing them is a sender that can forget one.
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
		if err := a.notificationPreferenceRefusal(send); err != nil {
			return err
		}
		if a.screenIsAlreadyLooking(send) {
			return &NotificationError{Code: NotificationScreenAttended}
		}
		// Both halves of the gate said yes, so this moment may interrupt
		// this screen. The cue rides that one answer rather than asking
		// again — see publishNotificationSound.
		//
		// EXACTLY ONE SOUND. The cue frame and the banner's own platform
		// sound are the two ways this notification can be heard, and they
		// are resolved from ONE reading of the screen's settings so they
		// cannot disagree: whichever the user chose plays, and the other
		// stays quiet.
		current := a.backendScreenSettings()
		send.Silent = hostBannerSilentIn(current, send.Kind)
		a.publishNotificationSound(current, send.Kind)
	}
	return a.notifyOSUngated(send)
}

// backendScreenSettings reads the preferences of the screen this process
// interrupts — the backend machine's own. A settings service that is not
// wired yet answers from DefaultSettings, which has every notification
// preference ON: an App that has not finished booting must not silently
// start swallowing the notices it does raise during boot (the WSL "update
// didn't apply" notice is exactly one).
func (a *App) backendScreenSettings() settings.Settings {
	if a.settings == nil {
		return settings.DefaultSettings
	}
	return a.settings.BackendScreen().Get()
}

// publishNotificationSound emits the cue for a send the gate has ALREADY
// admitted, from the settings reading its caller resolved the banner's own
// sound against. Both production callers pass the backend screen's settings:
// notifyOS, immediately after both gate halves passed and before
// presentation, and PreviewNotificationSound, whose whole purpose is to make
// this screen play the cue it is configured for. That is what makes the sound
// and the banner one decision: a kind the user silenced, a hidden thread they
// did not opt into, and an attended screen never reach notifyOS's call, so
// none of those can be heard.
//
// It is deliberately independent of whether the OS presentation SUCCEEDS. A
// cue is a notification channel of its own: on a machine whose notification
// permission was denied, or in a mode with no presenter, the sound is the
// only thing left that can say "your turn finished", and withholding it would
// silence the user twice for one platform failure.
func (a *App) publishNotificationSound(current settings.Settings, kind notify.Kind) {
	event, ok := notify.SoundEventFor(kind)
	if !ok {
		return
	}
	cue, enabled := notificationSoundCueIn(current, event)
	if !enabled {
		return
	}
	// The system sound is the banner's to play, not a frame: the presenter
	// sends that banner with the platform default sound, and publishing a
	// cue named "system" would only make the player report an unknown cue.
	if cue == settings.NotifyCueSystem {
		return
	}
	a.emit(eventchan.NotificationSound, notify.SoundCue{Event: event, Cue: cue})
}

// notificationSoundCueIn answers "does this screen play a cue for this event,
// and which one" from ONE screen's settings.
//
// Its own function, taken apart from the App, for the reason
// notificationKindEnabledIn is: it is a total switch over a closed set, and a
// second copy of it would eventually disagree with this one about an event.
// TOTAL with no permissive default — an event this build does not know is one
// with no preference behind it, and playing an unrequested sound is worse
// than playing none.
func notificationSoundCueIn(current settings.Settings, event notify.SoundEvent) (string, bool) {
	if !current.NotificationSoundsEnabled {
		return "", false
	}
	switch event {
	case notify.SoundTurnComplete:
		return current.NotifySoundCueTurnComplete, current.NotifySoundTurnComplete
	case notify.SoundInputNeeded:
		return current.NotifySoundCueInputNeeded, current.NotifySoundInputNeeded
	case notify.SoundAttention:
		return current.NotifySoundCueAttention, current.NotifySoundAttention
	default:
		return "", false
	}
}

// hostBannerSilentIn answers whether the OS banner this host raises must be
// presented WITHOUT the platform's own notification sound, from ONE screen's
// settings.
//
// EXACTLY ONE SOUND PER NOTIFICATION. A notification can be heard twice —
// once as the in-app cue on `notification:sound`, once as the sound the
// platform attaches to the banner — and before this answer existed both fired
// for every send on macOS and Windows. The banner keeps the platform sound in
// exactly one case: sounds are on, this event's toggle is on, and the cue the
// user picked for it IS the system sound (settings.NotifyCueSystem), which is
// the one choice that means "let the banner make the noise". Every other
// reading — master switch off, event toggle off, any built-in cue — is silent,
// because either nothing should be heard or the cue frame is already playing.
//
// It shares notificationSoundCueIn rather than restating the preference
// switch, so the cue and the banner cannot disagree about an event and leave
// the user with two sounds or none. Total over the closed set: a kind with no
// sound event publishes no cue frame, so its banner is silent too.
func hostBannerSilentIn(current settings.Settings, kind notify.Kind) bool {
	event, ok := notify.SoundEventFor(kind)
	if !ok {
		return true
	}
	cue, enabled := notificationSoundCueIn(current, event)
	return !enabled || cue != settings.NotifyCueSystem
}

// PreviewNotificationSound plays what one sound event will sound like when it
// happens, on the backend machine's own screen: a real OS notification
// carrying the platform sound when that event's cue is the system sound, and
// the in-app cue frame when it is a built-in.
//
// A PREVIEW IS THE ONLY WAY TO HEAR THE SYSTEM SOUND. Every built-in cue can
// be auditioned by the settings page itself, because the asset is in the
// bundle; the platform sound is not a file this app owns and only arrives
// attached to a banner, so the only honest preview of it is a banner.
//
// HOST-SCOPED for the reason SetAppearance is (app_appearance.go): it makes
// THIS machine's screen do something — raise a banner and make a noise — and
// a paired device asking for that would be interrupting a desk it is not
// sitting at. The event names what the user is auditioning, not a thread, so
// the send carries no route.
//
// It sends through notifyOSUngated, the second and last bypass of the
// preference and attended-screen gates. The user clicked a button on the
// settings page, so the attended-screen gate would swallow every preview by
// definition — they ARE looking at the app — and the per-kind toggles answer
// "is this moment worth an interruption" about a moment that is not
// happening. The two sound preferences the preview does NOT bypass are its
// own: Silent is resolved exactly as notifyOS resolves it, so a muted screen
// previews silently rather than lying about what the event will do.
//
//ao:scope host
//ao:route home
func (a *App) PreviewNotificationSound(event string) error {
	kind, body, ok := notificationPreviewFor(notify.SoundEvent(event))
	if !ok {
		return fmt.Errorf("notification sound event %q is unsupported", event)
	}
	current := a.backendScreenSettings()
	// The one preference the preview honours from the banner gate: a screen
	// that turned notifications off has nothing to audition, and a banner
	// raised anyway would contradict the switch the user can see.
	if !current.NotificationsEnabled {
		return &NotificationError{Code: NotificationSuppressed}
	}
	a.publishNotificationSound(current, kind)
	return a.notifyOSUngated(notify.Send{
		ID:     "preview:" + event,
		Kind:   kind,
		Title:  "Agent Overflow",
		Body:   body,
		Target: notify.Target{Kind: notify.TargetNone, BackendID: a.notificationBackendID()},
		Silent: hostBannerSilentIn(current, kind),
	})
}

// notificationPreviewFor answers the kind and the body one preview is raised
// under, or false for a value that is not one of the three sound events.
//
// The kind is REPRESENTATIVE, not arbitrary: it has to be one that
// notify.SoundEventFor folds back onto the event being previewed, or the
// preview would resolve its sound from the wrong row of the settings page.
// TOTAL over the closed set, with no permissive default — an unrecognised
// event is wire input from a settings page, and raising a banner for it would
// be inventing a preference.
func notificationPreviewFor(event notify.SoundEvent) (notify.Kind, string, bool) {
	switch event {
	case notify.SoundTurnComplete:
		return notify.KindTurnComplete, "This is the turn complete sound.", true
	case notify.SoundInputNeeded:
		return notify.KindApprovalNeeded, "This is the approval needed sound.", true
	case notify.SoundAttention:
		return notify.KindError, "This is the attention sound.", true
	default:
		return "", "", false
	}
}

// notificationPreferenceRefusal answers the PER-KIND half of the gate for the
// backend machine's own screen, as the typed refusal notifyOS returns, or nil
// when the screen's preferences let the send through.
func (a *App) notificationPreferenceRefusal(send notify.Send) error {
	current := a.backendScreenSettings()
	return notificationPreferenceRefusalIn(current, send)
}

// notificationPreferenceRefusalIn is the per-kind gate plus the one
// narrowing that sits inside it, with the screen taken out: given ONE
// screen's settings, may this send interrupt it?
//
// ONE COPY, TWO SCREENS, the same rule notificationKindEnabledIn states: the
// desktop asks it of the backend machine's own settings and the push fan-out
// asks it of each phone's bucket (app_push.go pushAllowed). The hidden-thread
// question lives here rather than beside the kind switch because it is not a
// kind. A hidden thread's turn is still a turn; the thread is simply one the
// sidebar does not list, so it passes its kind's toggle AND the opt-in for
// threads off the sidebar (settings.NotifyHiddenThreads, default off).
//
// A RETRACTION IS NEVER GATED, and this function is only ever asked about a
// presentation; both callers branch on Retract before reaching it.
func notificationPreferenceRefusalIn(current settings.Settings, send notify.Send) error {
	if !notificationKindEnabledIn(current, send.Kind) {
		return &NotificationError{Code: NotificationSuppressed}
	}
	if send.HiddenThread && !current.NotifyHiddenThreads {
		return &NotificationError{Code: NotificationHiddenThread}
	}
	return nil
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
// The preference comes from that screen (Service.BackendScreen) for the same
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
// The preference is ONE picker with four readings (settings.NotifyQuiet*),
// not two toggles, because the reading most people want — quiet about a
// thread I have open while I am in the app, everything else always — is the
// AND of the two facts, and independent toggles can only say OR. The
// thread-visible fact is only ever true for a send whose Target NAMES a
// thread: a workflow-attention target names a work item and the signed-out /
// update kinds name nothing, so there is no thread for a pane to be showing.
// Under the two thread readings those sends are therefore always raised, and
// under the focused reading they are judged by focus alone.
func (a *App) screenIsAlreadyLooking(send notify.Send) bool {
	current := a.backendScreenSettings()
	if current.NotifyQuietWhen == settings.NotifyQuietNever {
		// screenAttendedIn answers the same for this reading. Short-circuited
		// here only to skip the presence read, which takes the bus lock.
		return false
	}
	bus := a.eventBus.Load()
	if bus == nil {
		// No transport, so no screen has told us anything. "Not attended" is
		// the answer that raises the notification, which is the behavior
		// before this gate existed.
		return false
	}
	hasThreadTarget := send.Target.Kind == notify.TargetThread
	threadID := ""
	if hasThreadTarget {
		threadID = send.Target.ThreadID
	}
	focused, threadVisible := bus.LocalScreenPresence(threadID)
	return screenAttendedIn(current.NotifyQuietWhen, focused, threadVisible, hasThreadTarget)
}

// screenAttendedIn is the attended-screen switch with the App, the settings
// service and the transport taken out: given the reading a screen chose and
// the two facts that screen last stated, is it already looking?
//
// Its own function for the reason notificationKindEnabledIn and
// notificationSoundCueIn are: it is a total switch over a closed set, and the
// REMOTE presenter runs the same decision against its own screen
// (frontend/src/lib/notifications/gate.ts). The two are held together by a
// shared decision table, internal/notify/testdata/gate_cases.json, which both
// sides run case for case; a copy that drifted would mean one screen staying
// quiet where the other spoke, from settings that read identically.
//
// hasThreadTarget is a parameter rather than something the caller folds into
// threadVisible because it is the narrowing the two thread readings depend
// on: a send whose Target does not NAME a thread has no thread for a pane to
// be showing, so a workflow-attention or signed-out notice is raised under
// both of them and judged by focus alone under the third.
func screenAttendedIn(quietWhen string, focused, threadVisible, hasThreadTarget bool) bool {
	threadVisible = threadVisible && hasThreadTarget
	switch quietWhen {
	case settings.NotifyQuietNever:
		return false
	case settings.NotifyQuietWhenFocused:
		return focused
	case settings.NotifyQuietWhenThreadVisible:
		return threadVisible
	case settings.NotifyQuietWhenFocusedAndThreadVisible:
		return focused && threadVisible
	default:
		// sanitizeLoadedSettings and validateSettings keep the value inside
		// the four readings; an unknown one here is a settings bug, and the
		// answer that raises the notification is the one that loses nothing.
		log.Printf("notifications: unknown notifyQuietWhen %q, raising", quietWhen)
		return false
	}
}

// notificationKindEnabledIn answers the user's preference for one kind on
// ONE screen's settings: may this kind interrupt it?
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
