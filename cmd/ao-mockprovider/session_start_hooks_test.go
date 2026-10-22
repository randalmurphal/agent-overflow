package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// Matchers read as both CLIs read them: empty and "*" take everything, a list
// of plain names takes exactly those, anything else is a regular expression.
func TestSessionStartMatcherTakes(t *testing.T) {
	for _, tc := range []struct {
		matcher, source string
		want            bool
	}{
		{"", "resume", true},
		{"*", "compact", true},
		{"startup|compact|clear", "startup", true},
		{"startup|compact|clear", "clear", true},
		{"startup|compact|clear", "resume", false},
		{"startup|compact|clear", "start", false},
		{"^comp", "compact", true},
		{"^comp", "startup", false},
		{"(", "startup", false},
	} {
		if got := sessionStartMatcherTakes(tc.matcher, tc.source); got != tc.want {
			t.Errorf("matcher %q takes %q = %v, want %v", tc.matcher, tc.source, got, tc.want)
		}
	}
}

// The trust hash is upstream's: sha256 of the identity serde_json writes,
// here computed by Python's json.dumps(sort_keys, compact separators,
// ensure_ascii=False), which spells these characters as serde does. <, > and
// & stay raw, as encoding/json would not keep them by default.
func TestCodexHookHashMatchesUpstream(t *testing.T) {
	command := `'/opt/ao/bin/agent overflow' __agent-home-context 'MEMORY.md' '/home/a&b/<it>/"q"\\é'`
	const want = "sha256:cc5fb41f21244abd1137a7526fef1383cea100fe9d56b1c7d1bd6bfddd3f4611"
	if got := codexHookHash("session_start", "startup|compact|clear", command, 60); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	if codexHookHash("session_start", "startup|compact", command, 60) == want ||
		codexHookHash("session_start", "startup|compact|clear", command, 600) == want ||
		codexHookHash("user_prompt_submit", "startup|compact|clear", command, 60) == want {
		t.Fatal("the event, the matcher and the timeout must be part of the identity")
	}
}

// hookScripts writes the commands the hook tests run: each records its stdin
// in dir and prints its output.
type hookScripts struct {
	dir string
}

func newHookScripts(t *testing.T) hookScripts {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SessionStart hook commands run through sh")
	}
	return hookScripts{dir: t.TempDir()}
}

// record is a command that saves its input as name.json and prints out.
func (h hookScripts) record(name, out string) string {
	return "cat > " + shellQuoteTest(filepath.Join(h.dir, name+".json")) + "; printf '%s\\n' " + shellQuoteTest(out)
}

// input is the hook input the command named name last received.
func (h hookScripts) input(t *testing.T, name string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, name+".json"))
	if err != nil {
		t.Fatalf("hook %s did not run: %v", name, err)
	}
	var input map[string]string
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatalf("hook %s input %q: %v", name, data, err)
	}
	return input
}

func shellQuoteTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func jsonContext(text string) string {
	return mustJSON(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": text}})
}

func claudeHookSettings(groups ...map[string]any) string {
	return mustJSON(map[string]any{"hooks": map[string]any{"SessionStart": groups}})
}

func hookGroup(matcher string, commands ...string) map[string]any {
	handlers := make([]any, 0, len(commands))
	for _, command := range commands {
		handlers = append(handlers, map[string]any{"type": "command", "command": command, "timeout": 60})
	}
	return map[string]any{"matcher": matcher, "hooks": handlers}
}

// frameFields decodes a frame for the fields a test asserts.
func frameFields(t *testing.T, line string) map[string]any {
	t.Helper()
	var frame map[string]any
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("frame %q: %v", line, err)
	}
	return frame
}

