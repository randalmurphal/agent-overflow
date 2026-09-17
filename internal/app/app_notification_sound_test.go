package app

import (
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
