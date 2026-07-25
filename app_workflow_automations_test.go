package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/scheduler"
)

// automationHarness is a real app with a real engine and scheduler over a
// temp config root, with the engine globally paused so a started run is
// admitted and persisted without any provider session existing.
type automationHarness struct {
	app       *App
	projectID string
}

func newAutomationHarness(t *testing.T) *automationHarness {
	t.Helper()
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeAutomationWorkflowFixture(t, configRoot)
	startWorkflowEngineForTest(t, app, configRoot)
	if err := app.WorkflowSetGlobalPause(true); err != nil {
		t.Fatal(err)
	}
	return &automationHarness{app: app, projectID: defaultTestProjectID}
}

func (h *automationHarness) input() WorkflowAutomationInput {
	return WorkflowAutomationInput{
		ProjectID: h.projectID, WorkflowID: "automation-flow", WorkflowScope: "shared",
		Name: "Nightly audit", Enabled: true,
		Trigger: json.RawMessage(`{"kind":"cron","expr":"0 3 * * *"}`),
		Seeds:   json.RawMessage(`{"goal":"audit the API"}`),
	}
}

func writeAutomationWorkflowFixture(t *testing.T, configRoot string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := `id: automation-flow
name: Automation flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: run.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`
	for name, content := range map[string]string{
		"automation-flow.yaml": workflow,
		"run.md":               "Audit {{goal}} and return the envelope.",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkflowCreateAutomationRefusesUnrunnableDefinitions(t *testing.T) {
	h := newAutomationHarness(t)
	cases := []struct {
		name   string
		mutate func(*WorkflowAutomationInput)
		want   string
	}{
		{
			name: "invalid cron",
			mutate: func(in *WorkflowAutomationInput) {
				in.Trigger = json.RawMessage(`{"kind":"cron","expr":"every night"}`)
			},
			want: "fields",
		},
		{
			name:   "cron field out of range",
			mutate: func(in *WorkflowAutomationInput) { in.Trigger = json.RawMessage(`{"kind":"cron","expr":"99 3 * * *"}`) },
			want:   "is invalid",
		},
		{
			name:   "unknown trigger kind",
			mutate: func(in *WorkflowAutomationInput) { in.Trigger = json.RawMessage(`{"kind":"webhook"}`) },
			want:   "must be cron or event",
		},
		{
			name:   "event outside the closed set",
			mutate: func(in *WorkflowAutomationInput) { in.Trigger = json.RawMessage(`{"kind":"event","on":"phase-done"}`) },
			want:   "event trigger on must be one of",
		},
		{
			name:   "unknown workflow",
			mutate: func(in *WorkflowAutomationInput) { in.WorkflowID = "no-such-flow" },
			want:   "no-such-flow",
		},
		{
			name:   "unknown project",
			mutate: func(in *WorkflowAutomationInput) { in.ProjectID = "no-such-project" },
			want:   "no rows in result set",
		},
		{
			name:   "reserved trigger seed",
			mutate: func(in *WorkflowAutomationInput) { in.Seeds = json.RawMessage(`{"trigger":"mine"}`) },
			want:   `seed "trigger" is reserved`,
		},
		{
			name:   "reserved job-notes seed",
			mutate: func(in *WorkflowAutomationInput) { in.Seeds = json.RawMessage(`{"job-notes":"mine"}`) },
			want:   `seed "job-notes" is reserved`,
		},
		{
			name:   "seeds that are not an object",
			mutate: func(in *WorkflowAutomationInput) { in.Seeds = json.RawMessage(`["goal"]`) },
			want:   "seeds must be an object",
		},
		{
			name: "malformed condition",
			mutate: func(in *WorkflowAutomationInput) {
				in.Condition = json.RawMessage(`{"eq":{"ref":"a","value":1},"exists":"b"}`)
			},
			want: "condition is malformed",
		},
		{
			name:   "missing name",
			mutate: func(in *WorkflowAutomationInput) { in.Name = "  " },
			want:   "name are required",
		},
		{
			name:   "bad scope",
			mutate: func(in *WorkflowAutomationInput) { in.WorkflowScope = "global" },
			want:   "scope must be project or shared",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := h.input()
			testCase.mutate(&input)
			if _, err := h.app.WorkflowCreateAutomation(input); err == nil {
				t.Fatalf("create succeeded, want a refusal containing %q", testCase.want)
			} else if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to contain %q", err, testCase.want)
			}
			automations, err := h.app.WorkflowListAutomations(h.projectID)
			if err != nil {
				t.Fatal(err)
			}
			if len(automations) != 0 {
				t.Fatalf("a refused create persisted a row: %#v", automations)
			}
		})
	}
}

