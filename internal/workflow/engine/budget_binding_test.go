package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

// budgetBinding pulls the reserved `budget` read out of the first started
// attempt's variables. Absent means the run is under no ceiling.
func budgetBinding(t *testing.T, h *testHarness) (map[string]any, bool) {
	t.Helper()
	starts := h.runner.started()
	if len(starts) == 0 {
		t.Fatal("nothing started, so no attempt rendered a variable context")
	}
	value, present := starts[0].Vars[def.BudgetVariable]
	if !present {
		return nil, false
	}
	binding, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("budget binding = %T, want an object", value)
	}
	return binding, true
}

// TestBudgetBindingRendersTheEnforcedNumbers — an element reads the ceiling and
// the tree's spend against it, and the spend is the composed number the check
// would park on, estimate included.
func TestBudgetBindingRendersTheEnforcedNumbers(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	h.spend.spends["priced"] = Spend{Tokens: 4_000, USD: 6, Estimated: true}
	item := testItem("priced", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"usd":25}`)

	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	binding, present := budgetBinding(t, h)
	if !present {
		t.Fatal("a run with a ceiling must bind the reserved budget read")
	}
	if binding["kind"] != BudgetKindUSD {
		t.Fatalf("kind = %v, want usd", binding["kind"])
	}
	if binding["ceiling"] != float64(25) || binding["spent"] != float64(6) || binding["remaining"] != float64(19) {
		t.Fatalf("budget binding = %+v", binding)
	}
	// The one caveat a prompt has to be able to state: part of that $6 was
	// priced from a rate table because the provider reported tokens only.
	if binding["estimated"] != true {
		t.Fatalf("estimated = %v, want true when part of the spend was rate-table priced", binding["estimated"])
	}
}

// TestBudgetBindingIsAbsentWithoutACeiling — absence is a real state, and it
// renders as absence: a run under no budget leaves the name unbound so
// `{{budget}}` reads as an absent optional input rather than a zero ceiling.
func TestBudgetBindingIsAbsentWithoutACeiling(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	h.spend.spends["unbounded"] = Spend{Tokens: 900_000, USD: 42}

	if err := h.engine.StartItem(testItem("unbounded", "p", "flow", 0)); err != nil {
		t.Fatal(err)
	}
	if binding, present := budgetBinding(t, h); present {
		t.Fatalf("budget binding = %+v, want absent for a run with no ceiling", binding)
	}
	// The ledger is not even read for a run with no ceiling.
	if h.spend.callCount() != 0 {
		t.Fatalf("tree spend queries = %d, want none for a run under no budget", h.spend.callCount())
	}
}

// TestBudgetBindingRendersTokensAndWallClockInTheirOwnUnits — the binding says
// which ceiling is in force and renders it in units a model can reason about.
func TestBudgetBindingRendersTokensAndWallClockInTheirOwnUnits(t *testing.T) {
	t.Run("tokens", func(t *testing.T) {
		h := newHarness(t, Config{}, map[string]def.Workflow{
			"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
		}, []string{"p"}, nil)
		h.spend.spends["counted"] = Spend{Tokens: 40, Unpriced: 3}
		item := testItem("counted", "p", "flow", 0)
		item.Budget = json.RawMessage(`{"tokens":100}`)
		if err := h.engine.StartItem(item); err != nil {
			t.Fatal(err)
		}
		binding, present := budgetBinding(t, h)
		if !present {
			t.Fatal("token ceiling must bind")
		}
		if binding["kind"] != BudgetKindTokens ||
			binding["ceiling"] != int64(100) || binding["spent"] != int64(40) || binding["remaining"] != int64(60) {
			t.Fatalf("budget binding = %+v", binding)
		}
		// Token counts are exact whatever the rate table knows, so unpriced rows
		// never make a token ceiling an estimate.
		if binding["estimated"] != false {
			t.Fatalf("estimated = %v, want false for a token ceiling", binding["estimated"])
		}
	})

	t.Run("wall clock", func(t *testing.T) {
		// Two phases, because the run's start is stamped from the engine clock at
		// StartItem: the elapsed time a binding can show is the time between that
		// stamp and a LATER phase entry.
		workflow := def.Workflow{ID: "flow", Phases: []def.Phase{
			agentPhase("first", nil, []def.Route{{To: "second"}}),
			agentPhase("second", nil, []def.Route{{To: "done"}}),
		}}
		h := newHarness(t, Config{}, map[string]def.Workflow{"flow": workflow}, []string{"p"}, nil)
		ceiling := profile.Duration("2h")
		h.profiles.profiles["p"].Reliability.PerItemBudget = &profile.Budget{WallClock: &ceiling}
		now := time.UnixMilli(1_000_000)
		h.engine.now = func() time.Time { return now }
		item := testItem("timed", "p", "flow", 0)
		if err := h.engine.StartItem(item); err != nil {
			t.Fatal(err)
		}
		now = now.Add(30 * time.Minute)
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		starts := h.runner.started()
		if len(starts) != 2 {
			t.Fatalf("runner starts = %d, want both phases", len(starts))
		}
		binding, ok := starts[1].Vars[def.BudgetVariable].(map[string]any)
		if !ok {
			t.Fatalf("second attempt budget binding = %+v, want an object", starts[1].Vars[def.BudgetVariable])
		}
		if binding["kind"] != BudgetKindWallClock ||
			binding["ceiling"] != "2h0m0s" || binding["spent"] != "30m0s" || binding["remaining"] != "1h30m0s" {
			t.Fatalf("budget binding = %+v", binding)
		}
		// A wall clock never touches the ledger.
		if h.spend.callCount() != 0 {
			t.Fatalf("tree spend queries = %d, want none for a wall-clock ceiling", h.spend.callCount())
		}
	})
}

// TestUSDBudgetParkCarriesTheEstimatedTotal — the number the park (and the wake
// that quotes its cause) states is the composed one. A campaign whose spend was
// mostly Codex would otherwise read as $0.00 against its own ceiling.
func TestUSDBudgetParkCarriesTheEstimatedTotal(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	// Every dollar here is estimated: Codex reports tokens and no cost at all.
	h.spend.spends["codex-heavy"] = Spend{Tokens: 900_000, USD: 12.5, Estimated: true}
	item := testItem("codex-heavy", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"usd":10}`)

	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonBudgetExhausted)
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || !strings.Contains(phases[0].ParkCause, "has spent $12.50, past its budget of $10.00") {
		t.Fatalf("park cause = %q, want the composed spend", phases[0].ParkCause)
	}
	events := h.emitter.errorEvents(item.ID)
	if len(events) != 1 || events[0].Spend == nil || events[0].Spend.USD != 12.5 || !events[0].Spend.Estimated {
		t.Fatalf("budget error events = %+v", events)
	}
}

