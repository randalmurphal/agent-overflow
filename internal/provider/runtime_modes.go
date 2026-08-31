package provider

// RuntimeMode is the approval/permission axis (three tiers mirror t3-code,
// plus AO's own read-only and auto tiers). It controls whether the agent
// prompts for tool use, auto-approves file edits, routes approvals to a
// model-based reviewer, bypasses approvals entirely, or is denied every
// mutating action outright. Provider packages own the wire-level mapping.
// Orthogonal to InteractionMode ("plan" / "discussion"), which
// shapes *what* the agent does, not how much friction is in the way.
//
// The modes are ordered from most to least restrictive on the *mutation*
// dimension, but note that RuntimeReadOnly is not simply "stricter than
// approval-required": it is the only mode that never produces an interactive
// request at all, which is what makes it the mode unattended work runs under.
type RuntimeMode string

const (
	// RuntimeReadOnly denies every mutating action instead of asking about
	// it, and never emits an interactive request. Reads and non-mutating
	// shell commands run normally; writes, edits, and mutating commands are
	// refused immediately and the refusal is handed straight back to the
	// model, so the turn keeps moving rather than stalling on a prompt
	// nobody is present to answer.
	//
	// This is the mode unattended work runs under — a workflow phase that
	// declares `access: read-only` maps here (docs/specs/workflows-system.md
	// §9, decision D22). It is deliberately NOT reachable by falling back
	// from an unknown value: a session that was meant to be restricted must
	// never silently widen, and one that was meant to be permissive must
	// never silently seize up.
	RuntimeReadOnly RuntimeMode = "read-only"

	// RuntimeApprovalRequired prompts the user for every tool use.
	RuntimeApprovalRequired RuntimeMode = "approval-required"

	// RuntimeAutoAcceptEdits auto-approves file edits inside the workspace
	// but still prompts for shell commands and other escalation.
	RuntimeAutoAcceptEdits RuntimeMode = "auto-accept-edits"

	// RuntimeAuto keeps the human out of the loop without giving up the
	// veto: a model-based reviewer answers each sensitive tool use with an
	// approve or a DENY. It is the only tier besides RuntimeReadOnly that
	// can refuse an action, and unlike read-only the refusal is a judgement
	// rather than a blanket rule.
	//
	// Two honest costs ride along and the picker copy states both: every
	// reviewed tool call is a billed model call (Claude bills a Haiku
	// classifier turn; Codex runs an auto_review subagent), and the reviewer
	// can and does say no. Neither provider guarantees prompt-free operation
	// — both fall back to a real interactive request for the cases their
	// reviewer refuses to adjudicate (Claude: safety_check / ask_rule /
	// plan_mode_floor), so the ordinary approval plumbing stays live.
	RuntimeAuto RuntimeMode = "auto"

	// RuntimeFullAccess bypasses approvals entirely. This is the
	// agent-overflow default — chosen deliberately over safer defaults
	// because our target user is an agent operator who explicitly wants
	// frictionless runs and will opt in to the stricter tiers on a
	// per-thread basis.
	RuntimeFullAccess RuntimeMode = "full-access"
)

// AllRuntimeModes is the canonical list, ordered most- to least-restrictive
// on the mutation dimension. CHECK constraints, frontend pickers, and
// migration fallbacks all reference it — keep in sync with the const block
// above.
//
// RuntimeAuto sits after RuntimeAutoAcceptEdits because it lets strictly more
// mutations through unprompted: auto-accept-edits still stops at every shell
// command, auto reviews it and usually allows it.
var AllRuntimeModes = []RuntimeMode{
	RuntimeReadOnly,
	RuntimeApprovalRequired,
	RuntimeAutoAcceptEdits,
	RuntimeAuto,
	RuntimeFullAccess,
}

// DefaultRuntimeMode is the seed value for new threads and for the global
// settings default. Intentionally frictionless — see RuntimeFullAccess.
const DefaultRuntimeMode = RuntimeFullAccess

// knownRuntimeModes is derived from AllRuntimeModes so a mode added to the
// canonical list is recognised by NormalizeRuntimeMode without a second edit.
// Adding a constant and forgetting the validator would silently coerce that
// mode to full-access — the exact failure this indirection removes.
var knownRuntimeModes = func() map[RuntimeMode]struct{} {
	known := make(map[RuntimeMode]struct{}, len(AllRuntimeModes))
	for _, mode := range AllRuntimeModes {
		known[mode] = struct{}{}
	}
	return known
}()

// IsRuntimeMode reports whether mode is one of the canonical values.
func IsRuntimeMode(mode RuntimeMode) bool {
	_, ok := knownRuntimeModes[mode]
	return ok
}

// NormalizeRuntimeMode returns the input if it's a known mode; otherwise
// falls back to DefaultRuntimeMode. Callers pass arbitrary strings coming
// from the wire or an older DB row; this is the chokepoint that keeps
// unknown values out of the session-config mapping.
func NormalizeRuntimeMode(mode string) RuntimeMode {
	if candidate := RuntimeMode(mode); IsRuntimeMode(candidate) {
		return candidate
	}
	return DefaultRuntimeMode
}
