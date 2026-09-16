package notify

import "testing"

// TestSoundEventCoversEveryKind is the totality guard sound.go names. A kind
// with no event raises a banner and no cue, with no setting that explains it.
func TestSoundEventCoversEveryKind(t *testing.T) {
	for kind := range kinds {
		event, ok := SoundEventFor(kind)
		if !ok {
			t.Errorf("kind %q has no sound event", kind)
			continue
		}
		switch event {
		case SoundTurnComplete, SoundInputNeeded, SoundAttention:
		default:
			t.Errorf("kind %q maps to unknown event %q", kind, event)
		}
	}
	if len(soundEvents) != len(kinds) {
		t.Errorf("soundEvents has %d entries, kinds has %d", len(soundEvents), len(kinds))
	}
}

func TestSoundEventForRefusesUndeclaredKind(t *testing.T) {
	if event, ok := SoundEventFor(Kind("not-a-kind")); ok || event != "" {
		t.Fatalf("SoundEventFor(unknown) = %q, %v; want \"\", false", event, ok)
	}
}

// The three events are distinct: a person tells "finished" from "needs you"
// from "something is wrong" by ear, which is the whole reason for three.
func TestSoundEventsSeparateTheThreeMoments(t *testing.T) {
	if got, _ := SoundEventFor(KindTurnComplete); got != SoundTurnComplete {
		t.Errorf("turn-complete maps to %q", got)
	}
	if got, _ := SoundEventFor(KindApprovalNeeded); got != SoundInputNeeded {
		t.Errorf("approval-needed maps to %q", got)
	}
	for _, kind := range []Kind{KindError, KindProviderSignedOut, KindWorkflowAttention, KindAppUpdate} {
		if got, _ := SoundEventFor(kind); got != SoundAttention {
			t.Errorf("%q maps to %q, want attention", kind, got)
		}
	}
}
