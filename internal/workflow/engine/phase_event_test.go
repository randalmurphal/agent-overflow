package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// Every phase-channel event carries the engine's own event time, so a consumer
// patching a live view never has to stamp its own clock. The stamp is the
// emitter's, not the caller's, which is what makes "one emit site forgot it"
// unreachable — the assertions below cover the phase half and the unit half of
// the same channel.

func requireStampedPhaseEvents(t *testing.T, events []PhaseEvent, want int) {
	t.Helper()
	if len(events) != want {
		t.Fatalf("phase events = %d, want %d: %+v", len(events), want, events)
	}
	previous := int64(0)
	for _, event := range events {
		if event.OccurredAt <= previous {
			t.Fatalf("phase event %+v occurredAt = %d, want strictly after %d", event, event.OccurredAt, previous)
		}
		previous = event.OccurredAt
	}
}

func TestPhaseEventsCarryTheEngineEventTime(t *testing.T) {
	workflow := def.Workflow{ID: "stamped", Phases: []def.Phase{
		agentPhase("work", nil, []def.Route{{To: "wrap"}}),
		agentPhase("wrap", nil, []def.Route{{To: "done"}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"stamped": workflow}, []string{"project"}, nil)
	item := testItem("stamped-run", "project", "stamped", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")

	// running(work), completed(work), running(wrap), completed(wrap).
	requireStampedPhaseEvents(t, h.emitter.phaseEvents(item.ID), 4)
}

func TestUnitPhaseEventsCarryTheEngineEventTime(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"fan": fanOutWorkflow("fan", 1)}, []string{"project"}, nil)
	item := testItem("fan-run", "project", "fan", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-unit-0"), Outcome{
		Kind: OutcomeDone, Envelope: doneEnvelope(true),
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-join"), Outcome{
		Kind: OutcomeDone, Envelope: doneEnvelope(true),
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")

	events := h.emitter.phaseEvents(item.ID)
	units := make([]PhaseEvent, 0, len(events))
	for _, event := range events {
		if event.UnitID != "" {
			units = append(units, event)
		}
	}
	if len(units) == 0 {
		t.Fatalf("no unit events on the phase channel: %+v", events)
	}
	requireStampedPhaseEvents(t, events, len(events))
	requireStampedPhaseEvents(t, units, len(units))
}

// TestPhaseStateEventsHaveOneConstructionPath is the structural half of the
// stamp invariant. The two tests above prove the events this engine emits today
// carry a time; this one proves a NEW emit site cannot skip the path that
// guarantees it — the failure mode the whole design is arranged against, and
// the one no behavioral test can see, because a second emitter's events are
// simply not in the stream the assertions above read.
//
// It parses this package's own sources rather than grepping so a channel name
// inside a comment or a test fixture cannot fail it, in the style of the
// frontend's architecture.test.ts.
//
// The walk is over the whole FILE, not over function bodies: a package-level
// `const phaseStateChannel = "workflow:phase-state"` plus
// `emitter.Emit(phaseStateChannel, …)` is a second construction path that no
// body-scoped scan can see, and it is the exact shape a future refactor
// reaches for. So EVERY occurrence of the string counts — const, var,
// composite literal, or a fragment concatenated into one — and the only
// occurrence this package tolerates is the one inside `emitPhaseState`.
func TestPhaseStateEventsHaveOneConstructionPath(t *testing.T) {
	// The unit half of the channel rides the same emitter, so there is one
	// string to police rather than two. A future unit-only channel joins this
	// list and gets the same treatment.
	for _, channel := range []string{"workflow:phase-state"} {
		requireSingleChannelSite(t, channel, "emitPhaseState")
	}
}

// requireSingleChannelSite fails unless the channel string occurs exactly once
// in the package's non-test sources, inside the named function.
func requireSingleChannelSite(t *testing.T, channel, emitter string) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse engine package: %v", err)
	}
	sites := map[string][]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, hit := range channelLiterals(file, channel) {
				owner := enclosingFunc(file, hit)
				sites[owner] = append(sites[owner], fset.Position(hit).String())
			}
		}
	}
	if len(sites) != 1 || len(sites[emitter]) != 1 {
		t.Fatalf(
			"%q occurs in %v, want %s alone — route the new site through it so its "+
				"OccurredAt cannot be forgotten, and do not lift the channel into a shared constant",
			channel, sites, emitter,
		)
	}
}

