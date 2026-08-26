package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// `ui diff` answers "what moved between these two snapshots". The failure
// that matters is a diff that says nothing about a page that changed, so
// these tests drive canned viewports through the comparison.

func cannedRow(id string, index int, y float64, inView bool) uiRow {
	return uiRow{
		ItemID: id, Kind: "assistant_text", Role: "assistant", Status: "complete",
		RowIndex: index, InViewport: inView,
		Rect:     uiRect{X: 10, Y: y, W: 600, H: 40},
		TextHead: "row " + id,
	}
}

func cannedViewport(rows ...uiRow) uiViewport {
	return uiViewport{
		V: 1, Settled: true, ActiveThreadID: "thread-1", DomNodes: 1200,
		Panes: []uiPane{{
			PaneID: "pane-a", PaneKind: "thread", Focused: true, ThreadID: "thread-1",
			Rect:        uiRect{X: 0, Y: 0, W: 800, H: 600},
			Scroll:      &uiScroll{Top: 100, Height: 4000, Client: 600, DistanceFromBottom: 3300},
			MountedRows: len(rows), Rows: rows,
		}},
	}
}

func TestDiffViewportsFindsNothingWhenNothingMoved(t *testing.T) {
	view := cannedViewport(cannedRow("a", 0, 10, true), cannedRow("b", 1, 60, true))
	diff := diffViewports(view, view, uiGeometryThresholdPx)
	if !diff.Empty() {
		t.Errorf("identical snapshots must diff empty: %+v", diff)
	}
	if !strings.Contains(renderUIDiff(diff, uiSnapshotFile{}, uiSnapshotFile{}), "no change") {
		t.Error("an empty diff must say so rather than printing nothing")
	}
}

func TestDiffViewportsIgnoresSubPixelNoise(t *testing.T) {
	before := cannedViewport(cannedRow("a", 0, 10, true))
	after := cannedViewport(cannedRow("a", 0, 11.4, true))
	if diff := diffViewports(before, after, uiGeometryThresholdPx); !diff.Empty() {
		t.Errorf("a 1.4px shift is layout noise, not a finding: %+v", diff)
	}
	after = cannedViewport(cannedRow("a", 0, 14, true))
	diff := diffViewports(before, after, uiGeometryThresholdPx)
	if len(diff.Panes) != 1 || len(diff.Panes[0].RowsMoved) != 1 {
		t.Fatalf("a 4px shift must be reported: %+v", diff)
	}
	if got := diff.Panes[0].RowsMoved[0].DY; got != 4 {
		t.Errorf("dy = %v, want 4", got)
	}
}

func TestDiffViewportsReportsViewportEntryAndExit(t *testing.T) {
	before := cannedViewport(
		cannedRow("a", 0, 10, true),
		cannedRow("b", 1, 700, false),
	)
	after := cannedViewport(
		cannedRow("a", 0, 10, false),
		cannedRow("b", 1, 700, true),
	)
	diff := diffViewports(before, after, uiGeometryThresholdPx)
	if len(diff.Panes) != 1 {
		t.Fatalf("panes = %+v", diff.Panes)
	}
	pane := diff.Panes[0]
	if len(pane.EnteredView) != 1 || pane.EnteredView[0] != "b" {
		t.Errorf("enteredViewport = %v, want [b]", pane.EnteredView)
	}
	if len(pane.LeftView) != 1 || pane.LeftView[0] != "a" {
		t.Errorf("leftViewport = %v, want [a]", pane.LeftView)
	}
}

