package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// subagentRow is a written row as the card rules read it.
type subagentRow struct {
	id, parentID, kind, toolName, summary string
	turn, index                           int
	// prompt reports a resume prompt (aggPromptSQL) and carrier the
	// carrier it names, or "".
	prompt  bool
	carrier string
	// root is an anchorable row's transcript root (transcriptRootFromMeta),
	// or "" when it names none or itself.
	root string
	// completionOf is the launch a completion sibling settles, or "".
	completionOf string
	// status and background are the row's columns, and inactive reports
	// a meta whose live_background_active is false or 0: what decides
	// whether the row is a running agent (running).
	status               string
	background, inactive bool
}

func subagentRowOf(item Item) subagentRow {
	row := subagentRow{
		id: item.ID, parentID: item.ParentID, kind: item.Kind, toolName: item.ToolName,
		summary: item.Summary, turn: item.TurnIndex, index: item.ItemIndex, completionOf: item.CompletionOf,
		status: item.Status, background: item.IsBackground,
	}
	row.setMeta(item.Meta)
	return row
}

// setMeta derives the row's prompt identity, transcript root and
// background liveness from its meta.
func (r *subagentRow) setMeta(meta string) {
	r.prompt, r.carrier = subagentPromptFromMeta(r.kind, r.parentID, meta)
	r.root = ""
	if r.anchorable() {
		if root := transcriptRootFromMeta(meta); root != r.id {
			r.root = root
		}
	}
	r.inactive = liveBackgroundInactive(meta)
}

// liveBackgroundInactive is COALESCE(json_extract(meta,
// '$.live_background_active'), 1) = 0 on a valid meta: a teardown or a
// completion sibling settled the background launch.
func liveBackgroundInactive(meta string) bool {
	if !strings.Contains(meta, metaKeyLiveBackgroundActive) {
		return false
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return false
	}
	switch v := decoded[metaKeyLiveBackgroundActive].(type) {
	case bool:
		return !v
	case float64:
		return v == 0
	}
	return false
}

// subagentPromptFromMeta is aggPromptSQL and aggPromptCarrierSQL in Go:
// a user_text row with a parent whose subagent_resume_prompt is true and
// whose resume_carrier_id is absent, null or a string.
func subagentPromptFromMeta(kind, parentID, meta string) (bool, string) {
	if kind != "user_text" || parentID == "" || !strings.Contains(meta, metaKeySubagentResumePrompt) {
		return false, ""
	}
	var decoded map[string]json.RawMessage
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return false, ""
	}
	if string(decoded[metaKeySubagentResumePrompt]) != "true" {
		return false, ""
	}
	raw, named := decoded[metaKeyResumeCarrierID]
	if !named || string(raw) == "null" {
		return true, ""
	}
	var carrier string
	if json.Unmarshal(raw, &carrier) != nil {
		return false, ""
	}
	return true, strings.Trim(carrier, subagentPreviewBlank)
}

// subagentRowColumns projects a stored row as subagentRowOf reads an
// Item, for scanSubagentRow. The meta is parsed only for a row that may
// be a resume prompt, a user_text row with a parent, or a carrier, an
// anchorable row whose meta names a transcript root, and for a meta that
// names live_background_active. `a` is the row reference with its
// trailing dot.
func subagentRowColumns(a string) string {
	return a + "id, " + a + "parent_id, " + a + "kind, " + a + "tool_name, " + a + "summary, " +
		a + "turn_index, " + a + "item_index, COALESCE(" + aggPromptSQL(a) + ", 0), " +
		"CASE WHEN " + aggPromptSQL(a) + " THEN " + aggPromptCarrierSQL(a) + " ELSE '' END, " +
		"CASE WHEN " + aggAnchorableSQL(a) + " AND instr(" + a + "meta, '" + metaKeyTranscriptRootID + "') THEN COALESCE(" +
		aggTranscriptRootSQL(a) + ", '') ELSE '' END, " + a + "completion_of, " + a + "status, " + a + "is_background, " +
		"CASE WHEN instr(" + a + "meta, '" + metaKeyLiveBackgroundActive + "') AND json_valid(" + a + "meta) THEN COALESCE(json_extract(" +
		a + "meta, '$." + metaKeyLiveBackgroundActive + "'), 1) = 0 ELSE 0 END"
}

