package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestPlanLiveUpdateSystemPromptTransitions covers the TRANSITIONS, not the
// states. The axis is ASYMMETRIC: `set_model.system_prompt` must be a
// non-empty string, so any change that LANDS on a prompt goes live (the CLI's
// setter is an unguarded assignment onto the same slot `--system-prompt-file`
// fills, so an empty prior value is not a special case), while turning an
// override OFF has no wire form at all and falls through to the restart path,
// where the spawn rebuilds argv without the flag.
//
// Getting this wrong in the permissive direction is silent: the session would
// keep running the old prompt while launchOpts claimed the new one.
func TestPlanLiveUpdateSystemPromptTransitions(t *testing.T) {
	tests := []struct {
		name       string
		prev       string
		next       string
		wantOK     bool
		wantUpdate LiveUpdate
	}{
		{
			name:       "override edited: one non-empty prompt to another",
			prev:       "old prompt",
			next:       "new prompt",
			wantOK:     true,
			wantUpdate: LiveUpdate{SystemPrompt: "new prompt"},
		},
		{
			name:   "override turned off: no revert-to-built-in wire form",
			prev:   "old prompt",
			next:   "",
			wantOK: false,
		},
		{
			name:       "override turned on: an empty slot takes a prompt like any other",
			prev:       "",
			next:       "new prompt",
			wantOK:     true,
			wantUpdate: LiveUpdate{SystemPrompt: "new prompt"},
		},
		{
			name:   "unchanged prompt carries nothing",
			prev:   "same prompt",
			next:   "same prompt",
			wantOK: true,
		},
		{
			name:   "both sides empty carries nothing",
			prev:   "",
			next:   "",
			wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := liveUpdateBaseOptions()
			prev.SystemPrompt = tc.prev
			next := liveUpdateBaseOptions()
			next.SystemPrompt = tc.next

			update, ok := PlanLiveUpdate(prev, next)
			if ok != tc.wantOK {
				t.Fatalf("PlanLiveUpdate ok = %v, want %v (update %+v)", ok, tc.wantOK, update)
			}
			if ok && update != tc.wantUpdate {
				t.Fatalf("PlanLiveUpdate update = %+v, want %+v", update, tc.wantUpdate)
			}
		})
	}
}

// TestPlanLiveUpdateSystemPromptRidesModelChange pins that a prompt swap and
// a model change compose into ONE update. They share a single set_model on
// the wire, so planning them as separate axes would be a lie about the
// request count and — worse — invite a caller to send the prompt with the
// stale model.
func TestPlanLiveUpdateSystemPromptRidesModelChange(t *testing.T) {
	prev := liveUpdateBaseOptions()
	prev.SystemPrompt = "old prompt"
	next := liveUpdateBaseOptions()
	next.SystemPrompt = "new prompt"
	next.Model = "claude-fable-5"

	update, ok := PlanLiveUpdate(prev, next)
	if !ok {
		t.Fatal("PlanLiveUpdate refused a model + prompt change")
	}
	want := LiveUpdate{Model: "claude-fable-5", SystemPrompt: "new prompt"}
	if update != want {
		t.Fatalf("update = %+v, want %+v", update, want)
	}
}

// TestPlanLiveUpdateIgnoresOverrideSourceProvenance pins that
// SessionOptions.SystemPromptOverrideSource — the app-layer record of WHICH
// stored override produced the rendered prompt — never reaches the diff. It
// is deliberately absent from Config, so two sessions running the identical
// rendered prompt must plan as unchanged even when the provenance differs
// (an override edited to render to the same text, or a session started before
// the field existed).
func TestPlanLiveUpdateIgnoresOverrideSourceProvenance(t *testing.T) {
	prev := liveUpdateBaseOptions()
	prev.SystemPrompt = "rendered prompt"
	prev.SystemPromptOverrideSource = "stored {{PLATFORM}} prompt"
	next := liveUpdateBaseOptions()
	next.SystemPrompt = "rendered prompt"
	next.SystemPromptOverrideSource = "a completely different stored prompt"

	update, ok := PlanLiveUpdate(prev, next)
	if !ok || !update.Empty() {
		t.Fatalf("PlanLiveUpdate = (%+v, %v), want an empty live update", update, ok)
	}
}

