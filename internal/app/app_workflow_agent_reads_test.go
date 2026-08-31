package app

import (
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two reads behind `run inspect` and `run narrative`. They are projections
// over records the engine already writes, so the fixture writes those records
// directly: what is under test is what the projections answer, not how a run
// gets into the state being read.

type inspectHarness struct {
	app *App
}

func newInspectHarness(t *testing.T) *inspectHarness {
	t.Helper()
	app, _ := setupE2EApp(t)
	app.configDir = t.TempDir()
	return &inspectHarness{app: app}
}

// scope is a phase session holding `introspect`, which is what a babysitting
// agent runs as: it may read every run in its project and act on none it did
// not start.
func (h *inspectHarness) scope(grants ...def.Grant) context.Context {
	names := make([]string, 0, len(grants))
	for _, grant := range grants {
		names = append(names, string(grant))
	}
	return transport.WithCallerScope(context.Background(), transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: "babysitter", ProjectID: defaultTestProjectID,
		ItemID: "supervisor", PhaseID: "watch", Grants: names,
	})
}

func (h *inspectHarness) run(t *testing.T, item store.WorkItem) store.WorkItem {
	t.Helper()
	item.ProjectID = defaultTestProjectID
	if item.WorkflowScope == "" {
		item.WorkflowScope = "shared"
	}
	if item.Source == "" {
		item.Source = "manual"
	}
	if item.CreatedAt == 0 {
		item.CreatedAt = 1
	}
	if err := h.app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func (h *inspectHarness) phase(t *testing.T, phase store.WorkItemPhase) {
	t.Helper()
	if phase.Status == "" {
		phase.Status = "completed"
	}
	if err := h.app.store.CreateWorkItemPhase(phase); err != nil {
		t.Fatal(err)
	}
}

// narrative writes the account an element would have written, at the path the
// runner would have handed it.
func (h *inspectHarness) narrative(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gateTrace(t *testing.T, kind def.DecisionKind, target string, exhausted ...string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(def.GateTrace{
		Decision: def.RouteDecision{Kind: kind, Target: target}, ExhaustedLoops: exhausted,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// The one call the campaign agent had to run 45 SQL queries to assemble.
func TestWorkflowAgentInspectRunAnswersTheWholePicture(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{
		ID: "root", Goal: "port the store", WorkflowID: "campaign",
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonGate),
		WorktreePath: "/w/root", Branch: "campaign/wave-3", BaseBranch: "main",
		Seeds: json.RawMessage(`{"wave":3,"package":"internal/store"}`),
	})
	h.run(t, store.WorkItem{
		ID: "child-a", Goal: "wave 3", WorkflowID: "port", State: string(engine.StateDone),
		Source: "call", ParentItemID: item.ID, ParentPhaseID: "fan", ParentUnitID: "unit-a",
		ParentAttempt: 1, CallDepth: 1, CreatedAt: 2,
	})
	h.run(t, store.WorkItem{
		ID: "child-b", Goal: "wave 3", WorkflowID: "port",
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion),
		Source: "call", ParentItemID: item.ID, ParentPhaseID: "fan", ParentUnitID: "unit-b",
		ParentAttempt: 1, CallDepth: 1, CreatedAt: 3,
	})
	// A superseded attempt and the one that is current, so the digest can be
	// shown to follow the latest rather than every attempt.
	h.phase(t, store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "review", Attempt: 1, StartedAt: 10,
		OutputEnvelope: json.RawMessage(`{"status":"done","outputs":{"verdict":"stale"}}`),
		GateTrace:      gateTrace(t, def.DecisionLoop, "review"),
	})
	h.phase(t, store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "review", Attempt: 2, StartedAt: 20, Status: "parked",
		OutputEnvelope: json.RawMessage(
			`{"status":"done","outputs":{"verdict":"changes-requested","worst-severity":"P1",` +
				`"findings":[{"severity":"P1","note":"unchecked error"}]}}`),
		GateTrace: gateTrace(t, def.DecisionHuman, "land"),
	})

	inspection, err := h.app.WorkflowAgentInspectRun(
		h.scope(def.GrantIntrospect), WorkflowAgentInspectInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.WorktreePath != "/w/root" || inspection.Branch != "campaign/wave-3" ||
		inspection.BaseBranch != "main" {
		t.Fatalf("workspace facts = %#v", inspection)
	}
	// Seeds ride the run view, which is also what `run status --json` returns.
	var seeds map[string]any
	if err := json.Unmarshal(inspection.Run.Seeds, &seeds); err != nil {
		t.Fatalf("seeds did not decode: %v (%s)", err, inspection.Run.Seeds)
	}
	if seeds["wave"] != float64(3) || seeds["package"] != "internal/store" {
		t.Fatalf("seeds = %#v", seeds)
	}
	if len(inspection.Children) != 2 {
		t.Fatalf("children = %#v", inspection.Children)
	}
	if inspection.Children[0].ItemID != "child-a" || inspection.Children[0].ParentUnitID != "unit-a" ||
		inspection.Children[1].State != string(engine.StateNeedsHuman) ||
		inspection.Children[1].Reason != string(engine.ReasonQuestion) {
		t.Fatalf("children = %#v", inspection.Children)
	}
	if len(inspection.Run.Phases) != 2 {
		t.Fatalf("attempts = %#v", inspection.Run.Phases)
	}
	superseded, latest := inspection.Run.Phases[0], inspection.Run.Phases[1]
	if len(superseded.Outputs) != 0 {
		t.Fatalf("a superseded attempt carried a digest: %#v", superseded)
	}
	if latest.Decision != string(def.DecisionHuman) || latest.DecisionTarget != "land" {
		t.Fatalf("latest attempt = %#v", latest)
	}
	digest := map[string]string{}
	for _, output := range latest.Outputs {
		digest[output.Name] = output.Value
	}
	if digest["verdict"] != "changes-requested" || digest["worst-severity"] != "P1" {
		t.Fatalf("digest = %#v", digest)
	}
	// A structured value keeps its JSON so a reader can see the shape without a
	// second call; a string value is its text.
	if !strings.HasPrefix(digest["findings"], `[{"severity":"P1"`) {
		t.Fatalf("findings digest = %q", digest["findings"])
	}
	if latest.OutputOverflow != 0 {
		t.Fatalf("three outputs overflowed a digest of %d: %d", maxDigestOutputs, latest.OutputOverflow)
	}
	// The base form deliberately does not drill down.
	if inspection.Phase != nil {
		t.Fatalf("an unasked-for phase was returned: %#v", inspection.Phase)
	}
}

// A digest is a stand-in, so it has to say when it is standing in for more than
// it shows — and each value has to stay small enough that a whole run's worth is
// one readable answer.
func TestWorkflowAgentInspectRunBoundsTheDigest(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{ID: "wide", WorkflowID: "campaign", State: string(engine.StateDone)})
	outputs := map[string]any{"aaa-long": strings.Repeat("x", maxDigestValueRunes+50)}
	for index := 0; index < maxDigestOutputs+3; index++ {
		outputs[string(rune('b'+index))] = index
	}
	envelope, err := json.Marshal(map[string]any{"status": "done", "outputs": outputs})
	if err != nil {
		t.Fatal(err)
	}
	h.phase(t, store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "survey", Attempt: 1, StartedAt: 10, OutputEnvelope: envelope,
	})

	inspection, err := h.app.WorkflowAgentInspectRun(
		h.scope(def.GrantIntrospect), WorkflowAgentInspectInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	attempt := inspection.Run.Phases[0]
	if len(attempt.Outputs) != maxDigestOutputs || attempt.OutputOverflow != 4 {
		t.Fatalf("digest = %d entries, overflow = %d", len(attempt.Outputs), attempt.OutputOverflow)
	}
	if !strings.HasSuffix(attempt.Outputs[0].Value, "…[truncated]") {
		t.Fatalf("an oversized value was not capped: %q", attempt.Outputs[0].Value)
	}
}

