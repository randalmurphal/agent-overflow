package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// Codex reports tokens and no cost anywhere on its wire, so every ledger row a
// Codex phase writes carries `cost_source='none'` and a zero `cost_usd`.
// Everything below is about that one fact: a run whose work ran on Codex must
// not read as free — not to the budget check, not to a status line, and not to
// the overlay.

const codexPhaseModel = "gpt-5.6-sol"

// codexUsageRow is one settled Codex phase turn as triage persists it.
func codexUsageRow(itemID, threadID string, createdAt int64, input, output, cacheRead int) store.UsageLedgerRow {
	return store.UsageLedgerRow{
		CreatedAt: createdAt, ProjectID: defaultTestProjectID, WorkItemID: itemID,
		ThreadID: threadID, TurnID: threadID + "-turn", Provider: "codex", Model: codexPhaseModel,
		InputTokens: input, OutputTokens: output, CacheReadInputTokens: cacheRead,
		CostSource: "none",
	}
}

func createBudgetTestRun(t *testing.T, app *App, itemID string, budget json.RawMessage) store.WorkItem {
	t.Helper()
	item := store.WorkItem{
		ID: itemID, ProjectID: defaultTestProjectID, Goal: "port a slice", WorkflowID: "wf",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "manual",
		Budget: budget, CreatedAt: 1, StartedAt: 1,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

// TestWorkflowTreeSpendPricesCodexTokenOnlyRows — the spend a budget is
// enforced against includes the rows the wire priced at nothing. This is the
// number `checkBudget` compares, so a zero here is a ceiling that never fires.
func TestWorkflowTreeSpendPricesCodexTokenOnlyRows(t *testing.T) {
	app := newTestAppWithStore(t)
	item := createBudgetTestRun(t, app, "codex-run", nil)
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		// 1M input + 1M output + 1M cache read at gpt-5.6-sol rates:
		// $5.00 + $30.00 + $0.50.
		codexUsageRow(item.ID, "phase-1", 2, 1_000_000, 1_000_000, 1_000_000),
	}); err != nil {
		t.Fatal(err)
	}

	spend, err := workflowSpendSource{store: app.store}.TreeSpend(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if spend.Tokens != 3_000_000 {
		t.Fatalf("tree tokens = %d, want 3000000", spend.Tokens)
	}
	if spend.USD < 35.49 || spend.USD > 35.51 {
		t.Fatalf("tree USD = %v, want ~35.50 from the rate table", spend.USD)
	}
	if !spend.Estimated {
		t.Fatal("spend composed entirely from the rate table must report itself as estimated")
	}
	if spend.Unpriced != 0 {
		t.Fatalf("unpriced rows = %d, want 0 for a known model", spend.Unpriced)
	}
}

// TestWorkflowTreeSpendReportsUnpricedRowsRatherThanFailing — a model with no
// rate leaves the dollars incomplete and SAYS so. It used to fail the whole
// spend read, which parked every budgeted run in the project on the first turn
// of a model the rate table had not learned yet.
func TestWorkflowTreeSpendReportsUnpricedRowsRatherThanFailing(t *testing.T) {
	app := newTestAppWithStore(t)
	item := createBudgetTestRun(t, app, "future-model", nil)
	row := codexUsageRow(item.ID, "phase-1", 2, 1_000, 500, 0)
	row.Model = "gpt-7-unreleased"
	if err := app.store.AppendUsage([]store.UsageLedgerRow{row}); err != nil {
		t.Fatal(err)
	}

	spend, err := workflowSpendSource{store: app.store}.TreeSpend(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("an unpriceable model must not fail the spend read: %v", err)
	}
	if spend.Tokens != 1_500 || spend.USD != 0 || spend.Unpriced != 1 {
		t.Fatalf("spend = %+v, want exact tokens, no dollars, one unpriced row", spend)
	}
}

// TestWorkflowUSDBudgetIsCrossedByEstimatedCodexSpend — the end-to-end claim:
// a $-ceilinged run whose spend is entirely Codex is judged against the priced
// total, through the same ResolveBudget the engine enforces with.
func TestWorkflowUSDBudgetIsCrossedByEstimatedCodexSpend(t *testing.T) {
	app := newTestAppWithStore(t)
	item := createBudgetTestRun(t, app, "budgeted", json.RawMessage(`{"usd":10}`))
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		codexUsageRow(item.ID, "phase-1", 2, 1_000_000, 1_000_000, 0),
	}); err != nil {
		t.Fatal(err)
	}

	budget, err := app.workflowRunBudget(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if budget == nil {
		t.Fatal("a run with a declared ceiling must resolve a budget")
	}
	if budget.Kind != engine.BudgetKindUSD || budget.CeilingUSD != 10 {
		t.Fatalf("budget = %+v, want the declared $10 ceiling", budget)
	}
	if budget.SpentUSD < 34.99 || budget.SpentUSD > 35.01 {
		t.Fatalf("spent = %v, want ~35.00 — the wire reported none of it", budget.SpentUSD)
	}
	if !budget.Estimated || !budget.Exhausted || budget.Percent != 350 {
		t.Fatalf("budget = %+v, want estimated, exhausted, 350%%", budget)
	}
}

