package codex

import (
	"strings"

	"agent-overflow/internal/provider"
)

// codexEffortFromOption translates AO's stored ReasoningEffort onto Codex's
// native app-server enum exposed by the live model catalog.
func codexEffortFromOption(effort provider.ReasoningEffort) string {
	switch effort {
	case provider.EffortNone:
		return "none"
	case provider.EffortMinimal:
		return "minimal"
	case provider.EffortLow:
		return "low"
	case provider.EffortMedium:
		return "medium"
	case provider.EffortHigh:
		return "high"
	case provider.EffortXHigh:
		return "xhigh"
	case provider.EffortMax:
		return "max"
	case provider.EffortUltra:
		return "ultra"
	default:
		// Empty / unknown — let Codex pick its own default by omitting the
		// parameter upstream.
		return ""
	}
}

// runtimeModeToCodex bundles the ApprovalPolicy + Sandbox + ApprovalsReviewer
// triple that Codex expects for a given RuntimeMode. Splitting into one helper
// keeps the translation intentional — a RuntimeMode change must touch every
// field in lockstep, and routing through a single function makes that obvious.
type codexRuntime struct {
	ApprovalPolicy    string
	Sandbox           string
	ApprovalsReviewer string
}

func runtimeModeToCodex(mode provider.RuntimeMode) codexRuntime {
	return codexRuntime{
		ApprovalPolicy:    codexApprovalPolicy(mode),
		Sandbox:           codexSandbox(mode),
		ApprovalsReviewer: codexApprovalsReviewer(mode),
	}
}

// Each of the three mappers below enumerates every tier; the trailing return
// is the unknown-value path, not a tier's mapping. Callers reach these through
// provider.NormalizeRuntimeMode, which never yields a non-canonical value, so
// the fallback is unreachable in practice — and it deliberately lands on the
// most supervised posture rather than on anything that would widen a session.
// TestRuntimeModeToCodexCoversEveryMode is what forces a new tier to be
// enumerated here rather than absorbed by that fallback.

func codexApprovalPolicy(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeReadOnly:
		// AskForApproval::Never — "Never ask the user to approve commands.
		// Failures are immediately returned to the model, and never escalated
		// to the user for approval" (codex-rs/protocol/src/protocol.rs). Paired
		// with the read-only sandbox this is the unattended-restricted config:
		// a sandbox denial comes straight back as a tool failure the model can
		// react to, and nothing waits on a human. `Never` also suppresses MCP
		// auth elicitation outright (codex-rs/core/src/mcp_tool_call.rs), which
		// is what keeps an unattended phase from parking on a prompt.
		//
		// NOT "untrusted": that escalates every non-read command to a human,
		// which is precisely the stall a read-only workflow phase cannot afford.
		return "never"
	case provider.RuntimeApprovalRequired:
		return "untrusted"
	case provider.RuntimeAutoAcceptEdits:
		return "on-request"
	case provider.RuntimeAuto:
		// Deliberately identical to approval-required. `auto` differs from it
		// on exactly one axis — who answers the escalation — and
		// `approvalsReviewer` is the wire knob for precisely that. Relaxing the
		// policy to `on-request` (the auto-accept-edits pair) would let
		// workspace writes proceed without ever reaching the reviewer, which
		// would quietly narrow the reviewer's veto to shell commands while the
		// tier's label still promises review of each sensitive tool use.
		return "untrusted"
	case provider.RuntimeFullAccess:
		return "never"
	}
	return "untrusted"
}

func codexSandbox(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeReadOnly:
		// OS-level sandbox: writes are refused by the kernel, not by policy.
		return "read-only"
	case provider.RuntimeApprovalRequired:
		return "read-only"
	case provider.RuntimeAutoAcceptEdits:
		return "workspace-write"
	case provider.RuntimeAuto:
		// Same reasoning as codexApprovalPolicy: the sandbox is what turns a
		// write into an escalation, and an escalation is what the reviewer
		// gets to see. Widening it would remove writes from the reviewer's
		// jurisdiction.
		return "read-only"
	case provider.RuntimeFullAccess:
		return "danger-full-access"
	}
	return "read-only"
}