func TestWorkflowAgentInspectRunDrillsIntoOneAttempt(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{
		ID: "fanned", WorkflowID: "campaign",
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonUnitFailed),
	})
	h.phase(t, store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "fan", Attempt: 1, StartedAt: 10,
		OutputEnvelope: json.RawMessage(`{"status":"done","outputs":{"verdict":"first"}}`),
	})
	h.phase(t, store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "fan", Attempt: 2, StartedAt: 20, Status: "parked",
		OutputEnvelope: json.RawMessage(`{"status":"done","outputs":{"verdict":"second","count":7}}`),
		GateTrace:      gateTrace(t, def.DecisionPark, "needs-review", "fan:0"),
	})
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: item.ID, PhaseID: "fan", Attempt: 2, UnitID: "unit-a", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitDone, UnitAttempt: 1,
			Branch: "campaign/unit-a", WorktreePath: "/w/unit-a"},
		{ItemID: item.ID, PhaseID: "fan", Attempt: 2, UnitID: "join", UnitIndex: 1,
			Kind: store.WorkItemUnitKindJoin, Status: store.WorkItemUnitFailed, UnitAttempt: 2},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := h.scope(def.GrantIntrospect)
	// No attempt named: the latest is what a parked run is resting on.
	latest, err := h.app.WorkflowAgentInspectRun(ctx, WorkflowAgentInspectInput{ItemID: item.ID, PhaseID: "fan"})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Phase == nil || latest.Phase.Attempt != 2 {
		t.Fatalf("phase detail = %#v", latest.Phase)
	}
	if string(latest.Phase.Outputs["verdict"]) != `"second"` || string(latest.Phase.Outputs["count"]) != "7" {
		t.Fatalf("outputs = %#v", latest.Phase.Outputs)
	}
	if latest.Phase.Decision != string(def.DecisionPark) || latest.Phase.DecisionTarget != "needs-review" ||
		len(latest.Phase.ExhaustedLoops) != 1 {
		t.Fatalf("gate detail = %#v", latest.Phase)
	}
	if len(latest.Phase.Units) != 2 || latest.Phase.Units[1].UnitID != "join" ||
		latest.Phase.Units[1].Status != store.WorkItemUnitFailed || latest.Phase.Units[1].UnitAttempt != 2 {
		t.Fatalf("units = %#v", latest.Phase.Units)
	}
	if latest.Phase.Units[0].Branch != "campaign/unit-a" || latest.Phase.Units[0].WorktreePath != "/w/unit-a" {
		t.Fatalf("unit workspace facts = %#v", latest.Phase.Units[0])
	}
	// A drill-down replaces the digest rather than printing the same values
	// twice.
	for _, attempt := range latest.Run.Phases {
		if len(attempt.Outputs) != 0 {
			t.Fatalf("a drill-down also computed a digest: %#v", attempt)
		}
	}

	older, err := h.app.WorkflowAgentInspectRun(ctx,
		WorkflowAgentInspectInput{ItemID: item.ID, PhaseID: "fan", Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if older.Phase.Attempt != 1 || string(older.Phase.Outputs["verdict"]) != `"first"` {
		t.Fatalf("older attempt = %#v", older.Phase)
	}
	if older.Phase.Units == nil {
		t.Fatalf("an attempt with no units returned a nil list rather than an empty one")
	}
}

// A coordinate that does not exist is refused by name, and the refusal carries
// what the run actually has: an agent that mistyped a phase id must not have to
// run a second command to find the right one.
func TestWorkflowAgentInspectRunRefusesUnknownCoordinates(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{ID: "typo", WorkflowID: "campaign", State: string(engine.StateDone)})
	h.phase(t, store.WorkItemPhase{ItemID: item.ID, PhaseID: "review", Attempt: 1, StartedAt: 10})
	ctx := h.scope(def.GrantIntrospect)

	_, err := h.app.WorkflowAgentInspectRun(ctx, WorkflowAgentInspectInput{ItemID: item.ID, PhaseID: "reviw"})
	if err == nil || !strings.Contains(err.Error(), "phases review") {
		t.Fatalf("unknown phase error = %v", err)
	}
	_, err = h.app.WorkflowAgentInspectRun(ctx,
		WorkflowAgentInspectInput{ItemID: item.ID, PhaseID: "review", Attempt: 4})
	if err == nil || !strings.Contains(err.Error(), "attempts 1") {
		t.Fatalf("unknown attempt error = %v", err)
	}
	// An attempt with no phase names nothing at all, and is refused before any
	// of the above is even looked up.
	_, err = h.app.WorkflowAgentInspectRun(ctx, WorkflowAgentInspectInput{ItemID: item.ID, Attempt: 2})
	if err == nil || !strings.Contains(err.Error(), "supply the phase id too") {
		t.Fatalf("attempt without phase error = %v", err)
	}
}