// A fresh Claude process runs the hooks whose matcher takes "startup" before
// its first init, and a compaction's before the compact_boundary a scenario
// writes, each reported as hook_started then hook_response frames the app's
// parser reads. A JSON object's additionalContext and plain stdout are
// context; a failing hook is an error response with its exit code.
func TestClaudeRunsSessionStartHooksAtTheStartAndBeforeACompaction(t *testing.T) {
	hooks := newHookScripts(t)
	settings := claudeHookSettings(
		hookGroup("startup|compact|clear",
			hooks.record("instructions", jsonContext("## Your standing instructions\n\nShip it.")),
			hooks.record("memory", "## Your memory index\n\n- a note"),
			"echo broken >&2; exit 3",
		),
		hookGroup("resume", hooks.record("resumed", "resumed")),
	)
	sc := claudeTwoTurnScenario()
	sc.Turns = append(sc.Turns, scenario.Turn{Label: "compact", Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
		`{"type":"system","subtype":"compact_boundary","session_id":"${SESSION_ID}","compact_metadata":{"trigger":"auto","pre_tokens":1000}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}}}}})
	cwd := t.TempDir()
	args := append(append([]string(nil), claudeSessionArgs...), "--settings", settings)
	p := startMock(t, args, writeScenarioFile(t, sc, ""), cwd)

	p.send(userLine)
	stdouts := []string{jsonContext("## Your standing instructions\n\nShip it.") + "\n", "## Your memory index\n\n- a note\n", ""}
	expectClaudeHookRun(t, p, "SessionStart:startup", stdouts)
	if init := p.expectLine(testTimeout); !strings.Contains(init, `"subtype":"init"`) {
		t.Fatalf("after the start's hooks = %q, want the first init", init)
	}
	for _, name := range []string{"instructions", "memory"} {
		input := hooks.input(t, name)
		if input["hook_event_name"] != "SessionStart" || input["source"] != "startup" || input["cwd"] == "" || input["session_id"] == "" {
			t.Fatalf("hook %s input = %v", name, input)
		}
	}
	p.expectLineContaining(`"type":"result"`, testTimeout)

	// The next turn's init runs no hooks.
	p.send(userLine)
	if init := p.expectLine(testTimeout); !strings.Contains(init, `"subtype":"init"`) {
		t.Fatalf("turn 2 opened with %q, want its init and no hook", init)
	}
	p.expectLineContaining(`"type":"result"`, testTimeout)

	// The compaction's hooks run before its boundary.
	p.send(userLine)
	p.expectLineContaining(`"subtype":"init"`, testTimeout)
	p.expectLineContaining(`"isReplay":true`, testTimeout)
	expectClaudeHookRun(t, p, "SessionStart:compact", stdouts)
	if boundary := p.expectLine(testTimeout); !strings.Contains(boundary, `"subtype":"compact_boundary"`) {
		t.Fatalf("after the compaction's hooks = %q, want the boundary", boundary)
	}
	if input := hooks.input(t, "memory"); input["source"] != "compact" {
		t.Fatalf("compaction hook input = %v, want source compact", input)
	}
	p.expectLineContaining(`"type":"result"`, testTimeout)
	p.closeStdinAndExpectExit(0, testTimeout)
	if _, err := os.Stat(filepath.Join(hooks.dir, "resumed.json")); !os.IsNotExist(err) {
		t.Fatalf("the resume group ran on a fresh start: %v", err)
	}
	validateClaudeFrames(t, p.all)
}

// expectClaudeHookRun reads one run's frames: a hook_started per hook, then a
// hook_response per hook in order carrying its stdout, or failing with exit
// code 3 where stdouts holds "".
func expectClaudeHookRun(t *testing.T, p *mockProc, name string, stdouts []string) {
	t.Helper()
	ids := make([]string, len(stdouts))
	for i := range stdouts {
		frame := frameFields(t, p.expectLine(testTimeout))
		if frame["subtype"] != "hook_started" || frame["hook_name"] != name || frame["hook_event"] != "SessionStart" {
			t.Fatalf("frame %d = %v, want %s hook_started", i, frame, name)
		}
		ids[i], _ = frame["hook_id"].(string)
	}
	for i, stdout := range stdouts {
		frame := frameFields(t, p.expectLine(testTimeout))
		if frame["subtype"] != "hook_response" || frame["hook_id"] != ids[i] || frame["hook_name"] != name || frame["hook_event"] != "SessionStart" {
			t.Fatalf("response %d = %v, want the response of hook %s", i, frame, ids[i])
		}
		if stdout == "" {
			if frame["exit_code"] != float64(3) || frame["outcome"] != "error" || frame["stderr"] != "broken\n" || frame["output"] != "broken\n" {
				t.Fatalf("failing hook response = %v", frame)
			}
			continue
		}
		if frame["exit_code"] != float64(0) || frame["outcome"] != "success" || frame["stderr"] != "" || frame["stdout"] != stdout || frame["output"] != stdout {
			t.Fatalf("hook response %d = %v, want success with stdout %q", i, frame, stdout)
		}
	}
}

// A process started with --resume runs the hooks whose matcher takes
// "resume" and none of the others.
func TestClaudeResumeRunsOnlyResumeHooks(t *testing.T) {
	hooks := newHookScripts(t)
	settings := claudeHookSettings(
		hookGroup("startup|compact|clear", hooks.record("start", "## start")),
		hookGroup("resume", hooks.record("resumed", "## resumed")),
	)
	args := append(append([]string(nil), claudeSessionArgs...), "--settings", settings, "--resume", "sess-7")
	p := startMock(t, args, writeScenarioFile(t, claudeTwoTurnScenario(), ""), t.TempDir())

	p.send(userLine)
	started := frameFields(t, p.expectLine(testTimeout))
	response := frameFields(t, p.expectLine(testTimeout))
	if started["hook_name"] != "SessionStart:resume" || response["subtype"] != "hook_response" || response["stdout"] != "## resumed\n" {
		t.Fatalf("resume run = %v then %v", started, response)
	}
	if init := p.expectLine(testTimeout); !strings.Contains(init, `"subtype":"init"`) {
		t.Fatalf("after the resume hook = %q, want init", init)
	}
	p.expectLineContaining(`"type":"result"`, testTimeout)
	p.closeStdinAndExpectExit(0, testTimeout)
	if _, err := os.Stat(filepath.Join(hooks.dir, "start.json")); !os.IsNotExist(err) {
		t.Fatalf("a startup hook ran on a resume: %v", err)
	}
}

func TestClaudeStartSource(t *testing.T) {
	for want, args := range map[string][]string{
		"startup": {"-p", "--fork-session"},
		"resume":  {"-p", "--resume", "sess-7"},
		"fork":    {"-p", "--resume", "sess-7", "--fork-session"},
	} {
		if got := claudeStartSource(args); got != want {
			t.Errorf("claudeStartSource(%q) = %q, want %q", args, got, want)
		}
	}
}

// Each run reaches the spec as session_start_context: the source and the
// context of each hook that delivered, in order, before the frames.
func TestClaudeReportsEachSessionStartRun(t *testing.T) {
	hooks := newHookScripts(t)
	var buf bytes.Buffer
	e := newUnitEngine(t, &buf)
	e.cwd = t.TempDir()
	var reports []control.Report
	e.rep.observe = func(rep control.Report) {
		if rep.Kind == control.ReportSessionStartContext {
			if !strings.Contains(buf.String(), "hook_started") || strings.Contains(buf.String(), "hook_response") {
				t.Errorf("report posted at the wrong point; the wire so far:\n%s", buf.String())
			}
			reports = append(reports, rep)
		}
	}
	a := newClaudeAdapter(e, e.w, []string{"--settings", claudeHookSettings(hookGroup("startup|compact|clear",
		hooks.record("instructions", jsonContext("## Your standing instructions")),
		"exit 1",
		hooks.record("invalid", "{not json"),
		hooks.record("memory", "## Your memory index"),
	))})
	e.adapter = a
	vars := e.varsForTurn(0)

	a.writeInit(vars)
	a.writeInit(vars)
	buf.Reset()
	e.writeScenarioLines(vars, []string{`{"type":"system","subtype":"compact_boundary"}`}, false, 0, 0, 0)
	want := []control.Report{
		{Kind: control.ReportSessionStartContext, SessionRef: "s1", SessionStartRun: &control.SessionStartRun{Source: "startup", Contexts: []string{"## Your standing instructions", "## Your memory index"}}},
		{Kind: control.ReportSessionStartContext, SessionRef: "s1", SessionStartRun: &control.SessionStartRun{Source: "compact", Contexts: []string{"## Your standing instructions", "## Your memory index"}}},
	}
	if !reflect.DeepEqual(reports, want) {
		t.Fatalf("reports = %s, want %s", mustJSON(reports), mustJSON(want))
	}
	lines := outputLines(&buf)
	if len(lines) != 9 || !strings.Contains(lines[8], "compact_boundary") {
		t.Fatalf("compaction wire = %q, want 4 starts, 4 responses, then the boundary", lines)
	}
}

// A Codex thread runs the trusted hooks of its config at the first turn start
// after thread/start and right after a compaction, reporting each run as
// hook/started and hook/completed of the session-flags layer. A hook whose
// trust hash does not match never runs.
func TestCodexRunsTrustedSessionStartHooksAtTheTurnStartAndAfterACompaction(t *testing.T) {
	hooks := newHookScripts(t)
	instructions := hooks.record("instructions", jsonContext("## Your standing instructions"))
	memory := hooks.record("memory", "## Your memory index")
	untrusted := hooks.record("untrusted", "## nope")
	config := mustJSON(map[string]any{"hooks": map[string]any{
		"SessionStart": []any{hookGroup("startup|compact|clear", instructions, memory, untrusted)},
		"state": map[string]any{
			codexSessionFlagsSource + ":session_start:0:0": map[string]any{"trusted_hash": codexHookHash("session_start", "startup|compact|clear", instructions, 60)},
			codexSessionFlagsSource + ":session_start:0:1": map[string]any{"trusted_hash": codexHookHash("session_start", "startup|compact|clear", memory, 60)},
			codexSessionFlagsSource + ":session_start:0:2": map[string]any{"trusted_hash": codexHookHash("session_start", "startup|compact|clear", "echo other", 60)},
		},
	}})
	sc := codexScenario()
	sc.Turns = append(sc.Turns, scenario.Turn{Label: "compact", Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
		`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"cc-1","type":"contextCompaction"}}}`,
		`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
	}}}}})
	p := startMock(t, []string{"app-server"}, writeScenarioFile(t, sc, ""), t.TempDir())
	p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.expectLine(testTimeout)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"config":` + config + `}}`)
	p.expectLineContaining(`"id":2`, testTimeout)

	p.send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"th-42","input":[]}}`)
	p.expectLineContaining(`"id":3`, testTimeout)
	p.expectLineContaining(`"method":"turn/started"`, testTimeout)
	expectCodexHookRun(t, p, "turn-1")
	if hooks.input(t, "memory")["source"] != "startup" {
		t.Fatalf("start hook input = %v", hooks.input(t, "memory"))
	}
	p.expectLineContaining(`"text":"codex turn 1"`, testTimeout)
	p.expectLineContaining(`"method":"turn/completed"`, testTimeout)

	p.send(`{"jsonrpc":"2.0","id":4,"method":"turn/start","params":{"threadId":"th-42","input":[]}}`)
	p.expectLineContaining(`"id":4`, testTimeout)
	p.expectLineContaining(`"method":"turn/started"`, testTimeout)
	if next := p.expectLine(testTimeout); !strings.Contains(next, `"contextCompaction"`) {
		t.Fatalf("turn 2 ran hooks at its start: %q", next)
	}
	expectCodexHookRun(t, p, "turn-2")
	if hooks.input(t, "memory")["source"] != "compact" {
		t.Fatalf("compaction hook input = %v", hooks.input(t, "memory"))
	}
	p.expectLineContaining(`"method":"turn/completed"`, testTimeout)
	p.closeStdinAndExpectExit(0, testTimeout)
	if _, err := os.Stat(filepath.Join(hooks.dir, "untrusted.json")); !os.IsNotExist(err) {
		t.Fatalf("the untrusted hook ran: %v", err)
	}
}

