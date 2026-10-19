package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

var testSessionStartHooks = []provider.SessionStartHook{
	{Label: "instructions.md", Command: `'/opt/a o/agent-overflow' __agent-home-context 'instructions.md' '/h/it'\''s'`, Heading: "## Your standing instructions"},
	{Label: "MEMORY.md", Command: `'/opt/a o/agent-overflow' __agent-home-context 'MEMORY.md' '/h/it'\''s'`, Heading: "## Your memory index"},
}

func settingsArg(t *testing.T, args []string) map[string]any {
	t.Helper()
	var found []string
	for i, arg := range args {
		if arg == "--settings" && i+1 < len(args) {
			found = append(found, args[i+1])
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one --settings flag, got %d in %v", len(found), args)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(found[0]), &settings); err != nil {
		t.Fatalf("--settings %q: %v", found[0], err)
	}
	return settings
}

// The hooks ride the one inline settings block beside its other keys, as a
// SessionStart group for start, compaction and clear with a command handler
// per hook, and a session without hooks carries no hooks key.
func TestBuildArgsRegistersSessionStartHooksInTheSettingsBlock(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", Model: "claude-opus-4-7", FastMode: true})
	if _, ok := settingsArg(t, buildArgs(cfg, "", ""))["hooks"]; ok {
		t.Fatal("a session without hooks carries a hooks key")
	}
	cfg.SessionStartHooks = testSessionStartHooks
	settings := settingsArg(t, buildArgs(cfg, "", ""))
	if settings["fastMode"] != true || settings["crossSessionInbound"] != "refuse" {
		t.Fatalf("the hooks displaced the block's other keys: %v", settings)
	}
	got, _ := json.Marshal(settings["hooks"])
	want, _ := json.Marshal(map[string]any{"SessionStart": []any{map[string]any{
		"matcher": "startup|compact|clear",
		"hooks": []any{
			map[string]any{"type": "command", "command": testSessionStartHooks[0].Command, "timeout": 60},
			map[string]any{"type": "command", "command": testSessionStartHooks[1].Command, "timeout": 60},
		},
	}}})
	if string(got) != string(want) {
		t.Fatalf("hooks = %s\nwant    %s", got, want)
	}

	// Hooks alone are enough to render the block.
	if settings := settingsArg(t, buildArgs(Config{Model: "claude-opus-4-7", SessionStartHooks: testSessionStartHooks}, "", "")); settings["hooks"] == nil {
		t.Fatalf("settings without the hooks: %v", settings)
	}
}

func sessionStartParser(t *testing.T) *Parser {
	t.Helper()
	p := NewParser()
	delivery, err := provider.NewSessionStartDelivery(testSessionStartHooks)
	if err != nil {
		t.Fatalf("NewSessionStartDelivery: %v", err)
	}
	p.sessionStart = delivery
	return p
}

func hookResponseLine(source, stdout string, exitCode int) []byte {
	return hookResponseLineWith(source, stdout, "", exitCode)
}

func hookResponseLineWith(source, stdout, stderr string, exitCode int) []byte {
	line, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "hook_response", "hook_id": "h-1",
		"hook_name": "SessionStart:" + source, "hook_event": "SessionStart",
		"output": stdout + stderr, "stdout": stdout, "stderr": stderr, "exit_code": exitCode, "outcome": "success",
		"uuid": "u-1", "session_id": "s-1",
	})
	return line
}

func hookContextStdout(context string) string {
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": context}})
	return string(out) + "\n"
}

const (
	sessionStartInitLine    = `{"type":"system","subtype":"init","session_id":"s-1","model":"claude-opus-4-7","cwd":"/tmp","tools":[]}`
	sessionStartCompactLine = `{"type":"system","subtype":"compact_boundary","uuid":"c-1","content":"Conversation compacted","compactMetadata":{"trigger":"auto"}}`
	sessionStartResultLine  = `{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","session_id":"s-1","result":"ok"}`
)

var (
	instructionsDelivered = hookContextStdout("## Your standing instructions\n\nRole")
	memoryDelivered       = hookContextStdout("## Your memory index\n\n- a")
)

func parseLines(t *testing.T, p *Parser, lines ...[]byte) []provider.ProviderEvent {
	t.Helper()
	var events []provider.ProviderEvent
	for _, line := range lines {
		got, err := p.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		events = append(events, got...)
	}
	return events
}

// warnings are the non-fatal error rows among events, with the index of each.
func sessionStartWarnings(events []provider.ProviderEvent) (warnings []string, at []int) {
	for i, event := range events {
		if event.Kind == provider.EventError && strings.Contains(event.Content, "did not reach this session") {
			var meta map[string]any
			if json.Unmarshal(event.Meta, &meta) != nil || meta["fatal"] != false {
				return append(warnings, "fatal: "+event.Content), append(at, i)
			}
			warnings, at = append(warnings, event.Content), append(at, i)
		}
	}
	return warnings, at
}

func kindIndex(events []provider.ProviderEvent, kind provider.EventKind) int {
	for i, event := range events {
		if event.Kind == kind {
			return i
		}
	}
	return -1
}

