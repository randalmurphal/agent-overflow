package claude

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"unicode"

	"agent-overflow/internal/provider"
)

// claudeEffortForModel resolves the `--effort` flag value for a session,
// dropping it entirely for a model the catalog says has no reasoning tiers
// (Haiku, per the CLI's own model list). Mirrors claudeFastMode, which gates
// the same way on the same catalog. See
// provider.ModelDeclaresNoReasoningEffort for why absence beats coercion here.
func claudeEffortForModel(model string, effort provider.ReasoningEffort) string {
	if provider.ModelDeclaresNoReasoningEffort(string(provider.Claude), model) {
		return ""
	}
	return claudeEffortFromOption(effort)
}

func claudeEffortFromOption(effort provider.ReasoningEffort) string {
	switch effort {
	case provider.EffortLow:
		return "low"
	case provider.EffortMedium:
		return "medium"
	case provider.EffortHigh:
		return "high"
	case provider.EffortXHigh:
		// 2.1.170's --effort accepts `xhigh` natively (a distinct tier between
		// high and max), so pass it through instead of collapsing to max — the
		// old workaround from when the flag's value set lacked xhigh.
		return "xhigh"
	case provider.EffortMax:
		return "max"
	default:
		return ""
	}
}

// claudeModelForContextWindow appends the `[1m]` context-tier marker the CLI
// reads the extended window from. It is the inverse of
// provider.TrimContextMarker, and trims first so a model id that already
// carries a marker (the CLI's own list bakes them into id strings) yields one
// marker rather than `model[1m][1m]`.
func claudeModelForContextWindow(model string, contextWindow int) string {
	model = provider.TrimContextMarker(model)
	if model != "" && contextWindow == provider.ClaudeExtendedContextWindow {
		return model + "[1m]"
	}
	return model
}

// ThinkingMode is the extended-thinking axis of a Claude launch config.
// Three states, and their asymmetry is the whole reason the axis needs its
// own type rather than a bare int:
//
//   - ThinkingDefault says nothing. The spawn passes no `--thinking` /
//     `--max-thinking-tokens` and the CLI picks per model (adaptive where
//     supported, its own budget otherwise).
//   - ThinkingOff spawns `--thinking disabled` and lives as
//     `set_max_thinking_tokens {max_thinking_tokens: 0}`.
//   - ThinkingBudget spawns `--max-thinking-tokens N` and lives as
//     `set_max_thinking_tokens {max_thinking_tokens: N}`.
//
// There is NO live form for the return to ThinkingDefault:
// `max_thinking_tokens: null` is accepted and does nothing (spike-verified
// 2.1.237), so only a respawn drops the flag. PlanLiveUpdate refuses that
// one direction and the restart converges it.
type ThinkingMode string

const (
	ThinkingDefault ThinkingMode = ""
	ThinkingOff     ThinkingMode = "off"
	ThinkingBudget  ThinkingMode = "budget"
)

// Thinking display values — the CLI's `--thinking-display` /
// `thinking_display` vocabulary. "summarized" puts thinking text on the
// wire; "omitted" keeps the thinking BLOCK (signature included, so
// multi-turn replay still works) and drops its text.
const (
	ThinkingDisplaySummarized = "summarized"
	ThinkingDisplayOmitted    = "omitted"
)

// ThinkingConfig is the NORMALIZED thinking axis of a Config. Normalized
// is load-bearing on two counts:
//
//   - Display is resolved for every mode but ThinkingOff, where it is
//     blanked. Agent Overflow has always spawned
//     `--thinking-display summarized` so newer models' `omitted` default
//     cannot silence the thinking pane, and that default has to be the
//     same value on the spawn path and the live path or a live apply would
//     "change" the display to something the process was already running.
//     A DISABLED session has no display at all — the CLI drops the flag —
//     so carrying one there would make two identical sessions compare
//     unequal and buy a control request that changes nothing.
//   - BudgetTokens is zero unless Mode is ThinkingBudget, for the same
//     reason. PlanLiveUpdate diffs this struct by value, so anything that
//     does not reach the process must not reach the struct either.
//
// Comparable by construction (two strings and an int) — LiveUpdate's
// emptiness check depends on it.
type ThinkingConfig struct {
	Mode         ThinkingMode
	BudgetTokens int
	Display      string
}

