package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

var testSessionStartHook = provider.SessionStartHook{
	Label: "the memory", Command: `'/opt/a o/agent-overflow' deliver '/h/it'\''s'`, PromptCommand: `'/opt/a o/agent-overflow' changes '/h/it'\''s'`,
	Heading: "## Memory", Ending: "(End of memory)", Stamp: "(Delivered at ", Failure: "deliver:",
}

// testHook is a copy of testSessionStartHook, so a test can change it.
func testHook() *provider.SessionStartHook {
	hook := testSessionStartHook
	return &hook
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

// The hook rides the one inline settings block beside its other keys, in a
// SessionStart group of its own for start, compaction and clear, and its
// PromptCommand in a UserPromptSubmit group; a session without the hook
// carries no hooks key.
func TestBuildArgsRegistersTheSessionStartHookInTheSettingsBlock(t *testing.T) {
	cfg := ConfigFromOptions(provider.SessionOptions{Provider: "claude", Model: "claude-opus-4-7", FastMode: true})
	if _, ok := settingsArg(t, buildArgs(cfg, "", ""))["hooks"]; ok {
		t.Fatal("a session without the hook carries a hooks key")
	}
	start := map[string]any{
		"matcher": "startup|compact|clear",
		"hooks":   []any{map[string]any{"type": "command", "command": testSessionStartHook.Command, "timeout": 60}},
	}
	prompt := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": testSessionStartHook.PromptCommand, "timeout": 60}}}
	for name, tc := range map[string]struct {
		prompt string
		want   map[string]any
	}{
		"with prompt command": {testSessionStartHook.PromptCommand, map[string]any{"SessionStart": []any{start}, "UserPromptSubmit": []any{prompt}}},
		"without":             {"", map[string]any{"SessionStart": []any{start}}},
	} {
		cfg.SessionStartHook = testHook()
		cfg.SessionStartHook.PromptCommand = tc.prompt
		settings := settingsArg(t, buildArgs(cfg, "", ""))
		if settings["fastMode"] != true || settings["crossSessionInbound"] != "refuse" {
			t.Fatalf("%s: the hook displaced the block's other keys: %v", name, settings)
		}
		got, _ := json.Marshal(settings["hooks"])
		want, _ := json.Marshal(tc.want)
		if string(got) != string(want) {
			t.Fatalf("%s: hooks = %s\nwant    %s", name, got, want)
		}
	}

	// The hook alone is enough to render the block.
	if settings := settingsArg(t, buildArgs(Config{Model: "claude-opus-4-7", SessionStartHook: testHook()}, "", "")); settings["hooks"] == nil {
		t.Fatalf("settings without the hook: %v", settings)
	}
}

func sessionStartParser(t *testing.T) *Parser {
	t.Helper()
	return sessionStartParserWith(t, nil)
}

func sessionStartParserWith(t *testing.T, missed func() error) *Parser {
	t.Helper()
	p := NewParser()
	delivery, err := provider.NewSessionStartDelivery(testHook(), missed)
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
	sessionStartCompactLine = `{"type":"system","subtype":"compact_boundary","uuid":"c-1","session_id":"s-1","content":"Conversation compacted","compactMetadata":{"trigger":"auto"}}`
	sessionStartResultLine  = `{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","session_id":"s-1","result":"ok"}`
)

const memoryContext = "## Memory\n\n- a\n\n(End of memory)"

var (
	memoryDelivered   = hookContextStdout(memoryContext)
	userHookDelivered = hookContextStdout("## The user's own hook")
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

// warningRows are the delivery warnings among events, with the index of
// each; a fatal one is marked.
func warningRows(events []provider.ProviderEvent) (warnings []string, at []int) {
	for i, event := range events {
		if event.Kind == provider.EventError && (strings.Contains(event.Content, "did not reach this session") || strings.Contains(event.Content, "may not have reached")) {
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
		hookResponseLine("startup", userHookDelivered, 0),
		hookResponseLine("startup", memoryDelivered, 0),
		[]byte(sessionStartInitLine), []byte(sessionStartResultLine))
	if warnings, _ := warningRows(events); len(warnings) > 0 || kindIndex(events, provider.EventError) >= 0 {
		t.Fatalf("warnings after the hook delivered: %v, events %+v", warnings, events)
	}
	for _, event := range events {
		if event.Kind == provider.EventNotification || strings.Contains(event.Content, "## Memory") {
			t.Fatalf("a hook response rendered: %+v", event)
		}
	}

	p = sessionStartParser(t)
	p.sessionStart.Expect(provider.SessionStartAfterStart)
	events = parseLines(t, p,
		hookResponseLine("startup", userHookDelivered, 0),
		[]byte(sessionStartInitLine))
	warnings, at := warningRows(events)
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "the memory did not reach this session after its start: ") ||
		at[0] < kindIndex(events, provider.EventInit) {
		t.Fatalf("events = %+v", events)
	}
	// The next init and result have nothing to settle.
	if warnings, _ := warningRows(parseLines(t, p, []byte(sessionStartInitLine), []byte(sessionStartResultLine))); len(warnings) > 0 {
		t.Fatalf("settled twice: %v", warnings)
	}
}

// A compaction's hooks report before its boundary, which settles them.
func TestParserSettlesACompactionAtItsBoundary(t *testing.T) {
	p := sessionStartParser(t)
	events := parseLines(t, p,
		[]byte(sessionStartInitLine),
		hookResponseLine("compact", memoryDelivered, 0),
		[]byte(sessionStartCompactLine), []byte(sessionStartResultLine))
	if warnings, _ := warningRows(events); len(warnings) > 0 {
		t.Fatalf("warnings after the hook delivered: %v", warnings)
	}

	events = parseLines(t, p, []byte(sessionStartCompactLine))
	warnings, at := warningRows(events)
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "the memory did not reach this session after its compaction: ") ||
		at[0] < kindIndex(events, provider.EventCompactBoundary) {
		t.Fatalf("events = %+v", events)
	}
	if warnings, _ := warningRows(parseLines(t, p, []byte(sessionStartResultLine))); len(warnings) > 0 {
		t.Fatalf("the result settled the compaction again: %v", warnings)
	}

	// A start delivered before the first init does not count for a
	// compaction in the first turn.
	p = sessionStartParser(t)
	p.sessionStart.Expect(provider.SessionStartAfterStart)
	events = parseLines(t, p,
		hookResponseLine("startup", memoryDelivered, 0),
		[]byte(sessionStartInitLine), []byte(sessionStartCompactLine), []byte(sessionStartResultLine))
	if warnings, _ := warningRows(events); len(warnings) != 1 || !strings.Contains(warnings[0], "after its compaction") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// Each compaction of a turn answers for its own hook run: the first one's
// delivery does not cover the second, which dropped it from the context.
func TestParserSettlesEachCompactionOfATurn(t *testing.T) {
	p := sessionStartParser(t)
	events := parseLines(t, p,
		[]byte(sessionStartInitLine),
		hookResponseLine("compact", memoryDelivered, 0),
		[]byte(sessionStartCompactLine),
		hookResponseLine("compact", userHookDelivered, 0),
		[]byte(sessionStartCompactLine),
		[]byte(sessionStartResultLine))
	var boundaries []int
	for i, event := range events {
		if event.Kind == provider.EventCompactBoundary {
			boundaries = append(boundaries, i)
		}
	}
	warnings, at := warningRows(events)
	if len(boundaries) != 2 || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "the memory did not reach this session after its compaction: ") ||
		at[0] < boundaries[1] {
		t.Fatalf("events = %+v", events)
	}
}

// Only a successful SessionStart response whose context opens with the
// hook's heading counts, from JSON output or plain stdout.
func TestParserCountsOnlyTheHooksSuccessfulOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		line []byte
		want bool
	}{
		"json":            {hookResponseLine("compact", memoryDelivered, 0), true},
		"plain":           {hookResponseLine("compact", memoryContext+"\n", 0), true},
		"failed":          {hookResponseLine("compact", memoryDelivered, 1), false},
		"other hook":      {hookResponseLine("compact", hookContextStdout("user context"), 0), false},
		"heading later":   {hookResponseLine("compact", hookContextStdout("x ## Memory"), 0), false},
		"bad json":        {hookResponseLine("compact", `{"hookSpecificOutput":`, 0), false},
		"no exit code":    {[]byte(`{"type":"system","subtype":"hook_response","hook_event":"SessionStart","stdout":"## Memory"}`), false},
		"other event":     {[]byte(`{"type":"system","subtype":"hook_response","hook_event":"UserPromptSubmit","stdout":"## Memory","exit_code":0}`), false},
		"started, not ok": {[]byte(`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:compact","hook_event":"SessionStart"}`), false},
	} {
		p := sessionStartParser(t)
		if events := parseLines(t, p, tc.line); len(events) > 0 {
			t.Errorf("%s: the frame rendered %+v", name, events)
		}
		warnings, _ := warningRows(parseLines(t, p, []byte(sessionStartCompactLine)))
		if delivered := len(warnings) == 0; delivered != tc.want {
			t.Errorf("%s: warnings = %v, want delivered %v", name, warnings, tc.want)
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
		warnings, _ := warningRows(parseLines(t, p, tc.line, hookResponseLine("compact", userHookDelivered, 0), []byte(sessionStartCompactLine)))
		if len(warnings) != 1 || warnings[0] != "the memory did not reach this session after its compaction: "+tc.want {
			t.Errorf("%s: warnings = %v", name, warnings)
		}
	}
}

