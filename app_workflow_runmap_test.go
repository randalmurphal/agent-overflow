package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectpkg "agent-overflow/internal/project"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// The run map is a pure store read, so these tests build the campaign the
// engine would have written and assert the projection over it. Nothing here
// starts a session; the fixture's poisoned provider binaries stay poisoned.

// campaignSnapshot is the shape the map exists for: a definition whose LAST
// phase calls itself, so the chain root → wave → wave flattens into waves,
// alongside one of every other phase shape the skeleton has to describe.
func campaignSnapshot(t *testing.T) json.RawMessage {
	t.Helper()
	return marshalSnapshot(t, def.Workflow{ID: "campaign", Phases: []def.Phase{
		{ID: "plan", Name: "Plan the wave", Driver: def.DriverAgent, Provider: "claude", Model: "m"},
		{ID: "verify", Driver: def.DriverTool, Check: "lint"},
		{ID: "port", Driver: def.DriverAgent, Shape: def.ShapeFanOut, Provider: "claude", Model: "m"},
		{ID: "audit", Shape: def.ShapeCall, Call: "reviewer"},
		{ID: "next", Shape: def.ShapeCall, Call: "campaign", MaxDepth: 8},
	}})
}

func laneSnapshot(t *testing.T) json.RawMessage {
	t.Helper()
	return marshalSnapshot(t, def.Workflow{ID: "port-lane", Phases: []def.Phase{
		{ID: "work", Driver: def.DriverAgent, Provider: "claude", Model: "m"},
	}})
}

func marshalSnapshot(t *testing.T, workflow def.Workflow) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(engine.Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// seedRunMapCampaign writes root → wave 1 → wave 2, wave 2's fan-out with one
// unit of every reachable status, the child that fan-out unit called, and the
// non-self call child of the same wave. It returns the run ids by name.
func seedRunMapCampaign(t *testing.T, app *App) map[string]string {
	t.Helper()
	campaign := campaignSnapshot(t)
	items := []store.WorkItem{
		{
			ID: "root", ProjectID: defaultTestProjectID, Goal: "port everything",
			WorkflowID: "campaign", WorkflowScope: "project", Snapshot: campaign,
			State: string(engine.StateRunning), Source: "manual", SoftStop: true,
			Budget: json.RawMessage(`{"usd":25}`), CreatedAt: 10, StartedAt: 10,
		},
		{
			ID: "wave-1", ProjectID: defaultTestProjectID, Goal: "wave 1",
			WorkflowID: "campaign", WorkflowScope: "project", Snapshot: campaign,
			State: string(engine.StateDone), Source: "call",
			ParentItemID: "root", ParentPhaseID: "next", ParentAttempt: 1, CallDepth: 1,
			CreatedAt: 20, StartedAt: 20, EndedAt: 30,
		},
		{
			ID: "wave-2", ProjectID: defaultTestProjectID, Goal: "wave 2",
			WorkflowID: "campaign", WorkflowScope: "project", Snapshot: campaign,
			State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonProviderRetriesExhausted),
			Source: "call", ParentItemID: "wave-1", ParentPhaseID: "next", ParentAttempt: 1,
			CallDepth: 2, CreatedAt: 40, StartedAt: 40,
		},
		{
			ID: "lane-child", ProjectID: defaultTestProjectID, Goal: "port one lane",
			WorkflowID: "port-lane", WorkflowScope: "project", Snapshot: laneSnapshot(t),
			State: string(engine.StateRunning), Source: "call",
			ParentItemID: "wave-2", ParentPhaseID: "port", ParentUnitID: "port-0",
			ParentAttempt: 1, CallDepth: 3, CreatedAt: 50, StartedAt: 50,
		},
		{
			ID: "audit-child", ProjectID: defaultTestProjectID, Goal: "review the wave",
			WorkflowID: "reviewer", WorkflowScope: "project",
			Snapshot: marshalSnapshot(t, def.Workflow{ID: "reviewer", Phases: []def.Phase{
				{ID: "review", Driver: def.DriverAgent, Provider: "codex", Model: "m"},
			}}),
			State: string(engine.StateRunning), Source: "call",
			ParentItemID: "wave-2", ParentPhaseID: "audit", ParentAttempt: 1, CallDepth: 3,
			CreatedAt: 60, StartedAt: 60,
		},
	}
	for _, item := range items {
		if err := app.store.CreateWorkItem(item); err != nil {
			t.Fatalf("create run %s: %v", item.ID, err)
		}
	}

	takeover, err := json.Marshal(engine.TakeoverIntervention{Kind: engine.TakeoverInterventionKind, At: 45})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []store.WorkItemPhase{
		{ItemID: "wave-1", PhaseID: "plan", Attempt: 1, Status: "completed", StartedAt: 20, EndedAt: 22},
		{ItemID: "wave-1", PhaseID: "next", Attempt: 1, Status: "completed", StartedAt: 24, EndedAt: 30},
		{ItemID: "wave-2", PhaseID: "plan", Attempt: 1, Status: "completed", StartedAt: 41, EndedAt: 42,
			ThreadID: "thread-plan", Intervention: takeover},
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, Status: "parked", StartedAt: 43,
			ParkCause: "unit port-1 failed"},
	} {
		if err := app.store.CreateWorkItemPhase(phase); err != nil {
			t.Fatalf("create phase %s/%s: %v", phase.ItemID, phase.PhaseID, err)
		}
	}
	if err := app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, UnitID: "port-0", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Provider: "claude", Status: store.WorkItemUnitDone,
			ThreadID: "thread-port-0", UnitAttempt: 1, StartedAt: 44, EndedAt: 46},
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, UnitID: "port-1", UnitIndex: 1,
			Kind: store.WorkItemUnitKindUnit, Provider: "codex", Status: store.WorkItemUnitRunning,
			UnitAttempt: 2, StartedAt: 45},
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, UnitID: "port-2", UnitIndex: 2,
			Kind: store.WorkItemUnitKindUnit, Provider: "codex", Status: store.WorkItemUnitPending, UnitAttempt: 1},
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, UnitID: "port-join", UnitIndex: 3,
			Kind: store.WorkItemUnitKindJoin, Provider: "claude", Status: store.WorkItemUnitPending, UnitAttempt: 1},
	}); err != nil {
		t.Fatalf("create units: %v", err)
	}
	if err := app.store.SetWorkItemAutoResumeAt("wave-2", 9_999); err != nil {
		t.Fatalf("arm auto resume: %v", err)
	}
	// Spend lands on a CHILD, which is the whole reason the root's number is the
	// tree's: a campaign's dollars are almost never on the run that started it.
	if err := app.store.AppendUsage([]store.UsageLedgerRow{{
		CreatedAt: 47, ThreadID: "thread-port-0", ProjectID: defaultTestProjectID,
		WorkItemID: "lane-child", TurnID: "turn-1", Provider: "claude", Model: "claude-opus-4-7",
		InputTokens: 100, OutputTokens: 200, CostUSD: 1.5, CostSource: "wire",
	}}); err != nil {
		t.Fatalf("append usage: %v", err)
	}
	return map[string]string{"root": "root", "wave-1": "wave-1", "wave-2": "wave-2",
		"lane-child": "lane-child", "audit-child": "audit-child"}
}