// TestWorkflowRunBudgetSurvivesAnUnpriceableModel — the engine REFUSES to run
// a run on under a dollar ceiling it cannot judge, but the status read still
// answers, and it names the reason. Failing this read instead would take the
// operator's view of the run away over exactly the fact they need to see.
func TestWorkflowRunBudgetSurvivesAnUnpriceableModel(t *testing.T) {
	app := newTestAppWithStore(t)
	item := createBudgetTestRun(t, app, "unjudgeable", json.RawMessage(`{"usd":25}`))
	known := codexUsageRow(item.ID, "phase-1", 2, 200_000, 0, 0)
	unknown := codexUsageRow(item.ID, "phase-2", 3, 1_000, 500, 0)
	unknown.Model = "gpt-7-unreleased"
	if err := app.store.AppendUsage([]store.UsageLedgerRow{known, unknown}); err != nil {
		t.Fatal(err)
	}

	budget, err := app.workflowRunBudget(context.Background(), item)
	if err != nil {
		t.Fatalf("a status read must survive a model with no rate: %v", err)
	}
	if budget == nil {
		t.Fatal("a declared ceiling must still resolve")
	}
	if budget.UnpricedRows != 1 {
		t.Fatalf("unpriced rows = %d, want 1", budget.UnpricedRows)
	}
	// The dollars that COULD be priced are still reported, and the figure says
	// it is not exactly what the providers reported.
	if budget.SpentUSD != 1 || !budget.Estimated || budget.Exhausted {
		t.Fatalf("budget = %+v, want the priced lower bound, flagged, not exhausted", budget)
	}
}

// TestWorkflowRunBudgetIsAbsentWithoutACeiling — the ordinary case. Nothing to
// render, and nothing is rendered.
func TestWorkflowRunBudgetIsAbsentWithoutACeiling(t *testing.T) {
	app := newTestAppWithStore(t)
	item := createBudgetTestRun(t, app, "unbounded", nil)
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		codexUsageRow(item.ID, "phase-1", 2, 5_000_000, 5_000_000, 0),
	}); err != nil {
		t.Fatal(err)
	}

	budget, err := app.workflowRunBudget(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if budget != nil {
		t.Fatalf("budget = %+v, want none for a run that declared no ceiling", budget)
	}
}

