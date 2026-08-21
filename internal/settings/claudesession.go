package settings

import (
	"fmt"
	"log"
	"regexp"
	"strings"
)

// Claude-only session axes delivered through the CLI's `--settings`
// flagSettings block (internal/provider/claude/options.go). None of them
// has a CLI flag; the settings key IS the delivery mechanism.
//
// Every axis here is SPAWN-TIME ONLY. The App stamps them onto the Claude
// Config in `spawnProviderSession`, alongside the binary path and the
// process environment, rather than onto `provider.SessionOptions` — which
// is what makes them "next sessions only": `claude.PlanLiveUpdate` diffs
// `ConfigFromOptions(prev)` against `ConfigFromOptions(next)`, neither of
// which carries these fields, so editing one never queues a restart on a
// running session. That matches the prompt-override / disabled-tool
// contract (docs/specs/prompt-tool-overrides.md).

// ClaudeOutputStyle values. The four names are Claude Code's built-in
// output styles (2.1.237): the CLI resolves `outputStyle` against its
// built-in table plus any user-authored styles in
// `~/.claude/output-styles`. Agent Overflow deliberately offers only the
// BUILT-INS: a user style is a file the CLI owns, and a settings value
// naming one that has been renamed or deleted silently falls back with
// no way for AO to tell the user why.
//
// The empty string means "say nothing" — the key is omitted from the
// `--settings` block entirely and the CLI's own resolution (its
// `default` style, the user's settings.json, or `/output-style`) stands.
const (
	ClaudeOutputStyleConcise     = "Concise"
	ClaudeOutputStyleProactive   = "Proactive"
	ClaudeOutputStyleExplanatory = "Explanatory"
	ClaudeOutputStyleLearning    = "Learning"
)

var allowedClaudeOutputStyles = map[string]bool{
	"":                           true,
	ClaudeOutputStyleConcise:     true,
	ClaudeOutputStyleProactive:   true,
	ClaudeOutputStyleExplanatory: true,
	ClaudeOutputStyleLearning:    true,
}

// ClaudeSubagentLimits caps Claude Code's subagent fan-out for the
// sessions Agent Overflow spawns. Both are delivered as environment
// variables inside the `--settings` env block, which outranks the
// subprocess environment (`flagSettings` > `userSettings`), and both are
// `int({min:1, digitsOnly:true})` on the CLI side — so ZERO means "send
// nothing and let the CLI decide" rather than "no subagents".
//
// The CLI's own defaults are not fixed numbers this package can restate:
// the concurrency default is 20, but the spawn-depth default comes from a
// remote experiment value with a build-time fallback. Leaving a field at
// zero is therefore the only honest way to express "whatever the binary
// decides".
type ClaudeSubagentLimits struct {
	// MaxSpawnDepth → CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH. How many
	// levels of subagent-spawning-a-subagent the session permits.
	MaxSpawnDepth int `json:"maxSpawnDepth,omitempty"`
	// MaxConcurrent → CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS. How many
	// subagents may hold a concurrency slot at once.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
}

// MaxClaudeSubagentLimit bounds what the settings file may hold. The CLI
// enforces only `min: 1`, so an absurd value is accepted and then paid for
// in real processes; 512 is far above any plausible fan-out and far below
// the point where a typo forks a machine to a halt.
const MaxClaudeSubagentLimit = 512

// claudeToolMemoryLimitSize mirrors the CLI's own accepted grammar for
// CLAUDE_CODE_TOOL_MEMORY_LIMIT (2.1.237):
// `/^(\d+(?:\.\d+)?)\s*([kmgt]?)(?:i?b)?$/i`. A value that does not match
// — and is not one of the disable words below — is ignored by the CLI,
// which is exactly the silent-no-op a refused save exists to prevent.
var claudeToolMemoryLimitSize = regexp.MustCompile(`^(?i)\d+(\.\d+)?\s*[kmgt]?(i?b)?$`)

// claudeToolMemoryLimitDisableWords are the values the CLI reads as "do
// not install a cgroup limit at all": its falsy-string set plus the
// explicit "none".
var claudeToolMemoryLimitDisableWords = map[string]bool{
	"0": true, "false": true, "no": true, "off": true, "none": true,
}

