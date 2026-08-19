package settings

import (
	"fmt"
	"log"
	"strings"
	"unicode"
)

// PromptOverride replaces a provider's built-in system prompt for the
// models it names. Zero value is inert: a disabled entry, or one with no
// models, never matches.
type PromptOverride struct {
	// Enabled gates the entry without the user having to delete it.
	Enabled bool `json:"enabled"`
	// Models holds normalized model slugs WITHOUT a context-tier marker
	// (`claude-opus-5`, not `claude-opus-5[1m]`). Matching normalizes both
	// sides, so a marker that slips in still compares equal.
	Models []string `json:"models"`
	// Prompt is the replacement text, placeholders unrendered.
	Prompt string `json:"prompt"`
}

// Limits on the override lists. They exist to keep a hand-edited or
// corrupt file from carrying an unbounded payload into a spawn argv; none
// of them constrains a plausible configuration.
const (
	// MaxPromptOverrides caps entries per provider.
	MaxPromptOverrides = 50
	// MaxPromptOverrideModels caps the model selection of one entry. Both
	// catalogs are well under this.
	MaxPromptOverrideModels = 100
	// MaxPromptOverrideLen caps one prompt's byte length. Claude's own
	// default body is ~10.5 KB; 64 KB leaves a wide margin while keeping
	// the value off the "accidentally pasted a repository" end.
	MaxPromptOverrideLen = 64_000
	// MaxPromptOverridesTotalLen caps the summed prompt bytes of one
	// provider's list. The per-entry cap alone is not a bound on what the
	// settings file costs: 50 entries × 64 KB is 3.2 MB per provider, and
	// GetSettings ships BOTH lists whole over the WebSocket on every read
	// — including to a LAN client — so the per-entry cap would let a
	// corrupt file turn every settings read into a 6.4 MB payload. 512 KB
	// is eight full-length prompts, or fifty ~10 KB ones; no plausible
	// configuration comes near it.
	MaxPromptOverridesTotalLen = 512_000
	// MaxDisabledTools caps a provider's disabled-tool list.
	MaxDisabledTools = 100
	// MaxDisabledToolLen caps one entry. Claude tool names and Codex
	// toggle ids are identifiers, not prose.
	MaxDisabledToolLen = 128
)

// PromptOverridesForProvider returns the override list for a provider.
//
// claude-tui shares the Claude list, the same way HiddenModelsForProvider
// does: it is the same binary, and the interactive TUI honors
// `--system-prompt-file <path>` exactly as headless does — the request's
// `system` array becomes [billing header, the TUI's own fixed identity
// line, the file's content] (spike-verified on claude 2.1.234 via a PTY +
// wire capture, docs/references/claude-wire.md §"System prompt assembly").
// AO passes the flag on the PTY launch, so an entry configured here is
// applied, not inert.
func (s Settings) PromptOverridesForProvider(provider string) []PromptOverride {
	switch provider {
	case "claude", "claude-tui":
		return s.ClaudePromptOverrides
	case "codex":
		return s.CodexPromptOverrides
	default:
		return nil
	}
}

// DisabledToolsForProvider returns the disabled-tool list for a provider.
// claude-tui shares the Claude list for the same reason
// PromptOverridesForProvider does: the interactive TUI honors repeated
// `--disallowedTools <name>` and the named tools' schemas are absent from
// its requests (same 2.1.234 spike).
func (s Settings) DisabledToolsForProvider(provider string) []string {
	switch provider {
	case "claude", "claude-tui":
		return s.ClaudeDisabledTools
	case "codex":
		return s.CodexDisabledTools
	default:
		return nil
	}
}

// TodoRemindersDisabledForProvider reports whether the provider's
// sessions should export CLAUDE_CODE_TODO_REMINDER_MODE=off.
// claude-tui shares the Claude answer for the same reason the two
// selectors above route it onto the Claude lists: one binary, one nudge
// producer. Codex has no equivalent mechanism, so it is always false
// there.
func (s Settings) TodoRemindersDisabledForProvider(provider string) bool {
	switch provider {
	case "claude", "claude-tui":
		return s.ClaudeTodoRemindersDisabled
	default:
		return false
	}
}

// validatePromptOverrides is the strict write-time pass: an out-of-bounds
// or malformed entry is a caller bug and fails the save. Returns the
// normalized list (trimmed models, deduped, empties dropped); a list with
// nothing left in it returns nil so sparse serialization omits the field.
func validatePromptOverrides(field string, entries []PromptOverride) ([]PromptOverride, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > MaxPromptOverrides {
		return nil, fmt.Errorf("%s has %d entries, max is %d", field, len(entries), MaxPromptOverrides)
	}
	out := make([]PromptOverride, 0, len(entries))
	total := 0
	for i, entry := range entries {
		if len(entry.Models) > MaxPromptOverrideModels {
			return nil, fmt.Errorf(
				"%s[%d].models has %d entries, max is %d",
				field, i, len(entry.Models), MaxPromptOverrideModels,
			)
		}
		// Bound the TRIMMED prompt: trimming is what gets stored, so
		// measuring the raw value would reject a legal prompt for
		// whitespace that never survives the save.
		entry.Prompt = strings.TrimSpace(entry.Prompt)
		if len(entry.Prompt) > MaxPromptOverrideLen {
			return nil, fmt.Errorf(
				"%s[%d].prompt is %d bytes, max is %d",
				field, i, len(entry.Prompt), MaxPromptOverrideLen,
			)
		}
		total += len(entry.Prompt)
		if total > MaxPromptOverridesTotalLen {
			return nil, fmt.Errorf(
				"%s prompts total more than %d bytes",
				field, MaxPromptOverridesTotalLen,
			)
		}
		entry.Models = dedupeTrimmed(entry.Models, MaxPromptOverrideModels)
		out = append(out, entry)
	}
	return out, nil
}

