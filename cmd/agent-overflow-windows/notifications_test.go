//go:build windows

package main

import (
	"testing"

	"agent-overflow/internal/notify"
)

// EXACTLY ONE SOUND on the bridged path too. The launcher is the presenter
// for every WSL backend, so the same defect and the same fix apply here as
// in the in-process desktop presenter: a nil Sound is the vendored contract
// for "the platform's default sound", which used to play on top of the app's
// own cue on every toast.
//
// present() itself is not exercised here because the vendored service is a
// concrete type with no seam, and constructing it raises real Windows toasts
// on whoever runs the suite. The decision this file owns is the mapping.
func TestTheBackendsSilentAnswerBecomesASilentToast(t *testing.T) {
	sound := notificationSound(notify.Send{ID: "a", Kind: notify.KindTurnComplete, Silent: true})
	if sound == nil || !sound.Silent {
		t.Fatalf("Sound = %#v, want a silent toast", sound)
	}
	if sound.Name != "" {
		t.Fatalf("Sound.Name = %q, want empty: silence is not a named sound", sound.Name)
	}
}

// nil, not a named sound: the user chose "the system sound", and naming one
// here would pick a sound they did not choose.
func TestAnAudibleSendKeepsThePlatformDefault(t *testing.T) {
	if sound := notificationSound(notify.Send{ID: "a", Kind: notify.KindTurnComplete}); sound != nil {
		t.Fatalf("Sound = %#v, want nil (the platform's default sound)", sound)
	}
}
