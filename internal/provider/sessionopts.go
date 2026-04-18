package provider

// ReasoningEffort identifies a tier in the five-step composer effort
// enum. Claude supports the full set (low/medium/high/xhigh/max) via
// system-prompt prefix injection; Codex natively understands only
// low/medium/high and floors the top two tiers at its translation
// boundary.
type ReasoningEffort string

const (
	EffortLow    ReasoningEffort = "low"
	EffortMedium ReasoningEffort = "medium"
	EffortHigh   ReasoningEffort = "high"
	EffortXHigh  ReasoningEffort = "xhigh"
	// EffortMax is the top tier. Claude's native plan-mode + "think
	// harder" prefix combination drives it; Codex floors to xhigh.
	EffortMax ReasoningEffort = "max"
)

// AllReasoningEfforts is the canonical ordered list used by UI pickers,
// validation, and per-provider mappers. Kept in lockstep with the CHECK
// constraint on threads.reasoning_effort (see migrate.go::v13SQL).
var AllReasoningEfforts = []ReasoningEffort{
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
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
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

	Model           string
	WorkDir         string
	ReasoningEffort ReasoningEffort
	FastMode        bool
	ContextWindow   int
	Mode            InteractionMode
	RuntimeMode     RuntimeMode
	SystemPrompt    string

	// Resume carries the provider's resume reference. Claude: prior
	// session uuid. Codex: prior thread id. Empty for brand-new starts.
	Resume string

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
	GetMode() string
	GetRuntimeMode() string
	GetSessionRef() string
	GetPendingForkRef() string
}

// SessionOptionsFromThread assembles SessionOptions from a ThreadView +
// the caller-composed system prompt (which itself may depend on mode —
// that composition is the caller's job, because it reaches into design/
// and discussion/ which the provider package doesn't see).
//
// forkSession is an explicit arg instead of being read from the thread
// because it's a per-start decision (the fork is consumed on the next
// start; it isn't durable state).
func SessionOptionsFromThread(t ThreadView, systemPrompt string, forkSession bool) SessionOptions {
	resume := t.GetSessionRef()
	if forkSession && t.GetPendingForkRef() != "" {
		// Claude's pending-fork flow: the next start should resume
		// PendingForkRef (not the live SessionRef), and the act of
		// starting consumes the pending ref.
		resume = t.GetPendingForkRef()
	}
	return SessionOptions{
		Provider:        t.GetProvider(),
		Model:           t.GetModel(),
		WorkDir:         t.GetWorkspacePath(),
		ReasoningEffort: NormalizeReasoningEffort(t.GetReasoningEffort()),
		FastMode:        t.GetFastMode(),
		ContextWindow:   t.GetContextWindow(),
		Mode:            NormalizeInteractionMode(t.GetMode()),
		RuntimeMode:     NormalizeRuntimeMode(t.GetRuntimeMode()),
		SystemPrompt:    systemPrompt,
		Resume:          resume,
		ForkSession:     forkSession,
	}
}
