package app

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/store"
)

// --- deterministic heavy-thread fixture -------------------------------
//
// Shaped from the measurement in docs/specs/remote-access.md §14 — a
// 200-row cold window on a real 65,877-item thread — so the numbers this
// file asserts mean something against the numbers that motivated the
// projection:
//
//	200 rows, 109 of them tool_call
//	item meta        63 KB, 59 KB of it meta.input
//	payload metadata 100 KB
//	summary          48 KB
//	preview spans    17 KB
//	total            330 KB raw
//
// The `input` distribution is the skew §14 describes rather than a flat
// average: a dozen multi-KB argument objects (its 4.2 KB Bash example)
// against a long tail of short ones, which is what makes a size rule the
// right instrument.

// wordBank plus a stream of unique hex tokens gives the fixture text
// realistic entropy. This matters because the compressed ceilings below
// are the ones §14 states its budget in: word-salad from a small bank
// deflates ~24:1, real tool arguments and diffs deflate ~5.6:1 (the
// measured 330 KB window arrived as 59 KB), and a fixture that
// compresses four times too well turns every compressed ceiling into a
// number the projection clears without doing anything.
var wordBank = strings.Fields(`
func handler request context store thread item payload meta summary
window projection budget elision marker recovery route client server
return error nil string bytes encode decode append range switch case
value index cursor status kind role parent child launch result assert
`)

// fixtureText builds n bytes of deterministic text mixing bank words
// with unique hex tokens, in the proportion that lands the seeded window
// on the ~5.6:1 deflate ratio measured on the real thread. A plain LCG
// keyed by seed makes (seed, n) reproduce byte for byte.
func fixtureText(seed, n int) string {
	var b strings.Builder
	b.Grow(n + 16)
	state := uint32(seed)*2654435761 + 1
	for b.Len() < n {
		state = state*1664525 + 1013904223
		if state>>28 < 1 {
			b.WriteString(strconv.FormatUint(uint64(state), 16))
		} else {
			b.WriteString(wordBank[(state>>8)%uint32(len(wordBank))])
		}
		b.WriteByte(' ')
	}
	return b.String()[:n]
}

// heavyThreadRow describes one seeded row's variable-length content.
type heavyThreadRow struct {
	kind       string
	inputBytes int
	patchBytes int
	spanBytes  int
}

// heavyThreadShape returns the 200-row window described above.
func heavyThreadShape() []heavyThreadRow {
	rows := make([]heavyThreadRow, 0, 200)
	toolCallsLeft, resultsLeft := 109, 45
	bigInputs := 12
	for i := range 200 {
		switch {
		case toolCallsLeft > 0 && i%2 == 0:
			toolCallsLeft--
			// 12 argument objects at ~4.2 KB and 97 at ~90 B sum to the
			// measured 59 KB of meta.input across 109 tool_call rows.
			size := 90
			if bigInputs > 0 {
				size, bigInputs = 4200, bigInputs-1
			}
			rows = append(rows, heavyThreadRow{kind: "tool_call", inputBytes: size})
		case resultsLeft > 0:
			resultsLeft--
			rows = append(rows, heavyThreadRow{kind: "tool_completion", patchBytes: 755, spanBytes: 380})
		default:
			rows = append(rows, heavyThreadRow{kind: "assistant_text"})
		}
	}
	return rows
}