// MaxClaudeToolMemoryLimitLen bounds the stored string. The grammar above
// is already tight; this only keeps a pathological paste out of the file.
const MaxClaudeToolMemoryLimitLen = 32

// validateClaudeOutputStyle is the strict path: an unrecognised style is
// a refused save, because a style the CLI never heard of is silently
// ignored and the user would have no way to tell "saved" from "applied".
func validateClaudeOutputStyle(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !allowedClaudeOutputStyles[value] {
		return "", fmt.Errorf("%s must be one of %q, %q, %q, %q (or empty for the CLI default)",
			field, ClaudeOutputStyleConcise, ClaudeOutputStyleProactive,
			ClaudeOutputStyleExplanatory, ClaudeOutputStyleLearning)
	}
	return value, nil
}

func validateClaudeSubagentLimits(field string, value ClaudeSubagentLimits) (ClaudeSubagentLimits, error) {
	if err := validateClaudeSubagentLimit(field+".maxSpawnDepth", value.MaxSpawnDepth); err != nil {
		return ClaudeSubagentLimits{}, err
	}
	if err := validateClaudeSubagentLimit(field+".maxConcurrent", value.MaxConcurrent); err != nil {
		return ClaudeSubagentLimits{}, err
	}
	return value, nil
}

func validateClaudeSubagentLimit(field string, value int) error {
	if value == 0 {
		return nil
	}
	if value < 1 || value > MaxClaudeSubagentLimit {
		return fmt.Errorf("%s must be between 1 and %d (0 uses the CLI default)", field, MaxClaudeSubagentLimit)
	}
	return nil
}

func validateClaudeToolMemoryLimit(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > MaxClaudeToolMemoryLimitLen {
		return "", fmt.Errorf("%s must be at most %d characters", field, MaxClaudeToolMemoryLimitLen)
	}
	if claudeToolMemoryLimitDisableWords[strings.ToLower(value)] {
		return value, nil
	}
	if !claudeToolMemoryLimitSize.MatchString(value) {
		return "", fmt.Errorf(`%s must be a size like "4G", "512m" or "2GiB", or "none" to disable the limit`, field)
	}
	return value, nil
}

// The lenient load-time halves. A settings file can outlive the app
// version that wrote it and the CLI version it was written for, so a
// value this build no longer recognises degrades to the CLI default
// rather than making the whole file unloadable — audibly, because a value
// that vanishes on load is otherwise indistinguishable from a save that
// never happened.
func sanitizeClaudeOutputStyle(field, value string) string {
	value = strings.TrimSpace(value)
	if allowedClaudeOutputStyles[value] {
		return value
	}
	log.Printf("settings: %s: dropping unknown output style %q", field, value)
	return ""
}

func sanitizeClaudeSubagentLimits(field string, value ClaudeSubagentLimits) ClaudeSubagentLimits {
	value.MaxSpawnDepth = sanitizeClaudeSubagentLimit(field+".maxSpawnDepth", value.MaxSpawnDepth)
	value.MaxConcurrent = sanitizeClaudeSubagentLimit(field+".maxConcurrent", value.MaxConcurrent)
	return value
}

func sanitizeClaudeSubagentLimit(field string, value int) int {
	if value == 0 || (value >= 1 && value <= MaxClaudeSubagentLimit) {
		return value
	}
	log.Printf("settings: %s: dropping out-of-range subagent limit %d", field, value)
	return 0
}

func sanitizeClaudeToolMemoryLimit(field, value string) string {
	sanitized, err := validateClaudeToolMemoryLimit(field, value)
	if err != nil {
		log.Printf("settings: %s: dropping unusable tool memory limit %q: %v", field, value, err)
		return ""
	}
	return sanitized
}

