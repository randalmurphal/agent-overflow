package aocli

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/untrustedtext"
)

// `agent-overflow run …`. The shared skeleton — session resolution, usage and exit codes,
// the refusal paths — is exercised in exec_test.go against the same fake
// backend; these are the command-specific contracts. Seed parsing and `--json`
// fidelity are asserted here because `agent-overflow run start` is where a caller meets
// them, and `agent-overflow schedule` takes the identical flags.

func TestRunStartSendsSeedsAsJSONWhenTheyParse(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentStartRun", map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "running",
	})
	code, stdout, stderr := runCLI([]string{
		"run", "start", "flow", "--goal", "ship it",
		"--seed", "count=3", "--seed", "name=alice", "--seed", "flags=[1,2]",
		"--seed", "enabled=true", "--seed", "note=not json: really",
	}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "run=run-1") || !strings.Contains(stdout, "state=running") {
		t.Fatalf("human output = %q", stdout)
	}

	calls := backend.recorded("WorkflowAgentStartRun")
	if len(calls) != 1 || len(calls[0].Params) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	var sent struct {
		WorkflowID string         `json:"workflowId"`
		Goal       string         `json:"goal"`
		Seeds      map[string]any `json:"seeds"`
	}
	if err := json.Unmarshal(calls[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent.WorkflowID != "flow" || sent.Goal != "ship it" {
		t.Fatalf("sent = %#v", sent)
	}
	if sent.Seeds["count"] != float64(3) {
		t.Fatalf("count seed = %#v, want the number 3", sent.Seeds["count"])
	}
	if sent.Seeds["name"] != "alice" {
		t.Fatalf("name seed = %#v", sent.Seeds["name"])
	}
	if sent.Seeds["enabled"] != true {
		t.Fatalf("enabled seed = %#v", sent.Seeds["enabled"])
	}
	if list, ok := sent.Seeds["flags"].([]any); !ok || len(list) != 2 {
		t.Fatalf("flags seed = %#v, want an array", sent.Seeds["flags"])
	}
	// A value that is not JSON stays a string rather than failing the command.
	if sent.Seeds["note"] != "not json: really" {
		t.Fatalf("note seed = %#v", sent.Seeds["note"])
	}
}

func TestRunStartRejectsMalformedAndRepeatedSeeds(t *testing.T) {
	backend := newFakeBackend(t)
	for _, args := range [][]string{
		{"run", "start", "flow", "--seed", "novalue"},
		{"run", "start", "flow", "--seed", "=value"},
		{"run", "start", "flow", "--seed", "k=1", "--seed", "k=2"},
	} {
		code, _, stderr := runCLI(args, backend.env())
		if code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
		if stderr == "" {
			t.Fatalf("%v failed silently", args)
		}
	}
	if calls := backend.recorded("WorkflowAgentStartRun"); len(calls) != 0 {
		t.Fatalf("a bad seed still reached the backend: %#v", calls)
	}
}

func TestRunStartOmitsSeedsWhenNoneWereGiven(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentStartRun", map[string]any{"itemId": "run-1", "state": "running"})
	if code, _, stderr := runCLI([]string{"run", "start", "flow"}, backend.env()); code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	var sent map[string]any
	if err := json.Unmarshal(backend.recorded("WorkflowAgentStartRun")[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if _, present := sent["seeds"]; present {
		t.Fatalf("an unseeded start sent a seeds field: %#v", sent)
	}
}

func TestJSONOutputForwardsTheAppResultVerbatim(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentStartRun", map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "running",
		"aFieldThisCLIDoesNotKnow": "must survive",
	})
	code, stdout, stderr := runCLI([]string{"run", "start", "flow", "--json"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("--json did not print one JSON document: %v\n%s", err, stdout)
	}
	if decoded["aFieldThisCLIDoesNotKnow"] != "must survive" {
		t.Fatalf("--json dropped a field the CLI does not model: %v", decoded)
	}
}

func TestRunStartWaitsAndExitsOneWhenTheRunDoesNotFinishDone(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentStartRun", map[string]any{"itemId": "run-1", "state": "running"})
	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "state": "needs-human", "reason": "gate", "resting": true,
	})
	code, stdout, stderr := runCLI([]string{"run", "start", "flow", "--wait"}, backend.env())
	if code != exitFindings {
		t.Fatalf("exit = %d (%s), want %d for a run that rested needs-human", code, stderr, exitFindings)
	}
	if !strings.Contains(stdout, "state=needs-human") || !strings.Contains(stdout, "reason=gate") {
		t.Fatalf("wait output = %q", stdout)
	}

	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "state": "done", "resting": true,
	})
	if code, _, stderr := runCLI([]string{"run", "wait", "run-1"}, backend.env()); code != exitOK {
		t.Fatalf("wait on a done run exit = %d (%s)", code, stderr)
	}
	// A run still working past the deadline is an operational failure, not a
	// verdict on the run.
	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "state": "running", "resting": false,
	})
	code, _, stderr = runCLI([]string{"run", "wait", "run-1", "--timeout", "20ms"}, backend.env())
	if code != exitError {
		t.Fatalf("wait timeout exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "still") {
		t.Fatalf("timeout message = %q", stderr)
	}
}

