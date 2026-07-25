package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

func TestWorkflowJobNotesBoundedAndUnknownRejected(t *testing.T) {
	app, _ := setupE2EApp(t)
	automation := store.Automation{
		ID: "automation", ProjectID: "project", WorkflowID: "wf", WorkflowScope: "shared",
		Name: "Nightly", Enabled: true, Trigger: json.RawMessage(`{}`), CreatedAt: 1, UpdatedAt: 1,
	}
	if err := app.store.CreateAutomation(automation); err != nil {
		t.Fatal(err)
	}
	if err := app.WorkflowSetJobNotes(automation.ID, "continue here"); err != nil {
		t.Fatal(err)
	}
	if notes, err := app.WorkflowGetJobNotes(automation.ID); err != nil || notes != "continue here" {
		t.Fatalf("notes = %q err=%v", notes, err)
	}
	if err := app.WorkflowSetJobNotes(automation.ID, strings.Repeat("x", notify.MaxBodyBytes+1)); err == nil {
		t.Fatal("oversized notes unexpectedly succeeded")
	}
	if _, err := app.WorkflowGetJobNotes("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown notes error = %v, want sql.ErrNoRows", err)
	}
	if err := app.WorkflowSetJobNotes("missing", "notes"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown set error = %v, want sql.ErrNoRows", err)
	}
}

