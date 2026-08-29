package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// The rule for typing an RPC result in this CLI: a result this command
// COMPARES or AGGREGATES gets a full Go shape; a result it only passes
// through to the terminal stays json.RawMessage. The viewport snapshot is
// the compared one — `ui diff` reads two of them field by field, and a
// field name that silently decoded to its zero value would render a diff
// saying "nothing moved" about a page that moved, the failure the whole
// command exists to catch. The shapes mirror
// frontend/src/lib/harness/snapshot.ts; keep them in step. A rename on
// the TS side is caught by e2e/tests/harness-bridge.spec.ts, which runs
// this binary against a real page and asserts the decoded rows carry
// discriminating values rather than zeros.

type uiRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type uiRow struct {
	ItemID     string `json:"itemId"`
	Kind       string `json:"kind"`
	Role       string `json:"role"`
	Status     string `json:"status"`
	Streaming  bool   `json:"streaming"`
	Badge      string `json:"badge"`
	RowIndex   int    `json:"rowIndex"`
	InViewport bool   `json:"inViewport"`
	Rect       uiRect `json:"rect"`
	TextLength int    `json:"textLength"`
	TextHead   string `json:"textHead"`
}

type uiScroll struct {
	Top                float64 `json:"top"`
	Height             float64 `json:"height"`
	Client             float64 `json:"client"`
	DistanceFromBottom float64 `json:"distanceFromBottom"`
	AtBottom           bool    `json:"atBottom"`
}

type uiPane struct {
	PaneID      string    `json:"paneId"`
	PaneKind    string    `json:"paneKind"`
	Focused     bool      `json:"focused"`
	ThreadID    string    `json:"threadId"`
	Rect        uiRect    `json:"rect"`
	Scroll      *uiScroll `json:"scroll"`
	MountedRows int       `json:"mountedRows"`
	Rows        []uiRow   `json:"rows"`
}

type uiOverlay struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Rect uiRect `json:"rect"`
}

type uiViewport struct {
	V               int         `json:"v"`
	Settled         bool        `json:"settled"`
	SinceMutationMs float64     `json:"sinceMutationMs"`
	ActiveThreadID  string      `json:"activeThreadId"`
	DomNodes        int         `json:"domNodes"`
	Panes           []uiPane    `json:"panes"`
	Overlays        []uiOverlay `json:"overlays"`
}

// uiSnapshotFile is what `ui snapshot` leaves behind for `ui diff`: the
// snapshot plus when it was taken. Storing the raw bytes as well would
// double the file for nothing: the diff reads the decoded shape.
type uiSnapshotFile struct {
	TakenAt  string     `json:"takenAt"`
	Instance string     `json:"instance"`
	Viewport uiViewport `json:"viewport"`
}

// uiGeometryThresholdPx is how far a row may move before the diff reports
// it. Sub-pixel layout noise (a scrollbar, a font metric rounding) is not
// a change a reader wants to read about; two pixels is.
const uiGeometryThresholdPx = 2.0

// uiDiff is the machine-readable half of a comparison. Every list is
// deterministic in order, so `-o json` output diffs cleanly between runs.
type uiDiff struct {
	ActiveThreadChanged *uiChange[string] `json:"activeThreadChanged,omitempty"`
	SettledChanged      *uiChange[bool]   `json:"settledChanged,omitempty"`
	DomNodes            *uiChange[int]    `json:"domNodes,omitempty"`
	PanesAdded          []string          `json:"panesAdded,omitempty"`
	PanesRemoved        []string          `json:"panesRemoved,omitempty"`
	Panes               []uiPaneDiff      `json:"panes,omitempty"`
	OverlaysOpened      []string          `json:"overlaysOpened,omitempty"`
	OverlaysClosed      []string          `json:"overlaysClosed,omitempty"`
}

type uiChange[T comparable] struct {
	From T `json:"from"`
	To   T `json:"to"`
}