func TestWorkflowAutomationCRUDRoundTrip(t *testing.T) {
	h := newAutomationHarness(t)
	created, err := h.app.WorkflowCreateAutomation(h.input())
	if err != nil {
		t.Fatal(err)
	}
	if created.TriggerKind != "cron" || created.TriggerSummary != "cron 0 3 * * *" {
		t.Fatalf("created view = %#v", created)
	}
	if created.NextFireAt <= time.Now().UnixMilli() {
		t.Fatalf("next fire = %d, want a future time", created.NextFireAt)
	}
	if created.TriggerError != "" {
		t.Fatalf("trigger error = %q", created.TriggerError)
	}

	// Notes are their own surface: a definition update must not clobber what the
	// last run wrote back (§5 update-job-notes).
	if err := h.app.WorkflowSetJobNotes(created.ID, "the flaky suite is quarantined"); err != nil {
		t.Fatal(err)
	}
	updated := h.input()
	updated.Name = "Nightly audit v2"
	updated.Trigger = json.RawMessage(`{"kind":"event","on":"item-failed","workflowId":"automation-flow"}`)
	view, err := h.app.WorkflowUpdateAutomation(created.ID, updated)
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != "Nightly audit v2" || view.TriggerSummary != "event item-failed on automation-flow" {
		t.Fatalf("updated view = %#v", view)
	}
	if view.Notes != "the flaky suite is quarantined" {
		t.Fatalf("update clobbered the job notes: %q", view.Notes)
	}
	// An event trigger has no next fire, and saying otherwise would be a lie.
	if view.NextFireAt != 0 {
		t.Fatalf("event trigger reported next fire %d", view.NextFireAt)
	}

	// A disabled automation reports no next fire either.
	if err := h.app.WorkflowSetAutomationEnabled(created.ID, false); err != nil {
		t.Fatal(err)
	}
	disabledCron := h.input()
	if _, err := h.app.WorkflowUpdateAutomation(created.ID, disabledCron); err != nil {
		t.Fatal(err)
	}
	if err := h.app.WorkflowSetAutomationEnabled(created.ID, false); err != nil {
		t.Fatal(err)
	}
	listed, err := h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Enabled || listed[0].NextFireAt != 0 {
		t.Fatalf("listed = %#v, want one disabled row with no next fire", listed)
	}

	if err := h.app.WorkflowDeleteAutomation(created.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("listed after delete = %#v", listed)
	}
}