func runMapByID(t *testing.T, view WorkflowRunMapView) map[string]WorkflowRunMapRun {
	t.Helper()
	runs := make(map[string]WorkflowRunMapRun, len(view.Runs))
	for _, run := range view.Runs {
		runs[run.ItemID] = run
	}
	return runs
}

func TestWorkflowGetRunMapResolvesTheRootFromAnyRunInTheTree(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)

	for _, from := range []string{"root", "wave-2", "lane-child", "audit-child"} {
		view, err := app.WorkflowGetRunMap(t.Context(), from)
		if err != nil {
			t.Fatalf("run map from %s: %v", from, err)
		}
		if view.RootItemID != "root" {
			t.Fatalf("run map from %s resolved root %q", from, view.RootItemID)
		}
		if len(view.Runs) != 5 || view.Runs[0].ItemID != "root" {
			t.Fatalf("run map from %s = %d runs, first %q", from, len(view.Runs), view.Runs[0].ItemID)
		}
		// Parent before child: the consumer builds the tree in one pass.
		seen := map[string]bool{}
		for _, run := range view.Runs {
			if run.ParentItemID != "" && !seen[run.ParentItemID] {
				t.Fatalf("run %s arrived before its parent %s", run.ItemID, run.ParentItemID)
			}
			seen[run.ItemID] = true
		}
	}

	if _, err := app.WorkflowGetRunMap(t.Context(), "  "); err == nil {
		t.Fatal("blank item id was accepted")
	}
}