// liveUpdateSessionAtVersion builds a live-update test session whose CLI has
// reported a version, standing in for the `system/init` the fake script does
// not emit.
func liveUpdateSessionAtVersion(t *testing.T, cfg Config, version string) (*Session, string) {
	t.Helper()
	s, capturePath := liveUpdateTestSession(t, cfg)
	s.noteCLIVersion(version)
	return s, capturePath
}

func capturedSetModel(t *testing.T, line string) (model, systemPrompt, subtype string) {
	t.Helper()
	var captured struct {
		Type    string `json:"type"`
		Request struct {
			Subtype      string `json:"subtype"`
			Model        string `json:"model"`
			SystemPrompt string `json:"system_prompt"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(line), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "control_request" {
		t.Fatalf("captured type = %q, want control_request", captured.Type)
	}
	return captured.Request.Model, captured.Request.SystemPrompt, captured.Request.Subtype
}

// TestApplyLiveUpdateSystemPromptResendsCurrentModel pins the prompt-only
// case. set_model has no prompt-only form, so AO must re-send the model the
// session is already running — EXACTLY as last accepted, `[1m]` marker
// included. Sending a marker-trimmed id would silently drop the session out
// of the extended-context tier as a side effect of editing a prompt.
func TestApplyLiveUpdateSystemPromptResendsCurrentModel(t *testing.T) {
	s, capturePath := liveUpdateSessionAtVersion(t, Config{
		BasePermissionMode: "default",
		Model:              "claude-fable-5[1m]",
	}, "2.1.237")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.ApplyLiveUpdate(ctx, LiveUpdate{SystemPrompt: "new prompt"}, nil); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	model, prompt, subtype := capturedSetModel(t, lines[0])
	if subtype != "set_model" {
		t.Fatalf("subtype = %q, want set_model", subtype)
	}
	if model != "claude-fable-5[1m]" {
		t.Fatalf("model = %q, want the session's current model verbatim", model)
	}
	if prompt != "new prompt" {
		t.Fatalf("system_prompt = %q, want the new prompt", prompt)
	}
}

// TestApplyLiveUpdateSystemPromptRidesOneSetModel pins that a combined
// model + prompt update is ONE request. Two requests would leave a window in
// which the prompt had landed on the old model, and the CLI applies the
// prompt only when it accepts the model — so splitting them also splits the
// atomicity the accept/reject rule depends on.
func TestApplyLiveUpdateSystemPromptRidesOneSetModel(t *testing.T) {
	s, capturePath := liveUpdateSessionAtVersion(t, Config{
		BasePermissionMode: "default",
		Model:              "claude-sonnet-5",
	}, "2.1.237")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-fable-5", SystemPrompt: "new prompt"}, nil)
	if err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	if len(lines) != 1 {
		t.Fatalf("captured %d lines, want exactly one set_model: %v", len(lines), lines)
	}
	model, prompt, _ := capturedSetModel(t, lines[0])
	if model != "claude-fable-5" || prompt != "new prompt" {
		t.Fatalf("captured set_model model=%q system_prompt=%q, want the new model and prompt together", model, prompt)
	}
}

// TestApplyLiveUpdateSystemPromptRequiresNewEnoughCLI pins the version gate.
// The field's own schema doc says older builds ACK SUCCESS WITHOUT APPLYING
// IT — the one failure mode with no wire signal at all — so an unknown or
// too-old version must take the restart path before any byte reaches stdin.
func TestApplyLiveUpdateSystemPromptRequiresNewEnoughCLI(t *testing.T) {
	for _, version := range []string{"", "2.1.213", "1.0", "garbage"} {
		t.Run("version="+version, func(t *testing.T) {
			s, capturePath := liveUpdateSessionAtVersion(t, Config{
				BasePermissionMode: "default",
				Model:              "claude-sonnet-5",
			}, version)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{SystemPrompt: "new prompt"}, nil)
			if !errors.Is(err, ErrLiveUpdateRequiresRestart) {
				t.Fatalf("ApplyLiveUpdate error = %v, want ErrLiveUpdateRequiresRestart", err)
			}
			time.Sleep(100 * time.Millisecond)
			if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
				t.Fatalf("expected no stdin writes on the restart path, captured: %s", data)
			}
		})
	}
}

// TestApplyLiveUpdateSystemPromptRefusedWithoutAModel pins the other
// pre-send refusal: set_model must name a model and a session that cannot
// state its own has nothing to carry the prompt on. The restart is what
// grounds it.
func TestApplyLiveUpdateSystemPromptRefusedWithoutAModel(t *testing.T) {
	s, capturePath := liveUpdateSessionAtVersion(t, Config{BasePermissionMode: "default"}, "2.1.237")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{SystemPrompt: "new prompt"}, nil)
	if !errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want ErrLiveUpdateRequiresRestart", err)
	}
	time.Sleep(100 * time.Millisecond)
	if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
		t.Fatalf("expected no stdin writes, captured: %s", data)
	}
}

// TestApplyLiveUpdateSystemPromptRejectedSetModel pins the rejection rule.
// The CLI stores the prompt only AFTER the model passes its recognized /
// allowed checks, so a rejected model means the prompt did not apply either.
// The apply must surface a plain error (the reconciler's live-update failure
// state → restart), never ErrLiveUpdateRequiresRestart's quiet path and never
// a partially-committed configModel.
func TestApplyLiveUpdateSystemPromptRejectedSetModel(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"Model not allowed: claude-fable-5"}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	cfg := Config{
		Binary:             scriptPath,
		BasePermissionMode: "default",
		Model:              "claude-sonnet-5",
		Env:                map[string]string{"CAPTURE_FILE": capturePath},
	}
	s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.controlRequestTimeout = 2 * time.Second
	s.noteCLIVersion("2.1.237")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-fable-5", SystemPrompt: "new prompt"}, nil)
	if err == nil {
		t.Fatal("ApplyLiveUpdate accepted a rejected set_model")
	}
	if errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("error = %v, want a wire failure rather than the quiet restart signal", err)
	}
	// The rejection is model-shaped on the wire but decides BOTH axes; the
	// session must still be running its previous model.
	s.configModelMu.Lock()
	model := s.configModel
	s.configModelMu.Unlock()
	if model != "claude-sonnet-5" {
		t.Fatalf("configModel = %q after a rejected set_model, want the previous model", model)
	}
}

// TestClaudeCLIVersionAtLeast pins the comparison, including the two forms
// the version reaches us in: bare (`system/init.claude_code_version`) and
// suffixed (`claude --version` prints "2.1.237 (Claude Code)").
func TestClaudeCLIVersionAtLeast(t *testing.T) {
	tests := []struct {
		have string
		want bool
	}{
		{"2.1.214", true},
		{"2.1.219", true},
		{"2.1.237", true},
		{"2.1.237 (Claude Code)", true},
		{"2.2.0", true},
		{"3.0.0", true},
		{"2.1.213", false},
		{"2.0.999", false},
		{"1.9.9", false},
		{"1.0", false},
		{"", false},
		{"garbage", false},
		{"2.1", false},
	}
	for _, tc := range tests {
		if got := claudeCLIVersionAtLeast(tc.have, minLiveSystemPromptCLIVersion); got != tc.want {
			t.Errorf("claudeCLIVersionAtLeast(%q, %q) = %v, want %v",
				tc.have, minLiveSystemPromptCLIVersion, got, tc.want)
		}
	}
}

// TestSessionCLIVersionFromWireInit ties the version gate to the wire: the
// gate is only conservative-by-default if the version actually arrives, and
// `system/init.claude_code_version` is the sole in-session source.
func TestSessionCLIVersionFromWireInit(t *testing.T) {
	line := fixtureLines(t, effortLiveFixture)[2]
	events, err := (&Parser{}).ParseLine("thread-1", line)
	if err != nil {
		t.Fatalf("ParseLine(init): %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventInit {
		t.Fatalf("events = %+v, want one init", events)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	s := &Session{}
	s.noteCLIVersion(info.Version)
	if got := s.CLIVersion(); got != "2.1.219" {
		t.Fatalf("CLIVersion = %q, want the fixture's 2.1.219", got)
	}
	if !s.supportsLiveSystemPrompt() {
		t.Fatal("2.1.219 must clear the live-system-prompt floor")
	}
	// A later init that omits the key must not un-learn the version.
	s.noteCLIVersion("")
	if got := s.CLIVersion(); got != "2.1.219" {
		t.Fatalf("CLIVersion = %q after an empty note, want it unchanged", got)
	}
}
