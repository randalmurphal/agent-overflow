package starters

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// The reference merge script is CONTENT, but the contract it demonstrates is
// enforced by the engine: `accounts_for_units: true` post-validates the join's
// envelope against exactly the unit ids the join was SHOWN. A script that
// answers lists the engine refuses is not a reference, it is a wave that dies at
// its merge — so these tests run the real script and put its real envelope
// through the real post-validation.
//
// Nothing here needs a git repository: every case is decided before the script
// reaches a branch.

// runMergeScript writes the embedded script to a temp dir, runs it over one
// units array, and returns its envelope and exit code.
func runMergeScript(t *testing.T, units []any) (json.RawMessage, int) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH; the reference merge script cannot be executed here")
	}
	set, err := Fetch("port-campaign")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := ""
	for _, file := range set.Files {
		if strings.HasSuffix(file.Name, ".py") {
			script = filepath.Join(dir, file.Name)
			if err := os.WriteFile(script, file.Data, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if script == "" {
		t.Fatal("the campaign ships no reference merge script")
	}
	encoded, err := json.Marshal(units)
	if err != nil {
		t.Fatal(err)
	}
	envelopePath := filepath.Join(dir, "envelope.json")
	command := exec.Command(python, script, string(encoded))
	command.Dir = dir
	command.Env = append(os.Environ(), "AO_ENVELOPE="+envelopePath)
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run merge script: %v (%s)", err, output)
		}
		code = exit.ExitCode()
	}
	written, readErr := os.ReadFile(envelopePath)
	if readErr != nil {
		t.Fatalf("the script wrote no envelope (exit %d): %v\n%s", code, readErr, output)
	}
	t.Logf("merge script exit %d\n%s", code, output)
	return written, code
}

// validateJoinEnvelope puts the script's envelope through the very check the
// engine runs: the phase's declared outputs plus the accounting obligation, over
// the id set the engine itself would have read from the same units array.
func validateJoinEnvelope(t *testing.T, units []any, envelope json.RawMessage, exitCode int) {
	t.Helper()
	phase := phaseByID(t, starterWorkflow(t, "port-campaign"), "implement")
	ids := def.UnitIDsFromResults(map[string]any{def.UnitsVariable: units})
	contract := def.JoinEnvelope(phase, ids)
	// The tool driver overlays `passed` / `exit-code` before validation, because
	// a command cannot know its own exit status while writing the file.
	payload := workflowrunner.ApplyToolOutputs(envelope, exitCode)
	if err := contract.Validate(payload); err != nil {
		t.Fatalf("the engine would refuse this join envelope: %v\n%s", err, payload)
	}
}

func envelopeLists(t *testing.T, envelope json.RawMessage) (merged []string, blocked map[string]string) {
	t.Helper()
	var decoded struct {
		Outputs struct {
			Merged  []string `json:"merged"`
			Blocked []struct {
				Unit   string `json:"unit"`
				Reason string `json:"reason"`
			} `json:"blocked"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(envelope, &decoded); err != nil {
		t.Fatalf("decode envelope %s: %v", envelope, err)
	}
	blocked = make(map[string]string, len(decoded.Outputs.Blocked))
	for _, entry := range decoded.Outputs.Blocked {
		blocked[entry.Unit] = entry.Reason
	}
	return decoded.Outputs.Merged, blocked
}

// A DROPPED unit is a unit of the wave: the engine shows it to the join and
// holds the join to accounting for it. It is not a repair, though — a drop is
// the human's "proceed without it" — so it must not route the wave into the
// hand-landing phase.
func TestMergeScriptAccountsForADroppedLaneWithoutAskingForAHuman(t *testing.T) {
	units := []any{
		map[string]any{"id": "port-0", "index": 0, "status": "dropped", "branch": "lane/0"},
		map[string]any{"id": "port-1", "index": 1, "status": "done"},
	}
	envelope, code := runMergeScript(t, units)
	validateJoinEnvelope(t, units, envelope, code)

	merged, blocked := envelopeLists(t, envelope)
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want nothing merged", merged)
	}
	if reason, listed := blocked["port-0"]; !listed || !strings.Contains(reason, "dropped") {
		t.Fatalf("the dropped lane is not accounted for as dropped: %v", blocked)
	}
	if _, listed := blocked["port-1"]; !listed {
		t.Fatalf("a branchless lane was not blocked: %v", blocked)
	}
	// port-1 has no branch, so this wave DOES need a human; the assertion below
	// is that the drop is not what made it so.
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a lane nobody can land", code)
	}

	only := []any{units[0]}
	dropOnly, dropCode := runMergeScript(t, only)
	validateJoinEnvelope(t, only, dropOnly, dropCode)
	if dropCode != 0 {
		t.Fatalf("a wave whose only unmerged lane was dropped on purpose exited %d; "+
			"the gate would route it to the hand-landing phase to reverse a human's decision", dropCode)
	}
	if _, blockedOnly := envelopeLists(t, dropOnly); len(blockedOnly) != 1 {
		t.Fatalf("the dropped lane was left out of the accounting: %v", blockedOnly)
	}
}

// An entry the ENGINE could not read an id from is not a unit this join is
// accountable for — `def.UnitIDsFromResults` skips it, so naming it in either
// list is refused rather than credited. It is reported and it fails the gate; it
// is never silently absorbed into a clean-looking result.
func TestMergeScriptRefusesToReportACleanWaveOverAnUnusableEntry(t *testing.T) {
	for _, malformed := range []any{
		"not-an-object",
		map[string]any{"status": "done"},
		map[string]any{"id": "   ", "status": "done"},
		map[string]any{"id": 7, "status": "done"},
	} {
		units := []any{
			map[string]any{"id": "port-0", "index": 0, "status": "dropped"},
			malformed,
		}
		envelope, code := runMergeScript(t, units)
		validateJoinEnvelope(t, units, envelope, code)
		if code == 0 {
			t.Fatalf("entry %#v produced a clean wave: %s", malformed, envelope)
		}
		merged, blocked := envelopeLists(t, envelope)
		if len(merged) != 0 || len(blocked) != 1 {
			t.Fatalf("entry %#v changed the accounting of the real lanes: merged=%v blocked=%v",
				malformed, merged, blocked)
		}
	}
}

// The behaviour the script exists for, restated as a runnable check: zero units
// still owes two empty lists, and a wave of finished-but-branchless lanes is
// entirely accounted for rather than truncated at the first one.
func TestMergeScriptAccountsForEveryLaneIncludingNone(t *testing.T) {
	empty, code := runMergeScript(t, []any{})
	validateJoinEnvelope(t, []any{}, empty, code)
	if code != 0 {
		t.Fatalf("a join over zero units exited %d", code)
	}
	merged, blocked := envelopeLists(t, empty)
	if merged == nil || blocked == nil || len(merged) != 0 || len(blocked) != 0 {
		t.Fatalf("a join over zero units did not owe two empty lists: %s", empty)
	}

	units := make([]any, 0, 5)
	for _, id := range []string{"port-0", "port-1", "port-2", "port-3", "port-4"} {
		units = append(units, map[string]any{"id": id, "status": "done"})
	}
	all, allCode := runMergeScript(t, units)
	validateJoinEnvelope(t, units, all, allCode)
	if _, allBlocked := envelopeLists(t, all); len(allBlocked) != len(units) {
		t.Fatalf("the script stopped short of the whole wave: %v", allBlocked)
	}
	if allCode != 1 {
		t.Fatalf("a wave nobody could land exited %d", allCode)
	}
}
