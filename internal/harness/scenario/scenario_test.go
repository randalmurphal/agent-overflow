package scenario

import (
	"strings"
	"testing"
)

func validScenarioJSON() string {
	return `{
		"version": 1,
		"name": "test",
		"provider": "claude",
		"onStart": [
			{"emit": {"lines": ["{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"${SESSION_ID}\"}"]}}
		],
		"turns": [
			{"label": "greeting", "steps": [
				{"emit": {"lines": ["{\"type\":\"result\"}"], "delayBetweenMs": 5}},
				{"delayMs": 10},
				{"writeFile": {"path": "src/a.txt", "content": "hello"}},
				{"approval": {"toolName": "Bash", "onAllow": [{"emit": {"lines": ["ok"]}}], "onDeny": [{"exit": {"code": 0}}]}}
			]}
		]
	}`
}

func TestParseValidScenario(t *testing.T) {
	s, err := Parse([]byte(validScenarioJSON()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Name != "test" || len(s.Turns) != 1 || len(s.Turns[0].Steps) != 4 {
		t.Fatalf("unexpected scenario shape: %+v", s)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"wrong version":     `{"version": 2, "name": "x", "provider": "claude", "turns": [{"steps":[{"delayMs":1}]}]}`,
		"bad provider":      `{"version": 1, "name": "x", "provider": "gemini", "turns": [{"steps":[{"delayMs":1}]}]}`,
		"empty name":        `{"version": 1, "name": "", "provider": "claude", "turns": [{"steps":[{"delayMs":1}]}]}`,
		"no turns/onStart":  `{"version": 1, "name": "x", "provider": "claude", "turns": []}`,
		"empty turn":        `{"version": 1, "name": "x", "provider": "claude", "turns": [{"steps": []}]}`,
		"two actions":       `{"version": 1, "name": "x", "provider": "claude", "turns": [{"steps":[{"delayMs":1,"exit":{"code":0}}]}]}`,
		"zero actions":      `{"version": 1, "name": "x", "provider": "claude", "turns": [{"steps":[{}]}]}`,
		"empty emit":        `{"version": 1, "name": "x", "provider": "claude", "turns": [{"steps":[{"emit":{"lines":[]}}]}]}`,
		"fixture no path":   `{"version": 1, "name": "x", "provider": "claude", "turns": [{"steps":[{"fixture":{"path":""}}]}]}`,
		"fixture bad range": `{"version": 1, "name": "x", "provider": "claude", "turns": [{"steps":[{"fixture":{"path":"f","fromLine":5,"toLine":2}}]}]}`,
		"bad afterTurns":    `{"version": 1, "name": "x", "provider": "claude", "afterTurns": "loop", "turns": [{"steps":[{"delayMs":1}]}]}`,
		"unknown field":     `{"version": 1, "name": "x", "provider": "claude", "turnz": [], "turns": [{"steps":[{"delayMs":1}]}]}`,
		"bad approval sub":  `{"version": 1, "name": "x", "provider": "claude", "turns": [{"steps":[{"approval":{"toolName":"Bash","onAllow":[{}]}}]}]}`,
	}
	for label, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: Parse accepted invalid scenario", label)
		}
	}
}

func TestVarsSubstitute(t *testing.T) {
	vars := Vars{"SESSION_ID": "s-1", "TURN": "3"}
	got := vars.Substitute(`{"id":"${SESSION_ID}","turn":${TURN},"keep":"${UNKNOWN}"}`)
	want := `{"id":"s-1","turn":3,"keep":"${UNKNOWN}"}`
	if got != want {
		t.Fatalf("Substitute = %q, want %q", got, want)
	}
	// No-op fast path must not mangle plain lines.
	plain := `{"type":"result"}`
	if vars.Substitute(plain) != plain {
		t.Fatal("Substitute changed a var-free line")
	}
}

func TestValidateErrorsNameTheLocation(t *testing.T) {
	doc := `{"version": 1, "name": "loc", "provider": "claude", "turns": [
		{"steps":[{"delayMs":1}]},
		{"steps":[{"delayMs":1},{"emit":{"lines":[]}}]}
	]}`
	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "turn 2 step 2") {
		t.Fatalf("error %q does not locate the bad step", err)
	}
}

// TestRepeatStepValidation covers the repeat step's three refusals. The
// pacing rule is the load-bearing one: an unbounded loop with no waiting
// child is an unthrottled writer, and a soak scenario is exactly where
// nobody would notice for hours.
func TestRepeatStepValidation(t *testing.T) {
	emit := Step{Emit: &EmitStep{Lines: []string{`{"a":1}`}}}
	cases := []struct {
		name    string
		step    Step
		wantErr string
	}{
		{"empty body", Step{Repeat: &RepeatStep{Count: 2}}, "steps must be non-empty"},
		{"invalid child", Step{Repeat: &RepeatStep{Count: 2, Steps: []Step{{}}}}, "repeat step 1"},
		{"unpaced infinite", Step{Repeat: &RepeatStep{Count: 0, Steps: []Step{emit}}}, "pacing step"},
		{"two actions", Step{DelayMs: 5, Repeat: &RepeatStep{Count: 1, Steps: []Step{emit}}}, "exactly one action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step := tc.step
			err := step.validate()
			if err == nil {
				t.Fatalf("validate accepted %+v", tc.step)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}

	paced := Step{Repeat: &RepeatStep{Count: 0, Steps: []Step{{DelayMs: 100}, emit}}}
	if err := paced.validate(); err != nil {
		t.Fatalf("paced infinite repeat rejected: %v", err)
	}
	bounded := Step{Repeat: &RepeatStep{Count: 3, Steps: []Step{emit}}}
	if err := bounded.validate(); err != nil {
		t.Fatalf("bounded repeat without pacing rejected: %v", err)
	}
	// An emit that paces itself between lines counts.
	selfPaced := Step{Repeat: &RepeatStep{Count: 0, Steps: []Step{
		{Emit: &EmitStep{Lines: []string{`{"a":1}`, `{"b":2}`}, DelayBetweenMs: 50}},
	}}}
	if err := selfPaced.validate(); err != nil {
		t.Fatalf("self-paced infinite repeat rejected: %v", err)
	}
}

// TestRepeatFixturePathsAreCollected keeps the fail-at-set-time contract
// working through a loop body: a missing fixture inside a repeat must be
// caught by HarnessSetScenario, not by a mock skipping frames for hours.
func TestRepeatFixturePathsAreCollected(t *testing.T) {
	s := &Scenario{
		Version: CurrentVersion, Name: "repeat-fixtures", Provider: ProviderClaude,
		Turns: []Turn{{Steps: []Step{{Repeat: &RepeatStep{Count: 2, Steps: []Step{
			{Fixture: &FixtureStep{Path: "nested/inside-repeat.ndjson"}},
		}}}}}},
	}
	paths := s.FixturePaths()
	if len(paths) != 1 || paths[0] != "nested/inside-repeat.ndjson" {
		t.Fatalf("FixturePaths = %v, want the path nested inside the repeat", paths)
	}
}