func TestWorkflowAgentRunNarrativeResolvesPhaseAndUnitAccounts(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{ID: "narrated", WorkflowID: "campaign", State: string(engine.StateDone)})
	h.phase(t, store.WorkItemPhase{ItemID: item.ID, PhaseID: "fan", Attempt: 1, StartedAt: 10})
	h.phase(t, store.WorkItemPhase{ItemID: item.ID, PhaseID: "fan", Attempt: 2, StartedAt: 20})
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: item.ID, PhaseID: "fan", Attempt: 2, UnitID: "unit-a", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitDone, UnitAttempt: 3},
	}); err != nil {
		t.Fatal(err)
	}
	root := h.app.workflowDataRoot()
	phasePath, err := workflowrunner.NarrativePath(root, item.ID, "fan", 2)
	if err != nil {
		t.Fatal(err)
	}
	h.narrative(t, phasePath, "# The second attempt\n")
	unitPath, err := workflowrunner.UnitNarrativePath(root, item.ID, "fan", 2, "unit-a", 3)
	if err != nil {
		t.Fatal(err)
	}
	h.narrative(t, unitPath, "# Unit a, third try\n")

	ctx := h.scope(def.GrantIntrospect)
	// No attempt named resolves the latest, which is the account a reader wants.
	phase, err := h.app.WorkflowAgentRunNarrative(ctx,
		WorkflowAgentNarrativeInput{ItemID: item.ID, PhaseID: "fan"})
	if err != nil {
		t.Fatal(err)
	}
	if phase.Attempt != 2 || !phase.Present || phase.Content != "# The second attempt\n" {
		t.Fatalf("phase narrative = %#v", phase)
	}
	if phase.Path != phasePath || phase.Bytes != int64(len(phase.Content)) {
		t.Fatalf("phase narrative path/size = %#v", phase)
	}
	// The unit's try comes from its row: a caller that had to supply it would be
	// guessing at the one part of the path it cannot see.
	unit, err := h.app.WorkflowAgentRunNarrative(ctx,
		WorkflowAgentNarrativeInput{ItemID: item.ID, PhaseID: "fan", UnitID: "unit-a"})
	if err != nil {
		t.Fatal(err)
	}
	if unit.UnitAttempt != 3 || unit.Path != unitPath || unit.Content != "# Unit a, third try\n" {
		t.Fatalf("unit narrative = %#v", unit)
	}
}