// A session without the hook warns about nothing and a hook response is
// inert.
func TestParserWithoutTheSessionStartHook(t *testing.T) {
	p := NewParser()
	events := parseLines(t, p,
		hookResponseLine("startup", hookContextStdout("## Memory"), 1),
		[]byte(sessionStartInitLine), []byte(sessionStartCompactLine), []byte(sessionStartResultLine))
	if warnings, _ := warningRows(events); len(warnings) > 0 || kindIndex(events, provider.EventTurnComplete) < 0 {
		t.Fatalf("events = %+v", events)
	}
}

// A start or a compaction whose hook did not deliver forgets the thread's
// records, so the next prompt delivers the memory whole; one that did
// forgets nothing.
func TestParserForgetsTheRecordsWhenADeliveryIsMissed(t *testing.T) {
	missed := 0
	p := sessionStartParserWith(t, func() error { missed++; return nil })
	p.sessionStart.Expect(provider.SessionStartAfterStart)
	parseLines(t, p,
		hookResponseLine("startup", memoryDelivered, 0),
		[]byte(sessionStartInitLine),
		hookResponseLine("compact", memoryDelivered, 0),
		[]byte(sessionStartCompactLine), []byte(sessionStartResultLine))
	if missed != 0 {
		t.Fatalf("forgot %d times after deliveries that arrived", missed)
	}
	events := parseLines(t, p, []byte(sessionStartCompactLine))
	if warnings, _ := warningRows(events); len(warnings) != 1 || missed != 1 || !strings.HasSuffix(warnings[0], " The next prompt delivers it whole.") {
		t.Fatalf("warnings %v, forgot %d times", warnings, missed)
	}
}

