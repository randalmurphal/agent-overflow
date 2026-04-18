package provider

import "testing"

// TestNormalizeRuntimeMode covers the three valid inputs and the fallback
// paths: empty string and arbitrary junk both collapse to the default. The
// fallback is the reason the normalizer exists — without it, a stale DB
// column or a typo in a wire payload would silently flow into the provider
// config mapping and produce unpredictable CLI flags.
func TestNormalizeRuntimeMode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want RuntimeMode
	}{
		{"approval-required", "approval-required", RuntimeApprovalRequired},
		{"auto-accept-edits", "auto-accept-edits", RuntimeAutoAcceptEdits},
		{"full-access", "full-access", RuntimeFullAccess},
		{"empty falls back to default", "", DefaultRuntimeMode},
		{"unknown falls back to default", "yolo", DefaultRuntimeMode},
		{"case-sensitive (upper case falls back)", "FULL-ACCESS", DefaultRuntimeMode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeRuntimeMode(tc.in); got != tc.want {
				t.Errorf("NormalizeRuntimeMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestClaudePermissionFlagsMapping encodes the Claude CLI's expected flag
// sequence per RuntimeMode. Full-access emits the bare
// `--dangerously-skip-permissions` flag (no value argument); auto-accept
// emits `--permission-mode acceptEdits`; approval-required emits nothing so
// the CLI's built-in default prompting engages.
func TestClaudePermissionFlagsMapping(t *testing.T) {
	cases := map[RuntimeMode][]string{
		RuntimeApprovalRequired: nil,
		RuntimeAutoAcceptEdits:  {"--permission-mode", "acceptEdits"},
		RuntimeFullAccess:       {"--dangerously-skip-permissions"},
	}
	for mode, want := range cases {
		t.Run(string(mode), func(t *testing.T) {
			got := ClaudePermissionFlags(mode)
			if !stringSliceEqual(got, want) {
				t.Errorf("ClaudePermissionFlags(%q) = %v, want %v", mode, got, want)
			}
		})
	}
	// Unknown mode also falls back to no flag — never passes a bogus value
	// downstream.
	if got := ClaudePermissionFlags("unknown-mode"); got != nil {
		t.Errorf("ClaudePermissionFlags(unknown) = %v, want nil", got)
	}
}

// stringSliceEqual is a tiny deep-equality helper so the permission-flag test
// doesn't have to pull in reflect.DeepEqual for a three-element comparison.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCodexApprovalPolicyMapping encodes the Codex JSON-RPC shape.
func TestCodexApprovalPolicyMapping(t *testing.T) {
	cases := map[RuntimeMode]string{
		RuntimeApprovalRequired: "untrusted",
		RuntimeAutoAcceptEdits:  "on-request",
		RuntimeFullAccess:       "never",
	}
	for mode, want := range cases {
		t.Run(string(mode), func(t *testing.T) {
			if got := CodexApprovalPolicy(mode); got != want {
				t.Errorf("CodexApprovalPolicy(%q) = %q, want %q", mode, got, want)
			}
		})
	}
	// Unknown falls back to the safest tier — never let a bogus string
	// translate into "never" (full access).
	if got := CodexApprovalPolicy("unknown-mode"); got != "untrusted" {
		t.Errorf("CodexApprovalPolicy(unknown) = %q, want untrusted", got)
	}
}

// TestCodexSandboxMapping confirms the sandbox / approval-policy pair
// agree with t3-code's reference implementation: approval-required pairs
// with read-only, auto-accept-edits with workspace-write, full-access
// with danger-full-access.
func TestCodexSandboxMapping(t *testing.T) {
	cases := map[RuntimeMode]string{
		RuntimeApprovalRequired: "read-only",
		RuntimeAutoAcceptEdits:  "workspace-write",
		RuntimeFullAccess:       "danger-full-access",
	}
	for mode, want := range cases {
		t.Run(string(mode), func(t *testing.T) {
			if got := CodexSandbox(mode); got != want {
				t.Errorf("CodexSandbox(%q) = %q, want %q", mode, got, want)
			}
		})
	}
	if got := CodexSandbox("unknown-mode"); got != "read-only" {
		t.Errorf("CodexSandbox(unknown) = %q, want read-only", got)
	}
}

// TestAllRuntimeModesContainsEveryValue keeps the canonical list in sync
// with the const block. A new mode added without being appended here
// would bypass pickers and CHECK-constraint migrations, so this guard
// forces a coordinated update.
func TestAllRuntimeModesContainsEveryValue(t *testing.T) {
	want := map[RuntimeMode]bool{
		RuntimeApprovalRequired: true,
		RuntimeAutoAcceptEdits:  true,
		RuntimeFullAccess:       true,
	}
	got := make(map[RuntimeMode]bool, len(AllRuntimeModes))
	for _, m := range AllRuntimeModes {
		got[m] = true
	}
	for m := range want {
		if !got[m] {
			t.Errorf("AllRuntimeModes is missing %q", m)
		}
	}
	for m := range got {
		if !want[m] {
			t.Errorf("AllRuntimeModes has unknown %q — add to both lists", m)
		}
	}
}