// An attempt that wrote nothing is an answer that names the path; a coordinate
// that does not exist is an error. Conflating them is what sends a reader
// hand-globbing the run directory.
func TestWorkflowAgentRunNarrativeSeparatesAbsenceFromAWrongCoordinate(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{ID: "silent", WorkflowID: "campaign", State: string(engine.StateDone)})
	h.phase(t, store.WorkItemPhase{ItemID: item.ID, PhaseID: "review", Attempt: 1, StartedAt: 10})
	if err := h.app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: item.ID, PhaseID: "review", Attempt: 1, UnitID: "unit-a", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitDone, UnitAttempt: 1},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := h.scope(def.GrantIntrospect)

	absent, err := h.app.WorkflowAgentRunNarrative(ctx,
		WorkflowAgentNarrativeInput{ItemID: item.ID, PhaseID: "review"})
	if err != nil {
		t.Fatalf("an unwritten narrative was reported as a failure: %v", err)
	}
	if absent.Present || absent.Content != "" {
		t.Fatalf("absent narrative = %#v", absent)
	}
	if !strings.HasSuffix(absent.Path, filepath.Join("review.1", "narrative.md")) {
		t.Fatalf("absence did not name what was looked for: %#v", absent)
	}
	if _, err := h.app.WorkflowAgentRunNarrative(ctx,
		WorkflowAgentNarrativeInput{ItemID: item.ID, PhaseID: "nope"}); err == nil {
		t.Fatal("a phase this run never attempted read as an absent narrative")
	}
	_, err = h.app.WorkflowAgentRunNarrative(ctx,
		WorkflowAgentNarrativeInput{ItemID: item.ID, PhaseID: "review", UnitID: "unit-z"})
	if err == nil || !strings.Contains(err.Error(), "unit-a") {
		t.Fatalf("unknown unit error = %v", err)
	}
	if _, err := h.app.WorkflowAgentRunNarrative(ctx,
		WorkflowAgentNarrativeInput{ItemID: item.ID}); err == nil {
		t.Fatal("a narrative with no phase id was accepted")
	}
}

