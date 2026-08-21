package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestApprovalPolicyForCodexVersionAcrossModes is the whole product decision
// in one table: which wire approval policy each runtime mode sends to each
// generation of codex app-server.
//
// The remap exists because 0.149 (upstream 942af8447b, "Retire the untrusted
// approval policy") deleted the known-safe command allowlist that made
// "untrusted" mean "reads run, everything else escalates". Post-0.149 the same
// value prompts for EVERY command, `ls` included; "on-request" under a
// restricted sandbox is what now expresses the old meaning. See
// approvalPolicyForCodexVersion in options.go for the source citations.
//
// Every row is driven through the PRODUCTION chokepoint — ConfigFromOptions
// into buildThreadParams, which `thread/start` and `thread/resume` share —
// rather than through a mode→value helper written for the test. A helper that
// composes the same two functions the wire path composes proves only that the
// test can compose them: it stays green while the wire path stops calling one.
func modeApprovalPolicyOnTheWire(mode provider.RuntimeMode, codexVersion string) string {
	cfg := ConfigFromOptions(provider.SessionOptions{RuntimeMode: mode, WorkDir: "/tmp/x"})
	policy, ok := buildThreadParams(cfg, codexVersion)["approvalPolicy"].(string)
	if !ok {
		return ""
	}
	return policy
}

func TestApprovalPolicyForCodexVersionAcrossModes(t *testing.T) {
	// Versions on both sides of the floor, plus the two "we could not tell"
	// answers, which must behave identically to an old binary.
	versions := []struct {
		version   string
		remapped  bool
		whyItIsSo string
	}{
		{"", false, "unknown version must never widen a supervised tier"},
		{"not-a-version", false, "unparseable version reads as unknown, not as new"},
		{"0.143.0", false, "AO's provider floor still has the allowlist"},
		{"0.147.0", false, "the version currently installed"},
		{"0.148.0", false, "last release before the retirement"},
		{"0.149.0-alpha.1", false, "a prerelease sorts BELOW its release; the change landed in 0.149.0 final"},
		{"0.149.0", true, "the release that deleted the allowlist"},
		{"0.150.3", true, "everything after stays remapped"},
	}

	for _, v := range versions {
		for _, mode := range provider.AllRuntimeModes {
			base := codexApprovalPolicy(mode)
			want := base
			if v.remapped && base == codexApprovalPolicyUnlessTrusted {
				want = codexApprovalPolicyOnRequest
			}
			if got := modeApprovalPolicyOnTheWire(mode, v.version); got != want {
				t.Errorf("mode %q on codex %q reaches the wire as %q, want %q (%s)",
					mode, v.version, got, want, v.whyItIsSo)
			}
		}
	}

	// The two tiers the remap is FOR, spelled out rather than derived, so a
	// change to codexApprovalPolicy cannot quietly make this test tautological.
	for _, mode := range []provider.RuntimeMode{provider.RuntimeApprovalRequired, provider.RuntimeAuto} {
		if got := modeApprovalPolicyOnTheWire(mode, "0.148.0"); got != "untrusted" {
			t.Errorf("%s on 0.148 = %q, want untrusted", mode, got)
		}
		if got := modeApprovalPolicyOnTheWire(mode, "0.149.0"); got != "on-request" {
			t.Errorf("%s on 0.149 = %q, want on-request", mode, got)
		}
		// The sandbox is the half that keeps writes escalating, and it must
		// NOT move with the version. Without it, on-request would simply allow
		// everything the model asked for.
		if got := codexSandbox(mode); got != "read-only" {
			t.Errorf("codexSandbox(%s) = %q, want read-only — the remap depends on a restricted sandbox", mode, got)
		}
	}

	// The tiers that never send "untrusted" must be untouched by the version.
	for _, mode := range []provider.RuntimeMode{
		provider.RuntimeReadOnly, provider.RuntimeAutoAcceptEdits, provider.RuntimeFullAccess,
	} {
		for _, version := range []string{"", "0.148.0", "0.149.0", "1.0.0"} {
			if got, want := modeApprovalPolicyOnTheWire(mode, version), codexApprovalPolicy(mode); got != want {
				t.Errorf("mode %q on codex %q reaches the wire as %q, want %q — this tier has no version axis",
					mode, version, got, want)
			}
		}
	}
}