// claudeThinkingFromOptions normalizes the settings-shaped option bundle
// into the Config axis. A budget mode without a positive budget degrades to
// ThinkingDefault rather than spawning `--max-thinking-tokens 0`, which the
// CLI reads as DISABLED — the opposite of what an unfinished budget means.
// (Settings validation refuses that shape first; this is the second wall,
// for any caller that builds SessionOptions directly.)
func claudeThinkingFromOptions(thinking provider.ClaudeThinking) ThinkingConfig {
	cfg := ThinkingConfig{Display: normalizeThinkingDisplay(thinking.Display)}
	switch ThinkingMode(strings.TrimSpace(thinking.Mode)) {
	case ThinkingOff:
		cfg.Mode = ThinkingOff
		cfg.Display = ""
	case ThinkingBudget:
		if thinking.BudgetTokens > 0 {
			cfg.Mode = ThinkingBudget
			cfg.BudgetTokens = thinking.BudgetTokens
		}
	}
	return cfg
}

// normalizeThinkingDisplay resolves the display axis to a value the wire
// accepts. Everything that is not an explicit "omitted" lands on
// "summarized" — Agent Overflow's default, not the CLI's.
func normalizeThinkingDisplay(display string) string {
	if strings.TrimSpace(display) == ThinkingDisplayOmitted {
		return ThinkingDisplayOmitted
	}
	return ThinkingDisplaySummarized
}

// thinkingArgs renders the spawn form of the thinking axis.
//
// The three flags are read on the CLI's ordinary startup path (verified by
// inspection of the 2.1.237 bundle: `--thinking`/`MAX_THINKING_TOKENS`/
// `--max-thinking-tokens` resolve into the session's `thinkingConfig`
// before any print-vs-interactive branch), so they apply to the stream-json
// spawn exactly as they would to `claude -p`.
//
// Two CLI rules the shape below encodes:
//
//   - `--thinking enabled` means ADAPTIVE, not "use my budget". A fixed
//     budget has exactly one flag, `--max-thinking-tokens`, deprecation
//     notice in its help text notwithstanding.
//   - `--thinking` outranks `--max-thinking-tokens`, so the two are never
//     sent together.
//
// `--thinking-display` is dropped for a disabled session because the CLI
// ignores it there; every other case carries it, which is what keeps the
// pre-existing always-summarized spawn behavior intact for users who never
// touch the setting.
func thinkingArgs(thinking ThinkingConfig) []string {
	display := normalizeThinkingDisplay(thinking.Display)
	switch thinking.Mode {
	case ThinkingOff:
		return []string{"--thinking", "disabled"}
	case ThinkingBudget:
		return []string{
			"--max-thinking-tokens", strconv.Itoa(thinking.BudgetTokens),
			"--thinking-display", display,
		}
	default:
		return []string{"--thinking-display", display}
	}
}

func claudeFastMode(model string, fastMode bool) bool {
	return fastMode && provider.ModelSupportsCapability(string(provider.Claude), model, provider.ModelCapabilityFastMode)
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the Claude-specific launch Config. This is the single place where the
// effort flag, 1M-context model suffix, permission flags, and fast-mode
// settings are applied. Callers in app.go pass the result straight into
// claude.NewSession plus any ancillary wiring (Binary, EventLogger) that
// lives outside the session-options abstraction.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	model := opts.Model
	contextWindow := provider.ResolveContextWindowForModel(string(provider.Claude), model, opts.ContextWindow)
	autoCompactPercent := provider.AutoCompactPercentForContextTier(
		provider.ContextTierForModelWindow(string(provider.Claude), model, contextWindow),
		opts.AutoCompactStandardPercent,
		opts.AutoCompactExtendedPercent,
	)
	if autoCompactPercent == 0 {
		autoCompactPercent = opts.AutoCompactPercent
	}

	return Config{
		Model:                claudeModelForContextWindow(model, contextWindow),
		WorkDir:              opts.WorkDir,
		Resume:               opts.Resume,
		ResumeAt:             opts.ResumeAt,
		ForkSession:          opts.ForkSession,
		SystemPrompt:         opts.SystemPrompt,
		ReasoningEffort:      claudeEffortForModel(model, opts.ReasoningEffort),
		FastMode:             claudeFastMode(model, opts.FastMode),
		PermissionFlags:      claudePermissionFlags(opts.RuntimeMode),
		DisallowedTools:      mergeDisallowedTools(claudeDisallowedTools(opts.RuntimeMode), opts.DisabledTools),
		DisableTodoReminders: opts.DisableTodoReminders,
		BasePermissionMode:   claudeBasePermissionMode(opts.RuntimeMode),
		InteractionMode:      opts.Mode,
		AutoCompactPercent:   autoCompactPercent,
		ContextWindow:        contextWindow,
		Thinking:             claudeThinkingFromOptions(opts.ClaudeThinking),
		CrossSessionEnabled:  opts.ClaudeCrossSession.Enabled,
		CrossSessionInbound:  claudeCrossSessionInbound(opts.ClaudeCrossSession),
	}
}