// A trigger that stopped parsing — an older shape, a hand-edited row — is
// reported on the row rather than dropped in silence.
func TestWorkflowListAutomationsSurfacesBrokenTriggers(t *testing.T) {
	h := newAutomationHarness(t)
	now := time.Now().UnixMilli()
	if err := h.app.store.CreateAutomation(store.Automation{
		ID: "legacy", ProjectID: h.projectID, WorkflowID: "automation-flow", WorkflowScope: "shared",
		Name: "Legacy", Enabled: true, Trigger: json.RawMessage(`{"cron":"0 3 * * *"}`),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed = %#v", listed)
	}
	if listed[0].TriggerError == "" || listed[0].NextFireAt != 0 || listed[0].TriggerSummary != "" {
		t.Fatalf("broken row = %#v, want a standing trigger error and no schedule", listed[0])
	}
}

func TestWorkflowRunAutomationNowStartsThroughTheOneStartPath(t *testing.T) {
	h := newAutomationHarness(t)
	input := h.input()
	// Run now bypasses the condition: pressing the button is the decision. This
	// condition is false for a manual fire and would skip a scheduled one.
	input.Condition = json.RawMessage(`{"eq":{"ref":"trigger.kind","value":"event"}}`)
	created, err := h.app.WorkflowCreateAutomation(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.WorkflowSetJobNotes(created.ID, "last run left the migration half applied"); err != nil {
		t.Fatal(err)
	}

	item, err := h.app.WorkflowRunAutomationNow(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Source != scheduler.Source || item.SourceRef != created.ID {
		t.Fatalf("run provenance = (%q, %q), want (automation, %s)", item.Source, item.SourceRef, created.ID)
	}
	if item.Goal != "Nightly audit (run now)" {
		t.Fatalf("goal = %q", item.Goal)
	}
	if item.StepMode {
		t.Fatal("an unattended run started in step mode")
	}

	var seeds map[string]any
	if err := json.Unmarshal(item.Seeds, &seeds); err != nil {
		t.Fatal(err)
	}
	if seeds["goal"] != "audit the API" {
		t.Fatalf("stored seeds were dropped: %#v", seeds)
	}
	if seeds[scheduler.JobNotesVariable] != "last run left the migration half applied" {
		t.Fatalf("job notes seed = %#v", seeds[scheduler.JobNotesVariable])
	}
	trigger, ok := seeds[scheduler.TriggerVariable].(map[string]any)
	if !ok || trigger["kind"] != string(scheduler.KindManual) {
		t.Fatalf("trigger seed = %#v", seeds[scheduler.TriggerVariable])
	}

	// The fire is recorded on the row, and the second press is refused loudly
	// while that run is still live — no queueing, no overlap, no silent skip.
	listed, err := h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].LastRunItemID != item.ID || listed[0].LastFiredAt == 0 {
		t.Fatalf("fire record = %#v", listed[0])
	}
	_, err = h.app.WorkflowRunAutomationNow(created.ID)
	if err == nil {
		t.Fatal("a second Run now while the first run is live succeeded")
	}
	if !strings.Contains(err.Error(), item.ID) || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("error = %v, want it to name the active run", err)
	}
	listed, err = h.app.WorkflowListAutomations(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].SkipCount != 0 {
		t.Fatalf("a manual refusal recorded a skip: %#v", listed[0])
	}

	// Once that run is out of the way, the automation can fire again.
	if err := h.app.store.UpdateWorkItemState(item.ID, "done", "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	second, err := h.app.WorkflowRunAutomationNow(created.ID)
	if err != nil {
		t.Fatalf("Run now after the previous run settled: %v", err)
	}
	if second.ID == item.ID {
		t.Fatal("Run now returned the previous run")
	}
}

func TestWorkflowRunAutomationNowWorksWhileDisabledAndBroken(t *testing.T) {
	h := newAutomationHarness(t)
	created, err := h.app.WorkflowCreateAutomation(h.input())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.WorkflowSetAutomationEnabled(created.ID, false); err != nil {
		t.Fatal(err)
	}
	// A stored trigger that no longer parses says nothing about the workflow the
	// human is asking to run, so Run now still works on it.
	stored, err := h.app.store.GetAutomation(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Trigger = json.RawMessage(`{"kind":"cron","expr":"nope"}`)
	if err := h.app.store.UpdateAutomation(stored); err != nil {
		t.Fatal(err)
	}

	item, err := h.app.WorkflowRunAutomationNow(created.ID)
	if err != nil {
		t.Fatalf("Run now on a disabled, broken-trigger automation: %v", err)
	}
	if item.SourceRef != created.ID {
		t.Fatalf("run provenance = %q", item.SourceRef)
	}
}

func TestWorkflowAutomationRPCsRequireIdentifiers(t *testing.T) {
	h := newAutomationHarness(t)
	for name, err := range map[string]error{
		"update":  mustErr(h.app.WorkflowUpdateAutomation("  ", h.input())),
		"delete":  h.app.WorkflowDeleteAutomation("  "),
		"enable":  h.app.WorkflowSetAutomationEnabled("  ", true),
		"run now": mustErr(h.app.WorkflowRunAutomationNow("  ")),
		"list":    mustErrList(h.app.WorkflowListAutomations("  ")),
	} {
		if err == nil {
			t.Errorf("%s with a blank identifier succeeded", name)
		}
	}
}

func mustErr[T any](_ T, err error) error { return err }

func mustErrList(_ []WorkflowAutomationView, err error) error { return err }
