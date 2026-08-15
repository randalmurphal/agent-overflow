package aocli

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/untrustedtext"
)

// `agent-overflow run inspect` and `agent-overflow run narrative`: what each
// verb sends, and what a reader who has only this CLI actually sees. The shared
// skeleton is exercised in exec_test.go against the same fake backend.

func TestRunInspectRendersTheWholePicture(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentInspectRun", map[string]any{
		"run": map[string]any{
			"itemId": "run-1", "workflowId": "campaign", "state": "needs-human", "reason": "gate",
			"currentPhaseId": "review", "currentPhaseOrdinal": 2, "phaseCount": 3, "resting": true,
			"seeds": map[string]any{"wave": 3, "package": "internal/store"},
			"phases": []map[string]any{
				{"phaseId": "survey", "attempt": 1, "status": "completed"},
				{"phaseId": "review", "attempt": 1, "status": "completed", "provider": "codex",
					"model": "gpt-5.2-codex", "effort": "xhigh", "decision": "human", "decisionTarget": "land",
					"outputs": []map[string]any{
						{"name": "verdict", "value": "changes-requested"},
						{"name": "worst-severity", "value": "P1"},
					},
					"outputOverflow": 4},
			},
		},
		"worktreePath": "/w/campaign-run-1", "branch": "campaign/wave-3", "baseBranch": "main",
		"children": []map[string]any{
			{"itemId": "run-2", "workflowId": "port", "state": "done",
				"parentPhaseId": "fan", "parentUnitId": "unit-a", "parentAttempt": 1},
			{"itemId": "run-3", "workflowId": "port", "state": "needs-human", "reason": "question",
				"parentPhaseId": "fan", "parentUnitId": "unit-b", "parentAttempt": 1},
		},
	})
	code, stdout, stderr := runCLI([]string{"run", "inspect", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	for _, want := range []string{
		"run=run-1 workflow=campaign state=needs-human reason=gate phase=review(2/3)\n",
		// The three facts no other verb exposes at all.
		"worktree=/w/campaign-run-1 branch=campaign/wave-3 base-branch=main\n",
		// Seeds read like `run output`'s declared outputs: one per line, sorted.
		"seed package=\"internal/store\"\n",
		"seed wave=3\n",
		"child=run-2 workflow=port state=done called-by=fan/unit-a.1\n",
		"child=run-3 workflow=port state=needs-human reason=question called-by=fan/unit-b.1\n",
		"phase=review attempt=1 status=completed provider=codex model=gpt-5.2-codex effort=xhigh decision=human->land\n",
		// The digest is what a gate decision is read off, quoted as the untrusted
		// model output it is.
		"  output verdict=\"changes-requested\"\n",
		"  output worst-severity=\"P1\"\n",
		// Truncation is stated, and states the verb that shows the rest.
		"  …and 4 more (agent-overflow run inspect --phase review)\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("inspect output is missing %q:\n%s", want, stdout)
		}
	}
	var sent inspectInput
	if err := json.Unmarshal(backend.recorded("WorkflowAgentInspectRun")[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent != (inspectInput{ItemID: "run-1"}) {
		t.Fatalf("sent = %#v, want the run id alone", sent)
	}
}

func TestRunInspectPhaseRendersTheAttemptWhole(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentInspectRun", map[string]any{
		"run": map[string]any{"itemId": "run-1", "state": "needs-human", "reason": "unit-failed"},
		"phase": map[string]any{
			"phaseId": "fan", "attempt": 2, "status": "parked", "provider": "claude",
			"model": "claude-opus-4-7", "decision": "park", "decisionTarget": "needs-review",
			"outputs": map[string]any{
				"findings": []map[string]any{{"severity": "P1", "note": "unchecked error"}},
				"verdict":  "changes-requested",
			},
			"units": []map[string]any{
				{"unitId": "unit-a", "kind": "unit", "status": "done", "unitAttempt": 1,
					"branch": "campaign/unit-a", "worktreePath": "/w/unit-a"},
				// The status alone would read as an agent failure; the note is what
				// says this unit was torn down by the operator's own pause.
				{"unitId": "join", "kind": "join", "status": "failed", "unitAttempt": 2,
					"note": "interrupted with its phase attempt (parked)"},
			},
		},
	})
	code, stdout, stderr := runCLI([]string{
		"run", "inspect", "run-1", "--phase", "fan", "--attempt", "2",
	}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	for _, want := range []string{
		"attempt-of=fan attempt=2 status=parked provider=claude model=claude-opus-4-7 decision=park->needs-review\n",
		// A structured output keeps its JSON; a string output is its text. Both
		// are quoted, so neither can read as an instruction to whoever is reading.
		"  output findings=\"[{\\\"note\\\":\\\"unchecked error\\\",\\\"severity\\\":\\\"P1\\\"}]\"\n",
		"  output verdict=\"changes-requested\"\n",
		"  unit=unit-a kind=unit status=done try=1 branch=campaign/unit-a worktree=/w/unit-a\n",
		"  unit=join kind=join status=failed try=2 note=\"interrupted with its phase attempt (parked)\"\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("drill-down output is missing %q:\n%s", want, stdout)
		}
	}
	var sent inspectInput
	if err := json.Unmarshal(backend.recorded("WorkflowAgentInspectRun")[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent != (inspectInput{ItemID: "run-1", PhaseID: "fan", Attempt: 2}) {
		t.Fatalf("sent = %#v", sent)
	}
}

// --attempt without --phase names an attempt of nothing. It is refused before
// the wire, because a caller who typed it meant a phase.
func TestRunInspectRefusesAnAttemptWithoutAPhase(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentInspectRun", map[string]any{"run": map[string]any{"itemId": "run-1"}})
	code, _, stderr := runCLI([]string{"run", "inspect", "run-1", "--attempt", "2"}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "--attempt names an attempt of --phase") {
		t.Fatalf("stderr = %q", stderr)
	}
	if calls := backend.recorded("WorkflowAgentInspectRun"); len(calls) != 0 {
		t.Fatalf("a usage error still reached the backend: %#v", calls)
	}
}

func TestRunNarrativePrintsTheAccountAndItsCoordinate(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunNarrative", map[string]any{
		"itemId": "run-1", "phaseId": "fan", "attempt": 2, "unitId": "unit-a", "unitAttempt": 3,
		"path":    "/data/workflow-runs/run-1/fan.2/units/unit-a.3/narrative.md",
		"present": true, "bytes": 41,
		"content": "# Ported internal/store\n\nEverything compiles.\n",
	})
	code, stdout, stderr := runCLI([]string{
		"run", "narrative", "run-1", "--phase", "fan", "--attempt", "2", "--unit", "unit-a",
	}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout,
		"run=run-1 phase=fan attempt=2 unit=unit-a try=3 path=/data/workflow-runs/run-1/fan.2/units/unit-a.3/narrative.md bytes=41\n") {
		t.Fatalf("header is missing or wrong:\n%s", stdout)
	}
	// The account itself is printed verbatim: it is the point of the command,
	// and a human reads it as prose.
	if !strings.Contains(stdout, "# Ported internal/store\n\nEverything compiles.\n") {
		t.Fatalf("content was not printed verbatim:\n%s", stdout)
	}
	var sent narrativeInput
	if err := json.Unmarshal(backend.recorded("WorkflowAgentRunNarrative")[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent != (narrativeInput{ItemID: "run-1", PhaseID: "fan", Attempt: 2, UnitID: "unit-a"}) {
		t.Fatalf("sent = %#v", sent)
	}
}

// An attempt that wrote no account is an answer, not a failure: exit 1, and the
// path that was looked for, so the reader can tell "it wrote nothing" from "I
// looked in the wrong place".
func TestRunNarrativeReportsAnAbsentAccountWithoutFailing(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunNarrative", map[string]any{
		"itemId": "run-1", "phaseId": "review", "attempt": 1,
		"path": "/data/workflow-runs/run-1/review.1/narrative.md", "present": false,
	})
	code, stdout, stderr := runCLI([]string{"run", "narrative", "run-1", "--phase", "review"}, backend.env())
	if code != exitFindings {
		t.Fatalf("exit = %d (%s), want %d", code, stderr, exitFindings)
	}
	for _, want := range []string{
		"run=run-1 phase=review attempt=1 narrative=absent\n",
		"nothing wrote /data/workflow-runs/run-1/review.1/narrative.md\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("absent output is missing %q:\n%s", want, stdout)
		}
	}
	// --json still prints exactly the app's document, absence included.
	code, stdout, _ = runCLI([]string{"run", "narrative", "run-1", "--phase", "review", "--json"}, backend.env())
	if code != exitFindings {
		t.Fatalf("--json exit = %d, want %d", code, exitFindings)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("--json did not print one document: %v\n%s", err, stdout)
	}
	if decoded["present"] != false {
		t.Fatalf("--json document = %v", decoded)
	}
}