// claudeCrossSessionInbound normalizes the inbound policy the
// `--settings` block will carry. It is never empty: OFF renders
// "refuse", and ON renders the chosen value.
//
// With the inbox ON an empty policy is filled in with "accept" rather
// than passed through — an empty key is the CLI's MODE-PARITY path,
// whose hold outcome discards the message with nothing on stdout, so
// "enabled" has to mean something explicit on the wire.
//
// With the inbox OFF the key is emitted anyway, as "refuse", and that is
// the load-bearing case. AO's own gate is the CLAUDE_CODE_HARBOR_KITE
// env override, but it is not the ONLY thing that can bind the inbox:
// `tengu_harbor_kite` is a remote GrowthBook flag that can late-bind it
// for a user AO never opted in, and `tengu_harbor_kite_mode_emit` (which
// has NO env override at all) turns on the permission-class attestation
// that mode parity reads. With both remote flags live and this key
// absent, a peer whose permission class matches would auto-deliver — a
// turn started in a thread whose user never enabled the feature. Off has
// to mean off regardless of remote flags, so the refusal is stated
// rather than assumed. It costs one settings key on a spawn that would
// usually carry the block anyway.
//
// settings.ClaudeCrossSession.EffectiveInbound applies the enabled half
// of this rule one layer up; this is the wall for any caller that builds
// SessionOptions directly.
func claudeCrossSessionInbound(cross provider.ClaudeCrossSession) string {
	if !cross.Enabled {
		return crossSessionInboundRefuse
	}
	if inbound := strings.TrimSpace(cross.Inbound); inbound != "" {
		return inbound
	}
	return crossSessionInboundAccept
}

// The inbound policies Agent Overflow will emit. "hold" is deliberately
// absent — see internal/settings/claudecrosssession.go for the spike
// evidence that a held message in a headless session is a silent drop.
const (
	crossSessionInboundAccept = "accept"
	crossSessionInboundRefuse = "refuse"
)

// claudeHarborKiteEnv is the CLI's one environment override for the
// GrowthBook experiment that binds the cross-session peer inbox
// (`function jh(){if(q.CLAUDE_CODE_HARBOR_KITE)return!0; ...}`, 2.1.237).
// It is a PARSED BOOLEAN on the CLI side, so "0" and "" read as off.
const claudeHarborKiteEnv = "CLAUDE_CODE_HARBOR_KITE"

// CrossSessionGateEnv is claudeHarborKiteEnv under its exported name, for
// internal/provider/claudetui — which assembles a full []string environment
// for its PTY launch rather than the override map Spawn takes, and so has to
// set the gate itself. One constant, so the two Claude transports cannot
// disagree about which variable binds the inbox.
const CrossSessionGateEnv = claudeHarborKiteEnv

// withClaudeCrossSessionEnv adds the inbox gate variable to a session's
// environment when the setting asks for it.
//
// It is the ONE `--settings`-adjacent axis that travels as a REAL
// subprocess variable rather than inside the block's `env` map, and the
// reason is ordering, not preference: the inbox binds during the CLI's
// setup phase, and while the block's env is reapplied over the inherited
// environment at init, nothing proves that reapplication precedes the
// bind. The plain variable is what the spike drove and what worked
// (2026-08-21, 2.1.237, /tmp/spike-xsession) — `system/init` then carries
// `messaging_socket_path` and `ListAgents` joins `tools[]`. Reserving the
// name in provider.ReservedEnvNames is what keeps a user's custom
// environment from setting it behind the setting's back.
//
// Absent when off — never "0". The variable's presence is the signal a
// reader of a process listing goes by, and an explicit off-value would
// invite the belief that AO ever turns the gate off for a CLI that had it
// on by experiment. "Absent" is not free, though: the child INHERITS the
// host environment, so absence has to be produced — see
// CrossSessionUnsetEnv, which every Claude spawn passes to UnsetEnv.
func withClaudeCrossSessionEnv(env map[string]string, cfg Config) map[string]string {
	if !cfg.CrossSessionEnabled {
		return env
	}
	merged := make(map[string]string, len(env)+1)
	for k, v := range env {
		merged[k] = v
	}
	merged[claudeHarborKiteEnv] = "1"
	return merged
}

