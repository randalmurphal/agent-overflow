package aocli

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// `agent-overflow run guide` — leaving an instruction for a run's next phase
// entry. The verb's value is in the sentence the app returns, so the block has
// to carry it verbatim.

func TestRunGuideSendsTheTextAndPrintsWhenItIsRead(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentGuideRun", map[string]any{
		"itemId": "run-1", "pending": 2, "maxPending": 8, "by": "human",
		"state": "running", "phaseId": "wave",
		"deliversNote": "the run is working; this is delivered at its next FRESH phase entry",
		"callerNote":   "this run was called by root-1",
	})

	code, stdout, stderr := runCLI([]string{"run", "guide", "run-1", "prefer the smaller diff"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	for _, want := range []string{
		"run=run-1", "pending=2/8", "by=human", "state=running", "phase=wave",
		"when: the run is working", "note: this run was called by root-1",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
	// The caller just typed the text; reprinting it would spend an agent's
	// context on its own input.
	if strings.Contains(stdout, "prefer the smaller diff") {
		t.Fatalf("the block echoed the guidance back: %q", stdout)
	}

	calls := backend.recorded("WorkflowAgentGuideRun")
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	var sent struct {
		ItemID string `json:"itemId"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(calls[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ItemID != "run-1" || sent.Text != "prefer the smaller diff" {
		t.Fatalf("sent = %#v", sent)
	}
}

// The text is a positional because it is the point of the command, and a guide
// with no text is refused before the app is called — an empty steer steers
// nothing.
func TestRunGuideRefusesAMissingRunOrText(t *testing.T) {
	backend := newFakeBackend(t)
	for _, args := range [][]string{
		{"run", "guide"},
		{"run", "guide", "run-1"},
		{"run", "guide", "run-1", "steer", "run-2"},
	} {
		code, _, stderr := runCLI(args, backend.env())
		if code != exitError {
			t.Fatalf("%v exit = %d, want %d", args, code, exitError)
		}
		if stderr == "" {
			t.Fatalf("%v refused silently", args)
		}
	}
	if calls := backend.recorded("WorkflowAgentGuideRun"); len(calls) != 0 {
		t.Fatalf("a usage error still reached the app: %#v", calls)
	}
}

// The app's refusals — a terminal run, a full slot — reach the caller whole.
// They carry the numbers this CLI deliberately does not duplicate.
func TestRunGuideForwardsTheAppsRefusal(t *testing.T) {
	backend := newFakeBackend(t)
	backend.refuse("WorkflowAgentGuideRun", transport.ErrCodeInternal,
		`guide run "run-1": 8 entries are already waiting for this run's next phase entry`)
	code, _, stderr := runCLI([]string{"run", "guide", "run-1", "steer"}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "8 entries are already waiting") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// A missing grant is a typed refusal the CLI must not reword: the caller has to
// learn which grant to add.
func TestRunGuideCarriesTheGrantRefusal(t *testing.T) {
	backend := newFakeBackend(t)
	backend.refuse("WorkflowAgentGuideRun", transport.ErrCodeGrantRequired,
		`method "WorkflowAgentGuideRun" requires grant "start-run"`)
	code, _, stderr := runCLI([]string{"run", "guide", "run-1", "steer"}, backend.env())
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "start-run") {
		t.Fatalf("stderr = %q, want the missing grant named", stderr)
	}
}
