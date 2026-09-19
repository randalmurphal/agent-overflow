package store

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
)

// The narrow scan behind run classification and stub aggregation
// (docs/architecture/timeline-window-pages.md §2.1 step 3).
//
// A page that ships 30 members of a 5,000-member run still has to know
// what the other 4,970 add up to, so the scan must be able to walk a
// whole run cheaply. It reads ten columns and no row body: no
// `summary`, no full `meta`, no payload data. Ids and coordinates come
// from timelineArms (ordered, limited, one index walk per physical arm);
// the columns that need the payload join come from a narrow sibling of
// queryHydratedTimelineItems with the same two arms and the same
// local-overlay-wins rule for imported rows.

// maxActivityRunScanRows caps one run expansion. A run longer than this
// is an error rather than a truncated stub: a stub that under-counts
// would put a wrong number in the header and, worse, would leave rows in
// the page's range that no stub counts, which is the invariant every
// held-window and retention decision rests on.
const maxActivityRunScanRows = 100000

// activityScanChunkRows is how many rows one scan statement reads. Large
// enough that a page of prose costs one statement per side, small enough
// that walking to the edge of a 5,000-member run does not materialize it
// in one allocation. A run's rows are still held for the length of one
// page composition (the pairing rule needs the whole membership), which
// is why maxActivityRunScanRows bounds a run rather than a chunk.
const activityScanChunkRows = 512

// fileChangeToolNames are the tools whose one call renders as one row per
// file (frontend/src/lib/utils/fileChangeRows.ts). The scan reads the
// file-count expressions only for these, and the Go rule below gates on
// the same set, so a native tool never pays for a JSON walk.
var fileChangeToolNames = []string{
	"Edit", "MultiEdit", "Write", "NotebookEdit", "apply_patch", "file_change", "fileChange",
}

var fileChangeToolNameSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(fileChangeToolNames))
	for _, name := range fileChangeToolNames {
		set[name] = struct{}{}
	}
	return set
}()

// fileChangeToolPredicate is the SQL half of that gate. `trim` matches the
// client, which trims the tool name before looking it up.
var fileChangeToolPredicate = "trim(items.tool_name) IN ('" +
	strings.Join(fileChangeToolNames, "', '") + "')"

// activityScanRow is one visible top-level row, in the only shape run
// classification and stub aggregation need.
type activityScanRow struct {
	ID           string
	TurnIndex    int
	ItemIndex    int
	Kind         string
	ToolName     string
	Status       string
	CompletionOf string
	Rev          int64
	PayloadKind  string
	MCP          string
	// DisplayRows is the row's §4 display-row count, already resolved
	// from the three file-change sources.
	DisplayRows int
}

func (r activityScanRow) cursor() TimelineCursor {
	return TimelineCursor{TurnIndex: r.TurnIndex, ItemIndex: r.ItemIndex, ItemID: r.ID}
}

// jsonFieldExpr guards every json_extract in this projection. `meta` and
// `payloads.meta` are free-form TEXT columns that can be empty or absent,
// and json_extract over a non-JSON string is a hard SQLite error that
// would fail the whole scan rather than one row.
func jsonFieldExpr(column, path string) string {
	return "CASE WHEN json_valid(" + column + ")" +
		" THEN json_extract(" + column + ", '" + path + "') END"
}

// activityScanColumns is the narrow projection, with caller-supplied
// expressions for the two things an arm cannot write against the `items`
// alias: the payload fields (local payload, or imported payload under a
// local overlay) and the row revision.
//
// The three file-change expressions are read separately rather than
// COALESCEd in SQL because the client's precedence falls THROUGH a
// present-but-empty source: `inlineDiff.totalFiles` of 0 does not stop
// `inlineDiff.files` from counting. Folding them in SQL would answer 1
// where the client answers 4.
func activityScanColumns(payloadKind, payloadMeta, rev string) string {
	fileChange := func(expr string) string {
		return "CASE WHEN " + fileChangeToolPredicate + " THEN " + expr + " END"
	}
	return `items.id, items.turn_index, items.item_index,
    items.kind, items.tool_name, items.status, items.completion_of,
    ` + rev + `, ` + payloadKind + `,
    COALESCE(` + jsonFieldExpr("items.meta", "$.mcp") + `, ''),
    ` + fileChange(inlineDiffTotalFilesExpr(payloadMeta)) + `,
    ` + fileChange(inlineDiffFilesLengthExpr(payloadMeta)) + `,
    ` + fileChange(inputFilePathCountExpr("items.meta"))
}

