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
