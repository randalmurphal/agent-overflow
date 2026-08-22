package settings

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds for the spinner lists. Both are absurdly generous next to any
// real use — a user curating working-indicator verbs writes a handful,
// not hundreds — and exist for the same reason every other list bound in
// this package does: `GetSettings` ships the whole struct on every read,
// including to a LAN-attached client, so a hand-edited or
// agent-generated file must not be able to decide how big that answer is.
const (
	// MaxSpinnerCustomVerbs caps the user's verb list.
	MaxSpinnerCustomVerbs = 500
	// MaxSpinnerVerbRunes caps ONE verb, in runes rather than bytes: this
	// is display text that may be any script, and a byte bound would cut
	// a Japanese verb at a third the length of an English one.
	MaxSpinnerVerbRunes = 80
	// MaxSpinnerDisabledAnimations caps the exclusion list. Sized well
	// above the built-in set plus a full custom spinners directory.
	MaxSpinnerDisabledAnimations = 256
)

// spinnerAnimationIDPattern is the animation-id shape: kebab-case ASCII,
// the same rule internal/spinner applies to a sprite's filename stem.
//
// Duplicated rather than imported, for two reasons. This package must
// stay dependency-free (see the AGENTS.md anti-patterns), and the
// vocabulary here is WIDER than that package's: built-in sprites ship
// with the frontend and never appear in <configDir>/spinners at all, so
// an id in these lists may name something the spinner package has never
// seen. What the two share is the id GRAMMAR, and that is what is
// restated here.
var spinnerAnimationIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// SpinnerCompactionAnimationNone is the explicit "play nothing while
// compacting" selection. It is a real value, distinct from the empty
// string: empty means "the user never chose", which resolves to the
// default.
const SpinnerCompactionAnimationNone = "none"

// validateSpinnerCustomVerbs is the strict path (Update). Entries are
// trimmed and empties dropped — a blank row in the settings UI is not an
// error, it is an unfilled row — but an over-long or control-bearing verb
// is REFUSED rather than truncated: a verb cut mid-word would render as
// nonsense the user never wrote, and silently shortening what they just
// saved makes the next read disagree with the edit.
func validateSpinnerCustomVerbs(field string, verbs []string) ([]string, error) {
	if len(verbs) == 0 {
		return nil, nil
	}
	if len(verbs) > MaxSpinnerCustomVerbs {
		return nil, fmt.Errorf("%s has %d entries, max is %d", field, len(verbs), MaxSpinnerCustomVerbs)
	}
	for _, raw := range verbs {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if err := validateSpinnerVerb(field, trimmed); err != nil {
			return nil, err
		}
	}
	return dedupeTrimmed(verbs, MaxSpinnerCustomVerbs), nil
}

func validateSpinnerVerb(field, verb string) error {
	if count := utf8.RuneCountInString(verb); count > MaxSpinnerVerbRunes {
		return fmt.Errorf("%s entry %q is %d characters, max is %d", field, verb, count, MaxSpinnerVerbRunes)
	}
	for _, r := range verb {
		// A verb is one line of display text beside a spinning sprite. A
		// newline or control character does not shorten it — it breaks the
		// row it renders in.
		if unicode.IsControl(r) {
			return fmt.Errorf("%s entry %q must not contain control characters", field, verb)
		}
	}
	return nil
}

// sanitizeSpinnerCustomVerbs is the lenient load path: a hand-edited file
// with one bad verb keeps the rest of the list instead of losing all of
// it. Every drop is audible, because a tail that vanishes on load is
// otherwise indistinguishable from a save that never happened.
func sanitizeSpinnerCustomVerbs(field string, verbs []string) []string {
	if len(verbs) == 0 {
		return nil
	}
	kept := make([]string, 0, len(verbs))
	for _, raw := range verbs {
		verb := strings.TrimSpace(raw)
		if verb == "" {
			continue
		}
		if err := validateSpinnerVerb(field, verb); err != nil {
			log.Printf("settings: dropping invalid %s entry: %v", field, err)
			continue
		}
		kept = append(kept, verb)
	}
	return dedupeTrimmedLogged(field, kept, MaxSpinnerCustomVerbs)
}

// validateSpinnerDisabledAnimations is the strict path for the animation
// EXCLUSION list — the ids the user unchecked from the random pool.
//
// Exclusion rather than inclusion is the load-bearing choice: a sprite
// the user drops into <configDir>/spinners tomorrow, or one a later app
// version bundles, joins the pool automatically instead of being invisible
// until they go and tick it.
func validateSpinnerDisabledAnimations(field string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxSpinnerDisabledAnimations {
		return nil, fmt.Errorf("%s has %d entries, max is %d", field, len(ids), MaxSpinnerDisabledAnimations)
	}
	for _, raw := range ids {
		if err := validateSpinnerAnimationID(field, strings.TrimSpace(raw)); err != nil {
			return nil, err
		}
	}
	return dedupeTrimmed(ids, MaxSpinnerDisabledAnimations), nil
}

func validateSpinnerAnimationID(field, id string) error {
	if id == "" {
		return fmt.Errorf("%s contains an empty entry", field)
	}
	if !spinnerAnimationIDPattern.MatchString(id) {
		return fmt.Errorf(
			"%s entry %q is not a valid animation id (lowercase letters, digits and dashes, starting with a letter or digit, at most 64 characters)",
			field, id,
		)
	}
	return nil
}

// sanitizeSpinnerDisabledAnimations drops malformed ids on load instead
// of failing the file, mirroring sanitizeDisabledTools.
func sanitizeSpinnerDisabledAnimations(field string, ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	kept := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if err := validateSpinnerAnimationID(field, id); err != nil {
			log.Printf("settings: dropping invalid %s entry: %v", field, err)
			continue
		}
		kept = append(kept, id)
	}
	return dedupeTrimmedLogged(field, kept, MaxSpinnerDisabledAnimations)
}

// validateSpinnerCompactionAnimation is the strict path for the sprite
// pinned to auto-compaction. Three shapes are legal: "" (never chosen —
// the FRONTEND resolves it to its default sprite), "none" (chosen to be
// nothing), and an animation id.
//
// "" is stored as itself, never rewritten to an id: this package cannot
// see which sprites exist, so the default sprite's name belongs to the
// frontend catalog — and a stored id would make the settings UI's
// "Default" selection unrepresentable. An id that no longer resolves is
// likewise a FRONTEND fallback question, not a settings error.
func validateSpinnerCompactionAnimation(field, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if trimmed == SpinnerCompactionAnimationNone {
		return trimmed, nil
	}
	if err := validateSpinnerAnimationID(field, trimmed); err != nil {
		return "", err
	}
	return trimmed, nil
}

// sanitizeSpinnerCompactionAnimation falls back to "" (never chosen) on
// load rather than rejecting the file, mirroring sanitizeOption.
func sanitizeSpinnerCompactionAnimation(field, value string) string {
	sanitized, err := validateSpinnerCompactionAnimation(field, value)
	if err == nil {
		return sanitized
	}
	log.Printf("settings: invalid %s %q, using the default: %v", field, value, err)
	return ""
}