// A run that is simply GONE is the commonest thing this method is asked for —
// a stale nav entry, a discarded campaign — so it is an answer with a code the
// client can stop retrying on, not an error string.
func TestWorkflowGetRunMapRefusesAnUnknownRunPermanently(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)

	view, err := app.WorkflowGetRunMap(t.Context(), "no-such-run")
	if err != nil {
		t.Fatalf("an absent run must be an answer, not an error: %v", err)
	}
	if view.Refusal == nil || view.Refusal.Code != WorkflowRunMapRefusalNotFound {
		t.Fatalf("absent run = %#v", view.Refusal)
	}
	if len(view.Runs) != 0 || view.RootItemID != "" {
		t.Fatalf("a refusal carried a tree: %#v", view)
	}
	if !strings.Contains(view.Refusal.Message, "no-such-run") {
		t.Fatalf("refusal message does not name the run: %q", view.Refusal.Message)
	}
}

// The classification itself, over the store's typed refusals. Seeding a
// 4097-run campaign to reach the member cap would test SQLite rather than this
// contract; what matters is that each typed refusal maps to its code, and that
// everything else stays an error so the client keeps retrying it.
func TestWorkflowRunMapRefusalClassification(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		code string
	}{
		{"absent", fmt.Errorf("workflow run map: %w", sql.ErrNoRows), WorkflowRunMapRefusalNotFound},
		{"too large", fmt.Errorf("wrapped: %w", store.ErrWorkItemTreeTooLarge), WorkflowRunMapRefusalTooLarge},
		{"too deep", fmt.Errorf("wrapped: %w", store.ErrWorkItemTreeTooDeep), WorkflowRunMapRefusalCorruptLinkage},
		{"cyclic", fmt.Errorf("wrapped: %w", store.ErrWorkItemTreeCyclicLinkage), WorkflowRunMapRefusalCorruptLinkage},
	} {
		view, err := workflowRunMapRefusalFor("run", testCase.err)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if view.Refusal == nil || view.Refusal.Code != testCase.code {
			t.Fatalf("%s = %#v, want code %q", testCase.name, view.Refusal, testCase.code)
		}
		if view.Refusal.Message == "" {
			t.Fatalf("%s carried no sentence to render", testCase.name)
		}
	}
	// A failure retrying COULD fix keeps the retry: it stays an error and never
	// becomes a permanent-looking refusal.
	transient := errors.New("database is locked")
	if _, err := workflowRunMapRefusalFor("run", transient); !errors.Is(err, transient) {
		t.Fatalf("a transient failure was classified as permanent: %v", err)
	}
}