// claudePeerSessionNameEnv is the CLI's environment spelling of the
// peer-visible name. AO always states the name as `--name` instead
// (peerSessionNameArgs), which is why it is a reserved name a user's custom
// environment may not set (provider.ReservedEnvNames).
const claudePeerSessionNameEnv = "CLAUDE_CODE_SESSION_NAME"

// CrossSessionUnsetEnv names the INHERITED variables a Claude spawn must
// remove for AO's cross-session setting to be the answer.
//
// Both are removed unconditionally, for two different reasons:
//
//   - CLAUDE_CODE_HARBOR_KITE is a parsed boolean the CLI reads as "the peer
//     inbox is on" before any settings layer is consulted. If AO itself was
//     launched from a shell carrying it — which is exactly what happens when
//     a developer exported it to try the feature, or when AO is started from
//     a Claude session — the child inherits it and JOINS the peer registry
//     while the setting says off. `crossSessionInbound: "refuse"` does not
//     cover this: refusing blocks DELIVERY, not discovery, so the thread
//     still advertises itself in every peer's ListAgents. Off has to mean
//     off. When the setting is ON the variable is set explicitly instead
//     (withClaudeCrossSessionEnv), and an override is removed before it is
//     re-added by BuildEnvironment, so listing it here is harmless.
//   - CLAUDE_CODE_SESSION_NAME is removed in both states because AO owns the
//     name: an inherited value would name a thread something the app never
//     chose, and the app's own `--name` is what the peer registry must show.
func CrossSessionUnsetEnv() []string {
	return []string{claudeHarborKiteEnv, claudePeerSessionNameEnv}
}

// peerSessionNameArgs renders the `--name` flag, which is what makes a
// thread addressable by a name other than its cwd basename.
//
// Emitted only when the inbox is on: `--name` also feeds the /resume
// picker and the terminal title, but a name AO derived for peer
// addressing has no business overriding those for a session no peer can
// reach. Sanitized here rather than trusted, so the flag AO passes is the
// name the CLI will actually register (SanitizePeerSessionName mirrors
// its normalizer); an empty result drops the flag, because the CLI reads
// an empty `--name` as absent and falls back to the basename anyway.
func peerSessionNameArgs(cfg Config) []string {
	if !cfg.CrossSessionEnabled {
		return nil
	}
	name := SanitizePeerSessionName(cfg.PeerSessionName)
	if !PeerSessionNameUsableAsArg(name) {
		if name != "" {
			// A leading dash. The CLI's own normalizer keeps it, so this is
			// a name it would happily register — but `--name -foo` never
			// reaches the registry, it re-parses as a flag. Dropped here so
			// the session falls back to the basename rather than running a
			// CLI invocation nobody wrote.
			log.Printf("claude: dropping peer session name %q — a leading dash parses as a CLI flag", name)
		}
		return nil
	}
	return []string{"--name", name}
}

// claudeReadOnlyDisallowedTools is the set of built-in tools removed outright
// from a read-only session. See claudeDisallowedTools for why this is separate
// from (and not redundant with) the `dontAsk` permission mode.
var claudeReadOnlyDisallowedTools = []string{"Write", "Edit", "NotebookEdit"}

// claudePermissionFlags maps a RuntimeMode to the raw CLI flag sequence the
// Claude SDK would send to headless `claude -p`. These are exactly the flags
// whose effect `set_permission_mode` can reproduce on a live session; the
// spawn-only tool removal lives in claudeDisallowedTools.
func claudePermissionFlags(mode provider.RuntimeMode) []string {
	permissionMode := claudeBasePermissionMode(mode)
	if permissionMode == "default" {
		return nil
	}
	flags := []string{"--permission-mode", permissionMode}
	if permissionMode == "bypassPermissions" {
		flags = append(flags, "--allow-dangerously-skip-permissions")
	}
	return flags
}

