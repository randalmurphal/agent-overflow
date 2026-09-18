package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// The deterministic heavy thread every history-window test seeds: the
// projection tests, the wire-budget ceilings and the activity-run pages.
//
// --- shape ------------------------------------------------------------
//
// Shaped from the measurement in docs/specs/remote-access.md §14 — a
// 200-row cold window on a real 65,877-item thread — so the ceilings
// asserted against it mean something beside the numbers that motivated
// the projection:
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
// summaryBytes 0 takes the 240 B/row the measured window averages.
type heavyThreadRow struct {
	kind         string
	summaryBytes int
	inputBytes   int
	previewBytes int
	patchBytes   int
	spanBytes    int
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
			Summary:   fixtureText(i, cmp.Or(row.summaryBytes, 240)),
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
				"preview": fixtureText(i+1, cmp.Or(row.previewBytes, 1400)),
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