type uiPaneDiff struct {
	PaneID        string             `json:"paneId"`
	ThreadChanged *uiChange[string]  `json:"threadChanged,omitempty"`
	MountedRows   *uiChange[int]     `json:"mountedRows,omitempty"`
	ScrollTop     *uiChange[float64] `json:"scrollTop,omitempty"`
	ScrollHeight  *uiChange[float64] `json:"scrollHeight,omitempty"`
	AtBottom      *uiChange[bool]    `json:"atBottom,omitempty"`
	// ScrollerPresent is the pane GAINING or LOSING its scroller, which is
	// a different event from any number moving inside it: the bridge
	// reports `scroll: null` for a pane with no scroll container, so a
	// timeline that unmounted (or one that finally mounted) shows up here
	// and nowhere else.
	ScrollerPresent *uiChange[bool] `json:"scrollerPresent,omitempty"`
	RowsMounted     []string        `json:"rowsMounted,omitempty"`
	RowsUnmounted   []string        `json:"rowsUnmounted,omitempty"`
	EnteredView     []string        `json:"enteredViewport,omitempty"`
	LeftView        []string        `json:"leftViewport,omitempty"`
	RowsMoved       []uiRowMove     `json:"rowsMoved,omitempty"`
	StatusChanged   []uiRowStatus   `json:"statusChanged,omitempty"`
}

type uiRowMove struct {
	ItemID string  `json:"itemId"`
	DX     float64 `json:"dx"`
	DY     float64 `json:"dy"`
	DW     float64 `json:"dw"`
	DH     float64 `json:"dh"`
}

