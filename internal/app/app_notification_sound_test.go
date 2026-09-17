package app

import (
	"errors"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/settings"
)

// The cue rides the banner's ONE decision. Every test here states a
// preference, calls notifyOS, and asks what reached the channel: nothing that
// the gate refused may be heard, and nothing the gate admitted may be silent
// unless a sound preference said so.

type cueRecorder struct {
	mu   sync.Mutex
	cues []notify.SoundCue
}

func (r *cueRecorder) hook(name string, data any) {
	if name != eventchan.NotificationSound.String() {
		return
	}
	cue, ok := data.(notify.SoundCue)
	if !ok {
		return
	}
	r.mu.Lock()
	r.cues = append(r.cues, cue)
	r.mu.Unlock()
}

func (r *cueRecorder) snapshot() []notify.SoundCue {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.SoundCue(nil), r.cues...)
}

// soundApp is the notification fixture with the cue channel observed. No bus
// is attached, so no screen is stated as attended and the attended half of
// the gate is out of the way; the attended half has its own tests.
func soundApp(t *testing.T) (*App, *cueRecorder) {
	t.Helper()
	app, _ := newNotificationMappingApp(t)
	recorder := &cueRecorder{}
	app.testEmitHook = recorder.hook
	return app, recorder
}

func updateSoundSettings(t *testing.T, app *App, values map[string]any) {
	t.Helper()
	if _, err := app.settings.BackendScreen().Update(values); err != nil {
		t.Fatalf("update settings %v: %v", values, err)
	}
}

func kindSend(kind notify.Kind) notify.Send {
	send := threadSend()
	send.Kind = kind
	return send
}

func wantOneCue(t *testing.T, recorder *cueRecorder, event notify.SoundEvent, cue string) {
	t.Helper()
	got := recorder.snapshot()
	if len(got) != 1 {
		t.Fatalf("cues = %+v, want exactly one", got)
	}
	if got[0].Event != event || got[0].Cue != cue {
		t.Fatalf("cue = %+v, want {%s %s}", got[0], event, cue)
	}
}

func wantNoCue(t *testing.T, recorder *cueRecorder, context string) {
	t.Helper()
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("%s: cues = %+v, want none", context, got)
	}
}

// The shipped defaults: master on, all three events on, each on its own cue.
func TestNotificationSoundDefaultsPlayEveryEvent(t *testing.T) {
	cases := []struct {
		kind  notify.Kind
		event notify.SoundEvent
		cue   string
	}{
		{notify.KindTurnComplete, notify.SoundTurnComplete, settings.NotifyCueSwoosh},
		{notify.KindApprovalNeeded, notify.SoundInputNeeded, settings.NotifyCueKnock},
		{notify.KindError, notify.SoundAttention, settings.NotifyCueHum},
		{notify.KindProviderSignedOut, notify.SoundAttention, settings.NotifyCueHum},
		{notify.KindWorkflowAttention, notify.SoundAttention, settings.NotifyCueHum},
		{notify.KindAppUpdate, notify.SoundAttention, settings.NotifyCueHum},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			app, recorder := soundApp(t)
			if err := app.notifyOS(kindSend(tc.kind)); err != nil {
				t.Fatalf("notifyOS: %v", err)
			}
			wantOneCue(t, recorder, tc.event, tc.cue)
		})
	}
}

func TestNotificationSoundMasterSwitchSilencesEveryEvent(t *testing.T) {
	app, recorder := soundApp(t)
	updateSoundSettings(t, app, map[string]any{"notificationSoundsEnabled": false})

	for _, kind := range []notify.Kind{notify.KindTurnComplete, notify.KindApprovalNeeded, notify.KindError} {
		if err := app.notifyOS(kindSend(kind)); err != nil {
			t.Fatalf("notifyOS %s: %v", kind, err)
		}
	}
	wantNoCue(t, recorder, "master switch off")
}

func TestNotificationSoundPerEventToggleIsIndependent(t *testing.T) {
	app, recorder := soundApp(t)
	updateSoundSettings(t, app, map[string]any{"notifySoundTurnComplete": false})

	if err := app.notifyOS(kindSend(notify.KindTurnComplete)); err != nil {
		t.Fatalf("notifyOS turn-complete: %v", err)
	}
	wantNoCue(t, recorder, "turn-complete cue disabled")

	if err := app.notifyOS(kindSend(notify.KindApprovalNeeded)); err != nil {
		t.Fatalf("notifyOS approval-needed: %v", err)
	}
	wantOneCue(t, recorder, notify.SoundInputNeeded, settings.NotifyCueKnock)
}