func seedHeavyThread(t *testing.T, app *App, rows []heavyThreadRow) store.Thread {
	t.Helper()
	thread, err := createTestThread(t, app, "claude", "/tmp/w-heavy", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	for i, row := range rows {
		item := store.Item{
			ID:        fmt.Sprintf("item-%04d", i),
			ThreadID:  thread.ID,
			TurnIndex: i / 4,
			ItemIndex: i,
			Kind:      row.kind,
			Role:      "assistant",
			Status:    "completed",
			// 240 B/row is the measured 48 KB of summary over 200 rows.
			Summary:   fixtureText(i, 240),
			CreatedAt: int64(i) * 1000,
			UpdatedAt: int64(i) * 1000,
		}
		switch row.kind {
		case "tool_call":
			item.ToolName = "Write"
			item.Meta = mustFixtureJSON(t, map[string]any{
				"toolName": "Write",
				"input": map[string]any{
					"file_path": fmt.Sprintf("src/lib/mod%03d.ts", i),
					"content":   fixtureText(i, row.inputBytes),
				},
			})
		case "tool_completion":
			item.PayloadID = fmt.Sprintf("payload-%04d", i)
			item.PayloadKind = "diff"
			item.PayloadMeta = mustFixtureJSON(t, map[string]any{
				"itemType": "file_change",
				"title":    fmt.Sprintf("Edited src/lib/mod%03d.ts", i),
				// The measured window's payload metadata is ~2x its
				// preview patches; this is the rest of it.
				"preview": fixtureText(i+1, 1400),
				"inlineDiff": map[string]any{
					"availability": "exact_patch",
					"files": []any{map[string]any{
						"path":             fmt.Sprintf("src/lib/mod%03d.ts", i),
						"kind":             "modified",
						"insertions":       12,
						"deletions":        4,
						"previewPatch":     fixtureText(i+2, row.patchBytes),
						"previewLineCount": 30,
						"previewTruncated": true,
					}},
					"insertions": 12,
					"deletions":  4,
				},
			})
			item.PayloadPreviewSpans = mustFixtureJSON(t, map[string]any{
				"version": 1,
				"files":   []any{map[string]any{"path": "src/lib/mod.ts", "pad": fixtureText(i+3, row.spanBytes)}},
			})
		}
		if item.PayloadID == "" {
			if err := app.store.InsertItem(item); err != nil {
				t.Fatalf("insert item %d: %v", i, err)
			}
			continue
		}
		// preview_spans is a payload column joined onto the item read,
		// so it has to be written where it lives rather than set on the
		// row struct.
		previewSpans := item.PayloadPreviewSpans
		item.PayloadPreviewSpans = ""
		if err := app.store.InsertItemWithPayload(item, store.Payload{
			ID:        item.PayloadID,
			Kind:      item.PayloadKind,
			Meta:      item.PayloadMeta,
			Data:      []byte(fixtureText(i+4, 2048)),
			CreatedAt: item.CreatedAt,
		}); err != nil {
			t.Fatalf("insert item %d with payload: %v", i, err)
		}
		if err := app.store.UpdatePayloadSpans(thread.ID, item.PayloadID, previewSpans, previewSpans); err != nil {
			t.Fatalf("write preview spans %d: %v", i, err)
		}
	}
	return thread
}

func mustFixtureJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(encoded)
}

// --- the projection preference ---------------------------------------

func TestItemWindow_ProjectionPreferenceRidesEachRequest(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	on, err := app.ListThreadSliceAround(thread.ID, "", 200, true)
	if err != nil {
		t.Fatalf("ListThreadSliceAround(previews on): %v", err)
	}
	off, err := app.ListThreadSliceAround(thread.ID, "", 200, false)
	if err != nil {
		t.Fatalf("ListThreadSliceAround(previews off): %v", err)
	}

	if countPreviewPatches(on.Items) == 0 {
		t.Fatal("a client that asked for inline previews received none")
	}
	if got := countPreviewPatches(off.Items); got != 0 {
		t.Errorf("%d preview patches rode to a client that renders none of them", got)
	}
	if got := countPreviewElided(off.Items); got == 0 {
		t.Error("previews were dropped without the marker that makes them recoverable")
	}
	if got := countPreviewElided(on.Items); got != 0 {
		t.Errorf("%d rows were marked elided for a client that got its previews", got)
	}
	for _, item := range off.Items {
		if item.PayloadMeta != "" && item.PayloadPreviewSpans != "" {
			t.Errorf("item %s kept preview spans with no patch left to highlight", item.ID)
			break
		}
	}
	// Two clients, two answers, from one backend that never read a
	// setting of its own.
	if len(on.Items) != len(off.Items) {
		t.Errorf("row counts diverged: %d vs %d — the preference must change fields, not history",
			len(on.Items), len(off.Items))
	}
}