type uiRowStatus struct {
	ItemID string `json:"itemId"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// Empty reports whether anything at all changed. A diff that found nothing
// is a real answer ("the page is where you left it"), not an absence.
func (d uiDiff) Empty() bool {
	return d.ActiveThreadChanged == nil && d.SettledChanged == nil && d.DomNodes == nil &&
		len(d.PanesAdded) == 0 && len(d.PanesRemoved) == 0 && len(d.Panes) == 0 &&
		len(d.OverlaysOpened) == 0 && len(d.OverlaysClosed) == 0
}

// diffViewports compares two snapshots. Panes and rows are matched by id,
// never by position: a pane that moved in the array is the same pane, and
// a row that shifted by one index is the case a positional diff would
// report as "everything changed".
func diffViewports(before, after uiViewport, thresholdPx float64) uiDiff {
	var diff uiDiff
	if before.ActiveThreadID != after.ActiveThreadID {
		diff.ActiveThreadChanged = &uiChange[string]{before.ActiveThreadID, after.ActiveThreadID}
	}
	if before.Settled != after.Settled {
		diff.SettledChanged = &uiChange[bool]{before.Settled, after.Settled}
	}
	if before.DomNodes != after.DomNodes {
		diff.DomNodes = &uiChange[int]{before.DomNodes, after.DomNodes}
	}

	beforePanes := indexPanes(before.Panes)
	afterPanes := indexPanes(after.Panes)
	for _, id := range sortedPaneIDs(afterPanes) {
		if _, ok := beforePanes[id]; !ok {
			diff.PanesAdded = append(diff.PanesAdded, id)
		}
	}
	for _, id := range sortedPaneIDs(beforePanes) {
		afterPane, ok := afterPanes[id]
		if !ok {
			diff.PanesRemoved = append(diff.PanesRemoved, id)
			continue
		}
		if paneDiff, changed := diffPane(beforePanes[id], afterPane, thresholdPx); changed {
			diff.Panes = append(diff.Panes, paneDiff)
		}
	}

	beforeOverlays := overlayNames(before.Overlays)
	afterOverlays := overlayNames(after.Overlays)
	diff.OverlaysOpened = missingFrom(afterOverlays, beforeOverlays)
	diff.OverlaysClosed = missingFrom(beforeOverlays, afterOverlays)
	return diff
}

func diffPane(before, after uiPane, thresholdPx float64) (uiPaneDiff, bool) {
	out := uiPaneDiff{PaneID: after.PaneID}
	if before.ThreadID != after.ThreadID {
		out.ThreadChanged = &uiChange[string]{before.ThreadID, after.ThreadID}
	}
	if before.MountedRows != after.MountedRows {
		out.MountedRows = &uiChange[int]{before.MountedRows, after.MountedRows}
	}
	if (before.Scroll == nil) != (after.Scroll == nil) {
		out.ScrollerPresent = &uiChange[bool]{before.Scroll != nil, after.Scroll != nil}
	}
	if before.Scroll != nil && after.Scroll != nil {
		if math.Abs(before.Scroll.Top-after.Scroll.Top) >= thresholdPx {
			out.ScrollTop = &uiChange[float64]{before.Scroll.Top, after.Scroll.Top}
		}
		if math.Abs(before.Scroll.Height-after.Scroll.Height) >= thresholdPx {
			out.ScrollHeight = &uiChange[float64]{before.Scroll.Height, after.Scroll.Height}
		}
		if before.Scroll.AtBottom != after.Scroll.AtBottom {
			out.AtBottom = &uiChange[bool]{before.Scroll.AtBottom, after.Scroll.AtBottom}
		}
	}

	beforeRows := indexRows(before.Rows)
	afterRows := indexRows(after.Rows)
	for _, id := range sortedRowIDs(after.Rows) {
		beforeRow, existed := beforeRows[id]
		afterRow := afterRows[id]
		if !existed {
			out.RowsMounted = append(out.RowsMounted, id)
			continue
		}
		switch {
		case !beforeRow.InViewport && afterRow.InViewport:
			out.EnteredView = append(out.EnteredView, id)
		case beforeRow.InViewport && !afterRow.InViewport:
			out.LeftView = append(out.LeftView, id)
		}
		if beforeRow.Status != afterRow.Status || beforeRow.Streaming != afterRow.Streaming {
			out.StatusChanged = append(out.StatusChanged, uiRowStatus{
				ItemID: id,
				From:   rowStateLabel(beforeRow),
				To:     rowStateLabel(afterRow),
			})
		}
		if move, moved := rowMove(id, beforeRow.Rect, afterRow.Rect, thresholdPx); moved {
			out.RowsMoved = append(out.RowsMoved, move)
		}
	}
	for _, id := range sortedRowIDs(before.Rows) {
		if _, ok := afterRows[id]; !ok {
			out.RowsUnmounted = append(out.RowsUnmounted, id)
		}
	}

	changed := out.ThreadChanged != nil || out.MountedRows != nil || out.ScrollTop != nil ||
		out.ScrollHeight != nil || out.AtBottom != nil || out.ScrollerPresent != nil ||
		len(out.RowsMounted) > 0 ||
		len(out.RowsUnmounted) > 0 || len(out.EnteredView) > 0 || len(out.LeftView) > 0 ||
		len(out.RowsMoved) > 0 || len(out.StatusChanged) > 0
	return out, changed
}

func rowMove(id string, before, after uiRect, thresholdPx float64) (uiRowMove, bool) {
	move := uiRowMove{
		ItemID: id,
		DX:     round1(after.X - before.X),
		DY:     round1(after.Y - before.Y),
		DW:     round1(after.W - before.W),
		DH:     round1(after.H - before.H),
	}
	worst := math.Max(math.Max(math.Abs(move.DX), math.Abs(move.DY)),
		math.Max(math.Abs(move.DW), math.Abs(move.DH)))
	return move, worst >= thresholdPx
}

func rowStateLabel(row uiRow) string {
	if row.Streaming {
		return row.Status + "+streaming"
	}
	return row.Status
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func indexPanes(panes []uiPane) map[string]uiPane {
	out := make(map[string]uiPane, len(panes))
	for _, pane := range panes {
		out[pane.PaneID] = pane
	}
	return out
}

func indexRows(rows []uiRow) map[string]uiRow {
	out := make(map[string]uiRow, len(rows))
	for _, row := range rows {
		out[row.ItemID] = row
	}
	return out
}

func sortedPaneIDs(panes map[string]uiPane) []string {
	ids := make([]string, 0, len(panes))
	for id := range panes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// sortedRowIDs keeps DOCUMENT order rather than sorting: a reader scanning
// "which rows entered the viewport" wants them top to bottom, and rowIndex
// is the timeline's own order. Duplicate ids cannot happen in one pane.
func sortedRowIDs(rows []uiRow) []string {
	ordered := append([]uiRow(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].RowIndex < ordered[j].RowIndex })
	ids := make([]string, 0, len(ordered))
	for _, row := range ordered {
		ids = append(ids, row.ItemID)
	}
	return ids
}

func overlayNames(overlays []uiOverlay) []string {
	names := make([]string, 0, len(overlays))
	for _, overlay := range overlays {
		names = append(names, overlay.Kind+":"+overlay.Name)
	}
	sort.Strings(names)
	return names
}

func missingFrom(candidates, present []string) []string {
	index := make(map[string]bool, len(present))
	for _, name := range present {
		index[name] = true
	}
	var out []string
	for _, name := range candidates {
		if !index[name] {
			out = append(out, name)
		}
	}
	return out
}

// renderUIDiff writes the terminal form: one indented section per pane,
// counts before ids, and ids capped so a 200-row remount does not scroll
// the interesting lines away.
func renderUIDiff(diff uiDiff, before, after uiSnapshotFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ui diff  %s -> %s\n", before.TakenAt, after.TakenAt)
	if diff.Empty() {
		b.WriteString("  no change\n")
		return b.String()
	}
	if c := diff.ActiveThreadChanged; c != nil {
		fmt.Fprintf(&b, "  active thread  %s -> %s\n", orDash(c.From), orDash(c.To))
	}
	if c := diff.SettledChanged; c != nil {
		fmt.Fprintf(&b, "  settled        %t -> %t\n", c.From, c.To)
	}
	if c := diff.DomNodes; c != nil {
		fmt.Fprintf(&b, "  dom nodes      %d -> %d (%+d)\n", c.From, c.To, c.To-c.From)
	}
	writeIDLine(&b, "  panes added", diff.PanesAdded)
	writeIDLine(&b, "  panes removed", diff.PanesRemoved)
	writeIDLine(&b, "  overlays opened", diff.OverlaysOpened)
	writeIDLine(&b, "  overlays closed", diff.OverlaysClosed)
	for _, pane := range diff.Panes {
		fmt.Fprintf(&b, "  pane %s\n", pane.PaneID)
		if c := pane.ThreadChanged; c != nil {
			fmt.Fprintf(&b, "    thread       %s -> %s\n", orDash(c.From), orDash(c.To))
		}
		if c := pane.MountedRows; c != nil {
			fmt.Fprintf(&b, "    mounted rows %d -> %d (%+d)\n", c.From, c.To, c.To-c.From)
		}
		if c := pane.ScrollTop; c != nil {
			fmt.Fprintf(&b, "    scrollTop    %.0f -> %.0f (%+.0f)\n", c.From, c.To, c.To-c.From)
		}
		if c := pane.ScrollHeight; c != nil {
			fmt.Fprintf(&b, "    scrollHeight %.0f -> %.0f (%+.0f)\n", c.From, c.To, c.To-c.From)
		}
		if c := pane.AtBottom; c != nil {
			fmt.Fprintf(&b, "    atBottom     %t -> %t\n", c.From, c.To)
		}
		if c := pane.ScrollerPresent; c != nil {
			state := "lost its scroller"
			if c.To {
				state = "gained a scroller"
			}
			fmt.Fprintf(&b, "    scroller     %s\n", state)
		}
		writeIDLine(&b, "    mounted", pane.RowsMounted)
		writeIDLine(&b, "    unmounted", pane.RowsUnmounted)
		writeIDLine(&b, "    entered view", pane.EnteredView)
		writeIDLine(&b, "    left view", pane.LeftView)
		for _, status := range pane.StatusChanged {
			fmt.Fprintf(&b, "    status %s: %s -> %s\n", status.ItemID, status.From, status.To)
		}
		for _, move := range pane.RowsMoved {
			fmt.Fprintf(&b, "    moved  %s: dx %+.1f dy %+.1f dw %+.1f dh %+.1f\n",
				move.ItemID, move.DX, move.DY, move.DW, move.DH)
		}
	}
	return b.String()
}

// uiDiffIDCap bounds one list. The count is always exact; only the names
// are trimmed, because "312 rows unmounted" is the finding and the first
// eight ids are enough to recognise which rows they were.
const uiDiffIDCap = 8

func writeIDLine(b *strings.Builder, label string, ids []string) {
	if len(ids) == 0 {
		return
	}
	shown := ids
	suffix := ""
	if len(shown) > uiDiffIDCap {
		shown = shown[:uiDiffIDCap]
		suffix = fmt.Sprintf(" (+%d more)", len(ids)-uiDiffIDCap)
	}
	fmt.Fprintf(b, "%s (%d): %s%s\n", label, len(ids), strings.Join(shown, ", "), suffix)
}

func decodeViewport(raw json.RawMessage) (uiViewport, error) {
	var out uiViewport
	if err := json.Unmarshal(raw, &out); err != nil {
		return uiViewport{}, fmt.Errorf("decode viewport snapshot: %w", err)
	}
	if out.V != 1 {
		return uiViewport{}, fmt.Errorf("viewport snapshot is version %d; this ao-harness reads v1", out.V)
	}
	return out, nil
}