func TestNotificationSoundPlaysTheChosenCue(t *testing.T) {
	app, recorder := soundApp(t)
	updateSoundSettings(t, app, map[string]any{"notifySoundCueTurnComplete": settings.NotifyCueBoop})

	if err := app.notifyOS(kindSend(notify.KindTurnComplete)); err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	wantOneCue(t, recorder, notify.SoundTurnComplete, settings.NotifyCueBoop)
}

// The per-kind half of the banner gate. A kind the user silenced raises no
// banner, so it must raise no sound either: one decision, two presentations.
func TestASilencedKindIsAlsoSilentOnTheSpeaker(t *testing.T) {
	app, recorder := soundApp(t)
	updateSoundSettings(t, app, map[string]any{"notifyTurnComplete": false})

	if err := app.notifyOS(kindSend(notify.KindTurnComplete)); err == nil {
		t.Fatal("notifyOS must refuse a silenced kind")
	}
	wantNoCue(t, recorder, "kind silenced")
}

// The hidden-thread half. A thread off the sidebar is opt-in for banners; the
// cue inherits that without restating it.
func TestAHiddenThreadIsSilentOnTheSpeakerUntilOptedIn(t *testing.T) {
	app, recorder := soundApp(t)
	send := kindSend(notify.KindTurnComplete)
	send.HiddenThread = true

	if err := app.notifyOS(send); err == nil {
		t.Fatal("notifyOS must refuse a hidden thread by default")
	}
	wantNoCue(t, recorder, "hidden thread, not opted in")

	updateSoundSettings(t, app, map[string]any{"notifyHiddenThreads": true})
	if err := app.notifyOS(send); err != nil {
		t.Fatalf("notifyOS after opt-in: %v", err)
	}
	wantOneCue(t, recorder, notify.SoundTurnComplete, settings.NotifyCueSwoosh)
}

// A retraction removes a banner; there is nothing to hear about a moment
// being taken back.
func TestARetractionPlaysNoCue(t *testing.T) {
	app, recorder := soundApp(t)
	send := kindSend(notify.KindTurnComplete)
	send.Retract = true

	_ = app.notifyOS(send)
	wantNoCue(t, recorder, "retraction")
}

func TestNotificationSoundCueInIsTotal(t *testing.T) {
	current := settings.DefaultSettings
	if cue, ok := notificationSoundCueIn(current, notify.SoundEvent("not-an-event")); ok || cue != "" {
		t.Fatalf("unknown event = %q, %v; want \"\", false", cue, ok)
	}
	current.NotificationSoundsEnabled = false
	for _, event := range []notify.SoundEvent{notify.SoundTurnComplete, notify.SoundInputNeeded, notify.SoundAttention} {
		if cue, ok := notificationSoundCueIn(current, event); ok || cue != "" {
			t.Fatalf("master off, %s = %q, %v; want silence", event, cue, ok)
		}
	}
}

// The system sound is carried by the banner, so choosing it publishes no
// frame: a "system" cue on the wire would be a value no player has a file for.
func TestTheSystemCuePublishesNoFrame(t *testing.T) {
	app, recorder := soundApp(t)
	updateSoundSettings(t, app, map[string]any{"notifySoundCueTurnComplete": settings.NotifyCueSystem})

	if err := app.notifyOS(kindSend(notify.KindTurnComplete)); err != nil {
		t.Fatalf("notifyOS with the system cue: %v", err)
	}
	wantNoCue(t, recorder, "system cue chosen")
}

// EXACTLY ONE SOUND. A notification can be heard twice — the cue frame and
// the banner's own platform sound — and Silent is how the host says which of
// the two this one is. Every case below states a preference, calls notifyOS,
// and asks the presenter what it was handed AND the channel what it heard;
// the pair must never be two sounds, and never none where the user asked for
// one.
func wantSilent(t *testing.T, sends []notify.Send, silent bool, context string) {
	t.Helper()
	if len(sends) != 1 {
		t.Fatalf("%s: presenter sends = %+v, want exactly one", context, sends)
	}
	if sends[0].Silent != silent {
		t.Fatalf("%s: send.Silent = %v, want %v", context, sends[0].Silent, silent)
	}
}