func TestWorkflowListDefinitionsIncludesValidation(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	app.configDir = configRoot
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow, err := app.store.GetProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkflowListingFixtures(t, configRoot, projectRow.Slug)
	catalog, err := app.WorkflowListDefinitions(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.BaseBranch != "main" {
		t.Fatalf("catalog metadata = %+v", catalog)
	}
	listings := catalog.Workflows
	if len(listings) != 2 {
		t.Fatalf("listings = %+v", listings)
	}
	byID := make(map[string]WorkflowDefinitionListing, len(listings))
	for _, listing := range listings {
		byID[listing.ID] = listing
	}
	valid := byID["valid"]
	if !valid.Valid || !valid.AllBindingsAvailable || valid.PhaseCount != 2 || valid.HumanGateCount != 1 || len(valid.Phases) != 2 || valid.Phases[1].Provider != "codex" {
		t.Fatalf("valid listing = %+v", valid)
	}
	if len(valid.Inputs) != 4 || valid.Inputs[0].Name != "approved" || valid.Inputs[0].Type != "boolean" || !valid.Inputs[0].Required ||
		valid.Inputs[1].Name != "mode" || valid.Inputs[1].Type != "string" || len(valid.Inputs[1].Enum) != 2 || valid.Inputs[1].Required ||
		valid.Inputs[2].Name != "notes" || !valid.Inputs[2].Multiline || !valid.Inputs[2].Required ||
		valid.Inputs[3].Name != "source" || valid.Inputs[3].Format != "path" || !valid.Inputs[3].Required {
		t.Fatalf("valid inputs = %+v", valid.Inputs)
	}
	invalid := byID["invalid"]
	if invalid.Valid || invalid.AllBindingsAvailable || !strings.Contains(invalid.FirstValidationError, "not bindable") {
		t.Fatalf("invalid listing = %+v", invalid)
	}
}

func TestWorkflowItemDetailAndListCostsIncludeUsage(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		Outputs: map[string]def.WorkflowOutput{
			"summary": {From: "verify.result.summary"},
			"report":  {From: "verify.report", Artifact: true},
		},
		Phases: []def.Phase{
			{ID: "verify", Driver: def.DriverTool, Check: "go-test"},
			{ID: "build", Driver: def.DriverAgent},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item := store.WorkItem{
		ID: "usage-item", ProjectID: defaultTestProjectID, Goal: "measure", WorkflowID: "wf",
		WorkflowScope: "shared", State: string(engine.StateDone), Source: "manual", CreatedAt: 1,
		Disposition: json.RawMessage(`{"action":"merged","policy":"manual","at":2}`),
		Digest:      json.RawMessage(`{"whatHappened":"detail only","whatItNeeds":"nothing"}`),
		Snapshot:    snapshot,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		{CreatedAt: 2, ProjectID: defaultTestProjectID, WorkItemID: item.ID, ThreadID: "phase", Provider: "claude", Model: "m", InputTokens: 8, OutputTokens: 5, CostUSD: 1.25},
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "verify", Attempt: 1, ThreadID: "phase",
		InputEnvelope:  json.RawMessage(`{"input":true}`),
		OutputEnvelope: json.RawMessage(`{"status":"done","outputs":{"result":{"summary":"All checks passed"},"report":"report.md"}}`),
		GateTrace:      json.RawMessage(`{"trace":true}`),
		Intervention:   json.RawMessage(`{"kind":"manual"}`), NarrativePath: "/tmp/narrative",
		Status: "failed", StartedAt: 1, EndedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Usage.TotalTokens != 13 || detail.Usage.CostUSD != 1.25 {
		t.Fatalf("detail usage = %+v", detail.Usage)
	}
	if len(detail.CheckPhaseIDs) != 1 || detail.CheckPhaseIDs[0] != "verify" ||
		len(detail.Phases) != 1 || len(detail.Phases[0].OutputEnvelope) == 0 ||
		detail.Outputs["summary"] != "All checks passed" || len(detail.Outputs) != 1 {
		t.Fatalf("detail view = %+v", detail)
	}
	wire, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"snapshot", "inputEnvelope", "gateTrace", "intervention", "narrativePath"} {
		if strings.Contains(string(wire), `"`+omitted+`"`) {
			t.Fatalf("detail wire includes omitted field %q: %s", omitted, wire)
		}
	}
	if !strings.Contains(string(wire), `"outputEnvelope"`) {
		t.Fatalf("detail wire omitted output envelope: %s", wire)
	}
	costs, err := app.WorkflowListItemCosts(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[item.ID] != 1.25 {
		t.Fatalf("costs = %#v", costs)
	}
	summaries, err := app.WorkflowListItems(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || string(summaries[0].Disposition) != string(item.Disposition) || len(summaries[0].Digest) != 0 {
		t.Fatalf("summaries = %#v", summaries)
	}
}

func TestWorkflowStartRunResolvesBaseBranchAndCancelKeepsTheRecord(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeWorkspaceWorkflow(t, configRoot, "done")
	// Paused: every run is admitted and persisted, but its first phase is held,
	// so no provider process starts and the assertions stay deterministic.
	if _, err := app.settings.Update(map[string]any{"workflowPaused": true}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
	projectRow := testutil.EnsureProject(t, app.store, testutil.InitGitRepo(t))
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, "\nbase_branch: main\n")
	if _, err := app.WorkflowStartRun(projectRow.ID, "missing", "shared", "invalid", json.RawMessage(`{}`), nil, "", false); err == nil {
		t.Fatal("unknown workflow id did not return synchronously")
	}
	if count, err := app.store.CountWorkItemsInStates(string(engine.StateRunning)); err != nil || count != 0 {
		t.Fatalf("unknown workflow persistence count = %d err=%v, want zero", count, err)
	}
	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "cancel me", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != string(engine.StateRunning) || item.BaseBranch != "main" {
		t.Fatalf("started run = %+v, want running on main", item)
	}
	override, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "override base", json.RawMessage(`{}`), nil, "release/v2", false)
	if err != nil {
		t.Fatal(err)
	}
	if override.BaseBranch != "release/v2" {
		t.Fatalf("override start base branch = %q", override.BaseBranch)
	}
	if err := app.WorkflowCancelItem(override.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "invalid base", json.RawMessage(`{}`), nil, "--base", false); err == nil {
		t.Fatal("invalid base branch override succeeded")
	}
	if err := app.WorkflowCancelItem(item.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateCancelled) || stored.WorktreePath != "" {
		t.Fatalf("cancelled item = %+v", stored)
	}
	if err := app.WorkflowCancelItem(item.ID); err == nil {
		t.Fatal("second cancellation unexpectedly succeeded")
	}
}

func writeWorkflowListingFixtures(t *testing.T, configRoot, slug string) {
	t.Helper()
	shared := filepath.Join(configRoot, "workflows")
	projectDir := filepath.Join(configRoot, "projects", slug)
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := `id: valid
name: Valid workflow
default_step_mode: true
inputs:
  approved:
    schema:
      type: boolean
  mode:
    optional: true
    schema:
      type: string
      enum: [fast, thorough]
  notes:
    schema:
      type: string
      multiline: true
  source:
    schema:
      type: string
      format: path
phases:
  - id: prepare
    driver: agent
    provider: codex
    model: gpt-5
    prompt: prompt.md
    access: read-only
    gate:
      routes:
        - to: work
  - id: work
    driver: agent
    provider: codex
    model: gpt-5
    prompt: prompt.md
    access: read-only
    gate:
      routes:
        - human:
            approve: done
            reject:
              loop: prepare
              max: 1
`
	invalid := `id: invalid
name: Invalid workflow
phases:
  - id: check
    driver: tool
    check: missing-check
    access: read-only
    gate:
      routes:
        - to: done
`
	for name, content := range map[string]string{"valid.yaml": valid, "invalid.yaml": invalid, "prompt.md": "Do the work."} {
		if err := os.WriteFile(filepath.Join(shared, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "profile.yaml"), []byte("base_branch: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