// inlineDiffTotalFilesExpr is the client's `positiveInteger` test: a
// non-numeric `totalFiles` is not a count, and reading it as one would
// disagree with the client on the same row.
func inlineDiffTotalFilesExpr(payloadMeta string) string {
	return "CASE WHEN json_valid(" + payloadMeta + ")" +
		" AND json_type(" + payloadMeta + ", '$.inlineDiff.totalFiles') IN ('integer', 'real')" +
		" THEN json_extract(" + payloadMeta + ", '$.inlineDiff.totalFiles') END"
}

func inlineDiffFilesLengthExpr(payloadMeta string) string {
	return "CASE WHEN json_valid(" + payloadMeta + ")" +
		" AND json_type(" + payloadMeta + ", '$.inlineDiff.files') = 'array'" +
		" THEN json_array_length(" + payloadMeta + ", '$.inlineDiff.files') END"
}

// inputFilePathCountExpr counts `meta.input.files` the way the client
// does: array entries that are non-empty strings, nothing else.
func inputFilePathCountExpr(itemMeta string) string {
	return "CASE WHEN json_valid(" + itemMeta + ")" +
		" AND json_type(" + itemMeta + ", '$.input.files') = 'array'" +
		" THEN (SELECT COUNT(*) FROM json_each(" + itemMeta + ", '$.input.files') AS paths" +
		" WHERE paths.type = 'text' AND trim(paths.value) <> '') END"
}

var localActivityScanColumns = activityScanColumns(
	"COALESCE(payloads.kind, '')",
	"payloads.meta",
	"items.rev",
)

var importedActivityScanColumns = activityScanColumns(
	"COALESCE(local_payloads.kind, imported_payloads.kind, '')",
	"COALESCE(local_payloads.meta, imported_payloads.meta)",
	importedItemRevExpr,
)

// queryActivityScanRows resolves ids to scan rows through the physical
// branch that owns each one, in (turn_index, item_index) order. It is
// queryHydratedTimelineItems' narrow sibling and joins payloads exactly
// as that does, local overlay included, so payload kind and the
// file-change sources are the same values a hydrated row would carry.
//
// selectedSQL must return one `id` column.
func queryActivityScanRows(
	q sqlQueryer,
	threadID string,
	selectedSQL string,
	selectedArgs ...any,
) ([]activityScanRow, error) {
	args := append([]any{}, selectedArgs...)
	args = append(args, threadID, threadID)
	rows, err := q.Query(`
		WITH selected(id) AS MATERIALIZED (
			`+selectedSQL+`
		)
		SELECT `+localActivityScanColumns+`
		  FROM selected
		  CROSS JOIN items AS items
		    ON items.thread_id = ? AND items.id = selected.id
		  LEFT JOIN payloads AS payloads
		    ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		UNION ALL
		SELECT `+importedActivityScanColumns+`
		  FROM selected
		  CROSS JOIN thread_import_chunks AS refs
		  JOIN import_history_items AS items
		    ON items.chunk_id = refs.chunk_id AND items.id = selected.id
		  LEFT JOIN payloads AS local_payloads
		    ON local_payloads.thread_id = refs.thread_id AND local_payloads.id = items.payload_id
		  LEFT JOIN import_history_payloads AS imported_payloads
		    ON imported_payloads.chunk_id = items.chunk_id AND imported_payloads.id = items.payload_id
		  LEFT JOIN thread_import_item_overrides AS overrides
		    ON overrides.thread_id = refs.thread_id AND overrides.item_id = items.id
		 WHERE refs.thread_id = ? AND overrides.item_id IS NULL
		 ORDER BY 2, 3`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query activity scan rows for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := []activityScanRow{}
	for rows.Next() {
		row, err := scanActivityScanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan activity scan row for %s: %w", threadID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate activity scan rows for %s: %w", threadID, err)
	}
	return out, nil
}

func scanActivityScanRow(scanner interface{ Scan(...any) error }) (activityScanRow, error) {
	var row activityScanRow
	var totalFiles sql.NullFloat64
	var diffFiles, inputFiles sql.NullInt64
	if err := scanner.Scan(
		&row.ID, &row.TurnIndex, &row.ItemIndex,
		&row.Kind, &row.ToolName, &row.Status, &row.CompletionOf,
		&row.Rev, &row.PayloadKind, &row.MCP,
		&totalFiles, &diffFiles, &inputFiles,
	); err != nil {
		return activityScanRow{}, err
	}
	row.DisplayRows = displayRowCount(row.ToolName, totalFiles, diffFiles, inputFiles)
	return row, nil
}

// maxDisplayRows bounds one row's projected file count. A count is a
// number a header prints; a provider that reported an absurd one must not
// be able to overflow the sum a run reports.
const maxDisplayRows = 1 << 20