// ClaudeSessionAxes is the whole spawn-time bundle for one provider, read
// once so a save landing mid-spawn cannot produce a session whose axes
// came from two versions of the file. Non-Claude providers get the zero
// value — every field is Claude-CLI-specific.
//
// claude-tui is deliberately EXCLUDED, unlike the prompt-override and
// disabled-tool selectors that route it onto Claude's lists: those ride
// CLI flags the PTY launch already passes, while every axis here rides
// the `--settings` block, which `internal/provider/claudetui` does not
// send. The interactive TUI also already honors the user's own
// settings.json and has `/output-style`, so the missing coverage is a gap
// in AO's control surface, not in the user's.
type ClaudeSessionAxes struct {
	OutputStyle     string
	SubagentLimits  ClaudeSubagentLimits
	ToolMemoryLimit string
}

// ClaudeSessionAxesForProvider returns the spawn-time Claude axes for a
// provider name.
func (s Settings) ClaudeSessionAxesForProvider(providerName string) ClaudeSessionAxes {
	if strings.TrimSpace(providerName) != "claude" {
		return ClaudeSessionAxes{}
	}
	return ClaudeSessionAxes{
		OutputStyle:     s.ClaudeOutputStyle,
		SubagentLimits:  s.ClaudeSubagentLimits,
		ToolMemoryLimit: s.ClaudeToolMemoryLimit,
	}
}

// -- Extended thinking -------------------------------------------------
//
// The ONE axis in this file that is not spawn-only. Claude Code carries
// both a spawn form (`--thinking` / `--max-thinking-tokens` /
// `--thinking-display`, all read on the ordinary startup path — not the
// `--settings` block) and a live form (the `set_max_thinking_tokens`
// control_request), so a change here reaches RUNNING headless sessions
// too. `ClaudeThinking` is therefore deliberately NOT part of
// `ClaudeSessionAxes`: that bundle's whole contract is "next sessions
// only", and this one travels on `provider.SessionOptions` so
// `claude.PlanLiveUpdate` can diff it. See
// `app_session_prompt_override.go` for the spawn/reconcile pair and
// `internal/provider/claude/live_update.go` for the wire rules.

// ClaudeThinkingMode values. The empty string is the default, following
// this file's rule that a zero value means "say nothing and let Claude
// Code decide" — here that is the CLI's own per-model choice (adaptive on
// models that support it, its built-in budget otherwise).
//
// The asymmetry worth knowing before reading the validator: "off" and
// "budget" both have a live wire form, and the return to "default" does
// NOT. `max_thinking_tokens: null` is accepted and is a documented NO-OP
// (spike-verified 2.1.237), so there is no reset request — only a respawn
// without the flag restores the CLI's own choice.
const (
	ClaudeThinkingModeDefault = ""
	ClaudeThinkingModeOff     = "off"
	ClaudeThinkingModeBudget  = "budget"
)

var allowedClaudeThinkingModes = map[string]bool{
	ClaudeThinkingModeDefault: true,
	ClaudeThinkingModeOff:     true,
	ClaudeThinkingModeBudget:  true,
}

// ClaudeThinkingDisplay values — the CLI's `thinking_display` vocabulary.
// "summarized" puts thinking text on the wire; "omitted" keeps the
// thinking BLOCK (signature and all, so multi-turn replay still works)
// but drops its text.
//
// Empty means "Agent Overflow's default", which is NOT the CLI's: the
// spawn path has always passed `--thinking-display summarized` so newer
// models' `omitted` default cannot silence the thinking pane. Picking
// "summarized" explicitly is therefore the same behavior as leaving this
// unset — the option exists so the UI can state what unset does rather
// than leaving the user to guess.
const (
	ClaudeThinkingDisplayDefault    = ""
	ClaudeThinkingDisplaySummarized = "summarized"
	ClaudeThinkingDisplayOmitted    = "omitted"
)

var allowedClaudeThinkingDisplays = map[string]bool{
	ClaudeThinkingDisplayDefault:    true,
	ClaudeThinkingDisplaySummarized: true,
	ClaudeThinkingDisplayOmitted:    true,
}