func TestDiffViewportsMatchesRowsByIDNotPosition(t *testing.T) {
	// A row inserted at the top shifts every index. A positional diff would
	// call that "everything changed"; only the new row is news.
	before := cannedViewport(cannedRow("a", 0, 10, true), cannedRow("b", 1, 60, true))
	after := cannedViewport(
		cannedRow("new", 0, 10, true),
		cannedRow("a", 1, 60, true),
		cannedRow("b", 2, 110, true),
	)
	diff := diffViewports(before, after, uiGeometryThresholdPx)
	pane := diff.Panes[0]
	if len(pane.RowsMounted) != 1 || pane.RowsMounted[0] != "new" {
		t.Errorf("rowsMounted = %v, want [new]", pane.RowsMounted)
	}
	if len(pane.RowsUnmounted) != 0 {
		t.Errorf("rowsUnmounted = %v, want none", pane.RowsUnmounted)
	}
	// a and b both slid down 50px, which is a real geometry change.
	if len(pane.RowsMoved) != 2 {
		t.Errorf("rowsMoved = %+v, want a and b", pane.RowsMoved)
	}
}

func TestDiffViewportsRowListsKeepDocumentOrder(t *testing.T) {
	before := cannedViewport()
	rows := make([]uiRow, 0, 3)
	// Deliberately out of array order: the diff must sort by rowIndex, not
	// by id and not by array position.
	rows = append(rows, cannedRow("z", 2, 110, true), cannedRow("m", 0, 10, true), cannedRow("k", 1, 60, true))
	after := cannedViewport(rows...)
	pane := diffViewports(before, after, uiGeometryThresholdPx).Panes[0]
	want := []string{"m", "k", "z"}
	for i, id := range want {
		if pane.RowsMounted[i] != id {
			t.Fatalf("rowsMounted = %v, want %v (top to bottom)", pane.RowsMounted, want)
		}
	}
}

func TestDiffViewportsReportsScrollAndStatus(t *testing.T) {
	before := cannedViewport(cannedRow("a", 0, 10, true))
	streaming := cannedRow("a", 0, 10, true)
	streaming.Status = "running"
	streaming.Streaming = true
	after := cannedViewport(streaming)
	after.Panes[0].Scroll.Top = 3400
	after.Panes[0].Scroll.AtBottom = true
	after.ActiveThreadID = "thread-2"
	after.DomNodes = 1500
	// `dialog` and `popover` are the only two kinds the bridge can emit
	// (frontend/src/lib/harness/snapshot.ts); a fixture inventing a third
	// tests a string this diff will never be handed.
	after.Overlays = []uiOverlay{{Name: "command-palette", Kind: "dialog"}}

	diff := diffViewports(before, after, uiGeometryThresholdPx)
	if diff.ActiveThreadChanged == nil || diff.ActiveThreadChanged.To != "thread-2" {
		t.Errorf("activeThreadChanged = %+v", diff.ActiveThreadChanged)
	}
	if diff.DomNodes == nil || diff.DomNodes.To-diff.DomNodes.From != 300 {
		t.Errorf("domNodes = %+v", diff.DomNodes)
	}
	if len(diff.OverlaysOpened) != 1 || diff.OverlaysOpened[0] != "dialog:command-palette" {
		t.Errorf("overlaysOpened = %v", diff.OverlaysOpened)
	}
	pane := diff.Panes[0]
	if pane.ScrollTop == nil || pane.AtBottom == nil {
		t.Fatalf("scroll changes missing: %+v", pane)
	}
	if len(pane.StatusChanged) != 1 || pane.StatusChanged[0].To != "running+streaming" {
		t.Errorf("statusChanged = %+v", pane.StatusChanged)
	}
}

