package settings

import (
	"regexp"
	"strings"
	"testing"

	"agent-overflow/internal/soundlib"
)

// The id grammar is restated in this package so it stays dependency-free
// (see notifycue.go). Restated is not duplicated only while something checks
// the two are equal — the test side may import soundlib freely, so this is
// where that check belongs. Drift here means a cue the backend writes is one
// the settings validator refuses.
func TestNotifyCueCustomIDGrammarMatchesSoundlib(t *testing.T) {
	if notifyCueCustomIDGrammar != soundlib.IDPattern {
		t.Fatalf("notifyCueCustomIDGrammar = %q, want soundlib.IDPattern %q", notifyCueCustomIDGrammar, soundlib.IDPattern)
	}
}

// Every built-in constant must be in the list the pattern and the error
// message are built from, or a cue the frontend offers is one Update
// refuses.
func TestBuiltinNotifyCuesCoverEveryConstant(t *testing.T) {
	for _, cue := range []string{
		NotifyCueSwoosh, NotifyCueMarimba, NotifyCueChord, NotifyCueKnock,
		NotifyCuePop, NotifyCueHum, NotifyCueBoop, NotifyCueSystem,
	} {
		if !notifyCuePattern.MatchString(cue) {
			t.Fatalf("the cue pattern rejects the built-in %q", cue)
		}
	}
	if len(BuiltinNotifyCues) != 8 {
		t.Fatalf("BuiltinNotifyCues = %v, want the eight built-ins", BuiltinNotifyCues)
	}
}

func TestNotifyCuePatternAcceptsCustomValues(t *testing.T) {
	for _, value := range []string{"custom:desk-bell", "custom:a", "custom:ping-2", "custom:" + strings.Repeat("a", 64)} {
		if !notifyCuePattern.MatchString(value) {
			t.Fatalf("the cue pattern rejects %q", value)
		}
	}
	for _, value := range []string{
		"custom:", "custom:Desk-Bell", "custom:desk bell", "custom:-bell",
		"custom:../escape", "custom:desk-bell.wav", "custom:" + strings.Repeat("a", 65),
		"custom:custom:a", "applause", "", " swoosh",
	} {
		if notifyCuePattern.MatchString(value) {
			t.Fatalf("the cue pattern accepts %q", value)
		}
	}
}

// The frontend compiles NotifyCuePatternSource with `new RegExp`. A Go-only
// construct in it would throw there at module load and take the whole
// preferences read with it.
func TestNotifyCuePatternSourceIsPortable(t *testing.T) {
	for _, construct := range []string{"(?P<", "(?i)", `\p{`, `\A`, `\z`} {
		if strings.Contains(NotifyCuePatternSource, construct) {
			t.Fatalf("NotifyCuePatternSource carries %q, which JavaScript's RegExp does not accept", construct)
		}
	}
	if _, err := regexp.Compile(NotifyCuePatternSource); err != nil {
		t.Fatalf("NotifyCuePatternSource does not compile: %v", err)
	}
}

// Strict and lenient behavior stay paired (AGENTS.md): Update refuses a bad
// cue outright, load repairs it to THIS event's default and leaves the other
// two alone.
func TestNotifyCueValidationIsStrictAndLoadIsLenient(t *testing.T) {
	current := DefaultSettings
	current.NotifySoundCueTurnComplete = "custom:desk-bell"
	current.NotifySoundCueInputNeeded = NotifyCueBoop
	validated, err := validateSettings(current)
	if err != nil {
		t.Fatalf("validateSettings: %v", err)
	}
	if validated.NotifySoundCueTurnComplete != "custom:desk-bell" {
		t.Fatalf("turn-complete cue = %q, want the custom cue preserved", validated.NotifySoundCueTurnComplete)
	}

	current.NotifySoundCueTurnComplete = "custom:Desk Bell"
	if _, err := validateSettings(current); err == nil {
		t.Fatal("validateSettings accepted a malformed custom cue")
	} else if !strings.Contains(err.Error(), "notifySoundCueTurnComplete") {
		t.Fatalf("validateSettings error %q does not name the key", err)
	}

	loaded := sanitizeLoadedSettings(current)
	if loaded.NotifySoundCueTurnComplete != DefaultSettings.NotifySoundCueTurnComplete {
		t.Fatalf("turn-complete cue = %q, want this event's default %q",
			loaded.NotifySoundCueTurnComplete, DefaultSettings.NotifySoundCueTurnComplete)
	}
	if loaded.NotifySoundCueInputNeeded != NotifyCueBoop {
		t.Fatalf("input-needed cue = %q, want the untouched %q", loaded.NotifySoundCueInputNeeded, NotifyCueBoop)
	}
}

// The error names every choice a user actually has. The old shared helper
// silently dropped values missing from its candidate list, so the message
// read "must be one of system".
func TestNotifyCueErrorListsEveryBuiltIn(t *testing.T) {
	err := validateNotifyCue("notifySoundCueAttention", "applause")
	if err == nil {
		t.Fatal("validateNotifyCue accepted a value that is not a cue")
	}
	for _, cue := range BuiltinNotifyCues {
		if !strings.Contains(err.Error(), cue) {
			t.Fatalf("error %q does not name the built-in %q", err, cue)
		}
	}
	if !strings.Contains(err.Error(), NotifyCueCustomPrefix) {
		t.Fatalf("error %q does not mention the custom form", err)
	}
}
