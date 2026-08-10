package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

// The merge-join contract end to end (`accounts_for_units: true`).
//
// The live failure it exists for: a merge join that stopped at its first
// conflict, reported the lanes it had already taken, and said nothing at all
// about the one it dropped. Nothing downstream could tell — the join's envelope
// IS the phase's envelope, so a unit it does not mention simply does not exist.

func writeAccountingFanOutWorkflow(t *testing.T, configRoot string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: accounting-flow
name: Accounting flow
phases:
  - id: port
    name: Port in parallel
    shape: fan-out
    outputs:
      merged:
        schema:
          type: array
          items:
            type: string
      blocked:
        schema:
          type: array
          items:
            type: object
            properties:
              unit:
                type: string
              reason:
                type: string
            required: [unit, reason]
    fan_out:
      - id: alpha
        provider: claude
        model: claude-opus-4-7
        prompt: lane.md
        access: read-only
      - id: beta
        provider: claude
        model: claude-opus-4-7
        prompt: lane.md
        access: read-only
    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: merge.md
      access: read-only
      accounts_for_units: true
    gate:
      routes:
        - to: done
cleanup: manual
`
	if err := os.WriteFile(filepath.Join(dir, "accounting-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"lane.md":  "Port the LANE-UNIT slice",
		"merge.md": "MERGE-UNIT: consolidate {{units}}",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeAccountingClaude answers a lane turn, then the join's first turn with an
// accounting that drops `beta` entirely — the exact live failure — and its retry
// turn with a complete one. The three cases are told apart by what the prompt
// says, so the script needs no state: only a retry carries the validation
// feedback header.
func writeAccountingClaude(t *testing.T, promptLog string) string {
	t.Helper()
	envelope := func(payload string) string {
		return `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + payload + `}'`
	}
	script := `#!/bin/bash
while IFS= read -r line; do
  printf '%s\n' "$line" >> ` + workflowShellQuote(promptLog) + `
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"accounting","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  if [[ "$line" == *"did not produce a valid workflow control envelope"* ]]; then
    ` + envelope(`{"status":"done","outputs":{"merged":["alpha"],"blocked":[{"unit":"beta","reason":"conflicts in a.go"}]},"question":null,"reason":null,"narrative":"accounted for both"}`) + `
    continue
  fi
  if [[ "$line" == *MERGE-UNIT* ]]; then
    ` + envelope(`{"status":"done","outputs":{"merged":["alpha"],"blocked":[]},"question":null,"reason":null,"narrative":"took what merged cleanly"}`) + `
    continue
  fi
  ` + envelope(`{"status":"done","outputs":null,"question":null,"reason":null,"narrative":"ported the slice"}`) + `
done
`
	return writeExecutable(t, "accounting-claude.sh", script)
}

// A join that leaves a unit out of both lists is refused, told which unit, and
// RETRIED — the ordinary envelope-validation feedback path (D44), never a park
// and never a silent pass. The run then completes on the corrected accounting.
func TestJoinAccountingRefusalRetriesWithFeedbackInsteadOfParking(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	projectRow := testutil.EnsureProject(t, app.store, testutil.InitGitRepo(t))
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeAccountingFanOutWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	promptLog := filepath.Join(t.TempDir(), "prompts.txt")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeAccountingClaude(t, promptLog)}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "accounting-flow", "shared", "port it", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	// Done, not parked: a refused accounting is feedback the join can act on.
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")

	units := unitsByID(t, app, item.ID)
	if units["merge"].Status != store.WorkItemUnitDone {
		t.Fatalf("join did not complete: %+v", units["merge"])
	}

	prompts := strings.Join(readLines(t, promptLog), "\n")
	// The obligation is stated to the join BEFORE it answers, and names the exact
	// set it will be judged against.
	if !strings.Contains(prompts, "must account for every unit") {
		t.Fatalf("the join was never told the rule it is held to:\n%s", prompts)
	}
	for _, id := range []string{`\"alpha\"`, `\"beta\"`} {
		if !strings.Contains(prompts, id) {
			t.Fatalf("the join was not shown unit %s it is judged against:\n%s", id, prompts)
		}
	}
	// The refusal names the unit that went missing, so the retry has something to
	// act on rather than a restatement of the schema.
	if !strings.Contains(prompts, `unit \"beta\" is neither merged nor blocked`) {
		t.Fatalf("the retry feedback does not name the unaccounted unit:\n%s", prompts)
	}

	// The accepted envelope is the corrected one: the phase's outputs are what
	// the gate and everything downstream read.
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Phases) != 1 || detail.Phases[0].Status != "completed" {
		t.Fatalf("phase = %+v", detail.Phases)
	}
	var envelope struct {
		Outputs struct {
			Merged  []string `json:"merged"`
			Blocked []struct {
				Unit   string `json:"unit"`
				Reason string `json:"reason"`
			} `json:"blocked"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(detail.Phases[0].OutputEnvelope, &envelope); err != nil {
		t.Fatalf("phase envelope %s: %v", detail.Phases[0].OutputEnvelope, err)
	}
	if len(envelope.Outputs.Merged) != 1 || envelope.Outputs.Merged[0] != "alpha" {
		t.Fatalf("accepted merged list = %+v", envelope.Outputs.Merged)
	}
	if len(envelope.Outputs.Blocked) != 1 || envelope.Outputs.Blocked[0].Unit != "beta" ||
		envelope.Outputs.Blocked[0].Reason == "" {
		t.Fatalf("accepted blocked list = %+v", envelope.Outputs.Blocked)
	}
}