// TestTokenBudgetIgnoresUnpricedRows — a model with no rate cannot brick a
// token-budgeted run. Tokens are exact whatever the rate table knows, and the
// previous spend source failed the whole check on the first such row.
func TestTokenBudgetIgnoresUnpricedRows(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	h.spend.spends["unknown-model"] = Spend{Tokens: 10, Unpriced: 4}
	item := testItem("unknown-model", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"tokens":100}`)

	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	if starts := h.runner.started(); len(starts) != 1 {
		t.Fatalf("runner starts = %+v, want the phase to have started", starts)
	}
}

// TestUSDBudgetRefusesUnpricedRowsItIsStillInside — a dollar ceiling the run has
// not obviously crossed cannot be judged while some rows have no price at all.
// The run parks loudly rather than running on under a ceiling nobody can
// evaluate.
func TestUSDBudgetRefusesUnpricedRowsItIsStillInside(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	h.spend.spends["partly-priced"] = Spend{Tokens: 10, USD: 1, Estimated: true, Unpriced: 2}
	item := testItem("partly-priced", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"usd":25}`)

	// The evaluation failure reaches the caller as well as parking the run: a
	// budget that cannot be judged is an error, not a quiet pass.
	err := h.engine.StartItem(item)
	if err == nil || !strings.Contains(err.Error(), "have no USD rate") {
		t.Fatalf("start error = %v, want the unpriceable-spend refusal", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonSetupFailed)
	phases, listErr := h.store.ListWorkItemPhases(item.ID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(phases) != 1 || !strings.Contains(phases[0].ParkCause, "have no USD rate") {
		t.Fatalf("park cause = %q, want the unpriceable-spend refusal", phases[0].ParkCause)
	}
}

// TestUnjudgeableUSDBudgetStillRESOLVES — the refusal above is the ENFORCEMENT's,
// and it must not take the read surfaces with it. `ResolveBudget` answers with
// the numbers and the reason they cannot be trusted, so `run status` and the
// `{{budget}}` binding still tell the operator what is happening; only
// `checkBudget` turns that reason into the park. Taking the status read away
// over a model the rate table has not learned is the worst possible response —
// it is how the operator would have found out.
func TestUnjudgeableUSDBudgetStillResolves(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	h.spend.spends["partly-priced"] = Spend{Tokens: 10, USD: 1, Estimated: true, Unpriced: 2}
	item := testItem("partly-priced", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"usd":25}`)

	// ResolveBudget is handed the root row directly, so this is the read exactly
	// as `run status` performs it — no engine, no resident item.
	view, err := ResolveBudget(t.Context(), h.profiles, h.spend, item, time.UnixMilli(1_000))
	if err != nil {
		t.Fatalf("a read must answer even when the ceiling cannot be judged: %v", err)
	}
	if view == nil {
		t.Fatal("a declared ceiling must resolve")
	}
	if view.Exceeded != "" {
		t.Fatalf("exceeded = %q, want empty — the priced lower bound is inside the ceiling", view.Exceeded)
	}
	if !strings.Contains(view.Unjudged, "have no USD rate") {
		t.Fatalf("unjudged = %q, want the unpriceable-spend reason", view.Unjudged)
	}
	if view.CeilingUSD != 25 || view.Spend.USD != 1 || view.Spend.Unpriced != 2 {
		t.Fatalf("view = %+v, want the numbers the read has", view)
	}
}

// TestUSDBudgetBreachWinsOverUnpricedRows — a lower bound that ALREADY crosses
// the ceiling is a breach whatever the unpriced rows would add, so the run
// parks for the budget rather than for an evaluation failure.
func TestUSDBudgetBreachWinsOverUnpricedRows(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	h.spend.spends["over"] = Spend{Tokens: 10, USD: 30, Estimated: true, Unpriced: 2}
	item := testItem("over", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"usd":25}`)

	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonBudgetExhausted)
}