// expectCodexHookRun reads one run of the two trusted hooks.
func expectCodexHookRun(t *testing.T, p *mockProc, turnID string) {
	t.Helper()
	type wire struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string         `json:"threadId"`
			TurnID   string         `json:"turnId"`
			Run      map[string]any `json:"run"`
		} `json:"params"`
	}
	read := func(method string, order int) map[string]any {
		var frame wire
		line := p.expectLine(testTimeout)
		if err := json.Unmarshal([]byte(line), &frame); err != nil || frame.Method != method || frame.Params.ThreadID != "th-42" || frame.Params.TurnID != turnID {
			t.Fatalf("frame = %q (%v), want %s of %s", line, err, method, turnID)
		}
		run := frame.Params.Run
		if run["eventName"] != "sessionStart" || run["source"] != "sessionFlags" || run["sourcePath"] != codexSessionFlagsSource ||
			run["handlerType"] != "command" || run["scope"] != "thread" || run["displayOrder"] != float64(order) {
			t.Fatalf("%s run = %v", method, run)
		}
		return run
	}
	for order := range 2 {
		if run := read("hook/started", order); run["status"] != "running" || len(run["entries"].([]any)) != 0 {
			t.Fatalf("started run = %v", run)
		}
	}
	for order, context := range []string{"## Your standing instructions", "## Your memory index"} {
		run := read("hook/completed", order)
		want := []any{map[string]any{"kind": "context", "text": context}}
		if run["status"] != "completed" || !reflect.DeepEqual(run["entries"], want) || run["durationMs"] == nil {
			t.Fatalf("completed run = %v, want context %q", run, context)
		}
	}
}

