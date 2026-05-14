package provider

// RuntimeMode is the three-tier approval axis (mirrors t3-code). It controls
// whether the agent prompts for tool use, auto-approves file edits, or
// bypasses approvals entirely. Provider packages own the wire-level mapping.
// Orthogonal to InteractionMode ("plan" / "design" / "discussion"), which
// shapes *what* the agent does, not how much friction is in the way.
type RuntimeMode string

const (
	// RuntimeApprovalRequired prompts the user for every tool use.
	RuntimeApprovalRequired RuntimeMode = "approval-required"

	// RuntimeAutoAcceptEdits auto-approves file edits inside the workspace
	// but still prompts for shell commands and other escalation.
	RuntimeAutoAcceptEdits RuntimeMode = "auto-accept-edits"

	// RuntimeFullAccess bypasses approvals entirely. This is the
	// agent-overflow default — chosen deliberately over safer defaults
	// because our target user is an agent operator who explicitly wants
	// frictionless runs and will opt in to the stricter tiers on a
	// per-thread basis.
	RuntimeFullAccess RuntimeMode = "full-access"
)

// AllRuntimeModes is the canonical list. CHECK constraints, frontend
// pickers, and migration fallbacks all reference it — keep in sync with the
// const block above.
var AllRuntimeModes = []RuntimeMode{
	RuntimeApprovalRequired,
	RuntimeAutoAcceptEdits,
	RuntimeFullAccess,
}

// DefaultRuntimeMode is the seed value for new threads and for the global
// settings default. Intentionally frictionless — see RuntimeFullAccess.
const DefaultRuntimeMode = RuntimeFullAccess

// NormalizeRuntimeMode returns the input if it's a known mode; otherwise
// falls back to DefaultRuntimeMode. Callers pass arbitrary strings coming
// from the wire or an older DB row; this is the chokepoint that keeps
// unknown values out of the session-config mapping.
func NormalizeRuntimeMode(mode string) RuntimeMode {
	switch RuntimeMode(mode) {
	case RuntimeApprovalRequired, RuntimeAutoAcceptEdits, RuntimeFullAccess:
		return RuntimeMode(mode)
	default:
		return DefaultRuntimeMode
	}
}
