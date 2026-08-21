package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// liveUpdatePartialSession spawns a fake CLI that succeeds on every
// control_request EXCEPT the named subtype, which it answers with an error
// response — the wire shape of a request the CLI understood and refused.
func liveUpdatePartialSession(t *testing.T, cfg Config, failSubtype string) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude")
	capturePath := filepath.Join(dir, "stdin.ndjson")
	script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
fail="${FAIL_SUBTYPE:?}"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            case "$line" in
                *"\"subtype\":\"$fail\""*)
                    printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"refused"}}\n' "$reqid"
                    ;;
                *)
                    printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
                    ;;
            esac
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	cfg.Binary = scriptPath
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	cfg.Env["CAPTURE_FILE"] = capturePath
	cfg.Env["FAIL_SUBTYPE"] = failSubtype

	s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.noteCLIVersion("2.1.237")
	return s, capturePath
}

// A prompt+thinking update where set_model lands and
// set_max_thinking_tokens is refused. The session is genuinely running the
// new prompt on the old thinking budget, and the restart that converges the
// rest is deferred until the thread is quiet — so the outcome has to SAY
// the prompt applied. Reporting nothing applied would make the caller
// record a config the session is not running, and make the live-first retry
// re-send the prompt instead of the axis that failed.
func TestApplyLiveUpdateReportsThePartiallyAppliedAxes(t *testing.T) {
	s, capturePath := liveUpdatePartialSession(t, Config{
		BasePermissionMode: "default",
		Model:              "claude-fable-5",
	}, "set_max_thinking_tokens")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applied, err := s.ApplyLiveUpdate(ctx, LiveUpdate{
		SystemPrompt: "new prompt",
		Thinking:     ThinkingUpdate{Apply: true, SendBudget: true, Budget: 2048, Display: ThinkingDisplaySummarized},
	}, nil)
	if err == nil {
		t.Fatal("ApplyLiveUpdate returned no error for a refused thinking request")
	}
	if !applied.SystemPrompt {
		t.Fatal("the prompt swap landed on the wire but the outcome does not report it applied")
	}
	if applied.Thinking {
		t.Fatal("the outcome reports a thinking axis the CLI refused")
	}
	if applied.Model {
		t.Fatal("the outcome reports a model change the update never carried")
	}

	// Both requests really were written — the prompt one first.
	lines := waitCapturedLines(t, capturePath, 2)
	if !strings.Contains(lines[0], "set_model") || !strings.Contains(lines[0], "new prompt") {
		t.Fatalf("first wire line is not the prompt-carrying set_model: %s", lines[0])
	}
	if !strings.Contains(lines[1], "set_max_thinking_tokens") {
		t.Fatalf("second wire line is not the thinking request: %s", lines[1])
	}
}

// A refusal on the FIRST axis must report nothing applied — the caller
// would otherwise commit a prompt the CLI rejected.
func TestApplyLiveUpdateReportsNothingWhenTheFirstAxisFails(t *testing.T) {
	s, _ := liveUpdatePartialSession(t, Config{
		BasePermissionMode: "default",
		Model:              "claude-fable-5",
	}, "set_model")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applied, err := s.ApplyLiveUpdate(ctx, LiveUpdate{
		SystemPrompt: "new prompt",
		Thinking:     ThinkingUpdate{Apply: true, SendBudget: true, Budget: 2048, Display: ThinkingDisplaySummarized},
	}, nil)
	if err == nil {
		t.Fatal("ApplyLiveUpdate returned no error for a refused set_model")
	}
	if applied != (LiveApplyOutcome{}) {
		t.Fatalf("outcome after a first-axis refusal = %+v, want nothing applied", applied)
	}
}

