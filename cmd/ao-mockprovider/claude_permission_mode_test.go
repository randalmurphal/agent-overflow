package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// The mock reports permission modes the way 2.1.300 does: a set_model to a
// model without auto mode, on an auto session, reports "default" before
// the ack; a set_permission_mode reports the new mode after its ack; a
// set_model to a model with auto mode reports nothing; init restates the
// mode. Every frame parses with the application's parser.
func TestClaudeReportsPermissionModeChanges(t *testing.T) {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "claude-permission-mode",
		Provider: scenario.ProviderClaude,
		Turns:    []scenario.Turn{{Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{`{"type":"system","subtype":"status","status":"requesting"}`}}}}}},
	}
	args := append(slices.Clone(claudeSessionArgs), "--permission-mode", "auto", "--model", "claude-opus-5-5")
	p := startMock(t, args, writeScenarioFile(t, sc, ""), t.TempDir())

	p.send(`{"type":"control_request","request_id":"m-1","request":{"subtype":"set_model","model":"claude-fable-5-1"}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"request_id":"m-1"`) {
		t.Fatalf("set_model to a model with auto mode wrote %q before its ack", got)
	}

	p.send(`{"type":"control_request","request_id":"m-2","request":{"subtype":"set_model","model":"claude-sonnet-4-6[1m]"}}`)
	status := p.expectLine(testTimeout)
	events, err := claude.NewParser().ParseLine("thread-mode", []byte(status))
	if err != nil || len(events) != 1 || events[0].Kind != provider.EventPermissionModeChanged {
		t.Fatalf("line before the set_model ack = %q (%+v, %v), want the permission mode report", status, events, err)
	}
	var meta provider.PermissionModeChangeMeta
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil || meta.Mode != "default" {
		t.Fatalf("reported %s, want default", events[0].Meta)
	}
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"request_id":"m-2"`) {
		t.Fatalf("set_model ack = %q", got)
	}

	p.send(`{"type":"control_request","request_id":"pm-0","request":{"subtype":"set_permission_mode","mode":"auto"}}`)
	// The application routes control responses by their leading type.
	if got := p.expectLine(testTimeout); !strings.HasPrefix(got, `{"type":"control_response"`) ||
		!strings.Contains(got, `"subtype":"error"`) || !strings.Contains(got, "auto mode unavailable for this model") ||
		!strings.Contains(got, `"error_code":"auto_mode_model"`) {
		t.Fatalf("set_permission_mode auto on a model without it = %q, want the refusal and its code", got)
	}

	p.send(`{"type":"control_request","request_id":"pm-1","request":{"subtype":"set_permission_mode","mode":"acceptEdits"}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"request_id":"pm-1"`) {
		t.Fatalf("set_permission_mode ack = %q", got)
	}
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"permissionMode":"acceptEdits"`) {
		t.Fatalf("line after the set_permission_mode ack = %q, want its report", got)
	}

	p.send(userLine)
	if init := p.expectLineContaining(`"subtype":"init"`, testTimeout); !strings.Contains(init, `"permissionMode":"acceptEdits"`) {
		t.Fatalf("init = %q, want the current mode restated", init)
	}
	p.expectLineContaining(`"status":"requesting"`, testTimeout)
	p.closeStdinAndExpectExit(0, testTimeout)
	parser := claude.NewParser()
	t.Cleanup(parser.Close)
	for _, line := range p.all {
		if strings.HasPrefix(line, `{"type":"control_response"`) {
			continue // the session reads acks before the parser
		}
		if _, err := parser.ParseLine("thread-mode", []byte(line)); err != nil {
			t.Fatalf("the application's parser rejected %q: %v", line, err)
		}
	}
}

// In plan the CLI keeps auto as the mode to return to: a set_model to a
// model without auto mode changes and reports nothing, and leaving plan for
// auto is refused with its code.
func TestClaudeModelSwitchInPlanKeepsAutoForLater(t *testing.T) {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "claude-permission-mode-plan",
		Provider: scenario.ProviderClaude,
		Turns:    []scenario.Turn{{Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{`{"type":"system","subtype":"status","status":"requesting"}`}}}}}},
	}
	args := append(slices.Clone(claudeSessionArgs), "--permission-mode", "auto", "--model", "claude-opus-5-5")
	p := startMock(t, args, writeScenarioFile(t, sc, ""), t.TempDir())

	p.send(`{"type":"control_request","request_id":"pm-plan","request":{"subtype":"set_permission_mode","mode":"plan"}}`)
	p.expectLineContaining(`"request_id":"pm-plan"`, testTimeout)
	p.expectLineContaining(`"permissionMode":"plan"`, testTimeout)

	p.send(`{"type":"control_request","request_id":"m-1","request":{"subtype":"set_model","model":"claude-sonnet-4-6"}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"request_id":"m-1"`) || !strings.Contains(got, `"subtype":"success"`) {
		t.Fatalf("set_model in plan wrote %q, want only its ack", got)
	}

	p.send(`{"type":"control_request","request_id":"pm-auto","request":{"subtype":"set_permission_mode","mode":"auto"}}`)
	if got := p.expectLine(testTimeout); !strings.HasPrefix(got, `{"type":"control_response"`) ||
		!strings.Contains(got, `"request_id":"pm-auto"`) || !strings.Contains(got, `"error_code":"auto_mode_model"`) {
		t.Fatalf("leaving plan for auto = %q, want the refusal with its code", got)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
}

// A process started on auto for a model without it runs default and says so
// only in system/init.
func TestClaudeStartedOnAutoForAModelWithoutItRunsDefault(t *testing.T) {
	if got := newClaudePermissionMode([]string{"--permission-mode", "auto", "--model", "claude-sonnet-4-6[1m]"}).current(); got != "default" {
		t.Fatalf("auto on claude-sonnet-4-6 starts on %q, want default", got)
	}
	if got := newClaudePermissionMode([]string{"--permission-mode", "auto", "--model", "claude-opus-5-5"}).current(); got != "auto" {
		t.Fatalf("auto on claude-opus-5-5 starts on %q, want auto", got)
	}
	if got := newClaudePermissionMode([]string{"--permission-mode", "auto"}).current(); got != "auto" {
		t.Fatalf("auto with no --model starts on %q, want auto", got)
	}
}

func TestClaudeModelSupportsAutoModeFollowsTheCLI(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-opus-5-5":       true,
		"claude-fable-5-1[1m]":  true,
		"claude-haiku-5-5":      true,
		"claude-sonnet-4-6":     false,
		"claude-opus-4-6[1m]":   false,
		"claude-haiku-4-5":      false,
		"claude-3-5-haiku-2024": false,
	} {
		if got := claudeModelSupportsAutoMode(model); got != want {
			t.Errorf("claudeModelSupportsAutoMode(%q) = %v, want %v", model, got, want)
		}
	}
}
