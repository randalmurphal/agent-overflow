package claudetui

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// parseEnvelope feeds one reconstructed envelope through the real claude.Parser
// (fresh per call unless a parser is supplied) and returns its events.
func parseEnvelope(t *testing.T, p *claude.Parser, env json.RawMessage) []provider.ProviderEvent {
	t.Helper()
	if p == nil {
		p = claude.NewParser()
	}
	evs, err := p.ParseLine(testThread, env)
	if err != nil {
		t.Fatalf("ParseLine(%s): %v", env, err)
	}
	return evs
}

// TestPostToolUseEnvelope proves a successful PostToolUse reconstructs a
// tool_result that parse_user turns into an EventToolComplete carrying both the
// display text (from stdout) and the structured enrichment (exit_code).
func TestPostToolUseEnvelope(t *testing.T) {
	p := hookPayload{
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
		ToolUseID:     "toolu_1",
		ToolResponse:  json.RawMessage(`{"stdout":"file1\nfile2","stderr":"","exit_code":0}`),
	}
	events := parseEnvelope(t, nil, postToolUseEnvelope(p))

	completes := findKind(events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("expected 1 tool-complete, got %d (%v)", len(completes), kindsOf(events))
	}
	c := completes[0]
	if c.ItemID != "toolu_1" {
		t.Errorf("tool-complete ItemID = %q, want toolu_1", c.ItemID)
	}
	if !strings.Contains(c.Content, "file1") {
		t.Errorf("tool-complete content missing stdout: %q", c.Content)
	}
	if !strings.Contains(string(c.Meta), "exit_code") {
		t.Errorf("tool-complete meta missing exit_code enrichment: %s", c.Meta)
	}
}

// TestPostToolUseFailureEnvelope proves a PostToolUseFailure (no tool_response)
// still completes the tool, flagged is_error so triage renders a failure.
func TestPostToolUseFailureEnvelope(t *testing.T) {
	p := hookPayload{
		HookEventName: "PostToolUseFailure",
		ToolName:      "Bash",
		ToolUseID:     "toolu_2",
		Error:         "Exit code 3\nOOPS-STDERR",
	}
	events := parseEnvelope(t, nil, postToolUseFailureEnvelope(p))

	completes := findKind(events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("expected 1 tool-complete, got %d (%v)", len(completes), kindsOf(events))
	}
	c := completes[0]
	if c.ItemID != "toolu_2" {
		t.Errorf("tool-complete ItemID = %q, want toolu_2", c.ItemID)
	}
	if !strings.Contains(c.Content, "OOPS-STDERR") {
		t.Errorf("failure content missing error text: %q", c.Content)
	}
}

// TestAskUserQuestionControlRequest proves the PreToolUse AskUserQuestion
// payload reconstructs the can_use_tool control_request parse_control turns into
// an EventUserInputRequest carrying the question, keyed by the relay's
// request_id.
func TestAskUserQuestionControlRequest(t *testing.T) {
	p := hookPayload{
		HookEventName: "PreToolUse",
		ToolName:      "AskUserQuestion",
		ToolUseID:     "toolu_q",
		ToolInput: json.RawMessage(`{"questions":[{"question":"Pick a file","header":"File",` +
			`"options":[{"label":"alpha.txt","description":"a"},{"label":"beta.txt","description":"b"}]}]}`),
	}
	events := parseEnvelope(t, nil, askUserQuestionControlRequest("req_42", p))

	reqs := findKind(events, provider.EventUserInputRequest)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 user-input request, got %d (%v)", len(reqs), kindsOf(events))
	}
	if reqs[0].ItemID != "req_42" {
		t.Errorf("user-input request ItemID = %q, want req_42", reqs[0].ItemID)
	}
	var ui provider.UserInputRequest
	if err := json.Unmarshal(reqs[0].Meta, &ui); err != nil {
		t.Fatalf("decode UserInputRequest meta: %v", err)
	}
	if len(ui.Questions) != 1 || ui.Questions[0].Question != "Pick a file" {
		t.Fatalf("reconstructed questions = %+v", ui.Questions)
	}
	if len(ui.Questions[0].Options) != 2 || ui.Questions[0].Options[0].Label != "alpha.txt" {
		t.Errorf("reconstructed options = %+v", ui.Questions[0].Options)
	}
}