func TestWorkflowGetRunMapProjectsSkeletonRecordsAndMoney(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)

	view, err := app.WorkflowGetRunMap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	runs := runMapByID(t, view)

	root := runs["root"]
	if len(root.Skeleton) != 5 {
		t.Fatalf("root skeleton = %#v", root.Skeleton)
	}
	for index, want := range []WorkflowRunMapSkeletonPhase{
		{ID: "plan", Name: "Plan the wave", Shape: string(def.ShapeSingle)},
		{ID: "verify", Shape: string(def.ShapeSingle), IsCheck: true},
		{ID: "port", Shape: string(def.ShapeFanOut)},
		{ID: "audit", Shape: string(def.ShapeCall), CallTarget: "reviewer"},
		{ID: "next", Shape: string(def.ShapeCall), CallTarget: "campaign", MaxDepth: 8},
	} {
		if root.Skeleton[index] != want {
			t.Fatalf("skeleton[%d] = %#v, want %#v", index, root.Skeleton[index], want)
		}
	}
	if root.SkeletonMissing || !root.TailSelfCall || !root.SoftStop {
		t.Fatalf("root = %#v", root)
	}
	// The chain flattens; the runs a chain member CALLED do not.
	for _, id := range []string{"wave-1", "wave-2"} {
		if !runs[id].TailSelfCall {
			t.Fatalf("chain run %s is not tail-self-calling", id)
		}
	}
	for _, id := range []string{"lane-child", "audit-child"} {
		if runs[id].TailSelfCall {
			t.Fatalf("non-self callee %s reported a tail self call", id)
		}
	}
	if lane := runs["lane-child"]; lane.ParentUnitID != "port-0" || lane.ParentPhaseID != "port" || lane.CallDepth != 3 {
		t.Fatalf("unit-bound child linkage = %#v", lane)
	}
	if audit := runs["audit-child"]; audit.ParentUnitID != "" || audit.ParentPhaseID != "audit" {
		t.Fatalf("phase-call child linkage = %#v", audit)
	}

	wave := runs["wave-2"]
	if wave.State != string(engine.StateNeedsHuman) || wave.Reason != string(engine.ReasonProviderRetriesExhausted) {
		t.Fatalf("wave 2 state = %s/%s", wave.State, wave.Reason)
	}
	if wave.AutoResumeAt != 9_999 {
		t.Fatalf("wave 2 auto resume = %d", wave.AutoResumeAt)
	}
	if len(wave.Phases) != 2 || wave.Phases[0].PhaseID != "plan" ||
		wave.Phases[0].InterventionKind != engine.TakeoverInterventionKind ||
		wave.Phases[0].ThreadID != "thread-plan" {
		t.Fatalf("wave 2 phases = %#v", wave.Phases)
	}
	if wave.Phases[1].Status != "parked" || wave.Phases[1].Cause != "unit port-1 failed" {
		t.Fatalf("parked attempt = %#v", wave.Phases[1])
	}
	if len(wave.Units) != 4 {
		t.Fatalf("wave 2 units = %#v", wave.Units)
	}
	for index, want := range []struct {
		id     string
		status string
	}{
		{"port-0", store.WorkItemUnitDone},
		{"port-1", store.WorkItemUnitRunning},
		{"port-2", store.WorkItemUnitPending},
		{"port-join", store.WorkItemUnitPending},
	} {
		unit := wave.Units[index]
		if unit.UnitID != want.id || unit.Status != want.status {
			t.Fatalf("unit[%d] = %#v, want %s/%s", index, unit, want.id, want.status)
		}
	}
	if wave.Units[3].Kind != store.WorkItemUnitKindJoin || wave.Units[1].UnitAttempt != 2 {
		t.Fatalf("join/retry projection = %#v", wave.Units)
	}
	if runs["wave-1"].Units == nil || runs["wave-1"].Phases == nil {
		t.Fatal("a run with no units must carry empty lists, never null")
	}

	// Money is the tree's and the root's: the ledger row sits on a grandchild.
	if root.Spend == nil || root.Spend.CostUSD != 1.5 || root.Spend.WireCostUSD != 1.5 {
		t.Fatalf("root spend = %#v", root.Spend)
	}
	if root.Spend.EstimatedCostUSD != 0 || root.Spend.UnpricedRows != 0 {
		t.Fatalf("a fully wire-priced tree reported an estimate: %#v", root.Spend)
	}
	if root.Budget == nil || root.Budget.Kind != engine.BudgetKindUSD ||
		root.Budget.CeilingUSD != 25 || root.Budget.SpentUSD != 1.5 || root.Budget.Percent != 6 {
		t.Fatalf("root budget = %#v", root.Budget)
	}
	if root.Budget.Exhausted || root.Budget.RootItemID != "" {
		t.Fatalf("the root's own ceiling was reported as an ancestor's: %#v", root.Budget)
	}
	for _, id := range []string{"wave-1", "wave-2", "lane-child", "audit-child"} {
		if runs[id].Spend != nil || runs[id].Budget != nil {
			t.Fatalf("called run %s carries money: %#v", id, runs[id])
		}
	}
}