func TestItemWindow_EveryPathProjectsTheSameWay(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	slice, err := app.ListThreadSliceAround(thread.ID, "", 200, false)
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	synced, err := app.SyncThreadWindow(thread.ID, SyncThreadWindowRequest{
		ItemBudget: 200, HaveEpoch: -1, HaveRev: -1,
	})
	if err != nil {
		t.Fatalf("SyncThreadWindow: %v", err)
	}
	if synced.Page == nil {
		t.Fatal("SyncThreadWindow answered without a page")
	}
	// A cold open reaches one of these and a gap refresh reaches the
	// other. If they shaped a row differently, one window would end up
	// holding both shapes.
	if len(synced.Page.Items) != len(slice.Items) {
		t.Fatalf("page sizes differ: sync %d, slice %d", len(synced.Page.Items), len(slice.Items))
	}
	for i := range slice.Items {
		if synced.Page.Items[i].PayloadMeta != slice.Items[i].PayloadMeta {
			t.Fatalf("row %s is shaped differently by the two window paths", slice.Items[i].ID)
		}
		if synced.Page.Items[i].Meta != slice.Items[i].Meta {
			t.Fatalf("row %s has a different meta on the two window paths", slice.Items[i].ID)
		}
	}

	// The pagers, the single-row read and the unwindowed list are the
	// same surface and must not leak an unprojected row.
	older, err := app.ListItemsBeforeCursor(thread.ID, slice.NewestCursor, 50, false)
	if err != nil {
		t.Fatalf("ListItemsBeforeCursor: %v", err)
	}
	if got := countPreviewPatches(older.Items); got != 0 {
		t.Errorf("the older pager shipped %d unprojected previews into the same window", got)
	}
	all, err := app.ListItems(thread.ID, false)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if got := countPreviewPatches(all); got != 0 {
		t.Errorf("ListItems shipped %d unprojected previews", got)
	}
}

func countPreviewPatches(items []store.Item) int {
	count := 0
	for _, item := range items {
		count += strings.Count(item.PayloadMeta, `"previewPatch"`)
	}
	return count
}

func countPreviewElided(items []store.Item) int {
	count := 0
	for _, item := range items {
		count += strings.Count(item.PayloadMeta, `"previewElided"`)
	}
	return count
}

// --- the byte backstop ------------------------------------------------

func TestAdmittedRange_StopsAtTheByteBudgetKeepingTheAnchoredEnd(t *testing.T) {
	// Rows just over a tenth of the budget each: the eleventh cannot fit.
	rowBytes := itemWindowMaxBytes/10 + 1
	items := make([]store.Item, 20)
	for i := range items {
		items[i] = store.Item{ID: fmt.Sprintf("item-%02d", i), Summary: strings.Repeat("x", rowBytes)}
	}

	from, to := admittedRange(items, keepNewest)
	if to != len(items) {
		t.Errorf("keepNewest dropped from the wrong end: range [%d,%d)", from, to)
	}
	if from == 0 {
		t.Fatal("the byte budget admitted every row; the fixture is not over budget")
	}
	if spent := spentBytes(items[from:to]); spent > itemWindowMaxBytes {
		t.Errorf("admitted %d bytes over the %d budget", spent, itemWindowMaxBytes)
	}

	from, to = admittedRange(items, keepOldest)
	if from != 0 {
		t.Errorf("keepOldest dropped from the wrong end: range [%d,%d)", from, to)
	}
	if to == len(items) {
		t.Error("keepOldest admitted every row")
	}
}

func TestAdmittedRange_AlwaysAdmitsOneOversizedRow(t *testing.T) {
	huge := store.Item{ID: "huge", Summary: strings.Repeat("x", itemWindowMaxBytes*3)}
	for _, keep := range []windowEnd{keepNewest, keepOldest} {
		from, to := admittedRange([]store.Item{huge}, keep)
		if to-from != 1 {
			t.Fatalf("keep=%v refused the only row on the page; pagination would stall on it forever", keep)
		}
	}
	// And it is still exactly one: the oversized row does not license
	// the rest of the page.
	items := []store.Item{huge, huge, huge}
	if from, to := admittedRange(items, keepNewest); to-from != 1 {
		t.Errorf("admitted %d oversized rows, want 1", to-from)
	}
}

