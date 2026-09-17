package settings

import (
	"fmt"
	"log"
	"regexp"
	"strings"
)

// NotifyCueCustomPrefix marks a cue that is a file in <configDir>/sounds
// rather than one of the assets bundled with the frontend. A value reads
// `custom:desk-bell`.
//
// The prefix exists so the two vocabularies cannot collide: a user's cue
// named `swoosh` would otherwise silently shadow the built-in, and a future
// built-in would silently shadow a cue the user already chose.
const NotifyCueCustomPrefix = "custom:"

// BuiltinNotifyCues is every built-in cue, in the order a picker and an error
// message list them. Ordered rather than a set because both renderings are
// user-facing and must be stable.
var BuiltinNotifyCues = []string{
	NotifyCueSwoosh,
	NotifyCueMarimba,
	NotifyCueChord,
	NotifyCueKnock,
	NotifyCuePop,
	NotifyCueHum,
	NotifyCueChime,
	NotifyCueSystem,
}

// notifyCueCustomIDGrammar is the id shape a `custom:` cue may name:
// kebab-case ASCII, the same rule internal/soundlib applies to a cue's
// filename stem.
//
// Restated rather than imported, for the reason spinnerAnimationIDPattern is
// (spinner.go): this package stays dependency-free. Unlike that one the
// vocabulary here is not wider — a `custom:` value can only ever name a file
// in the sounds directory — so the two are pinned equal by
// TestNotifyCueCustomIDGrammarMatchesSoundlib, which imports soundlib from
// the test side where a dependency costs nothing.
const notifyCueCustomIDGrammar = `[a-z0-9][a-z0-9-]{0,63}`

// NotifyCuePatternSource is the complete rule for a notifySoundCue* value as
// a regular-expression SOURCE, and it is the ONE authority for that rule.
//
// It is exported because the frontend needs the same answer: gendefaults
// emits it into FRONTEND_SETTING_PATTERNS, and frontendPreferences validates
// a locally stored preference against it. These three keys are the only ones
// whose legal values are not a closed list — a cue the user added an hour ago
// is legal and no generated enum could name it — so a mirrored option list
// would reject every custom cue the moment it was written.
var NotifyCuePatternSource = `^(?:` + strings.Join(BuiltinNotifyCues, "|") + `|` +
	regexp.QuoteMeta(NotifyCueCustomPrefix) + notifyCueCustomIDGrammar + `)$`

var notifyCuePattern = regexp.MustCompile(NotifyCuePatternSource)

// notifyCueKeys are the three settings keys carrying a cue. Named once so
// the strict path, the lenient path and the frontend generator cannot
// disagree about which keys the pattern governs.
var notifyCueKeys = [3]string{
	"notifySoundCueTurnComplete",
	"notifySoundCueInputNeeded",
	"notifySoundCueAttention",
}

// notifyCueField binds one key to the field it governs and to the default
// that field falls back to.
type notifyCueField struct {
	key   string
	value *string
	// fallback is this EVENT's default, not a shared one: a cue file that
	// disappeared while a screen was away must not drag the other two
	// events onto one sound.
	fallback string
}

// notifyCueFields is the table both validation paths walk. An array, so the
// per-call cost is a stack copy of three headers rather than an allocation on
// every settings load.
func notifyCueFields(current *Settings) [3]notifyCueField {
	return [3]notifyCueField{
		{notifyCueKeys[0], &current.NotifySoundCueTurnComplete, DefaultSettings.NotifySoundCueTurnComplete},
		{notifyCueKeys[1], &current.NotifySoundCueInputNeeded, DefaultSettings.NotifySoundCueInputNeeded},
		{notifyCueKeys[2], &current.NotifySoundCueAttention, DefaultSettings.NotifySoundCueAttention},
	}
}

// validateNotifyCue is the strict path (Update): a value that is neither a
// built-in nor a well-formed `custom:<id>` is refused with the list.
//
// It deliberately does NOT check that the named cue file exists. The keys are
// device-tier, so they are written from a screen that may not be the backend
// host, and a cue can be deleted and restored while a screen holds the
// preference; refusing the write would turn a temporarily missing file into a
// lost setting. The player substitutes the event's default and says so.
func validateNotifyCue(field, value string) error {
	if notifyCuePattern.MatchString(value) {
		return nil
	}
	return fmt.Errorf(
		"%s must be one of %s, or %s<id> naming a sound in the sounds directory",
		field, strings.Join(BuiltinNotifyCues, ", "), NotifyCueCustomPrefix)
}

// sanitizeNotifyCue is the lenient path (load): an authored value that is not
// a cue falls back to this event's default, with the repair logged.
func sanitizeNotifyCue(field, value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if notifyCuePattern.MatchString(trimmed) {
		return trimmed
	}
	log.Printf("settings: invalid %s %q, using default %q", field, value, fallback)
	return fallback
}