// A narrative has no size ceiling of its own and this answer lands in a context
// window, so the read is bounded and says so.
func TestWorkflowAgentRunNarrativeTruncatesAnOversizedAccount(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{ID: "verbose", WorkflowID: "campaign", State: string(engine.StateDone)})
	h.phase(t, store.WorkItemPhase{ItemID: item.ID, PhaseID: "review", Attempt: 1, StartedAt: 10})
	path, err := workflowrunner.NarrativePath(h.app.workflowDataRoot(), item.ID, "review", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Multi-byte content, so a byte-bounded read has a rune to split.
	h.narrative(t, path, strings.Repeat("é", maxWorkflowNarrativeBytes))

	narrative, err := h.app.WorkflowAgentRunNarrative(h.scope(def.GrantIntrospect),
		WorkflowAgentNarrativeInput{ItemID: item.ID, PhaseID: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if !narrative.Truncated || narrative.Bytes != int64(maxWorkflowNarrativeBytes*2) {
		t.Fatalf("narrative = truncated %v, bytes %d", narrative.Truncated, narrative.Bytes)
	}
	if len(narrative.Content) > maxWorkflowNarrativeBytes {
		t.Fatalf("content is %d bytes, over the %d cap", len(narrative.Content), maxWorkflowNarrativeBytes)
	}
	if strings.HasSuffix(narrative.Content, "�") {
		t.Fatalf("the read split a rune: %q", narrative.Content[len(narrative.Content)-4:])
	}
}

// Row confinement is the same as the rest of the read family's: a phase sees
// the runs it started, and every run in the project only with `introspect`.
func TestWorkflowAgentInspectAndNarrativeAreRowConfined(t *testing.T) {
	h := newInspectHarness(t)
	item := h.run(t, store.WorkItem{ID: "foreign", WorkflowID: "campaign", State: string(engine.StateDone)})
	h.phase(t, store.WorkItemPhase{ItemID: item.ID, PhaseID: "review", Attempt: 1, StartedAt: 10})

	blind := h.scope(def.GrantStartRun)
	if _, err := h.app.WorkflowAgentInspectRun(blind, WorkflowAgentInspectInput{ItemID: item.ID}); err == nil ||
		!strings.Contains(err.Error(), "may only act on the runs it started") {
		t.Fatalf("a start-run-only phase inspected a run it did not start: %v", err)
	}
	if _, err := h.app.WorkflowAgentRunNarrative(blind,
		WorkflowAgentNarrativeInput{ItemID: item.ID, PhaseID: "review"}); err == nil ||
		!strings.Contains(err.Error(), "may only act on the runs it started") {
		t.Fatalf("a start-run-only phase read a foreign run's narrative: %v", err)
	}
	if _, err := h.app.WorkflowAgentInspectRun(h.scope(def.GrantIntrospect),
		WorkflowAgentInspectInput{ItemID: item.ID}); err != nil {
		t.Fatalf("introspect could not inspect a project run: %v", err)
	}
	// The webview carries no caller scope and has no business on this surface.
	if _, err := h.app.WorkflowAgentInspectRun(context.Background(),
		WorkflowAgentInspectInput{ItemID: item.ID}); err == nil {
		t.Fatal("an unscoped call reached the agent surface")
	}
}

// The blocking read behind `agent-overflow run watch`. Transitions reach the
// ring through the SAME listener a wake does (`afterWorkflowEngineEvent`), so
// what these drive is the wiring, not a private test entry point.

type watchHarness struct{ app *App }

func newWatchHarness(t *testing.T) *watchHarness {
	t.Helper()
	app, _ := setupE2EApp(t)
	app.configDir = t.TempDir()
	return &watchHarness{app: app}
}

// scope is a phase session holding `introspect`, which is what a supervising
// agent watching its own campaign runs as.
func (h *watchHarness) scope(grants ...def.Grant) context.Context {
	names := make([]string, 0, len(grants))
	for _, grant := range grants {
		names = append(names, string(grant))
	}
	return transport.WithCallerScope(context.Background(), transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: "babysitter", ProjectID: defaultTestProjectID,
		ItemID: "supervisor", PhaseID: "watch", Grants: names,
	})
}

func (h *watchHarness) run(t *testing.T, item store.WorkItem) store.WorkItem {
	t.Helper()
	item.ProjectID = defaultTestProjectID
	if item.WorkflowScope == "" {
		item.WorkflowScope = "shared"
	}
	if item.Source == "" {
		item.Source = "manual"
	}
	if item.CreatedAt == 0 {
		item.CreatedAt = 1
	}
	if err := h.app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

// move drives one engine transition through the app's real listener and then
// persists the row, in that order — which is the order the engine produces them
// in for everything except the transition it emits from inside its own write.
func (h *watchHarness) move(t *testing.T, itemID, phaseID string, attempt int, from, to engine.State, reason engine.Reason) {
	t.Helper()
	if err := h.app.store.UpdateWorkItemState(itemID, string(to), string(reason), 0); err != nil {
		t.Fatal(err)
	}
	h.app.afterWorkflowEngineEvent("workflow:item-state", engine.StateEvent{
		ItemID: itemID, ProjectID: defaultTestProjectID, From: from, To: to,
		Reason: reason, PhaseID: phaseID, Attempt: attempt,
	})
}

// A watch with no cursor answers immediately with where the run is and the
// sequence to continue from — never a backlog. A campaign's retained history
// replayed into a supervisor's context is the opposite of "tell me what happens
// next".
func TestWorkflowAgentWatchRunAnswersTheFirstCallWithoutBlocking(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	h.move(t, item.ID, "survey", 1, engine.StateRunning, engine.StateRunning, "")

	started := time.Now()
	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > maxWorkflowWatchHold/2 {
		t.Fatalf("a cursorless watch blocked for %s", elapsed)
	}
	if len(result.Transitions) != 0 {
		t.Fatalf("first call returned a backlog of %d transitions", len(result.Transitions))
	}
	if result.Cursor == 0 {
		t.Fatal("first call returned no cursor to continue from")
	}
	if result.Run.State != string(engine.StateRunning) || result.Run.Resting {
		t.Fatalf("run = %#v, want the running run", result.Run)
	}
}

// The blocking half: a call that arrives while nothing has happened parks, and
// the transition itself is what wakes it — not a timer, and not the caller
// asking again.
func TestWorkflowAgentWatchRunBlocksUntilTheRunMoves(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}

	answered := make(chan WorkflowAgentWatchResult, 1)
	go func() {
		result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
			WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 5000})
		if err != nil {
			t.Error(err)
			close(answered)
			return
		}
		answered <- result
	}()
	// The watcher is parked; the transition is what releases it.
	time.Sleep(20 * time.Millisecond)
	select {
	case result := <-answered:
		t.Fatalf("watch answered before anything happened: %#v", result)
	default:
	}
	h.move(t, item.ID, "survey", 2, engine.StateRunning, engine.StateNeedsHuman, engine.ReasonStuck)

	select {
	case result, ok := <-answered:
		if !ok {
			t.Fatal("watch failed")
		}
		if len(result.Transitions) != 1 {
			t.Fatalf("transitions = %#v, want the one that woke it", result.Transitions)
		}
		transition := result.Transitions[0]
		if transition.To != string(engine.StateNeedsHuman) || transition.Reason != string(engine.ReasonStuck) {
			t.Fatalf("transition = %#v", transition)
		}
		if transition.PhaseID != "survey" || transition.Attempt != 2 {
			t.Fatalf("transition coordinate = %s#%d, want the phase and attempt the run was in",
				transition.PhaseID, transition.Attempt)
		}
		if !transition.Resting {
			t.Fatal("a needs-human transition was not reported as resting")
		}
		if !result.Run.Resting || result.Run.Repair == "" {
			t.Fatalf("run = %#v, want the resting state and the verb that settles it", result.Run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the transition did not wake the watch")
	}
}

// A re-park under a NEW reason is a move, even though the state did not change.
// The wake filters those out; a monitor that did would report a run as waiting
// on something it is no longer waiting on.
func TestWorkflowAgentWatchRunReportsARepark(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion)})
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.move(t, item.ID, "survey", 1, engine.StateNeedsHuman, engine.StateNeedsHuman, engine.ReasonTakenOver)

	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Transitions) != 1 || result.Transitions[0].Reason != string(engine.ReasonTakenOver) {
		t.Fatalf("transitions = %#v, want the re-park", result.Transitions)
	}
}

