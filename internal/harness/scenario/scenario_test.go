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

// TestStartupDelayValidation: the field is a deliberate stall, so its
// bounds are what stop a typo (a stray zero, a negative) from becoming a
// harness run that looks hung rather than slow.
func TestStartupDelayValidation(t *testing.T) {
	doc := func(ms string) string {
		return `{"version":1,"name":"x","provider":"claude","startupDelayMs":` + ms +
			`,"turns":[{"steps":[{"delayMs":1}]}]}`
	}
	for _, ms := range []string{"0", "1", "30000"} {
		if _, err := Parse([]byte(doc(ms))); err != nil {
			t.Errorf("startupDelayMs %s was rejected: %v", ms, err)
		}
	}
	for _, ms := range []string{"-1", "30001", "600000"} {
		if _, err := Parse([]byte(doc(ms))); err == nil {
			t.Errorf("startupDelayMs %s was accepted; the cap is %d", ms, MaxStartupDelayMs)
		}
	}
}

// TestCoalesceIsExclusiveWithPacing: coalesce says "one write", the
// pacing knobs say "several writes, spread out". A scenario asking for
// both has no meaning, and silently honouring one of them is how a test
// ends up asserting against delivery it never actually got.
func TestCoalesceIsExclusiveWithPacing(t *testing.T) {
	step := func(extra string) string {
		return `{"version":1,"name":"x","provider":"claude","turns":[{"steps":[{"emit":{"lines":["a","b"],"coalesce":true` +
			extra + `}}]}]}`
	}
	if _, err := Parse([]byte(step(""))); err != nil {
		t.Fatalf("plain coalesce was rejected: %v", err)
	}
	for label, extra := range map[string]string{
		"delayBetweenMs":  `,"delayBetweenMs":5`,
		"chunkBytes":      `,"chunkBytes":8`,
		"chunkIntervalMs": `,"chunkIntervalMs":5`,
	} {
		if _, err := Parse([]byte(step(extra))); err == nil {
			t.Errorf("coalesce + %s was accepted; they contradict each other", label)
		}
	}
}

// TestProviderVersionValidation: the value is pasted into a userAgent
// string and into Claude's system/init, both of which are PARSED on the
// other side. Anything that is not a dotted number would read as an
// unparseable version rather than as the downgrade the author meant.
func TestProviderVersionValidation(t *testing.T) {
	doc := func(v string) string {
		return `{"version":1,"name":"x","provider":"codex","providerVersion":` + v +
			`,"turns":[{"steps":[{"delayMs":1}]}]}`
	}
	for _, v := range []string{`""`, `"0.147.0"`, `"99"`, `"1.2.3.4"`} {
		if _, err := Parse([]byte(doc(v))); err != nil {
			t.Errorf("providerVersion %s was rejected: %v", v, err)
		}
	}
	for _, v := range []string{`"v0.147.0"`, `" 0.147.0"`, `"0.147.0-beta"`, `"latest"`} {
		if _, err := Parse([]byte(doc(v))); err == nil {
			t.Errorf("providerVersion %s was accepted; it is not a dotted version", v)
		}
	}
}

// TestNewFieldsDefaultToOff: every field added here is opt-in, and the
// library scenarios (plus every scenario anyone has already written)
// carry none of them.
func TestNewFieldsDefaultToOff(t *testing.T) {
	s, err := Parse([]byte(validScenarioJSON()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.StartupDelayMs != 0 || s.ProviderVersion != "" {
		t.Fatalf("scenario defaults changed: startupDelayMs=%d providerVersion=%q", s.StartupDelayMs, s.ProviderVersion)
	}
	for _, turn := range s.Turns {
		for _, st := range turn.Steps {
			if st.Emit != nil && st.Emit.Coalesce {
				t.Fatal("emit defaulted to coalesce")
			}
		}
	}
}