// A fresh start's hooks report before the first init, which settles them.
func TestParserSettlesTheStartAtInit(t *testing.T) {
	p := sessionStartParser(t)
	p.sessionStart.Expect(provider.SessionStartAfterStart)
	events := parseLines(t, p,
		hookResponseLine("startup", instructionsDelivered, 0),
		hookResponseLine("startup", memoryDelivered, 0),
		[]byte(sessionStartInitLine), []byte(sessionStartResultLine))
	if warnings, _ := sessionStartWarnings(events); len(warnings) > 0 {
		t.Fatalf("warnings after both hooks delivered: %v", warnings)
	}
	for _, event := range events {
		if event.Kind == provider.EventNotification || strings.Contains(event.Content, "Your memory index") {
			t.Fatalf("a hook response rendered: %+v", event)
		}
	}

	p = sessionStartParser(t)
	p.sessionStart.Expect(provider.SessionStartAfterStart)
	events = parseLines(t, p,
		hookResponseLine("startup", memoryDelivered, 0),
		[]byte(sessionStartInitLine))
	warnings, at := sessionStartWarnings(events)
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "instructions.md did not reach this session after its start: ") ||
		at[0] < kindIndex(events, provider.EventInit) {
		t.Fatalf("events = %+v", events)
	}
	// The next init and result have nothing to settle.
	if warnings, _ := sessionStartWarnings(parseLines(t, p, []byte(sessionStartInitLine), []byte(sessionStartResultLine))); len(warnings) > 0 {
		t.Fatalf("settled twice: %v", warnings)
	}
}

// A compaction's hooks report before its boundary, which settles them.
func TestParserSettlesACompactionAtItsBoundary(t *testing.T) {
	p := sessionStartParser(t)
	events := parseLines(t, p,
		[]byte(sessionStartInitLine),
		hookResponseLine("compact", instructionsDelivered, 0),
		hookResponseLine("compact", memoryDelivered, 0),
		[]byte(sessionStartCompactLine), []byte(sessionStartResultLine))
	if warnings, _ := sessionStartWarnings(events); len(warnings) > 0 {
		t.Fatalf("warnings after both hooks delivered: %v", warnings)
	}

	events = parseLines(t, p, []byte(sessionStartCompactLine))
	warnings, at := sessionStartWarnings(events)
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "instructions.md and MEMORY.md did not reach this session after its compaction: ") ||
		at[0] < kindIndex(events, provider.EventCompactBoundary) {
		t.Fatalf("events = %+v", events)
	}
	if warnings, _ := sessionStartWarnings(parseLines(t, p, []byte(sessionStartResultLine))); len(warnings) > 0 {
		t.Fatalf("the result settled the compaction again: %v", warnings)
	}

	// A start delivered before the first init does not count for a
	// compaction in the first turn.
	p = sessionStartParser(t)
	p.sessionStart.Expect(provider.SessionStartAfterStart)
	events = parseLines(t, p,
		hookResponseLine("startup", instructionsDelivered, 0),
		hookResponseLine("startup", memoryDelivered, 0),
		[]byte(sessionStartInitLine), []byte(sessionStartCompactLine), []byte(sessionStartResultLine))
	if warnings, _ := sessionStartWarnings(events); len(warnings) != 1 || !strings.Contains(warnings[0], "after its compaction") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// Each compaction of a turn answers for its own hooks: the first one's
// delivery does not cover the second, which dropped it from the context.
func TestParserSettlesEachCompactionOfATurn(t *testing.T) {
	p := sessionStartParser(t)
	events := parseLines(t, p,
		[]byte(sessionStartInitLine),
		hookResponseLine("compact", instructionsDelivered, 0),
		hookResponseLine("compact", memoryDelivered, 0),
		[]byte(sessionStartCompactLine),
		hookResponseLine("compact", instructionsDelivered, 0),
		[]byte(sessionStartCompactLine),
		[]byte(sessionStartResultLine))
	var boundaries []int
	for i, event := range events {
		if event.Kind == provider.EventCompactBoundary {
			boundaries = append(boundaries, i)
		}
	}
	warnings, at := sessionStartWarnings(events)
	if len(boundaries) != 2 || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "MEMORY.md did not reach this session after its compaction: ") ||
		at[0] < boundaries[1] {
		t.Fatalf("events = %+v", events)
	}
}