// A resume queues a "resume" run: hooks whose matcher takes only startup,
// compact and clear do not run, one that takes resume does. The run reaches
// the spec as session_start_context; a failing hook is a failed run with its
// exit code, and invalid JSON a failed run Codex names.
func TestCodexResumeAndFailedRuns(t *testing.T) {
	hooks := newHookScripts(t)
	var buf bytes.Buffer
	a := newCodexUnitAdapter(t, &buf, nil)
	a.e.cwd = t.TempDir()
	var reports []control.Report
	a.e.rep.observe = func(rep control.Report) {
		if rep.Kind == control.ReportSessionStartContext {
			reports = append(reports, rep)
		}
	}
	trusted := func(matcher string, commands ...string) string {
		state := map[string]any{}
		for i, command := range commands {
			state[codexSessionFlagsSource+":session_start:0:"+string(rune('0'+i))] = map[string]any{"trusted_hash": codexHookHash("session_start", matcher, command, 60)}
		}
		return mustJSON(map[string]any{"threadId": "th-unit", "config": map[string]any{"hooks": map[string]any{
			"SessionStart": []any{hookGroup(matcher, commands...)},
			"state":        state,
		}}})
	}
	turnStarted := `{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"th-unit","turn":{"id":"turn-1"}}}`
	vars := scenario.Vars{"THREAD_ID": "th-unit", "TURN_ID": "turn-1"}

	codexRequest(t, a, &buf, "thread/resume", trusted("startup|compact|clear", hooks.record("start", "## start")))
	buf.Reset()
	a.e.writeScenarioLines(vars, []string{turnStarted}, false, 0, 0, 0)
	if lines := outputLines(&buf); len(lines) != 1 || len(reports) != 0 {
		t.Fatalf("a resume ran startup hooks: %q, reports %v", lines, reports)
	}

	codexRequest(t, a, &buf, "thread/resume", trusted("resume", hooks.record("resumed", "## resumed"), "exit 4", "printf '{oops'"))
	buf.Reset()
	a.e.writeScenarioLines(vars, []string{turnStarted}, false, 0, 0, 0)
	lines := outputLines(&buf)
	if len(lines) != 7 {
		t.Fatalf("resume run wrote %d lines, want turn/started and 3 started and 3 completed: %q", len(lines), lines)
	}
	var failed, invalid struct {
		Params struct {
			Run struct {
				Status  string `json:"status"`
				Entries []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"entries"`
			} `json:"run"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(lines[5]), &failed); err != nil || failed.Params.Run.Status != "failed" ||
		len(failed.Params.Run.Entries) != 1 || failed.Params.Run.Entries[0].Text != "hook exited with code 4" {
		t.Fatalf("failing hook = %s", lines[5])
	}
	if err := json.Unmarshal([]byte(lines[6]), &invalid); err != nil || invalid.Params.Run.Status != "failed" ||
		len(invalid.Params.Run.Entries) != 1 || invalid.Params.Run.Entries[0].Text != "hook returned invalid session start JSON output" {
		t.Fatalf("invalid JSON hook = %s", lines[6])
	}
	want := []control.Report{{Kind: control.ReportSessionStartContext, SessionRef: "th-unit", SessionStartRun: &control.SessionStartRun{Source: "resume", Contexts: []string{"## resumed"}}}}
	if !reflect.DeepEqual(reports, want) {
		t.Fatalf("reports = %s, want %s", mustJSON(reports), mustJSON(want))
	}

	// The run happened; the next turn start runs nothing.
	buf.Reset()
	a.e.writeScenarioLines(vars, []string{turnStarted}, false, 0, 0, 0)
	if lines := outputLines(&buf); len(lines) != 1 || len(reports) != 1 {
		t.Fatalf("hooks ran again: %q", lines)
	}

	// The legacy thread/compacted notification queues and runs "compact".
	codexRequest(t, a, &buf, "thread/resume", trusted("compact", hooks.record("compacted", "## compacted")))
	buf.Reset()
	a.e.writeScenarioLines(vars, []string{`{"jsonrpc":"2.0","method":"thread/compacted","params":{"threadId":"th-unit","turnId":"turn-1"}}`}, false, 0, 0, 0)
	if len(reports) != 2 || reports[1].SessionStartRun.Source != "compact" || !reflect.DeepEqual(reports[1].SessionStartRun.Contexts, []string{"## compacted"}) {
		t.Fatalf("legacy compaction reports = %s", mustJSON(reports))
	}

	// A subagent thread runs no SessionStart hooks: its compactions queue
	// nothing for the thread's compact hook.
	buf.Reset()
	a.e.writeScenarioLines(vars, []string{
		`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"th-child","turn":{"id":"child-turn"}}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"th-child","item":{"type":"contextCompaction"}}}`,
		`{"jsonrpc":"2.0","method":"thread/compacted","params":{"threadId":"th-child","turnId":"child-turn"}}`,
	}, false, 0, 0, 0)
	if lines := outputLines(&buf); len(lines) != 3 || len(reports) != 2 {
		t.Fatalf("a child thread ran the hooks: %q, reports %s", lines, mustJSON(reports))
	}
}
