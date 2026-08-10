package starters

import (
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// A starter's whole value is the pattern it composes. `TestEmbeddedStarters…`
// proves each one is a valid definition, which a flattened version of the same
// definition would be too — so the shapes these two starters exist to
// DEMONSTRATE are pinned here.

func starterWorkflow(t *testing.T, id string) def.Workflow {
	t.Helper()
	resolved, _ := materializeStarter(t, id)
	return resolved.Workflow
}

func phaseByID(t *testing.T, workflow def.Workflow, id string) def.Phase {
	t.Helper()
	for _, phase := range workflow.Phases {
		if phase.ID == id {
			return phase
		}
	}
	t.Fatalf("workflow %q has no phase %q", workflow.ID, id)
	return def.Phase{}
}

// The ratchet is route ORDER plus two budgets, and it only works in that order:
// a loop route whose budget is spent falls through to the next matching route,
// which is what makes minor findings stop extending the loop without any
// element counting its own laps. Reordering these routes, merging the two loop
// edges, or giving them one budget silently turns the convergence loop back
// into the oscillating one it replaces.
func TestConvergenceStarterRatchetsThroughRouteOrder(t *testing.T) {
	workflow := starterWorkflow(t, "converge-on-review")
	routes := phaseByID(t, workflow, "review").Gate.Routes
	if len(routes) != 5 {
		t.Fatalf("review gate has %d routes, want the five-step ratchet", len(routes))
	}

	if routes[0].To != "done" {
		t.Fatalf("route 0 = %+v, want the clean verdict shipping", routes[0])
	}
	// Deliberately undecorated: the run rests on this route and its resting wake
	// is the fuller message, so a notify: here is an inert line an author would
	// only discover by waiting for a wake that never comes.
	if routes[0].Notify {
		t.Fatal("the terminal clean route carries an inert notify:")
	}

	fix, converge := routes[1], routes[2]
	for label, route := range map[string]def.Route{"fix": fix, "converge": converge} {
		if route.Loop != "implement" {
			t.Fatalf("%s route loops to %q, want the implementer", label, route.Loop)
		}
		// The implementer is re-entered WARM: one that lost what it just tried
		// re-derives it from the rejection note alone.
		if route.Session != def.SessionContinue {
			t.Fatalf("%s route session = %q, want continue", label, route.Session)
		}
		// Later rounds ask a narrower question, which is what `prompt:` is for.
		if route.Prompt == "" {
			t.Fatalf("%s route re-enters the implementer with its build-it body", label)
		}
		if len(route.Feedback) == 0 {
			t.Fatalf("%s route carries no findings back to the implementer", label)
		}
	}
	if fix.Prompt == converge.Prompt {
		t.Fatal("both loop edges render the same body, so the later rounds do not narrow")
	}
	// Separate budgets are the ratchet: one exhausting is what promotes the run
	// into rounds that only blocking and material findings extend.
	if fix.Max == converge.Max {
		t.Fatalf("both loop edges share one budget %+v", fix.Max)
	}
	if !routeMatchesVerdict(fix, "minor") {
		t.Fatal("the fix rounds do not fix minor findings, which is what makes them the EARLY rounds")
	}
	if routeMatchesVerdict(converge, "minor") {
		t.Fatal("the convergence rounds still extend on minors, so the loop never narrows")
	}
	// A wake on entering the convergence rounds says something a resting wake
	// will not, and the run continues through the route, so it is not inert.
	if !converge.Notify {
		t.Fatal("entering the convergence rounds wakes nobody")
	}

	if routes[3].To != "done" || !routeMatchesVerdict(routes[3], "minor") {
		t.Fatalf("route 3 = %+v, want a minor-only verdict shipping", routes[3])
	}
	// Blocking or material findings that survived every round are a judgment
	// call, and it is a human's.
	if routes[4].Park == "" {
		t.Fatalf("route 4 = %+v, want the park", routes[4])
	}
}

func routeMatchesVerdict(route def.Route, verdict string) bool {
	if route.When == nil {
		return true
	}
	if route.When.Eq != nil {
		value, _ := route.When.Eq.Value.(string)
		return value == verdict
	}
	if route.When.In != nil {
		for _, candidate := range route.When.In.Values {
			if value, ok := candidate.(string); ok && value == verdict {
				return true
			}
		}
	}
	return false
}

// The reviewer reads its OWN prior attempts. Without it, round four sees one
// diff and one note and cannot tell a genuine regression from the fourth
// restatement of a preference it already lost — which is the oscillation the
// whole starter exists to end.
func TestConvergenceStarterBindsTheReviewHistory(t *testing.T) {
	workflow := starterWorkflow(t, "converge-on-review")
	for _, phaseID := range []string{"implement", "review"} {
		phase := phaseByID(t, workflow, phaseID)
		binding, declared := phase.Inputs[def.HistoryPrefix+"review"]
		if !declared {
			t.Fatalf("phase %q does not bind the review history", phaseID)
		}
		if binding.Schema.Type != "array" || binding.Window == 0 {
			t.Fatalf("phase %q history binding = %+v", phaseID, binding)
		}
	}
	// A minor finding that will not be fixed has to survive the run: shipping it
	// silently is the omission `residue` exists to make impossible.
	if _, declared := phaseByID(t, workflow, "review").Outputs["residue"]; !declared {
		t.Fatal("the reviewer has no residue output, so surviving minors are simply lost")
	}
	if _, declared := workflow.Outputs["residue"]; !declared {
		t.Fatal("residue does not leave the workflow")
	}
}

// The criteria ledger is CONTENT: typed criteria seeded once on the root, and
// each wave's coverage answer forwarded through the self-call's args exactly as
// the wave number is. Nothing in the engine knows it exists, which is precisely
// what has to keep being true — and the forwarding is the one link that makes it
// cumulative rather than one wave's opinion.
func TestCampaignStarterForwardsTheCriteriaLedgerThroughItsSelfCall(t *testing.T) {
	workflow := starterWorkflow(t, "port-campaign")
	for _, name := range []string{"criteria", "coverage"} {
		if _, declared := workflow.Inputs[name]; !declared {
			t.Fatalf("the campaign does not take %q as an input", name)
		}
	}
	// The criteria are fixed at the start; a wave that could rewrite them would
	// be re-forming the definition of done every round, which is the drift.
	if workflow.Inputs["criteria"].Optional {
		t.Fatal("the criteria are optional, so a wave can run with no definition of done")
	}
	// The first wave has no previous ledger to read.
	if !workflow.Inputs["coverage"].Optional {
		t.Fatal("the coverage ledger is required, so the campaign cannot start")
	}

	plan := phaseByID(t, workflow, "plan")
	if _, declared := plan.Outputs["coverage"]; !declared {
		t.Fatal("the planner answers no coverage, so the ledger is never updated")
	}
	for _, name := range []string{"criteria", "coverage"} {
		if _, declared := plan.Inputs[name]; !declared {
			t.Fatalf("the planner cannot read %q", name)
		}
	}

	next := phaseByID(t, workflow, "next-wave")
	if next.CallTarget() != workflow.ID {
		t.Fatalf("the next-wave phase calls %q, not itself", next.CallTarget())
	}
	if next.Args["criteria"] != "criteria" {
		t.Fatalf("the next wave is handed criteria %q, want the campaign's own unchanged", next.Args["criteria"])
	}
	if next.Args["coverage"] != "plan.coverage" {
		t.Fatalf("the next wave is handed coverage %q, want this wave's answer", next.Args["coverage"])
	}
}

// The merge join opts into the contract, so the engine refuses a done envelope
// that leaves a lane out of both lists — the failure that nearly lost 1900
// lines. The reference script is content the profile binds; the engine never
// runs it implicitly, and that separation is the design.
func TestCampaignStarterJoinAccountsForItsUnitsAndShipsTheScript(t *testing.T) {
	workflow := starterWorkflow(t, "port-campaign")
	implement := phaseByID(t, workflow, "implement")
	if implement.Join == nil || !implement.Join.AccountsForUnits {
		t.Fatalf("the merge join does not account for its units: %+v", implement.Join)
	}
	for _, name := range []string{def.JoinMergedOutput, def.JoinBlockedOutput} {
		if _, declared := implement.Outputs[name]; !declared {
			t.Fatalf("the fan-out phase declares no %q output for its join to answer", name)
		}
	}

	set, err := Fetch("port-campaign")
	if err != nil {
		t.Fatal(err)
	}
	var script string
	for _, file := range set.Files {
		if strings.HasSuffix(file.Name, ".py") {
			script = string(file.Data)
		}
	}
	if script == "" {
		t.Fatal("the campaign ships no reference merge script")
	}
	// The behaviour that makes it a reference rather than a sample: a conflict
	// SKIPS that lane and continues. Stopping at the first conflict is what
	// dropped an approved lane in the live run.
	for _, want := range []string{"merge --abort", `"blocked"`, `"merged"`, "AO_ENVELOPE"} {
		if !strings.Contains(script, want) {
			t.Fatalf("the reference merge script does not mention %q", want)
		}
	}
}

// Non-goals are authored, frozen with the snapshot, and ride into every
// element's prompt. The two campaign-shaped starters state theirs because scope
// growth is what a long call tree fails by.
func TestCampaignShapedStartersDeclareTheirNonGoals(t *testing.T) {
	for _, id := range []string{"port-campaign", "converge-on-review"} {
		workflow := starterWorkflow(t, id)
		if len(workflow.NonGoals) == 0 {
			t.Fatalf("starter %q states no non-goals", id)
		}
		if len(workflow.NonGoals) > def.MaxNonGoals {
			t.Fatalf("starter %q declares %d non-goals, over the authoring bound", id, len(workflow.NonGoals))
		}
		for index, nonGoal := range workflow.NonGoals {
			if strings.TrimSpace(nonGoal) == "" {
				t.Fatalf("starter %q non-goal %d is blank", id, index)
			}
		}
	}
}
