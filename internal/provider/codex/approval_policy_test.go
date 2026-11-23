package codex

import (
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
)

// modeApprovalPolicyOnTheWire drives a runtime mode through the production
// chokepoint, ConfigFromOptions into buildThreadParams, which `thread/start`
// and `thread/resume` share, rather than through a helper written for the
// test.
func modeApprovalPolicyOnTheWire(mode provider.RuntimeMode) string {
	cfg := ConfigFromOptions(provider.SessionOptions{RuntimeMode: mode, WorkDir: "/tmp/x"})
	cfg.Reaper = procutiltest.Reaper()
	policy, _ := buildThreadParams(cfg)["approvalPolicy"].(string)
	return policy
}

// TestApprovalPolicyAcrossModes is the product decision in one table: the
// wire approval policy each runtime mode sends. "untrusted" prompts for every
// command since Codex retired its allowlist, so the supervised tiers send
// "on-request" under a read-only sandbox (codexApprovalPolicyOnRequest).
func TestApprovalPolicyAcrossModes(t *testing.T) {
	t.Parallel()
	want := map[provider.RuntimeMode]string{
		provider.RuntimeReadOnly:         "never",
		provider.RuntimeApprovalRequired: "on-request",
		provider.RuntimeAutoAcceptEdits:  "on-request",
		provider.RuntimeAuto:             "on-request",
		provider.RuntimeFullAccess:       "never",
	}
	for _, mode := range provider.AllRuntimeModes {
		if got := modeApprovalPolicyOnTheWire(mode); got != want[mode] {
			t.Errorf("mode %q reaches the wire as %q, want %q", mode, got, want[mode])
		}
	}
	// The sandbox is the half that keeps writes escalating under
	// on-request.
	for _, mode := range []provider.RuntimeMode{provider.RuntimeApprovalRequired, provider.RuntimeAuto} {
		if got := codexSandbox(mode); got != "read-only" {
			t.Errorf("codexSandbox(%s) = %q, want read-only", mode, got)
		}
	}
}

// TestDefaultApprovalPolicyPairsTheUnsetBranchWithItsSandbox: a Config that
// never set the axis falls through to the policy its sandbox's tier sends,
// and an explicit one is sent as given.
func TestDefaultApprovalPolicyPairsTheUnsetBranchWithItsSandbox(t *testing.T) {
	t.Parallel()
	cases := []struct {
		policy, sandbox, want string
	}{
		{"", "read-only", "on-request"},
		{"", "workspace-write", "on-request"},
		{"", "danger-full-access", "never"},
		{"never", "read-only", "never"},
		{"on-request", "read-only", "on-request"},
	}
	for _, tc := range cases {
		if got := defaultApprovalPolicy(tc.policy, tc.sandbox); got != tc.want {
			t.Errorf("defaultApprovalPolicy(%q, %q) = %q, want %q", tc.policy, tc.sandbox, got, tc.want)
		}
	}
}
