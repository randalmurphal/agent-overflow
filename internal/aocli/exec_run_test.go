package aocli

import (
	"encoding/json"
	"strings"
	"testing"
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
		{"itemId": "run-2", "workflowId": "flow", "state": "needs-human", "reason": "gate"},
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
	calls := backend.recorded("WorkflowAgentListRuns")
	if len(calls) != 1 || string(calls[0].Params[0]) != "true" {
		t.Fatalf("--active was not sent: %#v", calls)
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
	backend.reply("WorkflowAgentRunStatus", map[string]any{"itemId": "run-1", "state": "running"})

	for _, test := range []struct {
		args   []string
		method string
		want   []string
	}{
		{[]string{"run", "rerun", "run-1", "--guidance", "try the other branch"}, "WorkflowRerunItem",
			[]string{`"run-1"`, `"try the other branch"`}},
		{[]string{"run", "resume", "run-1", "--phase", "verify"}, "WorkflowResumeItem",
			[]string{`"run-1"`, `"verify"`}},
		{[]string{"run", "retry-unit", "run-1", "beta", "--note", "fixed"}, "WorkflowRetryUnit",
			[]string{`"run-1"`, `"beta"`, `"fixed"`}},
		// One run id and a note, no unit id: the arity is what separates the
		// whole-attempt repair from the single-unit one.
		{[]string{"run", "retry-failed-units", "run-1", "--note", "limit reset"}, "WorkflowRetryFailedUnits",
			[]string{`"run-1"`, `"limit reset"`}},
		{[]string{"run", "pause", "run-1"}, "WorkflowPauseItem", []string{`"run-1"`}},
		{[]string{"run", "cancel", "run-1"}, "WorkflowCancelItem", []string{`"run-1"`}},
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
