package provider

// ReasoningEffort identifies a provider reasoning tier. Claude exposes
// low/medium/high plus model-specific top tiers; Codex exposes its native
// none/minimal/low/medium/high/xhigh/max/ultra set through app-server model
// metadata.
type ReasoningEffort string

const (
	EffortNone    ReasoningEffort = "none"
	EffortMinimal ReasoningEffort = "minimal"
	EffortLow     ReasoningEffort = "low"
	EffortMedium  ReasoningEffort = "medium"
	EffortHigh    ReasoningEffort = "high"
	EffortXHigh   ReasoningEffort = "xhigh"
	EffortMax     ReasoningEffort = "max"
	EffortUltra   ReasoningEffort = "ultra"
)

// AllReasoningEfforts is the canonical ordered list used by validation and
// per-provider mappers. Model metadata controls which subset appears in the UI.
var AllReasoningEfforts = []ReasoningEffort{
	EffortNone,
	EffortMinimal,
	EffortLow,
	EffortMedium,
	EffortHigh,
	EffortXHigh,
	EffortMax,
	EffortUltra,
}

// DefaultReasoningEffort is the seed value for new threads when a caller
// doesn't pass an explicit choice.
const DefaultReasoningEffort = EffortHigh

// NormalizeReasoningEffort coerces unknown strings to DefaultReasoningEffort
// so a stale client cannot plant a value outside the enum.
func NormalizeReasoningEffort(effort string) ReasoningEffort {
	switch ReasoningEffort(effort) {
	case EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax, EffortUltra:
		return ReasoningEffort(effort)
	default:
		return DefaultReasoningEffort
	}
}

// InteractionMode identifies the shape of the agent's turn (chat vs.
// plan-first vs. design-artifact vs. deliberation). Orthogonal to
// RuntimeMode.
type InteractionMode string

const (
	ModeChat       InteractionMode = "chat"
	ModePlan       InteractionMode = "plan"
	ModeDesign     InteractionMode = "design"
	ModeDiscussion InteractionMode = "discussion"
)

// AllInteractionModes is the canonical ordered list. Kept in sync with
// the CHECK constraint on threads.mode.
var AllInteractionModes = []InteractionMode{
	ModeChat,
	ModePlan,
	ModeDesign,
	ModeDiscussion,
}

// DefaultInteractionMode is the seed value for new threads.
const DefaultInteractionMode = ModeChat

// NormalizeInteractionMode coerces unknown strings to
// DefaultInteractionMode. Keeps the rest of the app from having to
// replicate the enum check.
func NormalizeInteractionMode(mode string) InteractionMode {
	switch InteractionMode(mode) {
	case ModeChat, ModePlan, ModeDesign, ModeDiscussion:
		return InteractionMode(mode)
	default:
		return DefaultInteractionMode
	}
}

// ClaudeThinking mirrors settings.ClaudeThinking on the provider side of
// the boundary. Two types rather than one because internal/settings must
// not import this package (see internal/settings/AGENTS.md §Anti-patterns);
// the app layer converts between them at its single stamping site.
//
// Mode is "" (leave it to the CLI), "off", or "budget"; BudgetTokens is
// meaningful only with "budget"; Display is "", "summarized" or "omitted".
// Validation lives in internal/settings — this is a transport shape, and
// internal/provider/claude re-normalizes what it renders anyway.
type ClaudeThinking struct {
	Mode         string
	BudgetTokens int
	Display      string
}

// ClaudeCrossSession mirrors settings.ClaudeCrossSession on the provider
// side of the boundary, for the same reason ClaudeThinking is duplicated:
// internal/settings must not import this package.
//
// Enabled opens Claude Code's machine-wide peer inbox for the session
// (the CLAUDE_CODE_HARBOR_KITE gate plus a `--name`); Inbound is the
// already-resolved delivery policy for the `--settings` block — "accept"
// or "refuse", never "hold" and never empty while Enabled. Resolution
// lives in settings.ClaudeCrossSession.EffectiveInbound; this is a
// transport shape.
type ClaudeCrossSession struct {
	Enabled bool
	Inbound string
}

