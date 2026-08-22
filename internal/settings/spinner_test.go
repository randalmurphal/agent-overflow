package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The verb axis is the inverse of ClaudeTUIEnabled and has to be pinned
// the same way: its default is TRUE, which only holds for an upgrading
// user because the value is in DefaultSettings. Turning it off is then the
// non-default value, so writeSparse persists the OFF and drops the ON.
func TestSpinnerVerbsEnabledDefaultsOnAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// A file that predates the field — every existing user's file.
	if err := os.WriteFile(path, []byte(`{"claudeEnabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if !DefaultSettings.SpinnerVerbsEnabled {
		t.Fatal("DefaultSettings.SpinnerVerbsEnabled = false; an absent key would then read as off for every upgrading user")
	}
	svc := NewService(dir)
	if !svc.Get().SpinnerVerbsEnabled {
		t.Fatal("SpinnerVerbsEnabled = false for a file without the key, want true")
	}

	updated, err := svc.Update(map[string]any{"spinnerVerbsEnabled": false})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.SpinnerVerbsEnabled {
		t.Fatal("SpinnerVerbsEnabled = true after disabling")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatal(err)
	}
	if fileMap["spinnerVerbsEnabled"] != false {
		t.Fatalf("settings file = %s, want spinnerVerbsEnabled:false persisted", data)
	}
	if NewService(dir).Get().SpinnerVerbsEnabled {
		t.Fatal("reloaded SpinnerVerbsEnabled = true, want false")
	}
}

// The animation axis is the opposite decision: motion beside the composer
// is opt-in, so its default is the zero value and it must stay OUT of
// DefaultSettings — otherwise writeSparse would drop the user's `true`.
func TestSpinnerAnimationsEnabledDefaultsOffAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	if DefaultSettings.SpinnerAnimationsEnabled {
		t.Fatal("DefaultSettings.SpinnerAnimationsEnabled = true; animations are opt-in")
	}
	svc := NewService(dir)
	if svc.Get().SpinnerAnimationsEnabled {
		t.Fatal("SpinnerAnimationsEnabled = true on a fresh install")
	}
	if _, err := svc.Update(map[string]any{"spinnerAnimationsEnabled": true}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !NewService(dir).Get().SpinnerAnimationsEnabled {
		t.Fatal("reloaded SpinnerAnimationsEnabled = false, want the enable to survive the sparse write")
	}
}

func TestSpinnerCompactionAnimationDefaultsAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	// "" is the stored default: "never chosen", which the FRONTEND resolves
	// to its default sprite. A concrete id here would bake a frontend sprite
	// name into this package and make the settings UI's "Default" selection
	// unrepresentable.
	if DefaultSettings.SpinnerCompactionAnimation != "" {
		t.Fatalf("default compaction animation = %q, want empty", DefaultSettings.SpinnerCompactionAnimation)
	}
	svc := NewService(dir)
	if got := svc.Get().SpinnerCompactionAnimation; got != "" {
		t.Fatalf("fresh install compaction animation = %q, want empty", got)
	}

	// A whitespace patch stores "" — that is what makes a patch that
	// clears the field back to "Default" safe.
	updated, err := svc.Update(map[string]any{"spinnerCompactionAnimation": "  "})
	if err != nil {
		t.Fatalf("Update(empty): %v", err)
	}
	if updated.SpinnerCompactionAnimation != "" {
		t.Fatalf("empty selection = %q, want empty", updated.SpinnerCompactionAnimation)
	}

	// "none" is a real choice and must survive as itself.
	if updated, err = svc.Update(map[string]any{"spinnerCompactionAnimation": SpinnerCompactionAnimationNone}); err != nil {
		t.Fatalf("Update(none): %v", err)
	}
	if updated.SpinnerCompactionAnimation != SpinnerCompactionAnimationNone {
		t.Fatalf("compaction animation = %q, want %q", updated.SpinnerCompactionAnimation, SpinnerCompactionAnimationNone)
	}
	if got := NewService(dir).Get().SpinnerCompactionAnimation; got != SpinnerCompactionAnimationNone {
		t.Fatalf("reloaded compaction animation = %q, want %q", got, SpinnerCompactionAnimationNone)
	}

	// An id the app has never seen is accepted: this package cannot know
	// which sprites exist, and refusing one would make a settings file
	// unloadable the moment a sprite is renamed.
	if updated, err = svc.Update(map[string]any{"spinnerCompactionAnimation": "cat-typing-2"}); err != nil {
		t.Fatalf("Update(unknown id): %v", err)
	}
	if updated.SpinnerCompactionAnimation != "cat-typing-2" {
		t.Fatalf("compaction animation = %q, want the id kept", updated.SpinnerCompactionAnimation)
	}

	for _, bad := range []string{"Robo Papers", "robo_papers", "-orb", strings.Repeat("a", 65), "../escape"} {
		if _, err := svc.Update(map[string]any{"spinnerCompactionAnimation": bad}); err == nil {
			t.Fatalf("Update(%q) accepted a non-id", bad)
		}
	}
}

func TestSpinnerCustomVerbsValidationAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{
		"spinnerCustomVerbs": []any{"  Reticulating  ", "", "   ", "Deliberating", "Reticulating"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	want := []string{"Reticulating", "Deliberating"}
	if len(updated.SpinnerCustomVerbs) != len(want) {
		t.Fatalf("verbs = %q, want trimmed, empties dropped, deduped: %q", updated.SpinnerCustomVerbs, want)
	}
	for index, verb := range want {
		if updated.SpinnerCustomVerbs[index] != verb {
			t.Fatalf("verbs = %q, want %q", updated.SpinnerCustomVerbs, want)
		}
	}
	if reloaded := NewService(dir).Get().SpinnerCustomVerbs; len(reloaded) != len(want) || reloaded[0] != want[0] {
		t.Fatalf("reloaded verbs = %q, want %q", reloaded, want)
	}

	// Over-long and control-bearing verbs are REFUSED, not truncated: a
	// verb cut mid-word would render as something the user never wrote.
	tooLong := strings.Repeat("é", MaxSpinnerVerbRunes+1)
	if _, err := svc.Update(map[string]any{"spinnerCustomVerbs": []any{tooLong}}); err == nil {
		t.Fatalf("a %d-rune verb was accepted", MaxSpinnerVerbRunes+1)
	}
	// The cap counts RUNES, so the same length in a multi-byte script is
	// legal where a byte cap would have rejected it.
	atTheCap := strings.Repeat("é", MaxSpinnerVerbRunes)
	if _, err := svc.Update(map[string]any{"spinnerCustomVerbs": []any{atTheCap}}); err != nil {
		t.Fatalf("a %d-rune verb was rejected: %v", MaxSpinnerVerbRunes, err)
	}
	if _, err := svc.Update(map[string]any{"spinnerCustomVerbs": []any{"two\nlines"}}); err == nil {
		t.Fatal("a verb containing a newline was accepted")
	}

	// The list cap is a refusal on write.
	overflowing := make([]any, MaxSpinnerCustomVerbs+1)
	for index := range overflowing {
		overflowing[index] = "verb"
	}
	if _, err := svc.Update(map[string]any{"spinnerCustomVerbs": overflowing}); err == nil {
		t.Fatalf("a %d-entry verb list was accepted", MaxSpinnerCustomVerbs+1)
	}
}

func TestSpinnerDisabledAnimationsValidationAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{
		"spinnerDisabledAnimations": []any{" robo-papers ", "orb", "robo-papers"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.SpinnerDisabledAnimations) != 2 {
		t.Fatalf("disabled animations = %q, want trimmed and deduped", updated.SpinnerDisabledAnimations)
	}
	if reloaded := NewService(dir).Get().SpinnerDisabledAnimations; len(reloaded) != 2 {
		t.Fatalf("reloaded disabled animations = %q, want 2", reloaded)
	}

	for _, bad := range []string{"Robo Papers", "", "-orb", strings.Repeat("a", 65)} {
		if _, err := svc.Update(map[string]any{"spinnerDisabledAnimations": []any{bad}}); err == nil {
			t.Fatalf("Update(%q) accepted a non-id", bad)
		}
	}
	overflowing := make([]any, MaxSpinnerDisabledAnimations+1)
	for index := range overflowing {
		overflowing[index] = "orb"
	}
	if _, err := svc.Update(map[string]any{"spinnerDisabledAnimations": overflowing}); err == nil {
		t.Fatalf("a %d-entry exclusion list was accepted", MaxSpinnerDisabledAnimations+1)
	}
}

// The load path is lenient where the write path is strict: a hand-edited
// file with one bad entry keeps the rest instead of losing the list.
func TestSpinnerListsSanitizeOnLoadInsteadOfFailing(t *testing.T) {
	dir := t.TempDir()
	body := `{
	  "spinnerCustomVerbs": ["Reticulating", "  ", "bro\nken", "Deliberating"],
	  "spinnerDisabledAnimations": ["orb", "Not An Id", "robo-papers"],
	  "spinnerCompactionAnimation": "Not An Id"
	}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded := NewService(dir).Get()
	if len(loaded.SpinnerCustomVerbs) != 2 {
		t.Fatalf("verbs = %q, want the two valid entries", loaded.SpinnerCustomVerbs)
	}
	if len(loaded.SpinnerDisabledAnimations) != 2 {
		t.Fatalf("disabled animations = %q, want the two valid ids", loaded.SpinnerDisabledAnimations)
	}
	if loaded.SpinnerCompactionAnimation != DefaultSettings.SpinnerCompactionAnimation {
		t.Fatalf("compaction animation = %q, want the default after an invalid value", loaded.SpinnerCompactionAnimation)
	}
	// The lists are capped on load too, and the cap is audible in the log
	// rather than silent.
	if len(sanitizeSpinnerCustomVerbs("spinnerCustomVerbs", makeVerbs(MaxSpinnerCustomVerbs+5))) != MaxSpinnerCustomVerbs {
		t.Fatal("the load path did not cap the verb list")
	}
}

func makeVerbs(count int) []string {
	verbs := make([]string, count)
	for index := range verbs {
		verbs[index] = "verb-" + string(rune('a'+index%26)) + string(rune('a'+index/26))
	}
	return verbs
}