// The shipped default is a built-in cue, so the app plays the sound and the
// banner must not add the platform's on top of it.
func TestABuiltInCueSilencesTheBanner(t *testing.T) {
	app, recorder := soundApp(t)
	sender := app.osNotifications.(*recordingNotificationSender)

	if err := app.notifyOS(kindSend(notify.KindTurnComplete)); err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	wantOneCue(t, recorder, notify.SoundTurnComplete, settings.NotifyCueSwoosh)
	wantSilent(t, sender.snapshot(), true, "default built-in cue")
}

// The system cue is the one choice that means "let the banner make the
// noise": no frame, and the banner keeps the platform sound.
func TestTheSystemCueLeavesTheBannerAudible(t *testing.T) {
	app, recorder := soundApp(t)
	sender := app.osNotifications.(*recordingNotificationSender)
	updateSoundSettings(t, app, map[string]any{"notifySoundCueTurnComplete": settings.NotifyCueSystem})

	if err := app.notifyOS(kindSend(notify.KindTurnComplete)); err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	wantNoCue(t, recorder, "system cue chosen")
	wantSilent(t, sender.snapshot(), false, "system cue")
}

// Muted means muted on both channels. The banner still appears — the sound
// preferences are not the notification toggles — but it makes no noise.
func TestTheSoundMasterSwitchSilencesTheBannerToo(t *testing.T) {
	app, recorder := soundApp(t)
	sender := app.osNotifications.(*recordingNotificationSender)
	updateSoundSettings(t, app, map[string]any{
		"notificationSoundsEnabled":  false,
		"notifySoundCueTurnComplete": settings.NotifyCueSystem,
	})

	if err := app.notifyOS(kindSend(notify.KindTurnComplete)); err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	wantNoCue(t, recorder, "sound master switch off")
	wantSilent(t, sender.snapshot(), true, "sound master switch off")
}

// The per-event toggle is the same answer one event down, and it holds even
// when that event's cue is the system sound — otherwise turning an event's
// sound off would leave the loudest one of all still playing.
func TestAnEventWithItsSoundOffSilencesTheBanner(t *testing.T) {
	app, recorder := soundApp(t)
	sender := app.osNotifications.(*recordingNotificationSender)
	updateSoundSettings(t, app, map[string]any{
		"notifySoundTurnComplete":    false,
		"notifySoundCueTurnComplete": settings.NotifyCueSystem,
	})

	if err := app.notifyOS(kindSend(notify.KindTurnComplete)); err != nil {
		t.Fatalf("notifyOS: %v", err)
	}
	wantNoCue(t, recorder, "turn-complete sound off")
	wantSilent(t, sender.snapshot(), true, "turn-complete sound off")
}

// A retraction carries no sound answer at all: there is no banner to attach
// one to, and ValidateSend refuses a retraction that claims otherwise.
func TestARetractionCarriesNoSoundAnswer(t *testing.T) {
	app, _ := soundApp(t)
	sender := app.osNotifications.(*recordingNotificationSender)
	send := kindSend(notify.KindTurnComplete)
	send.Retract = true
	send.Title = ""
	send.Target = notify.Target{}

	if err := app.notifyOS(send); err != nil {
		t.Fatalf("notifyOS retraction: %v", err)
	}
	got := sender.snapshot()
	if len(got) != 1 || !got[0].Retract {
		t.Fatalf("presenter sends = %+v, want one retraction", got)
	}
	if got[0].Silent {
		t.Fatalf("retraction carried Silent: %+v", got[0])
	}
}

// hostBannerSilentIn shares notificationSoundCueIn rather than restating the
// preference switch, so it is total for the same reason: an event this build
// does not know has no preference behind it, and the safe arm is the quiet
// one — matching publishNotificationSound, which publishes no frame either.
func TestHostBannerSilentInIsTotal(t *testing.T) {
	current := settings.DefaultSettings
	current.NotifySoundCueTurnComplete = settings.NotifyCueSystem
	current.NotifySoundCueInputNeeded = settings.NotifyCueSystem
	current.NotifySoundCueAttention = settings.NotifyCueSystem
	if !hostBannerSilentIn(current, notify.Kind("gossip")) {
		t.Fatal("an unknown kind was given the platform sound")
	}
	for _, kind := range []notify.Kind{
		notify.KindTurnComplete, notify.KindApprovalNeeded, notify.KindError,
		notify.KindProviderSignedOut, notify.KindWorkflowAttention, notify.KindAppUpdate,
	} {
		if hostBannerSilentIn(current, kind) {
			t.Fatalf("%s: silent under the system cue, want the platform sound", kind)
		}
	}
}