func TestRunNarrativeStatesTruncation(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunNarrative", map[string]any{
		"itemId": "run-1", "phaseId": "review", "attempt": 1,
		"path":    "/data/workflow-runs/run-1/review.1/narrative.md",
		"present": true, "bytes": 900_000, "truncated": true, "content": "the first part",
	})
	code, stdout, stderr := runCLI([]string{"run", "narrative", "run-1", "--phase", "review"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "bytes=900000 truncated-at=14\n") {
		t.Fatalf("truncation was not stated:\n%s", stdout)
	}
}

func TestRunNarrativeRequiresAPhase(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunNarrative", map[string]any{"itemId": "run-1", "present": false})
	for _, args := range [][]string{
		{"run", "narrative", "run-1"},
		{"run", "narrative", "run-1", "--phase", "  "},
		{"run", "narrative", "--phase", "review"},
		{"run", "narrative", "run-1", "extra", "--phase", "review"},
	} {
		if code, _, _ := runCLI(args, backend.env()); code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
	}
	if calls := backend.recorded("WorkflowAgentRunNarrative"); len(calls) != 0 {
		t.Fatalf("a usage error still reached the backend: %#v", calls)
	}
}

// Flags may sit before, after, or between the positionals on the new verbs too:
// the permuted parser is the binary's rule, not a per-command one.
func TestRunInspectAndNarrativeAcceptPermutedFlags(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentInspectRun", map[string]any{"run": map[string]any{"itemId": "run-1"}})
	backend.reply("WorkflowAgentRunNarrative", map[string]any{
		"itemId": "run-1", "phaseId": "review", "attempt": 1, "path": "/n", "present": true, "content": "x",
	})
	if code, _, stderr := runCLI([]string{
		"run", "inspect", "--phase", "review", "run-1", "--json",
	}, backend.env()); code != exitOK {
		t.Fatalf("permuted inspect exit = %d (%s)", code, stderr)
	}
	if code, _, stderr := runCLI([]string{
		"run", "narrative", "--unit", "unit-a", "run-1", "--phase", "review",
	}, backend.env()); code != exitOK {
		t.Fatalf("permuted narrative exit = %d (%s)", code, stderr)
	}
	var sent inspectInput
	if err := json.Unmarshal(backend.recorded("WorkflowAgentInspectRun")[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent != (inspectInput{ItemID: "run-1", PhaseID: "review"}) {
		t.Fatalf("permuted inspect sent = %#v", sent)
	}
}

// The seed lines belong to the single-run reads. A control verb reports where
// the run is now, and a run's frozen inputs are not that.
func TestRunStatusPrintsSeedsAndControlVerbsDoNot(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "state": "running",
		"seeds": map[string]any{"wave": 3, "label": "store"},
	})
	code, stdout, stderr := runCLI([]string{"run", "status", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "seed label=\"store\"\n") || !strings.Contains(stdout, "seed wave=3\n") {
		t.Fatalf("status did not print the run's seeds:\n%s", stdout)
	}
	backend.reply("WorkflowPauseItem", nil)
	code, stdout, stderr = runCLI([]string{"run", "pause", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("pause exit = %d (%s)", code, stderr)
	}
	if strings.Contains(stdout, "seed ") {
		t.Fatalf("a control verb printed the run's seeds:\n%s", stdout)
	}
}

// Naming an attempt is how a caller says the bounded form on the status line
// was not enough, so the drill-down prints the cause whole, on its own line,
// exactly as it prints output values whole.
func TestRunInspectPhaseRendersTheParkCauseWhole(t *testing.T) {
	cause := strings.Repeat("c", maxCauseRunes*2)
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentInspectRun", map[string]any{
		"run": map[string]any{"itemId": "run-1", "state": "needs-human", "reason": "setup-failed"},
		"phase": map[string]any{
			"phaseId": "implement", "attempt": 1, "status": "parked", "cause": cause,
		},
	})
	code, stdout, stderr := runCLI([]string{"run", "inspect", "run-1", "--phase", "implement"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "\n  cause="+untrustedtext.Quote(cause, 0)+"\n") {
		t.Fatalf("drill-down did not carry the whole park cause:\n%s", stdout)
	}
}

// An attempt with no engine-diagnosed cause prints no cause line: an empty one
// would read as a diagnosis that was lost.
func TestRunInspectPhaseOmitsAnAbsentParkCause(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentInspectRun", map[string]any{
		"run": map[string]any{"itemId": "run-1", "state": "needs-human", "reason": "question"},
		"phase": map[string]any{
			"phaseId": "ask", "attempt": 1, "status": "parked",
		},
	})
	code, stdout, stderr := runCLI([]string{"run", "inspect", "run-1", "--phase", "ask"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if strings.Contains(stdout, "cause=") {
		t.Fatalf("a causeless attempt rendered a cause line:\n%s", stdout)
	}
}