// scanSubagentRow scans subagentRowColumns, then dest.
func scanSubagentRow(sc interface{ Scan(...any) error }, dest ...any) (subagentRow, error) {
	var r subagentRow
	err := sc.Scan(append([]any{&r.id, &r.parentID, &r.kind, &r.toolName, &r.summary,
		&r.turn, &r.index, &r.prompt, &r.carrier, &r.root, &r.completionOf, &r.status, &r.background, &r.inactive}, dest...)...)
	if r.root == r.id {
		r.root = ""
	}
	return r, err
}

// subagentRowSQL reads one local row for scanSubagentRow.
var subagentRowSQL = `SELECT ` + subagentRowColumns("") + ` FROM items WHERE thread_id = ? AND id = ?`

// readMutableSubagentRowTx is requireMutableItemTx that returns the row as
// the card rules read it.
func readMutableSubagentRowTx(tx *sql.Tx, threadID, itemID, label string) (subagentRow, error) {
	row, err := scanSubagentRow(tx.QueryRow(subagentRowSQL, threadID, itemID))
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return subagentRow{}, fmt.Errorf("%s inspect local item %s/%s: %w", label, threadID, itemID, err)
		}
		return row, nil
	}
	if err := ownShownItemTx(tx, threadID, itemID, label); err != nil {
		return subagentRow{}, err
	}
	if row, err = scanSubagentRow(tx.QueryRow(subagentRowSQL, threadID, itemID)); err != nil {
		return subagentRow{}, fmt.Errorf("%s read copied item %s/%s: %w", label, threadID, itemID, err)
	}
	return row, nil
}

// visible is visibleItemsFilterFor.
func (r subagentRow) visible() bool {
	return !(r.kind == "notification" && r.toolName == "plan_update")
}

// counts reports a row some card may count: a visible row with a parent.
func (r subagentRow) counts() bool { return r.parentID != "" && r.visible() }

func (r subagentRow) anchorable() bool { return SubagentAnchorable(r.kind, r.toolName) }

// running is liveSubagentAgentSQL: a running tool call the boot pass
// recovers the cards of, in the foreground or a live background launch.
func (r subagentRow) running() bool {
	return r.anchorable() && r.status == "running" && (!r.background || !r.inactive)
}

func (r subagentRow) position() TimelineCursor {
	return TimelineCursor{TurnIndex: r.turn, ItemIndex: r.index}
}

// previewable is previewableSubagentRow.
func (r subagentRow) previewable() bool { return previewableSubagentRow(r.kind, r.summary) }

// toolable is aggToolableSQL.
func (r subagentRow) toolable() bool {
	return r.anchorable() && strings.Trim(r.summary, subagentPreviewBlank) != ""
}

// newerThan is "the row is after the stored position, or none is stored".
func (r subagentRow) newerThan(turn, item sql.NullInt64) bool {
	if !turn.Valid {
		return true
	}
	return int64(r.turn) > turn.Int64 || (int64(r.turn) == turn.Int64 && item.Valid && int64(r.index) > item.Int64)
}

// beats is the preview and tray rule against a stored row: newer, or at
// the same position with a smaller id.
func (r subagentRow) beats(turn, item sql.NullInt64, id sql.NullString) bool {
	if r.newerThan(turn, item) {
		return true
	}
	return turn.Int64 == int64(r.turn) && item.Valid && item.Int64 == int64(r.index) && id.Valid && r.id < id.String
}

func (r subagentRow) pick(v *subagentStampValues) {
	v.Summary = sql.NullString{String: r.summary, Valid: true}
	v.PickTurn, v.PickItem, v.PickID = validInt(r.turn), validInt(r.index), sql.NullString{String: r.id, Valid: true}
}

func (r subagentRow) tool(v *subagentStampValues) {
	v.ToolSummary = sql.NullString{String: strings.Trim(r.summary, subagentPreviewBlank), Valid: true}
	v.ToolTurn, v.ToolItem, v.ToolID = validInt(r.turn), validInt(r.index), sql.NullString{String: r.id, Valid: true}
}
