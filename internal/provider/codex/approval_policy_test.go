package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
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

// A policy the adapter does not send is refused where it enters: a new
// session's Config before anything launches, a pooled open before the
// thread is opened, and a live update before it changes the turn config.
// The retired "untrusted" is one such spelling; on 0.154.0 it prompts for
// every command.
func TestApprovalPolicyInputsRefuseAPolicyAONeverSends(t *testing.T) {
	t.Parallel()
	const want = `codex: approval policy "untrusted" is not one Agent Overflow sends`

	dir := t.TempDir()
	launched := filepath.Join(dir, "launched")
	binary := filepath.Join(dir, "codex")
	mockexec.Write(t, binary, "#!/bin/sh\ntouch '"+launched+"'\nexit 1\n")
	_, err := NewSession(context.Background(), testThread, Config{
		Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: "/tmp",
		ApprovalPolicy: "untrusted", Sandbox: "read-only",
	}, func(provider.ProviderEvent) {})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("NewSession error = %v, want %q", err, want)
	}
	if _, statErr := os.Stat(launched); !os.IsNotExist(statErr) {
		t.Fatalf("NewSession launched Codex for a refused policy (stat: %v)", statErr)
	}

	released := 0
	_, err = (&Conn{}).OpenPooled(context.Background(), testThread, Config{
		ApprovalPolicy: "untrusted", Sandbox: "read-only",
	}, func(provider.ProviderEvent) {}, func() { released++ })
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("OpenPooled error = %v, want %q", err, want)
	}
	if released != 1 {
		t.Fatalf("OpenPooled released its connection %d times, want 1", released)
	}

	s := &Session{}
	if err := s.ApplyLiveUpdate(LiveUpdate{Model: "m", ApprovalPolicy: codexApprovalPolicyOnRequest}); err != nil {
		t.Fatal(err)
	}
	err = s.ApplyLiveUpdate(LiveUpdate{Model: "other", ApprovalPolicy: "untrusted"})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ApplyLiveUpdate error = %v, want %q", err, want)
	}
	if got := s.snapshotTurnConfig(); got.Model != "m" || got.ApprovalPolicy != codexApprovalPolicyOnRequest {
		t.Fatalf("turn config after a refused update = %+v, want it unchanged", got)
	}
	for _, policy := range []string{"", "never", codexApprovalPolicyOnRequest} {
		if err := s.ApplyLiveUpdate(LiveUpdate{ApprovalPolicy: policy}); err != nil {
			t.Errorf("ApplyLiveUpdate(%q) = %v, want it accepted", policy, err)
		}
	}
}