// displayRowCount is §4's display-row rule, in the client's precedence
// order: a positive `inlineDiff.totalFiles`, else a non-empty
// `inlineDiff.files`, else the non-empty string paths of
// `meta.input.files`, else 1. Anything that is not a file-change tool is
// one row whatever its meta says.
func displayRowCount(toolName string, totalFiles sql.NullFloat64, diffFiles, inputFiles sql.NullInt64) int {
	if _, ok := fileChangeToolNameSet[strings.TrimSpace(toolName)]; !ok {
		return 1
	}
	if totalFiles.Valid && totalFiles.Float64 >= 1 && !math.IsInf(totalFiles.Float64, 0) {
		return clampDisplayRows(int64(math.Floor(math.Min(totalFiles.Float64, maxDisplayRows))))
	}
	if diffFiles.Valid && diffFiles.Int64 > 0 {
		return clampDisplayRows(diffFiles.Int64)
	}
	if inputFiles.Valid && inputFiles.Int64 > 0 {
		return clampDisplayRows(inputFiles.Int64)
	}
	return 1
}

func clampDisplayRows(count int64) int {
	if count > maxDisplayRows {
		return maxDisplayRows
	}
	return int(count)
}

// activityScanWalk yields a thread's visible top-level rows outward from
// a coordinate, one chunk of statements at a time, so a page reads only
// as far as its units reach.
//
// It buffers unconsumed rows and supports lookahead, which the older-side
// classification needs: a notification's membership depends on the row
// BEFORE it, so walking older means holding a chain of undecided bells
// until a rail row decides them or a prose row refuses them.
type activityScanWalk struct {
	q        sqlQueryer
	threadID string
	// newer picks the direction: true walks (turn_index, item_index)
	// ascending, false descending.
	newer bool
	// from is the exclusive bound the next chunk reads past. It moves to
	// the last row of each chunk.
	from TimelineCursor
	// buf holds the rows read and not yet consumed, from index `at`.
	buf       []activityScanRow
	at        int
	exhausted bool
}

// newActivityScanWalk starts a walk strictly outside `from`. `from` may
// name a row that no longer exists or was never visible: only its
// coordinate is used.
func newActivityScanWalk(q sqlQueryer, threadID string, from TimelineCursor, newer bool) *activityScanWalk {
	return &activityScanWalk{q: q, threadID: threadID, newer: newer, from: from}
}

// peekAt returns the k-th unconsumed row in walk order without consuming
// anything. ok=false means the walk reached the end of history.
func (w *activityScanWalk) peekAt(k int) (activityScanRow, bool, error) {
	for len(w.buf)-w.at <= k {
		if w.exhausted {
			return activityScanRow{}, false, nil
		}
		if err := w.fill(); err != nil {
			return activityScanRow{}, false, err
		}
	}
	return w.buf[w.at+k], true, nil
}

// take consumes the next n rows in walk order. The caller must have
// peeked them.
func (w *activityScanWalk) take(n int) {
	w.at += n
	if w.at == len(w.buf) {
		w.buf, w.at = w.buf[:0], 0
	}
}

func (w *activityScanWalk) fill() error {
	order := "turn_index DESC, item_index DESC"
	comparison := `
		   AND (items.turn_index < ? OR (items.turn_index = ? AND items.item_index < ?))`
	if w.newer {
		order = "turn_index ASC, item_index ASC"
		comparison = `
		   AND (items.turn_index > ? OR (items.turn_index = ? AND items.item_index > ?))`
	}
	selectedSQL, selectedArgs := timelineIDSelection(w.threadID, timelineSelection{
		Where:     windowedTimelineFilter + comparison,
		WhereArgs: []any{w.from.TurnIndex, w.from.TurnIndex, w.from.ItemIndex},
		OrderBy:   order,
		Limit:     activityScanChunkRows,
	})
	chunk, err := queryActivityScanRows(w.q, w.threadID, selectedSQL, selectedArgs...)
	if err != nil {
		return err
	}
	if len(chunk) == 0 {
		w.exhausted = true
		return nil
	}
	if !w.newer {
		reverseActivityScanRows(chunk)
	}
	w.from = chunk[len(chunk)-1].cursor()
	if w.at > 0 {
		w.buf = append(w.buf[:0], w.buf[w.at:]...)
		w.at = 0
	}
	w.buf = append(w.buf, chunk...)
	if len(chunk) < activityScanChunkRows {
		w.exhausted = true
	}
	return nil
}

func reverseActivityScanRows(rows []activityScanRow) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}