func promptResponseLine(stdout, stderr string, exitCode int) []byte {
	line, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "hook_response", "hook_id": "h-2",
		"hook_name": "UserPromptSubmit", "hook_event": "UserPromptSubmit",
		"output": stdout + stderr, "stdout": stdout, "stderr": stderr, "exit_code": exitCode, "outcome": "success",
		"uuid": "u-2", "session_id": "s-1",
	})
	return line
}

// A failed UserPromptSubmit run is the hook's when its error output opens
// with the hook's Failure prefix, and is a warning row; a successful run, or
// the user's own failed hook, renders nothing and delivers nothing.
func TestParserWarnsWhenThePromptHookFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		line []byte
		want string
	}{
		"ours failed":   {promptResponseLine("", "deliver: panic: boom\n", 1), "Changes to the memory may not have reached this turn: its UserPromptSubmit hook failed (exit code 1: deliver: panic: boom)."},
		"ours, nothing": {promptResponseLine("", "", 0), ""},
		"ours, changes": {promptResponseLine(hookContextStdout("## Changes"), "", 0), ""},
		"user's failed": {promptResponseLine("", "their hook broke\n", 1), ""},
		"no exit code":  {[]byte(`{"type":"system","subtype":"hook_response","hook_event":"UserPromptSubmit","stderr":"deliver: killed"}`), "Changes to the memory may not have reached this turn: its UserPromptSubmit hook failed (no exit code: deliver: killed)."},
	} {
		p := sessionStartParser(t)
		p.sessionStart.Expect(provider.SessionStartAfterCompaction)
		events := parseLines(t, p, tc.line)
		warnings, _ := warningRows(events)
		if tc.want == "" && len(events) > 0 || tc.want != "" && (len(warnings) != 1 || warnings[0] != tc.want) {
			t.Errorf("%s: events %+v", name, events)
		}
		if !p.sessionStart.Pending() {
			t.Errorf("%s: a prompt run counted as the SessionStart delivery", name)
		}
	}
}