// Without --tree a watch reports only its own run; with it, the runs the watched
// run called — which for a campaign is where every transition happens.
func TestWorkflowAgentWatchRunTreeFollowsCalledRuns(t *testing.T) {
	h := newWatchHarness(t)
	root := h.run(t, store.WorkItem{ID: "root", Goal: "campaign", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	child := h.run(t, store.WorkItem{ID: "wave-3", Goal: "wave 3", WorkflowID: "port",
		State: string(engine.StateRunning), Source: "call", ParentItemID: root.ID,
		ParentPhaseID: "wave", ParentAttempt: 3, CallDepth: 1, CreatedAt: 2})

	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: root.ID, Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	h.move(t, child.ID, "build", 1, engine.StateRunning, engine.StateNeedsHuman, engine.ReasonUnitFailed)

	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: root.ID, Cursor: first.Cursor, Tree: true, WaitMillis: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Transitions) != 1 || result.Transitions[0].ItemID != child.ID {
		t.Fatalf("tree transitions = %#v, want the called run's", result.Transitions)
	}
	// The run reported is still the one being watched: a descendant parking does
	// not make the root resting, and a watcher must not read it as such.
	if result.Run.ItemID != root.ID || result.Run.Resting {
		t.Fatalf("run = %#v, want the watched root still running", result.Run)
	}
	// The same transition is invisible without the flag: the watch is scoped to
	// what the caller asked to watch.
	narrow, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: root.ID, Cursor: first.Cursor, WaitMillis: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.Transitions) != 0 {
		t.Fatalf("a narrow watch saw a descendant's transition: %#v", narrow.Transitions)
	}
}