// The ceiling a run is under is not always the one it declared: a project
// profile's `reliability.per_item_budget` applies to every run that declared
// none, and the engine enforces it. A map that read the run's own column alone
// drew a profile-defaulted campaign as unbounded right up to the park.
func TestWorkflowGetRunMapBudgetResolvesTheProjectProfileDefault(t *testing.T) {
	app := newTestAppWithStore(t)
	// A run that declared NO budget, which is what makes the project profile the
	// only possible source of a ceiling.
	if err := app.store.CreateWorkItem(store.WorkItem{
		ID: "solo", ProjectID: defaultTestProjectID, Goal: "port everything",
		WorkflowID: "campaign", WorkflowScope: "project", Snapshot: campaignSnapshot(t),
		State: string(engine.StateRunning), Source: "manual", CreatedAt: 10, StartedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.AppendUsage([]store.UsageLedgerRow{{
		CreatedAt: 47, ThreadID: "thread-plan", ProjectID: defaultTestProjectID,
		WorkItemID: "solo", TurnID: "turn-1", Provider: "claude", Model: "claude-opus-4-7",
		InputTokens: 100, OutputTokens: 200, CostUSD: 1.5, CostSource: "wire",
	}}); err != nil {
		t.Fatal(err)
	}

	view, err := app.WorkflowGetRunMap(t.Context(), "solo")
	if err != nil {
		t.Fatal(err)
	}
	if budget := runMapByID(t, view)["solo"].Budget; budget != nil {
		t.Fatalf("no run budget and no profile budget must be genuinely unbounded, got %#v", budget)
	}

	app.configDir = t.TempDir()
	project, err := app.store.GetProject(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	configDir := projectpkg.ConfigDir(app.configDir, project.Slug)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "profile.yaml"),
		[]byte("reliability:\n  per_item_budget:\n    usd: 4\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	view, err = app.WorkflowGetRunMap(t.Context(), "solo")
	if err != nil {
		t.Fatal(err)
	}
	budget := runMapByID(t, view)["solo"].Budget
	if budget == nil || budget.Kind != engine.BudgetKindUSD || budget.CeilingUSD != 4 {
		t.Fatalf("profile-defaulted ceiling = %#v", budget)
	}
	// $1.50 of a $4 ceiling, through the same numbers the enforcement compares.
	if budget.SpentUSD != 1.5 || budget.Percent != 38 || budget.Exhausted {
		t.Fatalf("profile-defaulted budget stand = %#v", budget)
	}
}

// A dollar total is a LOWER BOUND whenever the rate table could not price a
// row, and every reader of it has to say so. The map carries the halves apart
// for exactly that, and the ceiling it shows carries the same caveat the
// enforcement refuses to judge on.
func TestWorkflowGetRunMapSpendSaysWhatItCouldNotPrice(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)
	if err := app.store.AppendUsage([]store.UsageLedgerRow{{
		CreatedAt: 48, ThreadID: "thread-port-1", ProjectID: defaultTestProjectID,
		WorkItemID: "wave-2", TurnID: "turn-2", Provider: "codex", Model: "not-a-model-anybody-prices",
		InputTokens: 1_000, OutputTokens: 2_000, CostSource: "none",
	}}); err != nil {
		t.Fatal(err)
	}

	view, err := app.WorkflowGetRunMap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	root := runMapByID(t, view)["root"]
	if root.Spend == nil || root.Spend.UnpricedRows != 1 {
		t.Fatalf("unpriced rows were dropped from the map's spend: %#v", root.Spend)
	}
	if root.Spend.CostUSD != 1.5 || root.Spend.WireCostUSD != 1.5 {
		t.Fatalf("an unpriceable row moved the total: %#v", root.Spend)
	}
	if root.Budget == nil || root.Budget.UnpricedRows != 1 || !root.Budget.Estimated {
		t.Fatalf("the ceiling did not carry the caveat its spend has: %#v", root.Budget)
	}
}

func TestWorkflowGetRunMapDegradesToRecordsOnlyWithoutASnapshot(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)

	// A run that never froze a definition (it failed before its first entry) and
	// one whose column does not decode as a snapshot are the same records-only
	// answer. The column is CHECK-constrained to valid JSON, so the reachable
	// corruption is valid JSON of the wrong shape.
	if err := app.store.UpdateWorkItemRunStart("wave-1", nil, "", "", "", 20); err != nil {
		t.Fatal(err)
	}
	if err := app.store.UpdateWorkItemRunStart("wave-2", json.RawMessage(`["not a snapshot"]`), "", "", "", 40); err != nil {
		t.Fatal(err)
	}

	view, err := app.WorkflowGetRunMap(t.Context(), "root")
	if err != nil {
		t.Fatalf("an unreadable snapshot took the whole map away: %v", err)
	}
	runs := runMapByID(t, view)
	for _, id := range []string{"wave-1", "wave-2"} {
		run := runs[id]
		if !run.SkeletonMissing || len(run.Skeleton) != 0 || run.TailSelfCall {
			t.Fatalf("records-only run %s = %#v", id, run)
		}
	}
	// The two are NOT the same answer, which is the whole point of the second
	// field: a run that never froze a definition is ordinary history, and one
	// whose column will not decode is corruption somebody has to see.
	if runs["wave-1"].SkeletonError != "" {
		t.Fatalf("an absent snapshot was reported as corruption: %q", runs["wave-1"].SkeletonError)
	}
	if runs["wave-2"].SkeletonError == "" {
		t.Fatal("an undecodable snapshot rendered as a run that simply never froze one")
	}
	// The records themselves are untouched — that is what records-only means.
	if len(runs["wave-2"].Phases) != 2 || len(runs["wave-2"].Units) != 4 {
		t.Fatalf("records-only run lost its records: %#v", runs["wave-2"])
	}
	if runs["root"].SkeletonMissing || runs["root"].SkeletonError != "" || !runs["root"].TailSelfCall {
		t.Fatalf("a sibling's bad snapshot changed the root: %#v", runs["root"])
	}
}
