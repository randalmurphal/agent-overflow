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
		// NOT an escalating policy of any spelling: a read-only workflow phase
		// runs with nobody watching, so a prompt is a permanent stall.
		return "never"
	case provider.RuntimeApprovalRequired:
		return codexApprovalPolicyUnlessTrusted
	case provider.RuntimeAutoAcceptEdits:
		return codexApprovalPolicyOnRequest
	case provider.RuntimeAuto:
		// Deliberately identical to approval-required. `auto` differs from it
		// on exactly one axis — who answers the escalation — and
		// `approvalsReviewer` is the wire knob for precisely that. The two
		// tiers therefore share this mapping, including its version remap.
		return codexApprovalPolicyUnlessTrusted
	case provider.RuntimeFullAccess:
		return "never"
	}
	return codexApprovalPolicyUnlessTrusted
}

// Codex's AskForApproval wire values that AO sends
// (codex-rs/app-server-protocol/src/protocol/v2/shared.rs; the v2 enum is
// kebab-case with `UnlessTrusted` explicitly renamed to "untrusted").
const (
	codexApprovalPolicyUnlessTrusted = "untrusted"
	codexApprovalPolicyOnRequest     = "on-request"
)

// codexOnRequestApprovalFloor is the first Codex release where "untrusted"
// stopped meaning what approval-required and auto promised, and "on-request"
// started meaning it.
//
// Upstream 942af8447b, "Retire the untrusted approval policy" (#39630), landed
// in 0.149.0. The wire VALUE survives — `AskForApproval::UnlessTrusted` still
// serializes as "untrusted", and the app-server only rejects it when it comes
// from a `config.toml` `approval_policy` key, never from a thread/turn
// override (`UnsupportedUntrustedApprovalPolicyError` is raised on
// `cfg.approval_policy`, not on `approval_policy_override`, in
// codex-rs/core/src/config/mod.rs) — but the known-safe command allowlist it
// depended on was DELETED. Compare `render_decision_for_unmatched_command`
// (codex-rs/core/src/exec_policy.rs) across the change:
//
//   - ≤ 0.148: `is_known_safe_command` short-circuits to Allow under
//     `UnlessTrusted`, so `ls` / `cat` / `rg` run and only non-read commands
//     escalate.
//   - ≥ 0.149: that branch is gone. `UnlessTrusted` returns Prompt for EVERY
//     command no explicit exec-policy rule allows — including `ls`.
//
// `OnRequest` under a RESTRICTED (read-only / workspace-write) sandbox is what
// now expresses the old intent: dangerous commands Prompt, anything that
// `requests_sandbox_override()` — a write or a network escape out of a
// read-only sandbox — Prompts, and everything else is allowed to run with the
// kernel sandbox as the enforcement. The escalation still reaches the human in
// approval-required and Codex's reviewer subagent in auto, because
// `approvalsReviewer` and the read-only sandbox are unchanged.
const codexOnRequestApprovalFloor = "0.149.0"

// approvalPolicyForCodexVersion remaps the policy AO computed for a runtime
// mode onto what the CONNECTED app-server actually implements.
//
// Only "untrusted" moves, and it moves for every tier that produces it
// (approval-required, auto, and the unreachable fallback) — which is exactly
// the mode-level remap, expressed once at the wire chokepoint so
// thread/start, thread/resume and every turn/start cannot disagree.
//
// An EMPTY or unparseable version keeps "untrusted". That is the safe
// direction and it is not symmetric: sending "on-request" to a ≤ 0.148 binary
// would silently let non-read commands run unapproved in a tier whose whole
// promise is that they do not, whereas sending "untrusted" to a 0.149 binary
// costs prompts, not permission. `provider.CodexCLIVersionAtLeast` already
// fails closed on an unparseable version; the explicit empty check is here so
// the intent is readable rather than inferred.
func approvalPolicyForCodexVersion(policy, codexVersion string) string {
	if policy != codexApprovalPolicyUnlessTrusted {
		return policy
	}
	if codexVersion == "" {
		return policy
	}
	if provider.CodexCLIVersionAtLeast(codexVersion, codexOnRequestApprovalFloor) {
		return codexApprovalPolicyOnRequest
	}
	return policy
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
		DisabledTools:         opts.DisabledTools,
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