// SessionOptions is the provider-agnostic bundle a Thread projects onto.
// The per-provider packages (claude, codex) each translate it into their
// own Config. Keeps the translation out of app.go and makes it testable
// without spawning a subprocess.
type SessionOptions struct {
	// Provider is the provider identifier (claude/codex). Copied from
	// the thread so the per-provider translator can assert it's looking
	// at the right bundle.
	Provider string

	Model           string
	WorkDir         string
	ReasoningEffort ReasoningEffort
	FastMode        bool

	// FastModeTierID is Codex-only: the wire `serviceTier` id to send while
	// FastMode is on, taken from the model's own `model/list` entry
	// (ModelInfo.FastModeTier). Empty means "no catalog opinion" and the Codex
	// translator falls back to the legacy `priority` id, so an unresolved model
	// behaves exactly as it did before the tier became wire-driven. Claude
	// ignores it — its fast mode is a spawn flag with no tier.
	//
	// SessionOptionsFromThread cannot fill this in: the thread row persists
	// only the bool, and resolving the tier needs the live catalog, which lives
	// above this package. The app layer stamps it at its single construction
	// site (buildSessionOptions) so the spawn path and the live-update
	// reconciler always agree on the value.
	FastModeTierID string

	ContextWindow              int
	AutoCompactPercent         int
	AutoCompactStandardPercent int
	AutoCompactExtendedPercent int
	Mode                       InteractionMode
	RuntimeMode                RuntimeMode
	SystemPrompt               string

	// SystemPromptOverrideSource is the STORED (unrendered) text of the
	// settings-level prompt override that produced SystemPrompt, or "" when
	// no settings override is in play (no match, or a feature owns the
	// prompt). App-layer provenance: it is deliberately mapped into NO
	// provider Config, so it never participates in a live-vs-restart diff.
	//
	// It exists so the reconcile path can answer "did the user edit the
	// override?" without rendering one. Comparing rendered prompts cannot
	// answer that — `{{GIT_BLOCK}}` re-renders on every commit, so a
	// rendered-vs-rendered diff would report a change every time the
	// workspace moved and never stop reconciling. Comparing the stored text
	// changes only when Settings does. See app_session_prompt_override.go.
	SystemPromptOverrideSource string

	// DisabledTools names built-in tools the session must not be given.
	// PROVIDER-INTERPRETED, and the two providers read it differently:
	// Claude takes RAW TOOL NAMES ("Workflow") and unions them into
	// `--disallowedTools`; Codex takes CURATED TOGGLE IDS ("web_search")
	// because it has no deny list, and each id maps to per-thread config
	// keys. Spawn-only on both sides — a change here forces a restart,
	// which is what each provider's PlanLiveUpdate reports by comparing
	// the Config field this lands on.
	//
	// SessionOptionsFromThread cannot fill this in: it is a settings-level
	// preference, not thread state, so the app layer stamps it at its
	// single construction site (buildSessionOptions) alongside SystemPrompt.
	DisabledTools []string

	// DisableTodoReminders turns off Claude Code's periodic "track your
	// work with the todo tools" nudges by exporting
	// CLAUDE_CODE_TODO_REMINDER_MODE=off into the session's environment
	// (as a default a user's custom env can still override). Claude-family
	// only — Codex ignores it. Spawn-only like DisabledTools, and stamped
	// from Settings the same way: the app layer sets it in
	// applySettingsOwnedAxes and the reconcile path pins it to the live
	// session's launch value (reconcileSettingsOwnedAxes).
	DisableTodoReminders bool

	// ClaudeThinking is the settings-owned extended-thinking preference.
	// Claude-family only; Codex ignores it (its reasoning axis is
	// ReasoningEffort).
	//
	// Unlike DisabledTools and DisableTodoReminders — the other two
	// settings-owned axes stamped here — this one is NOT spawn-only: the
	// Claude CLI's `set_max_thinking_tokens` control_request applies it to
	// a running session. That is exactly why it lives on SessionOptions
	// rather than on claude.Config: the live-config reconciler diffs
	// ConfigFromOptions(prev) against ConfigFromOptions(next), so an axis
	// that must converge on a live session has to travel through here.
	//
	// The app layer stamps it at the same two places the prompt override
	// uses (applySettingsOwnedAxes on spawn, reconcileSettingsOwnedAxes on
	// reconcile).
	ClaudeThinking ClaudeThinking

	// ClaudeCrossSession is the settings-owned peer-inbox preference:
	// whether other Claude sessions on this machine may discover and
	// message this thread, and what happens to a message that arrives.
	// Claude-family only; Codex ignores it.
	//
	// SPAWN-ONLY, and it travels here rather than being stamped straight
	// onto claude.Config (the way OutputStyle and the subagent caps are)
	// precisely BECAUSE it is spawn-only: the inbox binds once during the
	// CLI's setup and no control_request rebinds it, so a change has to
	// converge by RESTART. Riding SessionOptions is what puts it in front
	// of claude.PlanLiveUpdate, whose trailing DeepEqual then reports it
	// as a deferred restart — the same route DisabledTools and
	// DisableTodoReminders take. An axis stamped outside ConfigFromOptions
	// would instead be invisible to the reconciler and silently never
	// converge on a running thread.
	//
	// Stamped at the same two places the prompt override uses
	// (applySettingsOwnedAxes on spawn, reconcileSettingsOwnedAxes on
	// reconcile), and re-read on BOTH — pinning it would be the bug.
	ClaudeCrossSession ClaudeCrossSession

	// ClaudePeerSessionName is the name other Claude sessions see for this
	// thread in `ListAgents`, derived from the thread by the app layer.
	//
	// Deliberately NOT part of ClaudeCrossSession, and deliberately NOT
	// read by ConfigFromOptions: the name is LIVE-CHANGEABLE (`/rename`
	// costs no model turn), so a title change must never queue a restart.
	// claude.Config carries it as a spawn-stamped field for exactly the
	// same reason OutputStyle is spawn-stamped — invisibility to the
	// DeepEqual is the property being bought, in the opposite direction
	// from ClaudeCrossSession above.
	ClaudePeerSessionName string

	// Resume carries the provider's resume reference. Claude: prior
	// session uuid. Codex: prior thread id. Empty for brand-new starts.
	Resume string

	// ResumeAt carries a provider-native transcript/message UUID inside
	// Resume. Claude uses this with --resume-session-at to force a resumed
	// process onto AO's canonical settled leaf. Empty keeps provider default
	// resume behaviour.
	ResumeAt string

	// ForkSession signals that the next start should branch off Resume
	// instead of treating it as the live session. Claude-specific; Codex
	// handles forking via a separate app-server request.
	ForkSession bool
}

