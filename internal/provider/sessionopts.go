package provider

// ReasoningEffort identifies a provider reasoning tier. Claude exposes
// low/medium/high plus model-specific top tiers; Codex exposes its native
// none/minimal/low/medium/high/xhigh set through app-server model metadata.
type ReasoningEffort string

const (
	EffortNone    ReasoningEffort = "none"
	EffortMinimal ReasoningEffort = "minimal"
	EffortLow     ReasoningEffort = "low"
	EffortMedium  ReasoningEffort = "medium"
	EffortHigh    ReasoningEffort = "high"
	EffortXHigh   ReasoningEffort = "xhigh"
	EffortMax     ReasoningEffort = "max"
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
}

// DefaultReasoningEffort is the seed value for new threads when a caller
// doesn't pass an explicit choice.
const DefaultReasoningEffort = EffortHigh

// NormalizeReasoningEffort coerces unknown strings to DefaultReasoningEffort
// so a stale client cannot plant a value outside the enum.
func NormalizeReasoningEffort(effort string) ReasoningEffort {
	switch ReasoningEffort(effort) {
	case EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
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

// SessionOptions is the provider-agnostic bundle a Thread projects onto.
// The per-provider packages (claude, codex) each translate it into their
// own Config. Keeps the translation out of app.go and makes it testable
// without spawning a subprocess.
type SessionOptions struct {
	// Provider is the provider identifier (claude/codex). Copied from
	// the thread so the per-provider translator can assert it's looking
	// at the right bundle.
	Provider string

	Model                      string
	WorkDir                    string
	ReasoningEffort            ReasoningEffort
	FastMode                   bool
	ContextWindow              int
	AutoCompactPercent         int
	AutoCompactStandardPercent int
	AutoCompactExtendedPercent int
	Mode                       InteractionMode
	RuntimeMode                RuntimeMode
	SystemPrompt               string

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

func AutoCompactPercentForContextWindow(contextWindow, standardPercent, extendedPercent int) int {
	if contextWindow >= 1000000 {
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
