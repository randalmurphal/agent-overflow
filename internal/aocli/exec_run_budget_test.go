package aocli

import (
	"strings"
	"testing"
)

// The budget line: what a run has spent against the ceiling that will park it.
// Before it existed a caller holding only this CLI could not see a run's budget
// at all — the ceiling was enforced, announced once at the park, and invisible
// every moment before that.

func TestRunStatusRendersTheBudgetLine(t *testing.T) {
	for name, test := range map[string]struct {
		budget map[string]any
		want   []string
		absent []string
	}{
		// A dollar ceiling whose spend is partly rate-table priced says so: the
		// number is the one the check enforces, and a reader deciding whether to
		// raise the ceiling has to know it is an estimate.
		"usd estimated": {
			budget: map[string]any{
				"kind": "usd", "ceilingUsd": 25.0, "spentUsd": 6.4,
				"percent": 26, "estimated": true,
			},
			want:   []string{"budget=$6.40/$25.00", "(26%)", "estimated=true"},
			absent: []string{"exhausted=true", "of-run="},
		},
		"tokens exhausted": {
			budget: map[string]any{
				"kind": "tokens", "ceilingTokens": 1000, "spentTokens": 1400,
				"percent": 140, "exhausted": true,
			},
			// Tokens are exact whatever the rate table knows, so the caveat is
			// never printed for them.
			want:   []string{"budget=1400/1000 tokens", "(140%)", "exhausted=true"},
			absent: []string{"estimated=true"},
		},
		"wall clock": {
			budget: map[string]any{
				"kind": "wall_clock", "ceilingMillis": 7_200_000, "elapsedMillis": 1_800_000,
				"percent": 25,
			},
			want:   []string{"budget=30m0s/2h0m0s", "(25%)"},
			absent: []string{"estimated=true", "exhausted=true"},
		},
		// A row the rate table cannot price makes the total a LOWER BOUND, and
		// the run will park at its next phase boundary because a ceiling it has
		// not crossed cannot be judged. The line is where that is visible first.
		"usd with unpriceable rows": {
			budget: map[string]any{
				"kind": "usd", "ceilingUsd": 25.0, "spentUsd": 1.0,
				"percent": 4, "estimated": true, "unpricedRows": 3,
			},
			want:   []string{"budget=$1.00/$25.00", "estimated=true", "unpriced-rows=3"},
			absent: []string{"exhausted=true"},
		},
		// A called run spends its ROOT's ceiling, so the line names whose budget
		// it is — otherwise the numbers read as this run's own and a reader
		// wonders why a wave that ran twice shows the campaign's whole spend.
		"called run names its root": {
			budget: map[string]any{
				"kind": "tokens", "ceilingTokens": 500, "spentTokens": 120,
				"percent": 24, "rootItemId": "campaign-1",
			},
			want: []string{"budget=120/500 tokens", "of-run=campaign-1"},
		},
		// A kind this build does not know is still a real ceiling: the percent
		// survives under a name rather than the whole line disappearing and
		// reading as a run under no budget at all.
		"unknown kind": {
			budget: map[string]any{"kind": "credits", "percent": 61},
			want:   []string{"budget=?/? (credits)", "(61%)"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			backend := newFakeBackend(t)
			backend.reply("WorkflowAgentRunStatus", map[string]any{
				"itemId": "run-1", "workflowId": "flow", "state": "running",
				"budget": test.budget,
			})
			code, stdout, stderr := runCLI([]string{"run", "status", "run-1"}, backend.env())
			if code != exitOK {
				t.Fatalf("exit = %d (%s)", code, stderr)
			}
			for _, want := range test.want {
				if !strings.Contains(stdout, want) {
					t.Fatalf("status output is missing %q:\n%s", want, stdout)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(stdout, absent) {
					t.Fatalf("status output carries %q it should not:\n%s", absent, stdout)
				}
			}
		})
	}
}

// Most runs declare no ceiling, so the line is printed only when there is one.
// A `budget=none` on every run would be a field a reader learns to skip, on the
// surface they scan for what the run needs.
func TestRunStatusPrintsNoBudgetLineWithoutACeiling(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "running",
	})
	code, stdout, stderr := runCLI([]string{"run", "status", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if strings.Contains(stdout, "budget") {
		t.Fatalf("status output mentions a budget for a run with none:\n%s", stdout)
	}
}

// `run inspect` is the whole-run read, so it carries the same line from the same
// renderer — two spellings of one run's spend is exactly the drift the shared
// `writeBudgetLine` exists to prevent.
func TestRunInspectRendersTheBudgetLine(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentInspectRun", map[string]any{
		"run": map[string]any{
			"itemId": "run-1", "workflowId": "flow", "state": "running",
			"budget": map[string]any{
				"kind": "usd", "ceilingUsd": 10.0, "spentUsd": 3.25,
				"percent": 33, "estimated": true,
			},
		},
	})
	code, stdout, stderr := runCLI([]string{"run", "inspect", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "budget=$3.25/$10.00") || !strings.Contains(stdout, "estimated=true") {
		t.Fatalf("inspect output is missing the budget line:\n%s", stdout)
	}
}

// The acting verbs re-read status through reportRunState, which answers "where
// is it now" with the run line alone. The budget is a fact about the run's
// state, but a `run resume` that printed a spend line would be answering a
// question nobody asked in the one place the caller is scanning for the state.
func TestRunControlVerbsPrintOnlyTheRunLine(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowResumeItem", nil)
	backend.reply("WorkflowAgentRunStatus", map[string]any{
		"itemId": "run-1", "workflowId": "flow", "state": "running",
		"budget": map[string]any{"kind": "tokens", "ceilingTokens": 1000, "spentTokens": 10, "percent": 1},
	})
	code, stdout, stderr := runCLI([]string{"run", "resume", "run-1"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if strings.Contains(stdout, "budget=") {
		t.Fatalf("a control verb printed the budget line:\n%s", stdout)
	}
}