// The binding is prompt surface, so a ledger that will not answer must degrade
// to an unbound name — never fail the context it is one field of. A variable
// context is built at GATE EVALUATION too, and failing there marked an attempt
// that had already completed as parked and threw its gate advance away, with no
// verb that could repair it.
func TestBudgetReadFailureAtGateTimeDoesNotParkACompletedPhase(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	item := testItem("budgeted", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"tokens":1000}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// The ledger goes away after the phase started, so the failure lands on the
	// gate's own context build rather than on the entry's budget check.
	h.spend.errs[item.ID] = errors.New("database is locked")

	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, item.ID, StateDone, "")
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "completed" {
		t.Fatalf("phase rows = %+v, want the one attempt completed", phases)
	}
}

// Enforcement is the loud half and stays loud: the same unreadable ledger
// refuses to START a phase, because running on under a ceiling nobody can
// evaluate is exactly what a budget exists to prevent.
func TestBudgetReadFailureStillRefusesToStartAPhase(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"p"}, nil)
	item := testItem("budgeted", "p", "flow", 0)
	item.Budget = json.RawMessage(`{"tokens":1000}`)
	h.spend.errs[item.ID] = errors.New("database is locked")

	if err := h.engine.StartItem(item); err == nil {
		t.Fatal("a phase started under a ceiling whose spend could not be read")
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonSetupFailed)
}