// claudeDisallowedTools returns the tools to strip from the session via
// `--disallowedTools`. Only read-only sessions strip anything.
//
// This is deliberately NOT folded into claudePermissionFlags. `dontAsk` alone
// is not enforcement: it converts an "ask" decision into a "deny" at the end
// of the permission pipeline, so an action that some settings source already
// *allows* never becomes an ask and is permitted. A `permissions.allow` entry
// for Write in the user's `~/.claude/settings.json` would therefore let a
// read-only session write files. `--disallowedTools` removes the tools from
// the session's toolset entirely, which no allow rule can reinstate.
//
// Verified against claude 2.1.219 — see docs/references/claude-wire.md
// §"Permission modes for read-only sessions" for the spike transcript.
//
// The split also carries a lifecycle meaning: tool removal is applied once at
// spawn and there is no control_request that can add or drop a tool mid-session.
// Keeping it on its own Config field is what makes any transition into or out
// of read-only fail the PlanLiveUpdate equality check and demand a restart,
// rather than half-applying as a bare set_permission_mode.
func claudeDisallowedTools(mode provider.RuntimeMode) []string {
	if mode != provider.RuntimeReadOnly {
		return nil
	}
	return append([]string(nil), claudeReadOnlyDisallowedTools...)
}

// mergeDisallowedTools unions the runtime-mode strips with the user's
// settings-level list. Mode entries come first and the user's follow in
// their configured order, deduped — the CLI takes one `--disallowedTools`
// flag per name, and a stable order is what keeps PlanLiveUpdate's
// equality check (and the argv it produces) from flapping between two
// spellings of the same set.
//
// Only the headless transport has a mode strip to union in; claudetui
// runs the settings list alone through SanitizeDisallowedTools, because
// Capabilities.EnforcesRuntimeMode is false there and every tier must stay
// inert by construction.
func mergeDisallowedTools(modeTools, settingsTools []string) []string {
	if len(settingsTools) == 0 {
		return modeTools
	}
	return SanitizeDisallowedTools(append(append([]string(nil), modeTools...), settingsTools...))
}