// channelLiterals reports every position in one file where the channel string
// is constructed, whether written whole or assembled from constant fragments.
func channelLiterals(file *ast.File, channel string) []token.Pos {
	var hits []token.Pos
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.BinaryExpr:
			// `"workflow:" + "phase-state"` is the same channel, spelled to slip
			// past an equality check. Folded here, and its operands are not
			// descended into afterwards.
			if folded, ok := foldStringConcat(typed); ok {
				if folded == channel {
					hits = append(hits, typed.Pos())
				}
				return false
			}
		case *ast.BasicLit:
			if typed.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(typed.Value); err == nil && value == channel {
				hits = append(hits, typed.Pos())
			}
		}
		return true
	})
	return hits
}

// foldStringConcat evaluates a `+` tree of string literals, reporting false for
// anything that is not one (a variable operand, arithmetic, a call).
func foldStringConcat(expr ast.Expr) (string, bool) {
	switch typed := expr.(type) {
	case *ast.BasicLit:
		if typed.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(typed.Value)
		return value, err == nil
	case *ast.ParenExpr:
		return foldStringConcat(typed.X)
	case *ast.BinaryExpr:
		if typed.Op != token.ADD {
			return "", false
		}
		left, leftOK := foldStringConcat(typed.X)
		right, rightOK := foldStringConcat(typed.Y)
		if !leftOK || !rightOK {
			return "", false
		}
		return left + right, true
	}
	return "", false
}

// enclosingFunc names the function a position sits in — the declaration's whole
// range, so a literal inside a nested closure or a package-level `const` block
// is attributed to whatever actually contains it.
func enclosingFunc(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || pos < fn.Pos() || pos > fn.End() {
			continue
		}
		return fn.Name.Name
	}
	return "(package level)"
}

// TestUnitStateEmitRefusesAnUnstampedTime is the other half of "an emit beside a
// store write passes the time that write PERSISTED": `emitUnitStateAt` exists to
// carry that value, so a caller reaching it without one is an engine bug, and
// the guard has to be loud rather than quietly substituting a clock. It is not
// fatal — a status transition the UI never hears about leaves a node stuck
// mid-flight forever, which is worse than a stamp one tick late — so the event
// still ships, stamped and reported.
func TestUnitStateEmitRefusesAnUnstampedTime(t *testing.T) {
	sink := &recordingLog{}
	h := newHarness(t, Config{Log: sink}, map[string]def.Workflow{"fan": fanOutWorkflow("fan", 1)}, []string{"project"}, nil)
	item := &runtimeItem{
		item:    store.WorkItem{ID: "unstamped-run", ProjectID: "project"},
		phaseID: "work", attempt: 1,
	}
	unit := &unitRun{id: "work-unit-0", index: 0, kind: UnitWork, status: "pending"}

	h.engine.emitUnitStateAt(item, unit, 0)

	events := h.emitter.phaseEvents("unstamped-run")
	if len(events) != 1 {
		t.Fatalf("unstamped emit produced %d events, want the transition to ship anyway: %+v", len(events), events)
	}
	if events[0].OccurredAt <= 0 {
		t.Fatalf("unstamped emit reached the wire unstamped: %+v", events[0])
	}
	if reported := sink.matching(LogEventEmitTimeMissing); len(reported) != 1 ||
		reported[0].ItemID != "unstamped-run" {
		t.Fatalf("an emit with no persisted time was corrected silently: %+v", reported)
	}
}