// The preview is the only way to hear the system sound, so it raises a real
// banner past the attended-screen gate — the user is looking at the settings
// page by definition — and resolves Silent exactly as notifyOS does.
func TestPreviewNotificationSoundRaisesTheSystemSound(t *testing.T) {
	cases := []struct {
		event notify.SoundEvent
		kind  notify.Kind
		cue   string
	}{
		{notify.SoundTurnComplete, notify.KindTurnComplete, "notifySoundCueTurnComplete"},
		{notify.SoundInputNeeded, notify.KindApprovalNeeded, "notifySoundCueInputNeeded"},
		{notify.SoundAttention, notify.KindError, "notifySoundCueAttention"},
	}
	for _, tc := range cases {
		t.Run(string(tc.event), func(t *testing.T) {
			app, recorder := soundApp(t)
			sender := app.osNotifications.(*recordingNotificationSender)
			updateSoundSettings(t, app, map[string]any{tc.cue: settings.NotifyCueSystem})

			if err := app.PreviewNotificationSound(string(tc.event)); err != nil {
				t.Fatalf("PreviewNotificationSound: %v", err)
			}
			wantNoCue(t, recorder, "system cue preview")
			got := sender.snapshot()
			wantSilent(t, got, false, "system cue preview")
			if got[0].ID != "preview:"+string(tc.event) {
				t.Fatalf("preview id = %q, want preview:%s", got[0].ID, tc.event)
			}
			// The kind has to fold back onto the event being auditioned, or
			// the preview would read its sound off the wrong settings row.
			if got[0].Kind != tc.kind {
				t.Fatalf("preview kind = %q, want %q", got[0].Kind, tc.kind)
			}
			if event, _ := notify.SoundEventFor(got[0].Kind); event != tc.event {
				t.Fatalf("preview kind %q folds onto %q, want %q", got[0].Kind, event, tc.event)
			}
			if got[0].Title == "" || got[0].Body == "" {
				t.Fatalf("preview send = %+v, want a title and a body", got[0])
			}
			if got[0].Target.Kind != notify.TargetNone || got[0].Target.ThreadID != "" {
				t.Fatalf("preview target = %#v, want no route", got[0].Target)
			}
		})
	}
}

// A preview of a BUILT-IN cue plays the cue and leaves the banner silent:
// the same "exactly one sound" answer a real notification gets, so what the
// user auditions is what they will hear.
func TestPreviewNotificationSoundPlaysTheCueForABuiltIn(t *testing.T) {
	app, recorder := soundApp(t)
	sender := app.osNotifications.(*recordingNotificationSender)
	updateSoundSettings(t, app, map[string]any{"notifySoundCueTurnComplete": settings.NotifyCueBoop})

	if err := app.PreviewNotificationSound(string(notify.SoundTurnComplete)); err != nil {
		t.Fatalf("PreviewNotificationSound: %v", err)
	}
	wantOneCue(t, recorder, notify.SoundTurnComplete, settings.NotifyCueBoop)
	wantSilent(t, sender.snapshot(), true, "built-in cue preview")
}

// The event is wire input from a settings page, so an unrecognised one is
// refused rather than raising a banner nobody asked for.
func TestPreviewNotificationSoundRefusesAnUnknownEvent(t *testing.T) {
	app, recorder := soundApp(t)
	sender := app.osNotifications.(*recordingNotificationSender)

	if err := app.PreviewNotificationSound("applause"); err == nil {
		t.Fatal("an unknown sound event was accepted")
	}
	wantNoCue(t, recorder, "unknown event")
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("unknown event presented %+v", got)
	}
}

// A screen that switched notifications off gets no banner from the preview
// either: the master switch is the one gate the audition honours, so the
// settings page never contradicts itself by raising what it says it will not.
func TestPreviewNotificationSoundHonoursTheMasterSwitch(t *testing.T) {
	app, recorder := soundApp(t)
	sender := app.osNotifications.(*recordingNotificationSender)
	updateSoundSettings(t, app, map[string]any{"notificationsEnabled": false})

	err := app.PreviewNotificationSound(string(notify.SoundTurnComplete))
	var nerr *NotificationError
	if !errors.As(err, &nerr) || nerr.Code != NotificationSuppressed {
		t.Fatalf("PreviewNotificationSound with notifications off = %v, want NotificationSuppressed", err)
	}
	wantNoCue(t, recorder, "notifications off")
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("notifications off still presented %+v", got)
	}
}