// A `--tree` watch re-resolves its membership on EVERY wake of a globally
// broadcast loop, so the read it resolves through may not carry a run's frozen
// workflow snapshot — nor make SQLite parse one to compute a phase ordinal
// nobody on this path reads: one transition anywhere in the app would otherwise
// cost a campaign supervisor that work per tree member.
//
// The three assertions are one statement. The node walk must return the whole
// tree in the same order the full-row walk does, carrying nothing but linkage,
// and the full-row walk over the same rows must carry the frozen blobs —
// otherwise the fixture proves nothing and the first assertion would pass over
// an empty column.
func TestWorkflowWatchTreeResolvesThroughSnapshotFreeRows(t *testing.T) {
	h := newWatchHarness(t)
	snapshot := json.RawMessage(`{"workflow":{"id":"campaign","phases":[{"id":"wave"}]}}`)
	root := h.run(t, store.WorkItem{ID: "root", Goal: "campaign", WorkflowID: "campaign",
		State: string(engine.StateRunning), Snapshot: snapshot,
		Seeds: json.RawMessage(`{"wave-number":3}`)})
	wanted := map[string]bool{root.ID: true}
	for index, child := range []string{"wave-1", "wave-2", "wave-3"} {
		item := h.run(t, store.WorkItem{ID: child, Goal: "wave", WorkflowID: "port",
			State: string(engine.StateRunning), Source: "call", ParentItemID: root.ID,
			ParentPhaseID: "wave", ParentAttempt: 1, CallDepth: 1, CreatedAt: int64(2 + index),
			Snapshot: snapshot, Seeds: json.RawMessage(`{"task":"x"}`)})
		wanted[item.ID] = true
	}
	// A grandchild: the walk's own recursion, not just one level of children.
	grandchild := h.run(t, store.WorkItem{ID: "wave-1-unit", Goal: "unit", WorkflowID: "port",
		State: string(engine.StateRunning), Source: "call", ParentItemID: "wave-1",
		ParentPhaseID: "fan", ParentAttempt: 1, ParentUnitID: "u1", CallDepth: 2, CreatedAt: 9,
		Snapshot: snapshot, Seeds: json.RawMessage(`{"task":"y"}`)})
	wanted[grandchild.ID] = true

	nodes, err := h.app.workflowRunTreeNodes(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != len(wanted) {
		t.Fatalf("node tree = %d members, want %d", len(nodes), len(wanted))
	}
	for _, member := range nodes {
		if !wanted[member.ID] {
			t.Fatalf("node tree carries an unrelated run %q", member.ID)
		}
	}

	full, err := h.app.workflowRunTree(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Membership AND order are identical: the two projections read one tree, so
	// a narrower read may never be a differently-shaped one.
	if len(full) != len(nodes) {
		t.Fatalf("full tree = %d members, node tree = %d", len(full), len(nodes))
	}
	for index, member := range full {
		if member.ID != nodes[index].ID {
			t.Fatalf("tree member %d: full walk = %q, node walk = %q", index, member.ID, nodes[index].ID)
		}
		if member.CallDepth != nodes[index].CallDepth {
			t.Fatalf("tree member %q: full walk depth %d, node walk depth %d",
				member.ID, member.CallDepth, nodes[index].CallDepth)
		}
		if len(member.Snapshot) == 0 {
			t.Fatalf("fixture run %q has no snapshot, so the node assertion above proves nothing", member.ID)
		}
	}

}

// The id a watch AUTHORIZES is the trimmed one, so every read it then issues has
// to be keyed on that and not on the raw request field. A caller whose shell left
// a newline on the id used to pass authorization and be answered with a bare
// no-rows error from a lookup of a run that does not exist.
func TestWorkflowAgentWatchRunAnswersAnUntrimmedID(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion)})

	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: " " + item.ID + "\n", Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ItemID != item.ID {
		t.Fatalf("itemId = %q, want the resolved run id", result.ItemID)
	}
	if result.Run.ItemID != item.ID || !result.Run.Resting {
		t.Fatalf("run = %#v, want the resting run the caller named", result.Run)
	}
}