// TestWorkflowRunBudgetNamesTheAncestorWhoseCeilingItSpends — §12 enforces the
// ROOT's ceiling across the tree, so a called run's status has to say whose
// budget it is spending and count the whole tree's spend against it.
func TestWorkflowRunBudgetNamesTheAncestorWhoseCeilingItSpends(t *testing.T) {
	app := newTestAppWithStore(t)
	root := createBudgetTestRun(t, app, "campaign", json.RawMessage(`{"tokens":1000}`))
	child := store.WorkItem{
		ID: "wave-1", ProjectID: defaultTestProjectID, Goal: "wave", WorkflowID: "wf",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "call",
		ParentItemID: root.ID, ParentPhaseID: "next-wave", ParentAttempt: 1, CallDepth: 1,
		CreatedAt: 2, StartedAt: 2,
	}
	if err := app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		codexUsageRow(root.ID, "root-phase", 2, 100, 0, 0),
		codexUsageRow(child.ID, "child-phase", 3, 300, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}

	budget, err := app.workflowRunBudget(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if budget == nil {
		t.Fatal("a called run is bounded by its root's ceiling")
	}
	if budget.RootItemID != root.ID {
		t.Fatalf("rootItemId = %q, want the ancestor holding the ceiling", budget.RootItemID)
	}
	if budget.CeilingTokens != 1000 || budget.SpentTokens != 400 || budget.Percent != 40 {
		t.Fatalf("budget = %+v, want the whole tree's 400 tokens against the root's 1000", budget)
	}
}

// TestWorkflowRunStatusCarriesTheBudget — `run status` / `run inspect` carry
// the line, and a run with no ceiling carries no field at all.
func TestWorkflowRunStatusCarriesTheBudget(t *testing.T) {
	app := newTestAppWithStore(t)
	budgeted := createBudgetTestRun(t, app, "with-budget", json.RawMessage(`{"tokens":1000}`))
	unbudgeted := createBudgetTestRun(t, app, "no-budget", nil)
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		codexUsageRow(budgeted.ID, "phase-1", 2, 200, 50, 0),
		codexUsageRow(unbudgeted.ID, "phase-1", 3, 900, 100, 0),
	}); err != nil {
		t.Fatal(err)
	}
	ctx := transport.WithCallerScope(context.Background(), transport.CallerScope{
		ProjectID: defaultTestProjectID, ThreadID: "operator",
		Grants: []string{string(def.GrantIntrospect)},
	})

	view, err := app.WorkflowAgentRunStatus(ctx, budgeted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Budget == nil {
		t.Fatal("a run with a ceiling must carry its budget on run status")
	}
	if view.Budget.Kind != engine.BudgetKindTokens ||
		view.Budget.CeilingTokens != 1000 || view.Budget.SpentTokens != 250 || view.Budget.Percent != 25 {
		t.Fatalf("status budget = %+v", view.Budget)
	}

	plain, err := app.WorkflowAgentRunStatus(ctx, unbudgeted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Budget != nil {
		t.Fatalf("status budget = %+v, want absent for a run with no ceiling", plain.Budget)
	}
	encoded, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"budget"`) {
		t.Fatalf("run status wire carries a budget key for a run with none: %s", encoded)
	}
}

// TestWorkflowItemDetailAndCostsPriceCodexSpend — the two surfaces the overlay
// reads. Both summed `cost_usd` alone before, which is zero for every Codex row
// ever written: a codex-heavy campaign rendered as costing nothing.
func TestWorkflowItemDetailAndCostsPriceCodexSpend(t *testing.T) {
	app := newTestAppWithStore(t)
	item := createBudgetTestRun(t, app, "priced-detail", nil)
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		// One Claude turn that priced itself, one Codex turn that could not.
		{
			CreatedAt: 2, ProjectID: defaultTestProjectID, WorkItemID: item.ID, ThreadID: "claude-phase",
			Provider: "claude", Model: "claude-opus-5", InputTokens: 10, OutputTokens: 5,
			CostUSD: 1.25, CostSource: "wire",
		},
		codexUsageRow(item.ID, "codex-phase", 3, 1_000_000, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}

	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The wire half stays exactly what the providers said.
	if detail.Usage.CostUSD != 1.25 || detail.Spend.WireCostUSD != 1.25 {
		t.Fatalf("wire cost = %v / %v, want 1.25", detail.Usage.CostUSD, detail.Spend.WireCostUSD)
	}
	if detail.Spend.EstimatedCostUSD != 5 || detail.Spend.CostUSD != 6.25 {
		t.Fatalf("spend = %+v, want $5.00 estimated on top of $1.25 wire", detail.Spend)
	}
	if detail.Spend.UnpricedRows != 0 {
		t.Fatalf("unpriced rows = %d, want 0", detail.Spend.UnpricedRows)
	}

	costs, err := app.WorkflowListItemCosts(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[item.ID] != 6.25 {
		t.Fatalf("overview costs = %#v, want the composed total", costs)
	}
}
