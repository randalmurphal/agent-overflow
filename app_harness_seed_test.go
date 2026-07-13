package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness"
	projectconfig "agent-overflow/internal/project"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/engine"
)

// TestHarnessSeedRefusesTraversalProjectNames: a generated repo's name
// becomes a directory under <dataRoot>/workspaces — a traversal name
// would create (and later reset-wipe) a repository outside the
// harness-owned tree.
func TestHarnessSeedRefusesTraversalProjectNames(t *testing.T) {
	h, _ := newHarnessTestApp(t)
	for _, name := range []string{"../outside", "a/b", `a\b`, ".", ".."} {
		_, err := h.HarnessSeed(HarnessSeedSpec{Projects: []HarnessSeedProject{{
			Name: name,
			Repo: &harness.RepoSpec{},
		}}})
		if err == nil || !strings.Contains(err.Error(), "plain directory name") {
			t.Fatalf("HarnessSeed(name=%q): err = %v, want plain-directory-name refusal", name, err)
		}
	}
	// "../outside" resolves to <dataRoot>/outside — nothing may exist there.
	if _, err := os.Stat(filepath.Join(h.paths.DataRoot, "outside")); !os.IsNotExist(err) {
		t.Fatalf("traversal seed escaped the workspaces root (stat err %v)", err)
	}
}

func TestHarnessSeedWorkflowsUsesProductionPathsAndResetClearsState(t *testing.T) {
	app, _ := setupE2EApp(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "agent-overflow")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bus := transport.NewEventBus(128)
	app.SetEventBus(bus)
	t.Cleanup(bus.Close)

	mock := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"seed-workflow","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		`{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"done","outputs":{"summary":"seeded"},"question":null,"reason":null}}`,
	}})
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath":    mock,
		"workflowQueueActive": false,
		"workflowConcurrency": 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(dataDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })

	h := newHarness(app, harnessPaths{DataRoot: root, DataDir: dataDir})
	definition := `id: seeded-flow
name: Seeded flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: seeded-run.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      summary:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: manual
`
	workflowSeed := func(items []HarnessSeedWorkflowItem) *HarnessSeedWorkflows {
		return &HarnessSeedWorkflows{
			Definitions: []HarnessSeedWorkflowDefinition{{
				Name: "seeded-flow", YAML: definition,
				Prompts: map[string]string{"seeded-run.md": "Complete {{goal}}"},
			}},
			Profile: "reliability:\n  watchdog: 1s\n  backoff: [1ms]\n",
			Items:   items,
		}
	}
	result, err := h.HarnessSeed(HarnessSeedSpec{Projects: []HarnessSeedProject{
		{
			Name: "workflow-seed",
			Repo: &harness.RepoSpec{},
			Workflows: workflowSeed([]HarnessSeedWorkflowItem{
				{Workflow: "seeded-flow", Goal: "wait", Seeds: json.RawMessage(`{"goal":"wait"}`), Target: "queued", Count: 2},
				{Workflow: "seeded-flow", Goal: "finish", Seeds: json.RawMessage(`{"goal":"finish"}`), Target: "done"},
			}),
		},
		{
			Name: "workflow-seed-second",
			Repo: &harness.RepoSpec{},
			Workflows: workflowSeed([]HarnessSeedWorkflowItem{
				{Workflow: "seeded-flow", Goal: "finish second", Seeds: json.RawMessage(`{"goal":"finish second"}`), Target: "done"},
			}),
		},
	}})
	if err != nil {
		t.Fatalf("HarnessSeed: %v", err)
	}
	projectResult := result.Projects[0]
	if len(projectResult.WorkItemIDs) != 3 {
		t.Fatalf("work item ids = %v, want 3", projectResult.WorkItemIDs)
	}
	secondDone, err := app.WorkflowGetItem(result.Projects[1].WorkItemIDs[0])
	if err != nil || secondDone.Item.State != string(engine.StateDone) {
		t.Fatalf("second project driven item = %+v, %v", secondDone, err)
	}
	projectRow, err := app.store.GetProject(projectResult.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	projectConfig := projectconfig.ConfigDir(dataDir, projectRow.Slug)
	for _, path := range []string{
		filepath.Join(projectConfig, "profile.yaml"),
		filepath.Join(projectConfig, "workflows", "seeded-flow.yaml"),
		filepath.Join(projectConfig, "workflows", "seeded-run.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("seeded workflow file %s: %v", path, err)
		}
	}

	done, err := app.WorkflowGetItem(projectResult.WorkItemIDs[2])
	if err != nil {
		t.Fatal(err)
	}
	if done.Item.State != string(engine.StateDone) || len(done.Phases) != 1 ||
		len(done.Phases[0].OutputEnvelope) == 0 {
		t.Fatalf("production-driven done item = %+v", done)
	}
	persistedPhases, err := app.store.ListWorkItemPhases(projectResult.WorkItemIDs[2])
	if err != nil || len(persistedPhases) != 1 || len(persistedPhases[0].GateTrace) == 0 {
		t.Fatalf("production-driven persisted phase trace = %+v, %v", persistedPhases, err)
	}
	for _, itemID := range projectResult.WorkItemIDs[:2] {
		item, err := app.store.GetWorkItem(itemID)
		if err != nil || item.State != string(engine.StateQueued) {
			t.Fatalf("queued item %s = %+v, %v", itemID, item, err)
		}
	}
	settings := app.currentSettings()
	if settings.WorkflowQueueActive || settings.WorkflowConcurrency != 2 {
		t.Fatalf("queue settings after seed = %+v", settings)
	}

	runDir := filepath.Join(dataDir, "workflow-runs", projectResult.WorkItemIDs[2], "artifacts")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "report.txt"), []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	projects, err := app.store.ListProjects()
	if err != nil || len(projects) != 0 {
		t.Fatalf("projects after reset = %+v, %v", projects, err)
	}
	for _, path := range []string{
		filepath.Join(dataDir, "projects"),
		filepath.Join(dataDir, "workflow-runs"),
		filepath.Join(root, "workspaces"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("workflow reset left %s (stat err %v)", path, err)
		}
	}
	settings = app.currentSettings()
	if settings.WorkflowQueueActive || settings.WorkflowConcurrency != 2 {
		t.Fatalf("queue settings after reset = %+v", settings)
	}
}