// SanitizeDisallowedTools is the argv boundary for `--disallowedTools`
// names: it trims, drops anything that is not ONE safe CLI argument (with
// a log line naming the reason), and dedupes while preserving order.
// Returns nil when nothing survives, so callers can compare against a nil
// Config field without a length special-case.
//
// Names are re-validated here rather than trusted. Settings validation is
// the primary gate and the one that reports the problem to the user, but
// this is where the value becomes argv: a name with whitespace becomes two
// flag arguments and a name starting with `-` is parsed as a FLAG, which
// turns a bad tool name into an unpredictable CLI invocation. Doing it here
// makes that structurally impossible for any caller — a future one that
// builds Config directly included — instead of leaving the guarantee in
// caller discipline.
//
// Exported because both Claude transports need exactly this pass and a
// second copy would be one edit away from disagreeing: headless folds it
// into mergeDisallowedTools, and claudetui.ConfigFromOptions calls it
// directly on the settings list.
func SanitizeDisallowedTools(tools []string) []string {
	if len(tools) == 0 {
		return nil
	}
	kept := make([]string, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		trimmed := strings.TrimSpace(tool)
		switch {
		case trimmed == "":
			continue
		case strings.ContainsFunc(trimmed, unicode.IsSpace):
			log.Printf("claude: dropping disallowed-tool name %q — a name containing whitespace is not one CLI argument", tool)
			continue
		case strings.HasPrefix(trimmed, "-"):
			log.Printf("claude: dropping disallowed-tool name %q — a leading dash parses as a CLI flag", tool)
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// claudeBasePermissionMode maps a RuntimeMode to Claude's permission-mode
// value. Unlike claudePermissionFlags, it returns "default" for supervised
// mode so sessions can restore the base mode via set_permission_mode after a
// plan turn.
//
// Every tier is enumerated; the trailing return is the unknown-value path,
// not a tier's mapping. Callers reach this through
// provider.NormalizeRuntimeMode, which never yields a non-canonical value, so
// the fallback is unreachable in practice — and it deliberately lands on
// supervised prompting rather than on any tier that would widen or seize up a
// session. TestClaudeBasePermissionModeCoversEveryRuntimeMode is what forces a
// new tier to be enumerated here rather than absorbed by that fallback.
func claudeBasePermissionMode(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeReadOnly:
		// "dontAsk" denies anything that would otherwise prompt, immediately
		// and without emitting a CanUseTool control_request. Reads and
		// non-mutating bash still run; the denial arrives as an is_error
		// tool_result the model reads and continues from, so an unattended
		// turn completes instead of stalling. Verified on claude 2.1.219.
		return "dontAsk"
	case provider.RuntimeApprovalRequired:
		return "default"
	case provider.RuntimeAutoAcceptEdits:
		return "acceptEdits"
	case provider.RuntimeAuto:
		return claudeAutoPermissionMode
	case provider.RuntimeFullAccess:
		return "bypassPermissions"
	}
	return "default"
}

// claudeAutoPermissionMode is the CLI permission mode behind
// provider.RuntimeAuto. Spike-verified against claude 2.1.219 (captures under
// the 2026-08-02 Claude spike in t3-improvements.md §Decision log):
//
//   - `--permission-mode auto` is accepted verbatim and echoed back on
//     `system/init` as `permissionMode:"auto"`. The statsig gates the CLI
//     carries for auto (`tengu_harbor_willow` / `tengu_moss_anchor`) guard
//     only the *implicit* default-to-auto path taken when no mode was
//     requested; an explicit flag never reaches them.
//   - `set_permission_mode` accepts "auto" on the stream-json control channel
//     — the accepted set is
//     {acceptEdits, auto, bypassPermissions, default, dontAsk, plan} and the
//     CLI replied `{"mode":"auto"}`. Only "bypassPermissions" is additionally
//     gated on how the process was spawned, so auto ↔ every other non-read-only
//     tier is a live transition (see PlanLiveUpdate).
//   - The decision path is: "acceptEdits would allow it" → allow; safe-tool
//     allowlist → allow; otherwise a two-stage Haiku classifier that allows or
//     DENIES. It falls closed when the classifier is unavailable, and it falls
//     back to a REAL interactive ask on safety_check / ask_rule /
//     plan_mode_floor / org_ask_ceiling / requires_user_interaction.
//
// That last bullet is why auto does not change any of AO's approval plumbing:
// the fallback ask arrives as an ordinary `can_use_tool` control_request on the
// same channel `--permission-prompt-tool stdio` already installs, and
// parse_control.go answers it like any other tier's approval.
//
// The one failure mode worth naming is the CLI's headless posture. When
// `toolPermissionContext.shouldAvoidPermissionPrompts` is set, auto's
// fallback-to-ask becomes a hard deny and a denial streak throws
// "Agent aborted: too many classifier denials in headless mode" instead of
// prompting. AO is never in that posture: disassembly of 2.1.219 shows the flag
// has exactly two producers, both nested-loop constructors — the `avoid_prompts`
// permission layer pushed when a tool-use context is forked for a subagent that
// does not share the parent's app state, and the subagent context builder keyed
// on `agentType` / `requestDialog`. The top-level stream-json session AO spawns
// gets neither, and it supplies a CanUseTool responder, so the denial-limit
// branch takes the "falling back to prompting" path.
const claudeAutoPermissionMode = "auto"

// Environment variable names AO renders into the `--settings` env block
// rather than the subprocess environment. The `flagSettings` source
// outranks `userSettings`, and Claude Code reapplies its own
// `~/.claude/settings.json` `env` block over inherited values during init
// (managedEnv.ts → applySafeConfigEnvironmentVariables), so a value put
// in the child's real environment can be silently overwritten while one
// put here cannot. All of them are pinned in
// `provider.ReservedEnvNames`, so a user's custom environment cannot
// fight the setting that renders them.
const (
	claudeAutoCompactPctEnv    = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
	claudeAutoCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	claudeMaxSpawnDepthEnv     = "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH"
	claudeMaxConcurrentSubEnv  = "CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS"
	claudeToolMemoryLimitEnv   = "CLAUDE_CODE_TOOL_MEMORY_LIMIT"
)

// cliInlineSettings is the JSON shape projected into the `--settings`
// CLI flag, which Claude Code applies as the `flagSettings` source. The
// struct field order is the JSON key order; keep `FastMode` first so
// the rendered output remains `{"fastMode":true}` for the fastMode-only
// case (matches existing test fixtures and avoids needless churn for
// readers comparing CLI invocations).
//
// Every field is `omitempty`, and that is the contract the whole block
// rests on: a zero value means SAY NOTHING, so the CLI's own resolution
// (its defaults, the user's settings.json, a `/output-style` switch)
// stands. Sending an explicit "default" instead would silently overrule
// choices the user made outside Agent Overflow.
type cliInlineSettings struct {
	FastMode bool `json:"fastMode,omitempty"`
	// CrossSessionInbound is the policy for peer messages another Claude
	// session on this machine addresses to this one (2.1.224+
	// `SendMessage` / `ListAgents`): Agent Overflow emits "accept" or
	// "refuse" and NEVER the CLI's third value, "hold". A delivered peer
	// message arrives as a user-role turn wrapped in
	// `<cross-session-message from="..." from-name="...">`.
	//
	// Empty is NOT "off", which is why Agent Overflow never leaves it
	// empty. With the key unset the CLI applies MODE PARITY — matching
	// permission-mode classes auto-deliver, a mismatched sender is held
	// for approval, and a sender that asserts no class is held only while
	// this session bypasses permission prompts (verified 2.1.237). Both
	// of the non-delivering outcomes — parity's hold and an explicit
	// "hold" — park the message with NO approval surface in a headless
	// session and no output whatsoever on stdout, and parity's DELIVERING
	// outcome would start a turn in a session whose user never enabled
	// the inbox at all (the gate is a remote GrowthBook flag AO does not
	// control). So the key is always explicit: "refuse" whenever the
	// feature is off, the chosen policy when it is on. See
	// internal/settings/claudecrosssession.go for the spike evidence.
	CrossSessionInbound string `json:"crossSessionInbound,omitempty"`
	// OutputStyle names one of the CLI's built-in output styles
	// ("Concise" / "Proactive" / "Explanatory" / "Learning"). There is no
	// CLI flag for this axis — the settings key is the only delivery
	// mechanism.
	OutputStyle string            `json:"outputStyle,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// inlineSettingsForCLI builds the JSON payload for the `--settings` CLI
// flag, combining every setting we want to project as `flagSettings`.
// Returns ("", false) when there's nothing to set so the flag can be
// omitted entirely — an empty `{}` block would be a no-op the reader of a
// process listing has to decode before dismissing.
func inlineSettingsForCLI(cfg Config) (string, bool) {
	settings := cliInlineSettings{
		FastMode:            cfg.FastMode,
		CrossSessionInbound: strings.TrimSpace(cfg.CrossSessionInbound),
		OutputStyle:         strings.TrimSpace(cfg.OutputStyle),
		Env:                 inlineSettingsEnvForCLI(cfg),
	}
	if !settings.FastMode &&
		settings.CrossSessionInbound == "" &&
		settings.OutputStyle == "" &&
		len(settings.Env) == 0 {
		return "", false
	}
	data, err := json.Marshal(settings)
	if err != nil {
		// `cliInlineSettings` is bools + strings + map[string]string with
		// no custom marshalers, so json.Marshal cannot fail in practice.
		// Panic loudly if it ever does — silently dropping the flag would
		// mean the user's autocompact / fastMode / output-style /
		// cross-session / subagent-limit settings are quietly ignored,
		// which violates the "errors must never silently fail" rule in
		// CLAUDE.md.
		panic(fmt.Sprintf("claude: cliInlineSettings marshal failed (unreachable): %v", err))
	}
	return string(data), true
}

// inlineSettingsEnvForCLI builds the `env` half of the block. Returns nil
// — not an empty map — when nothing is set, so `omitempty` can drop the
// key: several argv tests assert the fastMode-only rendering carries no
// `"env"` at all, and that assertion is what catches accidental leakage
// from unrelated state.
func inlineSettingsEnvForCLI(cfg Config) map[string]string {
	env := map[string]string{}
	if cfg.AutoCompactPercent > 0 {
		// Defense in depth: upstream `provider.normalizeAutoCompactPercent`
		// already clamps to ≤90, but `Config.AutoCompactPercent` is a
		// public field a future caller could populate directly without
		// going through `ConfigFromOptions`. The clamp here keeps the
		// rendered CLI flag honest regardless of how `cfg` was built.
		percent := cfg.AutoCompactPercent
		if percent > 90 {
			percent = 90
		}
		env[claudeAutoCompactPctEnv] = strconv.Itoa(percent)
		// Claude Code ≥2.1.201 only runs auto-compact when an explicit
		// auto-compact window resolves (env var, SDK option, statsig
		// experiment, or a hardcoded model table that covers only
		// claude-sonnet-5). Without one of those the pct override above
		// is dead code inside the CLI — the should-compact check bails
		// before reading it and the session runs to the hard blocking
		// limit uncompacted. Sending the thread's context window as
		// CLAUDE_CODE_AUTO_COMPACT_WINDOW opens that gate
		// deterministically; the CLI clamps the value to [100k, 1M] and
		// takes min(model max, value), so passing the resolved window
		// straight through is safe. Verified against claude 2.1.201
		// (spike: pct=1 never compacts without this var, compacts
		// immediately with it, on both 200k and 1m windows).
		if cfg.ContextWindow > 0 {
			env[claudeAutoCompactWindowEnv] = strconv.Itoa(cfg.ContextWindow)
		}
	}
	// Subagent fan-out caps. Both are `int({min:1, digitsOnly:true})` on
	// the CLI side (2.1.237), so ZERO is unsendable by construction and
	// means "let the binary decide" — the concurrency default is 20 and
	// the spawn-depth default comes from a remote experiment value, so
	// there is no fixed number AO could restate here anyway.
	if cfg.MaxSubagentSpawnDepth > 0 {
		env[claudeMaxSpawnDepthEnv] = strconv.Itoa(cfg.MaxSubagentSpawnDepth)
	}
	if cfg.MaxConcurrentSubagents > 0 {
		env[claudeMaxConcurrentSubEnv] = strconv.Itoa(cfg.MaxConcurrentSubagents)
	}
	// Tool-subprocess memory cap. Passed through verbatim (already
	// grammar-checked in internal/settings against the CLI's own regex
	// plus its falsy-word set): the CLI parses it itself, and normalizing
	// the spelling here would be a second grammar to keep in sync.
	//
	// LINUX-ONLY IN EFFECT: the CLI implements this by reading
	// /proc/self/cgroup and writing memory.max, so on macOS and native
	// Windows the variable is read and then does nothing. Sending it
	// anyway is deliberate — the WSL backend IS Linux, and suppressing it
	// per-host would make the same settings file behave differently
	// depending on which machine opened it.
	if limit := strings.TrimSpace(cfg.ToolMemoryLimit); limit != "" {
		env[claudeToolMemoryLimitEnv] = limit
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// mcpConfigForCLI renders cfg.MCPServers into the JSON string the
// Claude CLI's --mcp-config flag accepts. Returns ("", false) when no
// servers are configured so the flag can be omitted entirely.
//
// The CLI's --mcp-config requires each server spec to carry an
// explicit "type" discriminator ("http" | "sse" | "stdio"). The
// design MCP (and any future provider-agnostic MCP server we share
// with Codex) returns the canonical untagged shape `{"url": "..."}`
// because Codex's serde uses `untagged + deny_unknown_fields` and
// rejects an unknown "type" field on the StreamableHttp variant —
// the providers can't share a single tagged shape. Backfill "type":
// "http" here for any server that has a `url` and no explicit type;
// pass through any spec that already carries one. Stdio specs (those
// with a "command") get backfilled to "stdio".
func mcpConfigForCLI(cfg Config) (string, bool) {
	if len(cfg.MCPServers) == 0 {
		return "", false
	}
	servers := make(map[string]any, len(cfg.MCPServers))
	for name, spec := range cfg.MCPServers {
		servers[name] = withClaudeTransportType(spec)
	}
	payload := map[string]any{"mcpServers": servers}
	data, err := json.Marshal(payload)
	if err != nil {
		// Same reasoning as inlineSettingsForCLI: nested maps with no
		// custom marshalers can't fail in practice. Panic loudly to
		// satisfy the "errors must never silently fail" rule.
		panic(fmt.Sprintf("claude: mcpConfigForCLI marshal failed (unreachable): %v", err))
	}
	return string(data), true
}

// withClaudeTransportType returns a server spec with a "type" field
// inferred from the present fields. Specs that already carry a "type"
// pass through untouched. Specs without a recognisable shape
// (neither url nor command) pass through too — the CLI will surface
// a clear error in that case.
func withClaudeTransportType(spec any) any {
	specMap, ok := spec.(map[string]any)
	if !ok {
		return spec
	}
	if _, hasType := specMap["type"]; hasType {
		return specMap
	}
	var inferred string
	switch {
	case specMap["url"] != nil:
		inferred = "http"
	case specMap["command"] != nil:
		inferred = "stdio"
	default:
		return specMap
	}
	out := make(map[string]any, len(specMap)+1)
	for k, v := range specMap {
		out[k] = v
	}
	out["type"] = inferred
	return out
}
