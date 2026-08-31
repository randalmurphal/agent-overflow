package app

import (
	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

// effortTestModel is a Claude model whose advertised tiers are a strict subset
// of the vocabulary: low → xhigh plus max, defaulting to xhigh. It is what makes
// "authored `ultra` is coerced" a real assertion rather than a tautology.
const effortTestModel = "claude-opus-4-7"

// TestWorkflowEffortTiersMatchTheProviderReasoningEfforts is the drift guard for
// the one vocabulary that is deliberately declared twice.
//
// `internal/workflow/def` is a pure package — it validates and publishes a
// workflow definition with no provider in reach — so it cannot import
// `internal/provider` to reuse its tier constants. The cost of that purity is
// two lists, and this is what keeps them one vocabulary: a tier added to the
// provider enum but not to def would validate as "unknown effort", and a tier
// added to def but not to the provider would be accepted by validation and then
// coerced away at thread creation, both silently.
func TestWorkflowEffortTiersMatchTheProviderReasoningEfforts(t *testing.T) {
	authored := def.EffortTiers()
	runtime := provider.AllReasoningEfforts

	inDef := make(map[string]bool, len(authored))
	for _, tier := range authored {
		inDef[string(tier)] = true
	}
	inProvider := make(map[string]bool, len(runtime))
	for _, effort := range runtime {
		inProvider[string(effort)] = true
	}
	for name := range inDef {
		if !inProvider[name] {
			t.Errorf("def declares effort tier %q but provider.AllReasoningEfforts does not; a workflow could author a tier no session can run", name)
		}
	}
	for name := range inProvider {
		if !inDef[name] {
			t.Errorf("provider.AllReasoningEfforts declares %q but def does not; workflow validation would refuse a tier the app supports", name)
		}
	}

	// Order is part of the contract too: it is what the published schema's enum
	// and every diagnostic list, so a reordered provider enum should surface here
	// rather than as an inconsistently ordered picker.
	if len(authored) == len(runtime) {
		for index := range authored {
			if string(authored[index]) != string(runtime[index]) {
				t.Errorf("tier %d: def has %q, provider has %q; the two lists must stay in the same order", index, authored[index], runtime[index])
			}
		}
	}
}

// TestWorkflowThreadIgnoresRememberedChatProfile is the no-bleed proof. A
// workflow lane is a deterministic piece of a definition, so its model settings
// must come from the catalog's defaults for (provider, model) — never from
// `chat_model_profiles`, which records how the user happened to configure their
// last interactive chat on the same model.
func TestWorkflowThreadIgnoresRememberedChatProfile(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)

	catalogDefault := chatmodel.FallbackProfile(string(provider.Claude), effortTestModel)
	remembered := store.ChatModelProfile{
		Provider:        string(provider.Claude),
		Model:           effortTestModel,
		ReasoningEffort: string(provider.EffortLow),
		FastMode:        true,
		ContextWindow:   catalogDefault.ContextWindow,
		RuntimeMode:     string(provider.DefaultRuntimeMode),
	}
	if remembered.ReasoningEffort == catalogDefault.ReasoningEffort {
		t.Fatalf("fixture is inert: remembered effort %q equals the catalog default", remembered.ReasoningEffort)
	}
	if err := app.store.UpsertChatModelProfile(remembered); err != nil {
		t.Fatal(err)
	}
	// Guard the guard: the chat path really would pick the remembered profile up.
	if seeded := app.seedChatModelProfile(string(provider.Claude), effortTestModel); seeded.ReasoningEffort != remembered.ReasoningEffort {
		t.Fatalf("chat seed effort = %q, want the remembered %q — fixture no longer proves anything", seeded.ReasoningEffort, remembered.ReasoningEffort)
	}

	thread := createWorkflowThreadForTest(t, app, projectRow, repo, "")
	if thread.ReasoningEffort != catalogDefault.ReasoningEffort {
		t.Errorf("workflow thread effort = %q, want the catalog default %q (the remembered chat profile bled through)",
			thread.ReasoningEffort, catalogDefault.ReasoningEffort)
	}
	if thread.FastMode {
		t.Error("workflow thread inherited fast mode from the remembered chat profile")
	}
}