func TestAdmitByBytes_ReportsWhatItDropped(t *testing.T) {
	rowBytes := itemWindowMaxBytes/4 + 1
	items := make([]store.Item, 8)
	for i := range items {
		items[i] = store.Item{
			ID: fmt.Sprintf("item-%02d", i), TurnIndex: i, ItemIndex: i,
			Summary: strings.Repeat("x", rowBytes),
		}
	}
	page := store.PagedItems{
		Items:        items,
		OldestCursor: store.TimelineCursor{TurnIndex: 0, ItemIndex: 0, ItemID: "item-00"},
		NewestCursor: store.TimelineCursor{TurnIndex: 7, ItemIndex: 7, ItemID: "item-07"},
	}
	trimmed := admitByBytes(page, keepNewest)
	if len(trimmed.Items) == len(items) {
		t.Fatal("fixture is not over budget")
	}
	if !trimmed.HasMoreOlder || !trimmed.HasMore {
		t.Error("rows were dropped without telling the client there is more history below")
	}
	if trimmed.OldestCursor.ItemID != trimmed.Items[0].ID {
		t.Errorf("OldestCursor = %q, want the surviving oldest row %q",
			trimmed.OldestCursor.ItemID, trimmed.Items[0].ID)
	}
	if trimmed.NewestCursor.ItemID != "item-07" {
		t.Errorf("NewestCursor moved: %q", trimmed.NewestCursor.ItemID)
	}
}

func spentBytes(items []store.Item) int {
	total := 0
	for _, item := range items {
		total += itemwire.EncodedBytes(item)
	}
	return total
}

// --- the recovery route -----------------------------------------------

func TestGetThreadItemProjectionSource_ReturnsWhatTheProjectionRemoved(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, heavyThreadShape())

	page, err := app.ListThreadSliceAround(thread.ID, "", 200, false)
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}

	var elidedPreview, elidedInput store.Item
	for _, item := range page.Items {
		if strings.Contains(item.PayloadMeta, `"previewElided"`) && elidedPreview.ID == "" {
			elidedPreview = item
		}
		if strings.Contains(item.Meta, itemwire.MarkerKey) && elidedInput.ID == "" {
			elidedInput = item
		}
	}
	if elidedPreview.ID == "" || elidedInput.ID == "" {
		t.Fatal("fixture produced no elided rows to recover")
	}

	for _, item := range []store.Item{elidedPreview, elidedInput} {
		source, err := app.GetThreadItemProjectionSource(thread.ID, item.ID)
		if err != nil {
			t.Fatalf("GetThreadItemProjectionSource(%s): %v", item.ID, err)
		}
		if source.ItemID != item.ID {
			t.Fatalf("route answered for %q, asked for %q", source.ItemID, item.ID)
		}
		stored, found, err := app.store.GetThreadItem(thread.ID, item.ID)
		if err != nil || !found {
			t.Fatalf("stored row %s missing: %v", item.ID, err)
		}
		if source.Meta != stored.Meta || source.PayloadMeta != stored.PayloadMeta ||
			source.PayloadPreviewSpans != stored.PayloadPreviewSpans {
			t.Errorf("row %s: the route did not return the complete stored fields", item.ID)
		}
	}

	// The route recovers the exact values the projection removed.
	source, err := app.GetThreadItemProjectionSource(thread.ID, elidedPreview.ID)
	if err != nil {
		t.Fatalf("GetThreadItemProjectionSource: %v", err)
	}
	if !strings.Contains(source.PayloadMeta, `"previewPatch"`) {
		t.Error("recovered payloadMeta carries no patch text")
	}
	if source.PayloadPreviewSpans == "" {
		t.Error("recovered preview spans are empty")
	}
	if strings.Contains(source.PayloadMeta, `"previewElided"`) {
		t.Error("the recovery route returned a projected value; it must be the stored one")
	}

	missing, err := app.GetThreadItemProjectionSource(thread.ID, "no-such-item")
	if err != nil {
		t.Fatalf("GetThreadItemProjectionSource(missing): %v", err)
	}
	if missing.ItemID != "" {
		t.Errorf("missing item answered %#v, want the zero value", missing)
	}
}