// The budget bounds. The CLI itself enforces NEITHER — its control-request
// handler takes any integer and its option schema is a bare number — so
// these are Agent Overflow's, and both exist to keep a typo from producing
// a session that fails every request:
//
//   - 1024 is the Anthropic API's floor for `thinking.budget_tokens`; below
//     it the request is rejected, and the rejection would arrive per turn
//     with nothing pointing back at this setting.
//   - 128000 is far above any model's real thinking allowance (Sonnet 4.5's
//     own default is 31999) and far below the point where a stray extra
//     digit silently buys a turn nobody wanted.
const (
	MinClaudeThinkingBudgetTokens = 1024
	MaxClaudeThinkingBudgetTokens = 128000
)

// ClaudeThinking is the user's extended-thinking preference for the
// headless Claude sessions Agent Overflow spawns.
//
// BudgetTokens is meaningful ONLY with Mode == budget, and the validators
// enforce that rather than trusting it: a stored budget beside mode "off"
// would be a value that reads as configuration and behaves as nothing.
//
// Mode and Display are two independent axes on the CLI side and stay
// independent here — `thinking_display` alone is an accepted live request
// (spike-verified 2.1.237) — with one exception the wire imposes: a
// DISABLED session drops display entirely, so Display beside Mode "off" is
// inert rather than wrong.
type ClaudeThinking struct {
	Mode         string `json:"mode,omitempty"`
	BudgetTokens int    `json:"budgetTokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

// validateClaudeThinking is the strict path. A budget mode without a
// usable budget is refused rather than coerced: silently demoting it to
// "Claude's default" would report a save the user never made.
func validateClaudeThinking(field string, value ClaudeThinking) (ClaudeThinking, error) {
	value.Mode = strings.TrimSpace(value.Mode)
	value.Display = strings.TrimSpace(value.Display)
	if !allowedClaudeThinkingModes[value.Mode] {
		return ClaudeThinking{}, fmt.Errorf("%s.mode must be %q, %q or empty for Claude Code's own choice",
			field, ClaudeThinkingModeOff, ClaudeThinkingModeBudget)
	}
	if !allowedClaudeThinkingDisplays[value.Display] {
		return ClaudeThinking{}, fmt.Errorf("%s.display must be %q, %q or empty",
			field, ClaudeThinkingDisplaySummarized, ClaudeThinkingDisplayOmitted)
	}
	if value.Mode != ClaudeThinkingModeBudget {
		// Not an error: the UI legitimately holds a budget while the user
		// flips to another mode. Dropping it is what keeps the stored
		// shape from claiming a budget that nothing reads.
		value.BudgetTokens = 0
		return value, nil
	}
	if value.BudgetTokens < MinClaudeThinkingBudgetTokens || value.BudgetTokens > MaxClaudeThinkingBudgetTokens {
		return ClaudeThinking{}, fmt.Errorf("%s.budgetTokens must be between %d and %d",
			field, MinClaudeThinkingBudgetTokens, MaxClaudeThinkingBudgetTokens)
	}
	return value, nil
}

// sanitizeClaudeThinking is the lenient load-time half. A settings file can
// outlive both the app version that wrote it and the CLI version it was
// written for, so an unusable value degrades to "Claude Code decides"
// audibly instead of making the whole file unloadable.
func sanitizeClaudeThinking(field string, value ClaudeThinking) ClaudeThinking {
	sanitized, err := validateClaudeThinking(field, value)
	if err != nil {
		log.Printf("settings: %s: dropping unusable thinking config %+v: %v", field, value, err)
		return ClaudeThinking{}
	}
	return sanitized
}

// ClaudeThinkingForProvider returns the thinking preference for a provider
// name. Headless `claude` only, for the same reason
// ClaudeSessionAxesForProvider excludes claude-tui — but a DIFFERENT one:
// the TUI does accept the spawn flags (internal/provider/claudetui/launch.go
// already passes `--thinking-display`), it just has no control-request
// channel, so honoring this there would offer a live setting that is
// silently spawn-only on one of the two Claude transports. Wiring it up is
// a deliberate follow-up, not an oversight.
func (s Settings) ClaudeThinkingForProvider(providerName string) ClaudeThinking {
	if strings.TrimSpace(providerName) != "claude" {
		return ClaudeThinking{}
	}
	return s.ClaudeThinking
}