// A resting transition carries the ENGINE's own diagnosis of the park, resolved
// from the exact attempt the transition recorded rather than from whatever the
// run's latest attempt is by the time the watch answers.
func TestWorkflowAgentWatchRunCarriesTheParkCauseOfThatAttempt(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	for _, phase := range []store.WorkItemPhase{
		{ItemID: item.ID, PhaseID: "survey", Attempt: 1, Status: "parked",
			ParkCause: "the worktree would not cut", StartedAt: 1},
		{ItemID: item.ID, PhaseID: "survey", Attempt: 2, Status: "parked",
			ParkCause: "the budget ran out", StartedAt: 2},
	} {
		if err := h.app.store.CreateWorkItemPhase(phase); err != nil {
			t.Fatal(err)
		}
	}
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.move(t, item.ID, "survey", 1, engine.StateRunning, engine.StateNeedsHuman, engine.ReasonSetupFailed)

	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Transitions) != 1 {
		t.Fatalf("transitions = %#v", result.Transitions)
	}
	if got := result.Transitions[0].Cause; got != "the worktree would not cut" {
		t.Fatalf("cause = %q, want the cause of attempt 1 rather than the latest attempt's", got)
	}
}

// A cursor the ring cannot honour is a resync instruction, never silence: the
// ring is a jitter buffer, and a watcher holding a sequence from a previous
// process would otherwise block on a number this one will never reach.
func TestWorkflowAgentWatchRunGapsAnImpossibleCursor(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID, Cursor: 9_000, WaitMillis: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Gap {
		t.Fatalf("a cursor above the head did not gap: %#v", result)
	}
	if result.Run.State != string(engine.StateRunning) {
		t.Fatalf("run = %#v, want the current state alongside the gap", result.Run)
	}
}

// A wait budget is honoured exactly: `--timeout` is only exact if the last poll
// waits the remainder and not a second more.
func TestWorkflowAgentWatchRunHonoursTheCallersWaitBudget(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 60})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed < 50*time.Millisecond {
		t.Fatalf("watch returned after %s, want it to have held for the requested 60ms", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("watch held for %s, want the caller's budget to bound it", elapsed)
	}
	if len(result.Transitions) != 0 || result.Gap {
		t.Fatalf("an expired hold reported %#v", result)
	}
}

// A cancelled caller is answered rather than left holding the goroutine: a CLI
// that hung up has already stopped listening, and the app must not keep a
// blocked request alive for it.
func TestWorkflowAgentWatchRunReturnsWhenTheCallerHangsUp(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.scope(def.GrantIntrospect))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := h.app.WorkflowAgentWatchRun(ctx,
			WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 20_000}); err != nil {
			t.Error(err)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled watch kept blocking")
	}
}

// The same row-level rule every read verb takes: a phase session with no read
// grant may watch only the runs it started, and a run it did not start is
// refused rather than watched.
func TestWorkflowAgentWatchRunRefusesARunThePhaseMayNotSee(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	_, err := h.app.WorkflowAgentWatchRun(h.scope(), WorkflowAgentWatchInput{ItemID: item.ID})
	if err == nil {
		t.Fatal("a phase with no grant watched a run it did not start")
	}
	if _, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: "nonexistent"}); err == nil {
		t.Fatal("watching a run that does not exist was accepted")
	}
}