// A validation refusal happens before any wire write, so it must also
// report nothing applied.
func TestApplyLiveUpdateReportsNothingOnAValidationRefusal(t *testing.T) {
	s, _ := liveUpdateTestSession(t, Config{BasePermissionMode: "default", Model: "claude-fable-5"})
	// No version noted: the thinking handler's floor is unproven.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	applied, err := s.ApplyLiveUpdate(ctx, LiveUpdate{
		Model:    "claude-fable-5-fast",
		Thinking: ThinkingUpdate{Apply: true, SendBudget: true, Budget: 2048},
	}, nil)
	if err == nil {
		t.Fatal("ApplyLiveUpdate accepted a thinking update below the version floor")
	}
	if applied != (LiveApplyOutcome{}) {
		t.Fatalf("outcome after a validation refusal = %+v, want nothing applied", applied)
	}
}

// CommitLiveUpdate is the inverse of ConfigFromOptions for the live axes,
// and the pairing is what makes a partial apply converge. Two properties:
// a FULL apply must leave nothing to re-plan, and a partial one must leave
// exactly the unapplied axes as the remaining diff. An axis whose option
// fields are missing from CommitLiveUpdate fails the first; an axis that
// copies too much fails the second.
func TestCommitLiveUpdateCoversEveryLiveAppliableAxis(t *testing.T) {
	prev := provider.SessionOptions{
		Model:           "claude-fable-5",
		WorkDir:         "/w",
		RuntimeMode:     "approval-required",
		ReasoningEffort: "low",
		SystemPrompt:    "old prompt",
		ClaudeThinking:  provider.ClaudeThinking{Mode: string(ThinkingBudget), BudgetTokens: 1024, Display: ThinkingDisplaySummarized},
	}
	next := prev
	next.Model = "claude-fable-5-fast"
	next.RuntimeMode = "auto-accept-edits"
	next.ReasoningEffort = "high"
	next.FastMode = true
	next.SystemPrompt = "new prompt"
	next.ClaudeThinking = provider.ClaudeThinking{Mode: string(ThinkingBudget), BudgetTokens: 4096, Display: ThinkingDisplayOmitted}

	full, ok := PlanLiveUpdate(prev, next)
	if !ok {
		t.Fatal("the all-axes transition is not live-appliable; the fixture is wrong, not the code")
	}
	if full.Empty() {
		t.Fatal("the all-axes transition planned an empty update")
	}

	everything := LiveApplyOutcome{
		Model: true, SystemPrompt: true, BasePermissionMode: true,
		Thinking: true, Effort: true, FastMode: true,
	}
	committed := CommitLiveUpdate(prev, next, everything)
	replan, ok := PlanLiveUpdate(committed, next)
	if !ok {
		t.Fatal("re-planning a fully committed apply demanded a restart")
	}
	if !replan.Empty() {
		t.Fatalf("a fully applied update did not converge; %+v is still outstanding — an axis is missing from CommitLiveUpdate", replan)
	}

	// Now each axis on its own: committing only it must leave exactly the
	// others outstanding.
	for _, tc := range []struct {
		name    string
		applied LiveApplyOutcome
		gone    func(LiveUpdate) bool
	}{
		{"model", LiveApplyOutcome{Model: true}, func(u LiveUpdate) bool { return u.Model == "" }},
		{"prompt", LiveApplyOutcome{SystemPrompt: true}, func(u LiveUpdate) bool { return u.SystemPrompt == "" }},
		{"permission mode", LiveApplyOutcome{BasePermissionMode: true}, func(u LiveUpdate) bool { return u.BasePermissionMode == "" }},
		{"thinking", LiveApplyOutcome{Thinking: true}, func(u LiveUpdate) bool { return !u.Thinking.Apply }},
		{"effort", LiveApplyOutcome{Effort: true}, func(u LiveUpdate) bool { return u.Effort == "" }},
		{"fast mode", LiveApplyOutcome{FastMode: true}, func(u LiveUpdate) bool { return u.FastMode == FastModeUnchanged }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			one := CommitLiveUpdate(prev, next, tc.applied)
			replan, ok := PlanLiveUpdate(one, next)
			if !ok {
				t.Fatalf("committing only %s made the remainder un-live-appliable", tc.name)
			}
			if !tc.gone(replan) {
				t.Fatalf("%s stayed in the re-plan after being committed: %+v", tc.name, replan)
			}
			if replan.Empty() {
				t.Fatalf("committing only %s converged every other axis too", tc.name)
			}
		})
	}
}