func TestSurfaceAndSkipExitsZeroAndSaysSo(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentStartRun", map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "done", "skipped": true,
	})
	code, stdout, stderr := runCLI([]string{"run", "start", "flow"}, backend.env())
	if code != exitOK {
		t.Fatalf("a skipped start exit = %d (%s), want 0", code, stderr)
	}
	if !strings.Contains(stdout, "skipped=true") {
		t.Fatalf("a skipped start did not say so: %q", stdout)
	}
}

func TestRunOutputRendersOutputsAndArtifacts(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunOutput", map[string]any{
		"itemId": "run-1", "state": "done", "resting": true,
		"outputs":   map[string]any{"report": "all green", "count": 2},
		"artifacts": []string{"report.md"},
	})
	code, stdout, stderr := runCLI([]string{"run", "output", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	for _, want := range []string{"run=run-1", "state=done", `output report="all green"`, "output count=2", "artifact report.md"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunListRendersOneLinePerRun(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentListRuns", []map[string]any{
		{"itemId": "run-1", "workflowId": "flow", "state": "running", "currentPhaseId": "build",
			"currentPhaseOrdinal": 2, "phaseCount": 3},
		{"itemId": "run-2", "workflowId": "flow", "state": "needs-human", "reason": "gate",
			"parentItemId": "run-1"},
	})
	code, stdout, stderr := runCLI([]string{"run", "list", "--active"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "run=run-1") || !strings.Contains(stdout, "phase=build(2/3)") {
		t.Fatalf("list output = %q", stdout)
	}
	if !strings.Contains(stdout, "reason=gate") {
		t.Fatalf("list output = %q", stdout)
	}
	// A campaign's runs are a tree; a flat list that never names the parent
	// leaves the reader unable to tell a wave from the root that called it.
	if !strings.Contains(stdout, "run=run-2 parent=run-1") {
		t.Fatalf("list output did not name the calling run:\n%s", stdout)
	}
	calls := backend.recorded("WorkflowAgentListRuns")
	if len(calls) != 1 || string(calls[0].Params[0]) != "true" {
		t.Fatalf("--active was not sent: %#v", calls)
	}
}

// Zero rows is an answer, not a failure. Printing nothing reads as a command
// that did not work, which is the reading that sends an agent looking for a
// broken session.
func TestRunListSaysSoWhenThereAreNoRuns(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentListRuns", []map[string]any{})
	code, stdout, stderr := runCLI([]string{"run", "list"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if stdout != "No runs in this project.\n" {
		t.Fatalf("empty list output = %q", stdout)
	}
	if code, stdout, stderr = runCLI([]string{"run", "list", "--active"}, backend.env()); code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if stdout != "No active runs in this project.\n" {
		t.Fatalf("empty --active list output = %q", stdout)
	}
	// --json still promises exactly the app's document, empty array included.
	if code, stdout, stderr = runCLI([]string{"run", "list", "--json"}, backend.env()); code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if strings.Contains(stdout, "No runs") || !strings.Contains(stdout, "[]") {
		t.Fatalf("--json empty list output = %q", stdout)
	}
}

// The reason says a fan-out needs repair; the ids are what `run retry-unit`
// takes, and an agent holding only a CLI has no other way to learn them.
func TestRunStatusNamesTheFailedUnits(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "needs-human", "reason": "unit-failed",
		"resting": true,
		"failedUnits": []map[string]any{
			{"unitId": "lane-3", "unitAttempt": 2},
			{"unitId": "lane-7", "unitAttempt": 1},
		},
	})
	code, stdout, stderr := runCLI([]string{"run", "status", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "failed-units=lane-3,lane-7") {
		t.Fatalf("status output did not name the failed units:\n%s", stdout)
	}
}

// A gate is a decision, so neither direction can be the default: the command
// has to refuse both "no decision" and "both decisions" before anything reaches
// the app, because either one would otherwise resolve as an approve.
func TestRunResolveRequiresExactlyOneDecision(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowResolveGate", nil)
	backend.reply("WorkflowAgentRunStatus", map[string]any{"itemId": "run-1", "state": "running"})
	for _, args := range [][]string{
		{"run", "resolve", "run-1"},
		{"run", "resolve", "run-1", "--approve", "--reject"},
	} {
		code, stdout, stderr := runCLI(args, backend.env())
		if code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
		if stdout != "" {
			t.Fatalf("%v wrote to stdout: %q", args, stdout)
		}
		if !strings.Contains(stderr, "--approve or --reject") {
			t.Fatalf("%v stderr = %q", args, stderr)
		}
	}
	if calls := backend.recorded("WorkflowResolveGate"); len(calls) != 0 {
		t.Fatalf("an undecided resolve still reached the backend: %#v", calls)
	}
}

// Both human-decision verbs put the decision on the wire and then report where
// the run went, the same contract the rest of the acting verbs hold to.
func TestRunResolveAndAnswerSendTheDecisionAndReportTheRun(t *testing.T) {
	for _, test := range []struct {
		args   []string
		method string
		want   []string
	}{
		{[]string{"run", "resolve", "run-1", "--approve"}, "WorkflowResolveGate",
			[]string{`"run-1"`, `"approve"`, `""`}},
		{[]string{"run", "resolve", "run-1", "--reject", "--note", "the diff is wrong"}, "WorkflowResolveGate",
			[]string{`"run-1"`, `"reject"`, `"the diff is wrong"`}},
		{[]string{"run", "answer", "run-1", "use the second option"}, "WorkflowAnswerQuestion",
			[]string{`"run-1"`, `"use the second option"`}},
	} {
		backend := newFakeBackend(t)
		backend.reply(test.method, nil)
		backend.reply("WorkflowAgentRunStatus", map[string]any{"itemId": "run-1", "state": "running"})
		code, stdout, stderr := runCLI(test.args, backend.env())
		if code != exitOK {
			t.Fatalf("%v exit = %d (%s)", test.args, code, stderr)
		}
		if !strings.Contains(stdout, "run=run-1") || !strings.Contains(stdout, "state=running") {
			t.Fatalf("%v output = %q", test.args, stdout)
		}
		calls := backend.recorded(test.method)
		if len(calls) != 1 {
			t.Fatalf("%s called %d times", test.method, len(calls))
		}
		if len(calls[0].Params) != len(test.want) {
			t.Fatalf("%s params = %v, want %v", test.method, calls[0].Params, test.want)
		}
		for index, want := range test.want {
			if string(calls[0].Params[index]) != want {
				t.Fatalf("%s param %d = %s, want %s", test.method, index, calls[0].Params[index], want)
			}
		}
	}
}

// An answer is the point of the command, so it is a positional — and a blank
// one is a question still unanswered rather than an answer of nothing.
func TestRunAnswerNeedsARunIdAndText(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAnswerQuestion", nil)
	for _, args := range [][]string{
		{"run", "answer", "run-1"},
		{"run", "answer", "run-1", "   "},
		{"run", "answer", "run-1", "text", "extra"},
	} {
		if code, _, _ := runCLI(args, backend.env()); code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
	}
	if calls := backend.recorded("WorkflowAnswerQuestion"); len(calls) != 0 {
		t.Fatalf("a usage error still reached the backend: %#v", calls)
	}
}

// A gate consumed one attempt's outputs; `run status` is where the reader finds
// out which one it was and what it ran with. The control verbs deliberately do
// not print these lines — they answer "where is it now".
func TestRunStatusRendersPerAttemptProvenance(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "needs-human", "reason": "gate", "resting": true,
		"phases": []map[string]any{
			{"phaseId": "review", "attempt": 1, "status": "completed", "provider": "codex",
				"model": "gpt-5.2-codex", "effort": "xhigh", "decision": "loop", "decisionTarget": "fix"},
			{"phaseId": "review", "attempt": 2, "status": "completed", "provider": "codex",
				"model": "gpt-5.2-codex", "effort": "xhigh", "decision": "retries-exhausted",
				"exhaustedLoops": []string{"review:0"}},
			{"phaseId": "check", "attempt": 1, "status": "completed"},
		},
	})
	code, stdout, stderr := runCLI([]string{"run", "status", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	for _, want := range []string{
		"run=run-1 workflow=flow state=needs-human reason=gate\n",
		"phase=review attempt=1 status=completed provider=codex model=gpt-5.2-codex effort=xhigh decision=loop->fix\n",
		"phase=review attempt=2 status=completed provider=codex model=gpt-5.2-codex effort=xhigh decision=retries-exhausted exhausted-loops=review:0\n",
		// A tool phase has no session, so its line carries no empty columns.
		"phase=check attempt=1 status=completed\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status output is missing %q:\n%s", want, stdout)
		}
	}

	backend.reply("WorkflowPauseItem", nil)
	code, stdout, stderr = runCLI([]string{"run", "pause", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("pause exit = %d (%s)", code, stderr)
	}
	if strings.Contains(stdout, "phase=review") {
		t.Fatalf("a control verb printed the per-attempt lines:\n%s", stdout)
	}
}

// --clear is the same verb withdrawing the request, so it must reach the app as
// the state asked for rather than as a different call the server would have to
// tell apart.
func TestRunSoftStopClearSendsTheWithdrawal(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowRequestSoftStop", nil)
	backend.reply("WorkflowAgentRunStatus", map[string]any{"itemId": "run-1", "state": "running"})

	code, _, stderr := runCLI([]string{"run", "soft-stop", "run-1", "--clear"}, backend.env())
	if code != exitOK {
		t.Fatalf("soft-stop --clear exit = %d (%s)", code, stderr)
	}
	calls := backend.recorded("WorkflowRequestSoftStop")
	if len(calls) != 1 {
		t.Fatalf("WorkflowRequestSoftStop called %d times", len(calls))
	}
	want := []string{`"run-1"`, `false`}
	if len(calls[0].Params) != len(want) {
		t.Fatalf("soft-stop --clear params = %v, want %v", calls[0].Params, want)
	}
	for index, expected := range want {
		if string(calls[0].Params[index]) != expected {
			t.Fatalf("soft-stop --clear param %d = %s, want %s", index, calls[0].Params[index], expected)
		}
	}
}

func TestRunControlCommandsSendTheirExtraArguments(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowRerunItem", nil)
	backend.reply("WorkflowResumeItem", nil)
	backend.reply("WorkflowRetryUnit", nil)
	backend.reply("WorkflowRetryFailedUnits", nil)
	backend.reply("WorkflowPauseItem", nil)
	backend.reply("WorkflowCancelItem", nil)
	backend.reply("WorkflowRequestSoftStop", nil)
	backend.reply("WorkflowAgentRunStatus", map[string]any{"itemId": "run-1", "state": "running"})

	for _, test := range []struct {
		args   []string
		method string
		want   []string
	}{
		{[]string{"run", "rerun", "run-1", "--guidance", "try the other branch"}, "WorkflowRerunItem",
			[]string{`"run-1"`, `"try the other branch"`, `false`}},
		{[]string{"run", "resume", "run-1", "--phase", "verify"}, "WorkflowResumeItem",
			[]string{`"run-1"`, `"verify"`, `false`}},
		{[]string{"run", "retry-unit", "run-1", "beta", "--note", "fixed"}, "WorkflowRetryUnit",
			[]string{`"run-1"`, `"beta"`, `"fixed"`}},
		// One run id and a note, no unit id: the arity is what separates the
		// whole-attempt repair from the single-unit one.
		{[]string{"run", "retry-failed-units", "run-1", "--note", "limit reset"}, "WorkflowRetryFailedUnits",
			[]string{`"run-1"`, `"limit reset"`}},
		{[]string{"run", "pause", "run-1"}, "WorkflowPauseItem", []string{`"run-1"`}},
		{[]string{"run", "cancel", "run-1"}, "WorkflowCancelItem", []string{`"run-1"`}},
		// The soft stop's two directions are one verb and one flag, so the wire
		// carries the state asked for rather than which of two verbs was typed.
		// (--clear is TestRunSoftStopClearSendsTheWithdrawal; one row per method
		// here, because this table asserts each method was called exactly once.)
		{[]string{"run", "soft-stop", "run-1"}, "WorkflowRequestSoftStop",
			[]string{`"run-1"`, `true`}},
	} {
		code, stdout, stderr := runCLI(test.args, backend.env())
		if code != exitOK {
			t.Fatalf("%v exit = %d (%s)", test.args, code, stderr)
		}
		// Every control command reports the run's state afterwards: "I stopped
		// it" is only useful with "and here is where it is now".
		if !strings.Contains(stdout, "run=run-1") {
			t.Fatalf("%v output = %q", test.args, stdout)
		}
		calls := backend.recorded(test.method)
		if len(calls) != 1 {
			t.Fatalf("%s called %d times", test.method, len(calls))
		}
		if len(calls[0].Params) != len(test.want) {
			t.Fatalf("%s params = %v, want %v", test.method, calls[0].Params, test.want)
		}
		for i, want := range test.want {
			if string(calls[0].Params[i]) != want {
				t.Fatalf("%s param %d = %s, want %s", test.method, i, calls[0].Params[i], want)
			}
		}
	}
}

// --refresh-def is the repair for a prompt edited while the run was parked, so
// it has to reach the app from wherever a caller typed it: flags permute around
// positionals, and a run id that follows the flag is still the run id.
func TestRunRefreshDefReachesTheAppFromEveryPosition(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowResumeItem", nil)
	backend.reply("WorkflowRerunItem", nil)
	backend.reply("WorkflowAgentRunStatus", map[string]any{"itemId": "run-1", "state": "running"})

	for _, test := range []struct {
		args   []string
		method string
		want   []string
	}{
		{[]string{"run", "resume", "--refresh-def", "run-1"}, "WorkflowResumeItem",
			[]string{`"run-1"`, `""`, `true`}},
		{[]string{"run", "resume", "run-1", "--refresh-def"}, "WorkflowResumeItem",
			[]string{`"run-1"`, `""`, `true`}},
		{[]string{"run", "resume", "--phase", "verify", "run-1", "--refresh-def"}, "WorkflowResumeItem",
			[]string{`"run-1"`, `"verify"`, `true`}},
		{[]string{"run", "rerun", "--refresh-def", "run-1", "--guidance", "the prompt changed"}, "WorkflowRerunItem",
			[]string{`"run-1"`, `"the prompt changed"`, `true`}},
	} {
		backend.reset()
		code, _, stderr := runCLI(test.args, backend.env())
		if code != exitOK {
			t.Fatalf("%v exit = %d (%s)", test.args, code, stderr)
		}
		calls := backend.recorded(test.method)
		if len(calls) != 1 {
			t.Fatalf("%v: %s called %d times", test.args, test.method, len(calls))
		}
		if len(calls[0].Params) != len(test.want) {
			t.Fatalf("%v params = %v, want %v", test.args, calls[0].Params, test.want)
		}
		for i, want := range test.want {
			if string(calls[0].Params[i]) != want {
				t.Fatalf("%v param %d = %s, want %s", test.args, i, calls[0].Params[i], want)
			}
		}
	}
}

// The freeze is invisible from the outside, so the two verbs that can undo it
// have to say so where a caller reads before typing.
//
// The comparison unwraps both sides. A usage page is prose wrapped for a
// terminal, so where its line breaks fall is a rendering detail — pinning them
// makes an edit to a neighbouring sentence fail on the wrap rather than on
// anything a caller would notice.
func TestRunUsageStatesTheDefinitionFreezeAndItsRepair(t *testing.T) {
	for _, test := range []struct{ args, wants []string }{
		{[]string{"run", "resume", "--help"}, []string{
			"[--refresh-def]",
			"The definition a run froze at start is what it runs",
			"re-reads the workflow and its prompt files from disk",
			"It applies at a fresh phase entry only",
			"a call reads its target from disk every time it is made",
			// Every continuable park, because this is the page that says which
			// resumes --refresh-def is refused on.
			"paused, interrupted, checkpoint, unit-failed, or retries-exhausted continues an attempt whose work was launched under the frozen definition",
		}},
		{[]string{"run", "rerun", "--help"}, []string{
			"[--refresh-def]",
			"re-reads the workflow and its prompt files from disk",
		}},
		{[]string{"run", "help"}, []string{"--refresh-def re-reads the definition from disk"}},
	} {
		code, stdout, stderr := runCLI(test.args, noEnv)
		if code != exitOK {
			t.Fatalf("%v exit = %d (%s)", test.args, code, stderr)
		}
		unwrapped := unwrapText(stdout)
		for _, want := range test.wants {
			if !strings.Contains(unwrapped, unwrapText(want)) {
				t.Fatalf("%v usage is missing %q:\n%s", test.args, want, stdout)
			}
		}
	}
}

// unwrapText collapses every run of whitespace to one space, so an assertion
// about what a usage page SAYS is not also an assertion about where it wraps.
func unwrapText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// An engine-diagnosed park had no readable surface at all: `run status` showed
// `status=parked` and the reader went to the filesystem. The cause is a field on
// the attempt line, bounded because a status block carries one line per attempt.
func TestRunStatusRendersTheParkCauseBounded(t *testing.T) {
	oversize := strings.Repeat("c", maxCauseRunes*3)
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "needs-human", "reason": "setup-failed",
		"resting": true,
		"phases": []map[string]any{
			{"phaseId": "implement", "attempt": 1, "status": "parked",
				"cause": `provision worktree: branch "ao/wave-3" already exists`},
			{"phaseId": "plan", "attempt": 1, "status": "completed"},
			{"phaseId": "wide", "attempt": 1, "status": "parked", "cause": oversize},
		},
	})
	code, stdout, stderr := runCLI([]string{"run", "status", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	// Quoted as untrusted data: the sentence is the engine's, but the branch
	// name inside it is not.
	if !strings.Contains(stdout,
		`phase=implement attempt=1 status=parked cause="provision worktree: branch \"ao/wave-3\" already exists"`) {
		t.Fatalf("status output is missing the park cause:\n%s", stdout)
	}
	// An attempt with no engine-diagnosed cause carries no empty column.
	if !strings.Contains(stdout, "phase=plan attempt=1 status=completed\n") {
		t.Fatalf("a causeless attempt rendered a cause field:\n%s", stdout)
	}
	if strings.Contains(stdout, oversize[:maxCauseRunes+1]) {
		t.Fatalf("the park cause escaped its rune budget:\n%s", stdout)
	}
	// The marker survives quoting with its ellipsis escaped to ASCII, which is
	// what makes a cut-off cause distinguishable from a short one.
	if !strings.Contains(stdout, untrustedtext.Quote(oversize, maxCauseRunes)) {
		t.Fatalf("the cause was truncated without the visible marker:\n%s", stdout)
	}
}