func TestHarnessSeedWorkflowTargetValidationNamesTarget(t *testing.T) {
	h, _ := newHarnessTestApp(t)
	_, err := h.seedWorkflowItems("project", HarnessSeedWorkflowItem{
		Workflow: "flow", Goal: "goal", Target: "running",
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported target "running"`) {
		t.Fatalf("unsupported target error = %v", err)
	}
}

func TestHarnessSeedWorkflowCountIsBoundedBeforeMutation(t *testing.T) {
	h, app := newHarnessTestApp(t)
	_, err := h.HarnessSeed(HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "too-many",
		Repo: &harness.RepoSpec{},
		Workflows: &HarnessSeedWorkflows{Items: []HarnessSeedWorkflowItem{{
			Workflow: "flow", Goal: "goal", Count: maxHarnessSeedWorkflowItems + 1,
		}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "expanded item count exceeds") {
		t.Fatalf("oversized workflow seed error = %v", err)
	}
	projects, listErr := app.store.ListProjects()
	if listErr != nil || len(projects) != 0 {
		t.Fatalf("oversized seed mutated projects: %+v, %v", projects, listErr)
	}
}

func TestHarnessResetCancelsRunningBeforeDrainingQueued(t *testing.T) {
	app, _ := setupE2EApp(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "agent-overflow")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bus := transport.NewEventBus(128)
	app.SetEventBus(bus)
	t.Cleanup(bus.Close)
	stallMock := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"reset-running","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
	}})
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath":    stallMock,
		"workflowQueueActive": false,
		"workflowConcurrency": 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(dataDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
	h := newHarness(app, harnessPaths{DataRoot: root, DataDir: dataDir})

	definition := `id: reset-flow
name: Reset flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: reset-flow.md
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
	seed, err := h.HarnessSeed(HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "reset-running",
		Repo: &harness.RepoSpec{},
		Workflows: &HarnessSeedWorkflows{Definitions: []HarnessSeedWorkflowDefinition{{
			Name: "reset-flow", YAML: definition,
			Prompts: map[string]string{"reset-flow.md": "Hold this phase."},
		}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	projectID := seed.Projects[0].ProjectID
	if err := app.WorkflowSetQueue(true, 0, 1); err != nil {
		t.Fatal(err)
	}
	running, err := app.WorkflowEnqueueItem(projectID, "reset-flow", "project", "run", json.RawMessage(`{"goal":"run"}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if running.State != string(engine.StateRunning) {
		t.Fatalf("first item state = %s, want running", running.State)
	}
	if err := app.WorkflowSetQueue(false, 0, 1); err != nil {
		t.Fatal(err)
	}
	queued, err := app.WorkflowEnqueueItem(projectID, "reset-flow", "project", "wait", json.RawMessage(`{"goal":"wait"}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != string(engine.StateQueued) {
		t.Fatalf("second item state = %s, want queued", queued.State)
	}

	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset with running + queued workflows: %v", err)
	}
	projects, err := app.store.ListProjects()
	if err != nil || len(projects) != 0 {
		t.Fatalf("projects after reset = %+v, %v", projects, err)
	}
}