// ThreadView is the narrow interface a Thread must satisfy to become
// SessionOptions. Defined here (not on store.Thread) so that
// internal/provider/ stays free of any dependency on internal/store/ —
// the store package attaches the Get* methods to satisfy the interface.
type ThreadView interface {
	GetProvider() string
	GetModel() string
	GetWorkspacePath() string
	GetReasoningEffort() string
	GetFastMode() bool
	GetContextWindow() int
	GetAutoCompactStandardPercent() int
	GetAutoCompactExtendedPercent() int
	GetMode() string
	GetRuntimeMode() string
	GetSessionRef() string
	GetPendingForkRef() string
}

// AutoCompactDefaults carries the live per-provider compact thresholds
// the user has configured in Settings. Passed into SessionOptionsFromThread
// so the resolved percent reflects the current setting on every session
// start — not a value frozen into the thread row at creation time. A
// per-thread override (Thread.AutoCompactStandardPercent /
// AutoCompactExtendedPercent > 0) still wins so the chat-meter override
// flow keeps working.
type AutoCompactDefaults struct {
	StandardPercent int
	ExtendedPercent int
}

// SessionOptionsFromThread assembles SessionOptions from a ThreadView +
// the caller-composed system prompt (which itself may depend on mode —
// that composition is the caller's job, because it reaches into design/
// and discussion/ which the provider package doesn't see).
//
// forkSession is an explicit arg instead of being read from the thread
// because it's a per-start decision (the fork is consumed on the next
// start; it isn't durable state).
//
// defaults holds the live per-provider compact thresholds from Settings;
// any zero per-thread percent falls back to the matching default so a
// settings change on the slider applies on the next turn without
// rewriting every thread row.
func SessionOptionsFromThread(
	t ThreadView,
	defaults AutoCompactDefaults,
	systemPrompt string,
	forkSession bool,
) SessionOptions {
	resume := t.GetSessionRef()
	if forkSession && t.GetPendingForkRef() != "" {
		// Claude's pending-fork flow: the next start should resume
		// PendingForkRef (not the live SessionRef), and the act of
		// starting consumes the pending ref.
		resume = t.GetPendingForkRef()
	}
	standardPercent := t.GetAutoCompactStandardPercent()
	if standardPercent <= 0 {
		standardPercent = defaults.StandardPercent
	}
	extendedPercent := t.GetAutoCompactExtendedPercent()
	if extendedPercent <= 0 {
		extendedPercent = defaults.ExtendedPercent
	}
	return SessionOptions{
		Provider: t.GetProvider(),
		Model:    t.GetModel(),
		WorkDir:  t.GetWorkspacePath(),
		ReasoningEffort: CoerceReasoningEffortForModel(
			t.GetProvider(),
			t.GetModel(),
			NormalizeReasoningEffort(t.GetReasoningEffort()),
		),
		FastMode:      t.GetFastMode(),
		ContextWindow: t.GetContextWindow(),
		AutoCompactPercent: AutoCompactPercentForContextTier(
			ContextTierForModelWindow(t.GetProvider(), t.GetModel(), t.GetContextWindow()),
			standardPercent,
			extendedPercent,
		),
		AutoCompactStandardPercent: standardPercent,
		AutoCompactExtendedPercent: extendedPercent,
		Mode:                       NormalizeInteractionMode(t.GetMode()),
		RuntimeMode:                NormalizeRuntimeMode(t.GetRuntimeMode()),
		SystemPrompt:               systemPrompt,
		Resume:                     resume,
		ForkSession:                forkSession,
	}
}

// AutoCompactPercentForContextTier chooses the compact threshold tied to the
// selected context tier. Zero means provider default/inherit.
func AutoCompactPercentForContextTier(tier string, standardPercent, extendedPercent int) int {
	if tier == ContextTierExtended {
		return normalizeAutoCompactPercent(extendedPercent)
	}
	return normalizeAutoCompactPercent(standardPercent)
}

func normalizeAutoCompactPercent(percent int) int {
	switch {
	case percent <= 0:
		return 0
	case percent > 90:
		return 90
	default:
		return percent
	}
}

// IsValidAutoCompactPercent reports whether the value is in the [0, 90]
// range the thread row's CHECK constraint enforces. Callers use it to
// reject bad inputs before issuing a CreateThread.
func IsValidAutoCompactPercent(percent int) bool {
	return percent >= 0 && percent <= 90
}
