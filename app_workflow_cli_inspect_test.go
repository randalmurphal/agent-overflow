package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
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