// Codex's ApprovalsReviewer values (codex-rs/protocol/src/config_types.rs).
// `guardian_subagent` is a legacy alias upstream deserializes onto AutoReview;
// AO never sends it — one spelling on the wire keeps the thread/start echo
// check a byte comparison rather than an alias table.
const (
	approvalsReviewerUser = "user"
	approvalsReviewerAuto = "auto_review"
)

// codexApprovalsReviewer decides who answers this thread's approval requests.
// Only RuntimeAuto routes them to Codex's reviewer subagent; every other tier
// keeps the human on the other end, including the tiers that never produce an
// approval request at all (read-only, full-access) — naming the reviewer there
// costs nothing and keeps "reviewer" from becoming sticky state a mode switch
// forgets to clear (t3-improvements.md §3.2).
func codexApprovalsReviewer(mode provider.RuntimeMode) string {
	switch mode {
	case provider.RuntimeReadOnly,
		provider.RuntimeApprovalRequired,
		provider.RuntimeAutoAcceptEdits,
		provider.RuntimeFullAccess:
		return approvalsReviewerUser
	case provider.RuntimeAuto:
		return approvalsReviewerAuto
	}
	return approvalsReviewerUser
}

// ConfigFromOptions translates a provider-agnostic SessionOptions bundle into
// the Codex-specific launch Config. Codex has a native `reasoning_effort`
// knob and carries the
// system prompt as `baseInstructions` on the thread/start request, so the
// shape of this translation is materially different from its Claude twin.
//
// The function intentionally does NOT handle:
//   - MCPServers: the caller merges app-owned per-thread MCP servers after
//     this returns because their registration and transport credentials are
//     outside the provider package.
//   - opts.ForkSession: Codex's fork flow is a separate `thread/fork`
//     app-server call and is NOT triggered by ForkSession. The field is a
//     no-op for Codex and we don't pretend otherwise.
func ConfigFromOptions(opts provider.SessionOptions) Config {
	runtime := runtimeModeToCodex(opts.RuntimeMode)

	model := opts.Model
	contextWindow := provider.ResolveContextWindowForModel(string(provider.Codex), model, opts.ContextWindow)
	autoCompactPercent := provider.AutoCompactPercentForContextTier(
		provider.ContextTierForModelWindow(string(provider.Codex), model, contextWindow),
		opts.AutoCompactStandardPercent,
		opts.AutoCompactExtendedPercent,
	)
	if autoCompactPercent == 0 {
		autoCompactPercent = opts.AutoCompactPercent
	}

	return Config{
		Model:                 model,
		WorkDir:               opts.WorkDir,
		ApprovalPolicy:        runtime.ApprovalPolicy,
		Sandbox:               runtime.Sandbox,
		ApprovalsReviewer:     runtime.ApprovalsReviewer,
		ResumeThreadID:        opts.Resume,
		SystemPrompt:          opts.SystemPrompt,
		ReasoningEffort:       codexEffortFromOption(opts.ReasoningEffort),
		ServiceTier:           codexServiceTier(opts.FastMode, opts.FastModeTierID),
		ContextWindow:         contextWindow,
		AutoCompactTokenLimit: autoCompactTokenLimit(contextWindow, autoCompactPercent),
	}
}

// codexServiceTier resolves the `serviceTier` a turn carries. Fast mode off
// omits the key entirely (Codex then uses the account default). On, the id
// comes from the model's own `model/list` entry, so an upstream tier rename
// lands as data rather than breaking the feature; the legacy `priority` id is
// the fallback for a model the catalog could not resolve or one whose
// app-server predates `serviceTiers`.
func codexServiceTier(fastMode bool, tierID string) string {
	if !fastMode {
		return ""
	}
	if trimmed := strings.TrimSpace(tierID); trimmed != "" {
		return trimmed
	}
	return fastServiceTier
}

func autoCompactTokenLimit(contextWindow, percent int) int {
	if contextWindow <= 0 || percent <= 0 {
		return 0
	}
	if percent > 90 {
		percent = 90
	}
	return contextWindow * percent / 100
}