// A pane whose scroller appeared or vanished changes no number inside the
// scroll block, so the "both non-nil" comparison sees nothing. Reporting
// that as "no change" is the same lie the whole command exists to catch.
func TestDiffViewportsReportsAScrollerAppearingAndVanishing(t *testing.T) {
	withScroll := cannedViewport(cannedRow("a", 0, 10, true))
	without := cannedViewport(cannedRow("a", 0, 10, true))
	without.Panes[0].Scroll = nil

	appeared := diffViewports(without, withScroll, uiGeometryThresholdPx)
	if len(appeared.Panes) != 1 {
		t.Fatalf("a pane that gained a scroller must be reported: %+v", appeared)
	}
	if c := appeared.Panes[0].ScrollerPresent; c == nil || c.From || !c.To {
		t.Errorf("scrollerPresent = %+v, want false -> true", c)
	}
	if out := renderUIDiff(appeared, uiSnapshotFile{}, uiSnapshotFile{}); !strings.Contains(out, "gained a scroller") {
		t.Errorf("the terminal form must say so:\n%s", out)
	}

	lost := diffViewports(withScroll, without, uiGeometryThresholdPx)
	if len(lost.Panes) != 1 {
		t.Fatalf("a pane that lost its scroller must be reported: %+v", lost)
	}
	if c := lost.Panes[0].ScrollerPresent; c == nil || !c.From || c.To {
		t.Errorf("scrollerPresent = %+v, want true -> false", c)
	}
	if out := renderUIDiff(lost, uiSnapshotFile{}, uiSnapshotFile{}); !strings.Contains(out, "lost its scroller") {
		t.Errorf("the terminal form must say so:\n%s", out)
	}

	// Two panes that never had one are not news.
	if diff := diffViewports(without, without, uiGeometryThresholdPx); !diff.Empty() {
		t.Errorf("neither snapshot has a scroller; nothing changed: %+v", diff)
	}
}

func TestDiffViewportsPanesAddedAndRemoved(t *testing.T) {
	before := cannedViewport(cannedRow("a", 0, 10, true))
	after := cannedViewport(cannedRow("a", 0, 10, true))
	after.Panes = append(after.Panes, uiPane{PaneID: "pane-b", PaneKind: "thread", ThreadID: "thread-2"})
	diff := diffViewports(before, after, uiGeometryThresholdPx)
	if len(diff.PanesAdded) != 1 || diff.PanesAdded[0] != "pane-b" {
		t.Errorf("panesAdded = %v", diff.PanesAdded)
	}
	back := diffViewports(after, before, uiGeometryThresholdPx)
	if len(back.PanesRemoved) != 1 || back.PanesRemoved[0] != "pane-b" {
		t.Errorf("panesRemoved = %v", back.PanesRemoved)
	}
}

func TestRenderUIDiffCapsIDsButNotCounts(t *testing.T) {
	before := cannedViewport()
	rows := make([]uiRow, 0, 40)
	for i := 0; i < 40; i++ {
		rows = append(rows, cannedRow(fmt.Sprintf("row-%02d", i), i, float64(i*50), true))
	}
	after := cannedViewport(rows...)
	out := renderUIDiff(diffViewports(before, after, uiGeometryThresholdPx),
		uiSnapshotFile{TakenAt: "t0"}, uiSnapshotFile{TakenAt: "t1"})

	if !strings.Contains(out, "mounted (40)") {
		t.Errorf("the count must be exact:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("(+%d more)", 40-uiDiffIDCap)) {
		t.Errorf("the id list must be capped with a remainder:\n%s", out)
	}
	if strings.Contains(out, "row-39") {
		t.Errorf("ids past the cap must not print:\n%s", out)
	}
	if !strings.Contains(out, "ui diff  t0 -> t1") {
		t.Errorf("the header must name both snapshots:\n%s", out)
	}
}

func TestDecodeViewportRejectsAnotherVersion(t *testing.T) {
	// A future bridge that changed the shape must fail loudly: silently
	// decoding v2 into v1 fields is exactly the "nothing moved" lie.
	raw, err := json.Marshal(map[string]any{"v": 2, "panes": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeViewport(raw); err == nil {
		t.Fatal("decodeViewport accepted a v2 snapshot")
	}
	raw, _ = json.Marshal(cannedViewport(cannedRow("a", 0, 10, true)))
	view, err := decodeViewport(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Panes) != 1 || len(view.Panes[0].Rows) != 1 {
		t.Errorf("v1 round trip lost content: %+v", view)
	}
}