// TestBuildThreadParamsRemapsApprovalPolicyByVersion pins the handshake half:
// thread/start and thread/resume share buildThreadParams, so both carry the
// remapped value.
func TestBuildThreadParamsRemapsApprovalPolicyByVersion(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{
		RuntimeMode: provider.RuntimeApprovalRequired,
		WorkDir:     "/tmp/x",
	})
	if cfg.ApprovalPolicy != "untrusted" {
		t.Fatalf("ConfigFromOptions approval policy = %q, want untrusted", cfg.ApprovalPolicy)
	}

	for _, tc := range []struct{ version, want string }{
		{"", "untrusted"},
		{"0.148.0", "untrusted"},
		{"0.149.0", "on-request"},
	} {
		params := buildThreadParams(cfg, tc.version)
		if got := params["approvalPolicy"]; got != tc.want {
			t.Errorf("buildThreadParams approvalPolicy on %q = %v, want %q", tc.version, got, tc.want)
		}
		if got := params["sandbox"]; got != "read-only" {
			t.Errorf("buildThreadParams sandbox on %q = %v, want read-only", tc.version, got)
		}
	}
}

// TestDefaultApprovalPolicyRemapsTheUnsetBranch — a Config that never set the
// axis falls through to the sandbox pairing, and that fallback lands on
// "untrusted" for read-only. It has to take the same remap, or an unset Config
// would send a different policy than an explicitly-approval-required one.
func TestDefaultApprovalPolicyRemapsTheUnsetBranch(t *testing.T) {
	cases := []struct {
		policy, sandbox, version, want string
	}{
		{"", "read-only", "0.148.0", "untrusted"},
		{"", "read-only", "0.149.0", "on-request"},
		{"", "workspace-write", "0.149.0", "on-request"},
		{"", "danger-full-access", "0.149.0", "never"},
		{"never", "read-only", "0.149.0", "never"},
		{"on-request", "read-only", "0.149.0", "on-request"},
		{"untrusted", "read-only", "0.149.0", "on-request"},
		{"untrusted", "read-only", "", "untrusted"},
	}
	for _, tc := range cases {
		if got := defaultApprovalPolicy(tc.policy, tc.sandbox, tc.version); got != tc.want {
			t.Errorf("defaultApprovalPolicy(%q, %q, %q) = %q, want %q",
				tc.policy, tc.sandbox, tc.version, got, tc.want)
		}
	}
}

// TestSessionAppServerVersionDrivesTurnStartApprovalPolicy proves the per-turn
// override reads the SAME version the handshake did. A mid-session
// runtime-mode switch rewrites Session.approvalPolicy directly
// (live_update.go), so a remap that only lived in buildThreadParams would be
// undone by the next turn.
func TestSessionAppServerVersionDrivesTurnStartApprovalPolicy(t *testing.T) {
	for _, tc := range []struct{ userAgent, want string }{
		{"", "untrusted"},
		{"codex_cli_rs/0.148.0 (Ubuntu 24.04; x86_64) codex_cli_rs", "untrusted"},
		{"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) codex_cli_rs", "on-request"},
	} {
		s := &Session{}
		s.recordAppServerVersion(json.RawMessage(mustJSON(t, map[string]string{"userAgent": tc.userAgent})))
		if got := approvalPolicyForCodexVersion("untrusted", s.AppServerVersion()); got != tc.want {
			t.Errorf("userAgent %q → approval policy %q, want %q", tc.userAgent, got, tc.want)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