// TestWorkflowThreadTakesAuthoredEffort covers the two ends of the authored
// field: a tier the model advertises lands verbatim, and one it does not is
// coerced onto the model's own default rather than stored illegally —
// `threads.reasoning_effort` is CHECKed, so an uncoerced value is a write error,
// not a cosmetic one.
func TestWorkflowThreadTakesAuthoredEffort(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)

	catalogDefault := chatmodel.FallbackProfile(string(provider.Claude), effortTestModel).ReasoningEffort
	cases := []struct {
		name     string
		authored string
		want     string
	}{
		{"supported tier is honoured", string(provider.EffortMedium), string(provider.EffortMedium)},
		// `ultra` is a Codex-only tier; Claude models advertise none of it.
		{"unsupported tier is coerced", string(provider.EffortUltra), catalogDefault},
		{"unset falls back to the catalog default", "", catalogDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.authored != "" && !def.KnownEffortTier(tc.authored) {
				t.Fatalf("fixture authors %q, which workflow validation would refuse", tc.authored)
			}
			thread := createWorkflowThreadForTest(t, app, projectRow, repo, tc.authored)
			if thread.ReasoningEffort != tc.want {
				t.Fatalf("thread effort = %q, want %q", thread.ReasoningEffort, tc.want)
			}
			// Whatever landed has to be a tier the model can actually be started
			// with, which is the property the CHECK constraint encodes.
			if !app.reasoningEffortSupportedForModel(string(provider.Claude), thread.Model, thread.ReasoningEffort) {
				t.Fatalf("thread effort %q is not supported by %s", thread.ReasoningEffort, thread.Model)
			}
		})
	}
}

// createWorkflowThreadForTest drives the real creation path with one authored
// effort, reading the row back the way every later session start does.
func createWorkflowThreadForTest(t *testing.T, app *App, projectRow store.Project, workspace, effort string) store.Thread {
	t.Helper()
	thread, err := app.createWorkflowThread(workflowhost.ThreadSpec{
		ItemID: "item-effort", Label: `phase "survey"`,
		Title:        workflowhost.ThreadTitle("Survey", "survey"),
		ProviderName: string(provider.Claude), Model: effortTestModel,
		Effort:    effort,
		Access:    def.AccessReadOnly,
		Workspace: workflowhost.PreparedWorkspace{Path: workspace, Project: projectRow},
	})
	if err != nil {
		t.Fatal(err)
	}
	return thread
}

// The runner-side quota rules live in `internal/workflowhost`. What stays here
// is the one App-level fact they depend on: the interrupt the workflow cleanup
// issues is bookkeeping, not a person pressing stop, so it must not leave a
// "Stopped by user" row on the thread.
func TestUsageLimitCleanupDoesNotRecordAUserInterrupt(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.configureTriageQueueCallbacks()
	thread := testThread("usage-cleanup-interrupt")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: thread.ID, Content: "usage is exhausted",
		Meta: json.RawMessage(`{"fatal":true,"expect_turn_complete":true,"codexErrorInfo":"usageLimitExceeded"}`),
		Failure: &provider.FailureMeta{
			Class: provider.FailureFatal, Boundary: provider.FailureBoundaryTurn,
			Reason: provider.FailureReasonUsageLimit, Code: "usageLimitExceeded",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if open := app.triage.OpenTurnIndex(thread.ID); open != -1 {
		t.Fatalf("usage refusal left turn %d open before workflow cleanup", open)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Token: "usage-cleanup-token", Codex: sess,
	})
	if err := app.InterruptTurn(thread.ID); err != nil {
		t.Fatal(err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == "error" && item.Summary == "Stopped by user" {
			t.Fatalf("usage-limit cleanup created user-interrupt row %+v", item)
		}
	}
}
