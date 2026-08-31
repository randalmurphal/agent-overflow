package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness"
	"agent-overflow/internal/harnessrpc"
	projectconfig "agent-overflow/internal/project"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/engine"
)

// A soak scenario leaves background launches running by design. Reset first
// stops every provider session, which makes those launches orphans, then must
// settle them before production project deletion applies its live-work guard.
// Otherwise the harness cannot reset the exact long-running workload it owns.
func TestHarnessResetSettlesBackgroundTasksAfterStoppingSessions(t *testing.T) {
	app, _ := setupE2EApp(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "agent-overflow")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(app, HarnessPaths{DataRoot: root, DataDir: dataDir})
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	seeded, err := harnessrpc.Seed(h, harnessrpc.HarnessSeedSpec{Projects: []harnessrpc.HarnessSeedProject{{
		Name: "reset-background-task",
		Repo: &harness.RepoSpec{},
		Threads: []harnessrpc.HarnessSeedThread{{
			Title: "running background task",
			Turns: []harnessrpc.HarnessSeedTurn{{
				UserText: "start it",
				Items: []harnessrpc.HarnessSeedItem{{
					Kind: "assistant_text", Summary: "Starting the task.",
				}},
			}},
		}},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	threadID := seeded.Projects[0].ThreadIDs[0]
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID: "tool-reset-background", ThreadID: threadID,
		TurnIndex: 0, ItemIndex: 2, Kind: "tool_call", Role: "assistant",
		Status: "running", IsBackground: true, ToolName: "Agent",
		Summary: "background review", Meta: `{"task_id":"task-reset-background"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert running background task: %v", err)
	}
	if count, err := app.countRunningBackgroundTasks(threadID); err != nil || count != 1 {
		t.Fatalf("running background task precondition = %d, %v", count, err)
	}

	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	projects, err := app.store.ListProjects()
	if err != nil || len(projects) != 0 {
		t.Fatalf("projects after reset = %+v, %v", projects, err)
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
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": mock}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(dataDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })

	h := NewHarness(app, HarnessPaths{DataRoot: root, DataDir: dataDir})
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
	workflowSeed := func(items []harnessrpc.HarnessSeedWorkflowItem) *harnessrpc.HarnessSeedWorkflows {
		return &harnessrpc.HarnessSeedWorkflows{
			Definitions: []harnessrpc.HarnessSeedWorkflowDefinition{{
				Name: "seeded-flow", YAML: definition,
				Prompts: map[string]string{"seeded-run.md": "Complete {{goal}}"},
			}},
			Profile: "reliability:\n  watchdog: 1s\n  backoff: [1ms]\n",
			Items:   items,
		}
	}
	result, err := harnessrpc.Seed(h, harnessrpc.HarnessSeedSpec{Projects: []harnessrpc.HarnessSeedProject{
		{
			Name: "workflow-seed",
			Repo: &harness.RepoSpec{},
			Workflows: workflowSeed([]harnessrpc.HarnessSeedWorkflowItem{
				{Workflow: "seeded-flow", Goal: "finish", Seeds: json.RawMessage(`{"goal":"finish"}`), Target: "done"},
			}),
		},
		{
			Name: "workflow-seed-second",
			Repo: &harness.RepoSpec{},
			Workflows: workflowSeed([]harnessrpc.HarnessSeedWorkflowItem{
				{Workflow: "seeded-flow", Goal: "finish second", Seeds: json.RawMessage(`{"goal":"finish second"}`), Target: "done"},
			}),
		},
	}})
	if err != nil {
		t.Fatalf("HarnessSeed: %v", err)
	}
	projectResult := result.Projects[0]
	if len(projectResult.WorkItemIDs) != 1 {
		t.Fatalf("work item ids = %v, want 1", projectResult.WorkItemIDs)
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

	doneItemID := projectResult.WorkItemIDs[0]
	done, err := app.WorkflowGetItem(doneItemID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Item.State != string(engine.StateDone) || len(done.Phases) != 1 ||
		len(done.Phases[0].OutputEnvelope) == 0 {
		t.Fatalf("production-driven done item = %+v", done)
	}
	persistedPhases, err := app.store.ListWorkItemPhases(doneItemID)
	if err != nil || len(persistedPhases) != 1 || len(persistedPhases[0].GateTrace) == 0 {
		t.Fatalf("production-driven persisted phase trace = %+v, %v", persistedPhases, err)
	}

	// A fixture that must not execute is seeded under the global pause: the run
	// is admitted and persisted running, but its first phase is held, so the
	// state is deterministic without any queue.
	if err := app.WorkflowSetGlobalPause(true); err != nil {
		t.Fatal(err)
	}
	held, err := harnessrpc.Seed(h, harnessrpc.HarnessSeedSpec{Projects: []harnessrpc.HarnessSeedProject{{
		Name: "workflow-seed-held",
		Repo: &harness.RepoSpec{},
		Workflows: workflowSeed([]harnessrpc.HarnessSeedWorkflowItem{
			{Workflow: "seeded-flow", Goal: "wait", Seeds: json.RawMessage(`{"goal":"wait"}`), Count: 2},
		}),
	}}})
	if err != nil {
		t.Fatalf("HarnessSeed under pause: %v", err)
	}
	if len(held.Projects[0].WorkItemIDs) != 2 {
		t.Fatalf("held work item ids = %v, want 2", held.Projects[0].WorkItemIDs)
	}
	for _, itemID := range held.Projects[0].WorkItemIDs {
		item, err := app.store.GetWorkItem(itemID)
		if err != nil || item.State != string(engine.StateRunning) || item.Reason != "" {
			t.Fatalf("held item %s = %+v, %v", itemID, item, err)
		}
		phases, err := app.store.ListWorkItemPhases(itemID)
		if err != nil || len(phases) != 1 || phases[0].Status != "running" || phases[0].ThreadID != "" {
			t.Fatalf("held item %s phases = %+v, %v", itemID, phases, err)
		}
	}
	if state, err := app.WorkflowGetEngineState(); err != nil || !state.Paused {
		t.Fatalf("engine state = %+v, %v, want paused", state, err)
	}

	runDir := filepath.Join(dataDir, "workflow-runs", doneItemID, "artifacts")
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
	// Reset is a blank slate: the pause a held-fixture seed left behind must
	// not survive into the next spec, in the engine or in settings.
	if app.currentSettings().WorkflowPaused {
		t.Fatal("reset left the persisted pause flag set")
	}
	if state, err := app.WorkflowGetEngineState(); err != nil || state.Paused {
		t.Fatalf("engine state after reset = %+v, %v, want unpaused", state, err)
	}
}

func TestHarnessResetCancelsRunningAndHeldRuns(t *testing.T) {
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
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": stallMock}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(dataDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	h := NewHarness(app, HarnessPaths{DataRoot: root, DataDir: dataDir})

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
	seed, err := harnessrpc.Seed(h, harnessrpc.HarnessSeedSpec{Projects: []harnessrpc.HarnessSeedProject{{
		Name: "reset-running",
		Repo: &harness.RepoSpec{},
		Workflows: &harnessrpc.HarnessSeedWorkflows{Definitions: []harnessrpc.HarnessSeedWorkflowDefinition{{
			Name: "reset-flow", YAML: definition,
			Prompts: map[string]string{"reset-flow.md": "Hold this phase."},
		}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	projectID := seed.Projects[0].ProjectID
	running, err := app.WorkflowStartRun(projectID, "reset-flow", "project", "run", json.RawMessage(`{"goal":"run"}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if running.State != string(engine.StateRunning) {
		t.Fatalf("first item state = %s, want running", running.State)
	}
	if err := app.WorkflowSetGlobalPause(true); err != nil {
		t.Fatal(err)
	}
	held, err := app.WorkflowStartRun(projectID, "reset-flow", "project", "wait", json.RawMessage(`{"goal":"wait"}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if held.State != string(engine.StateRunning) {
		t.Fatalf("held item state = %s, want running", held.State)
	}

	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset with live + held workflows: %v", err)
	}
	projects, err := app.store.ListProjects()
	if err != nil || len(projects) != 0 {
		t.Fatalf("projects after reset = %+v, %v", projects, err)
	}
	// Runs carry no foreign key to their project, so reset has to delete them
	// itself. Leaving them behind means a finished run outlives the test that
	// made it and shows up in the next test's workflows overlay.
	items, err := app.store.ListWorkItemSummaries(store.WorkItemListFilter{})
	if err != nil || len(items) != 0 {
		t.Fatalf("work items after reset = %+v, %v", items, err)
	}
	for _, itemID := range []string{running.ID, held.ID} {
		phases, err := app.store.ListWorkItemPhases(itemID)
		if err != nil {
			t.Fatalf("list phases for %s: %v", itemID, err)
		}
		if len(phases) != 0 {
			t.Fatalf("phases for %s after reset = %+v", itemID, phases)
		}
	}
}