// sanitizePromptOverrides is the lenient load-time counterpart: a
// hand-edited file with one over-long prompt or an over-full list is
// trimmed rather than rejected, so a single bad entry cannot strand every
// other setting in the file.
func sanitizePromptOverrides(field string, entries []PromptOverride) []PromptOverride {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) > MaxPromptOverrides {
		log.Printf("settings: %s has %d entries, keeping the first %d", field, len(entries), MaxPromptOverrides)
		entries = entries[:MaxPromptOverrides]
	}
	out := make([]PromptOverride, 0, len(entries))
	total := 0
	for i, entry := range entries {
		entry.Models = dedupeTrimmedLogged(
			fmt.Sprintf("%s[%d].models", field, i), entry.Models, MaxPromptOverrideModels,
		)
		prompt := strings.TrimSpace(entry.Prompt)
		if len(prompt) > MaxPromptOverrideLen {
			log.Printf("settings: %s[%d].prompt is %d bytes, truncating to %d",
				field, i, len(prompt), MaxPromptOverrideLen)
		}
		entry.Prompt = truncateRuneSafe(prompt, MaxPromptOverrideLen)
		// The aggregate cap drops whole entries rather than shortening one:
		// half a system prompt is a worse thing to hand a model than an
		// entry that is visibly absent from the list.
		if total+len(entry.Prompt) > MaxPromptOverridesTotalLen {
			log.Printf("settings: %s prompts exceed %d bytes, keeping the first %d entries",
				field, MaxPromptOverridesTotalLen, len(out))
			break
		}
		total += len(entry.Prompt)
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validateDisabledTools enforces the shape a tool entry must have to reach
// an argv or a config key: a bounded, whitespace-free identifier that
// cannot be mistaken for a flag.
func validateDisabledTools(field string, tools []string) ([]string, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	if len(tools) > MaxDisabledTools {
		return nil, fmt.Errorf("%s has %d entries, max is %d", field, len(tools), MaxDisabledTools)
	}
	for _, raw := range tools {
		if err := validateDisabledTool(field, strings.TrimSpace(raw)); err != nil {
			return nil, err
		}
	}
	return dedupeTrimmed(tools, MaxDisabledTools), nil
}

func validateDisabledTool(field, tool string) error {
	if tool == "" {
		return fmt.Errorf("%s contains an empty entry", field)
	}
	if len(tool) > MaxDisabledToolLen {
		return fmt.Errorf("%s entry %q is %d bytes, max is %d", field, tool, len(tool), MaxDisabledToolLen)
	}
	if strings.HasPrefix(tool, "-") {
		// `--disallowedTools <name>` is one flag per name; a leading dash
		// would be parsed by the CLI as a flag of its own.
		return fmt.Errorf("%s entry %q must not start with -", field, tool)
	}
	for _, r := range tool {
		if unicode.IsSpace(r) {
			return fmt.Errorf("%s entry %q must not contain whitespace", field, tool)
		}
	}
	return nil
}

// sanitizeDisabledTools drops malformed entries on load instead of
// failing the file, mirroring sanitizeGitLabHosts.
func sanitizeDisabledTools(field string, tools []string) []string {
	if len(tools) == 0 {
		return nil
	}
	kept := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool := strings.TrimSpace(raw)
		if err := validateDisabledTool(field, tool); err != nil {
			log.Printf("settings: dropping invalid %s entry: %v", field, err)
			continue
		}
		kept = append(kept, tool)
	}
	return dedupeTrimmedLogged(field, kept, MaxDisabledTools)
}

// dedupeTrimmedLogged is dedupeTrimmed with the entry cap made audible.
// Silence is right for the lists dedupeTrimmed serves directly (hidden
// models are a derived selection the user re-makes by clicking); these two
// are hand-authorable lists whose tail disappearing on load is otherwise
// indistinguishable from a save that never happened.
//
// The condition is "arrived over the cap and came back exactly full",
// which is when dedupeTrimmed stopped early. It can only over-report when
// every unread value was a duplicate of one already kept — a log line, not
// a behavior, and the alternative is a second dedupe pass to phrase it.
func dedupeTrimmedLogged(field string, values []string, limit int) []string {
	kept := dedupeTrimmed(values, limit)
	if len(values) > limit && len(kept) == limit {
		log.Printf("settings: %s has %d entries, keeping the first %d", field, len(values), limit)
	}
	return kept
}