// Only a successful SessionStart response whose context opens with a hook's
// heading counts, from JSON output or plain stdout.
func TestParserCountsOnlyTheHooksSuccessfulOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		line []byte
		want bool
	}{
		"json":            {hookResponseLine("compact", memoryDelivered, 0), true},
		"plain":           {hookResponseLine("compact", "## Your memory index\n\n- a\n", 0), true},
		"failed":          {hookResponseLine("compact", memoryDelivered, 1), false},
		"other hook":      {hookResponseLine("compact", hookContextStdout("user context"), 0), false},
		"heading later":   {hookResponseLine("compact", hookContextStdout("x ## Your memory index"), 0), false},
		"bad json":        {hookResponseLine("compact", `{"hookSpecificOutput":`, 0), false},
		"no exit code":    {[]byte(`{"type":"system","subtype":"hook_response","hook_event":"SessionStart","stdout":"## Your memory index"}`), false},
		"other event":     {[]byte(`{"type":"system","subtype":"hook_response","hook_event":"UserPromptSubmit","stdout":"## Your memory index","exit_code":0}`), false},
		"started, not ok": {[]byte(`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:compact","hook_event":"SessionStart"}`), false},
	} {
		p := sessionStartParser(t)
		if events := parseLines(t, p, tc.line); len(events) > 0 {
			t.Errorf("%s: the frame rendered %+v", name, events)
		}
		warnings, _ := sessionStartWarnings(parseLines(t, p, []byte(sessionStartCompactLine)))
		if delivered := len(warnings) == 1 && strings.HasPrefix(warnings[0], "instructions.md did not reach"); delivered != tc.want {
			t.Errorf("%s: warnings = %v, want MEMORY.md delivered %v", name, warnings, tc.want)
		}
	}
}

// A failed run's exit code and error output, or its outcome when it has no
// exit code, explain the warning.
func TestParserQuotesAFailedRun(t *testing.T) {
	for name, tc := range map[string]struct {
		line []byte
		want string
	}{
		"exit code": {hookResponseLineWith("compact", "", "sh: /gone/agent-overflow: not found\n", 127), "a SessionStart hook failed (exit code 127: sh: /gone/agent-overflow: not found)."},
		"cancelled": {[]byte(`{"type":"system","subtype":"hook_response","hook_event":"SessionStart","outcome":"cancelled"}`), "a SessionStart hook failed (outcome cancelled)."},
		"bare":      {[]byte(`{"type":"system","subtype":"hook_response","hook_event":"SessionStart"}`), "a SessionStart hook failed (no exit code)."},
	} {
		p := sessionStartParser(t)
		warnings, _ := sessionStartWarnings(parseLines(t, p, tc.line, hookResponseLine("compact", instructionsDelivered, 0), []byte(sessionStartCompactLine)))
		if len(warnings) != 1 || warnings[0] != "MEMORY.md did not reach this session after its compaction: "+tc.want {
			t.Errorf("%s: warnings = %v", name, warnings)
		}
	}
}

// A session without hooks warns about nothing and a hook response is inert.
func TestParserWithoutSessionStartHooks(t *testing.T) {
	p := NewParser()
	events := parseLines(t, p,
		hookResponseLine("startup", hookContextStdout("## Your memory index"), 1),
		[]byte(sessionStartInitLine), []byte(sessionStartCompactLine), []byte(sessionStartResultLine))
	if warnings, _ := sessionStartWarnings(events); len(warnings) > 0 || kindIndex(events, provider.EventTurnComplete) < 0 {
		t.Fatalf("events = %+v", events)
	}
}

// The session arms the start only when it starts fresh: a resume runs no
// hooks (provider.SessionStartHookSources). Hooks the delivery cannot tell
// apart are refused before anything spawns.
func TestNewSessionExpectsTheHooksOnlyOnAFreshStart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		resume string
		want   bool
	}{"fresh": {"", true}, "resume": {"sess-1", false}} {
		binary := mockexec.Write(t, t.TempDir()+"/mock-claude", "#!/bin/sh\nexec cat\n")
		events := make(chan provider.ProviderEvent, 100)
		s, err := NewSession(context.Background(), testThread, Config{Binary: binary, Resume: tc.resume, SessionStartHooks: testSessionStartHooks},
			func(event provider.ProviderEvent) { events <- event })
		if err != nil {
			t.Fatalf("%s: NewSession: %v", name, err)
		}
		for _, line := range []string{sessionStartInitLine, sessionStartResultLine} {
			if err := s.proc.WriteLine([]byte(line)); err != nil {
				t.Fatalf("%s: write: %v", name, err)
			}
		}
		var warned bool
		for evt := waitEvent(t, events); evt.Kind != provider.EventTurnComplete; evt = waitEvent(t, events) {
			if evt.Kind == provider.EventError {
				if warned || !strings.HasPrefix(evt.Content, "instructions.md and MEMORY.md did not reach this session after its start") {
					t.Fatalf("%s: error %+v", name, evt)
				}
				warned = true
			}
		}
		if warned != tc.want {
			t.Errorf("%s: warned %v, want %v", name, warned, tc.want)
		}
		if err := s.Close(); err != nil {
			t.Errorf("%s: Close: %v", name, err)
		}
	}

	binary := mockexec.Write(t, t.TempDir()+"/mock-claude", "#!/bin/sh\nexit 1\n")
	if _, err := NewSession(context.Background(), testThread, Config{Binary: binary, SessionStartHooks: []provider.SessionStartHook{{Label: "a", Command: "a"}}},
		func(provider.ProviderEvent) {}); err == nil || !strings.Contains(err.Error(), "heading are all required") {
		t.Fatalf("NewSession with a hook without a heading: %v", err)
	}
}