// The session expects the hook when it starts fresh, not when it resumes.
// A hook the delivery cannot tell apart is refused before anything spawns.
func TestNewSessionExpectsTheHookItsSpawnRuns(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		resume string
		hook   *provider.SessionStartHook
		want   string
	}{
		"fresh":  {"", testHook(), "the memory did not reach this session after its start"},
		"resume": {"sess-1", testHook(), ""},
	} {
		binary := mockexec.Write(t, t.TempDir()+"/mock-claude", "#!/bin/sh\nexec cat\n")
		events := make(chan provider.ProviderEvent, 100)
		s, err := NewSession(context.Background(), testThread, Config{Binary: binary, Resume: tc.resume, SessionStartHook: tc.hook},
			func(event provider.ProviderEvent) { events <- event })
		if err != nil {
			t.Fatalf("%s: NewSession: %v", name, err)
		}
		for _, line := range []string{sessionStartInitLine, sessionStartResultLine} {
			if err := s.proc.WriteLine([]byte(line)); err != nil {
				t.Fatalf("%s: write: %v", name, err)
			}
		}
		var warning string
		for evt := waitEvent(t, events); evt.Kind != provider.EventTurnComplete; evt = waitEvent(t, events) {
			if evt.Kind == provider.EventError {
				if warning != "" {
					t.Fatalf("%s: second error %+v", name, evt)
				}
				warning = evt.Content
			}
		}
		if (tc.want == "") != (warning == "") || !strings.HasPrefix(warning, tc.want) {
			t.Errorf("%s: warning %q, want %q", name, warning, tc.want)
		}
		if err := s.Close(); err != nil {
			t.Errorf("%s: Close: %v", name, err)
		}
	}

	binary := mockexec.Write(t, t.TempDir()+"/mock-claude", "#!/bin/sh\nexit 1\n")
	if _, err := NewSession(context.Background(), testThread, Config{Binary: binary, SessionStartHook: &provider.SessionStartHook{Label: "a", Command: "a"}},
		func(provider.ProviderEvent) {}); err == nil || !strings.Contains(err.Error(), "heading, ending and stamp are all required") {
		t.Fatalf("NewSession with a hook without a heading: %v", err)
	}
}
