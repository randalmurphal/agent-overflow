package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Keyed stamp writes for claimed item writes.
//
// A writer that knows the anchor a row counts toward names it on the
// write (Item.SubagentAnchor, ItemPartialUpdate.SubagentAnchor). The
// store makes an anchorable imported parent local, as any insert under it
// does (shadowImportedParentTx), checks the name against the row's local
// parent chain, stores the row with subagentClaimRev so the item triggers
// leave its mark out, and then applies the write's effect on the stamps
// here, in the same transaction:
// one primary-key read per nesting level (the row, its stamp and its
// round's prompt) and one keyed write per stamp that changes. No CTE, no
// walk of a subtree. What the rules below cannot keep exact they write
// dirty, and the writer's settle recomputes it; a claimed write that
// changes a row's place in a walk is left to the triggers' marks.
//
// An insert, per stamp:
//   - each anchor on the chain above the row, by the round the row lands
//     in: its own first round (`round`: count, preview, newest), a
//     carrier's round (`transcript`: the root's whole-transcript count),
//     or the round the row opens as a resume prompt (`promptRoot`); an
//     unstamped parent with no other child starts from the row (`init`,
//     `initRoot` for a prompt); anything else unstamped goes dirty;
//   - the carrier of the row's round: `round`, or `carrierInit` when the
//     row is the prompt that opens its round;
//   - the row's parent: its tray (`tray`), when the row is a tool call.
//
// A summary, kind or tool change of a preview-kind child re-picks the
// preview of each of its rounds' stamps when the row is that stamp's pick
// or now beats it, and the parent's tray likewise; a pick or tray row
// that stops qualifying makes its stamp dirty.
//
// TestSubagentAggregateStampsMatchTheReadTimeAggregator holds these rules
// and the recompute to the same stamp rows for every shape it drives.

// ErrSubagentAnchor reports a claimed write whose anchor the store cannot
// accept: a row with no parent, an anchor that is not a local anchorable
// row (SubagentAnchorable), or one that is not the row's parent or a
// local ancestor of it.
var ErrSubagentAnchor = errors.New("store: invalid subagent anchor")

// subagentClaimSetSQL ends the SET list of a claimed UPDATE.
var subagentClaimSetSQL = ", rev = " + strconv.Itoa(subagentClaimRev)

// itemInsertClaimedSQL is itemInsertSQL for a claimed row.
var itemInsertClaimedSQL = strings.TrimSuffix(itemInsertPrefix, ")") + ", rev) VALUES " +
	strings.TrimSuffix(itemInsertValues, ")") + ", " + strconv.Itoa(subagentClaimRev) + ")"

// subagentClaimRow is a written row as the keyed rules read it.
type subagentClaimRow struct {
	threadID, id, parentID  string
	kind, toolName, summary string
	turn, index             int
}

func subagentClaimRowOf(item Item) subagentClaimRow {
	return subagentClaimRow{
		threadID: item.ThreadID, id: item.ID, parentID: item.ParentID,
		kind: item.Kind, toolName: item.ToolName, summary: item.Summary,
		turn: item.TurnIndex, index: item.ItemIndex,
	}
}

// visible is visibleItemsFilterFor.
func (r subagentClaimRow) visible() bool {
	return !(r.kind == "notification" && r.toolName == "plan_update")
}

// previewable is aggPreviewableSQL: previewableSubagentRow.
func (r subagentClaimRow) previewable() bool { return previewableSubagentRow(r.kind, r.summary) }

// toolable is aggToolableSQL.
func (r subagentClaimRow) toolable() bool {
	return r.kind == "tool_call" && r.toolName != "collab_agent" && strings.Trim(r.summary, subagentPreviewBlank) != ""
}

// storedAfter is "the stored position (turn, item) is after the row's".
func (r subagentClaimRow) storedAfter(turn, item sql.NullInt64) bool {
	return turn.Valid &&
		(turn.Int64 > int64(r.turn) || (turn.Int64 == int64(r.turn) && item.Valid && item.Int64 > int64(r.index)))
}

// newerThan is "the row is after the stored position, or none is stored".
func (r subagentClaimRow) newerThan(turn, item sql.NullInt64) bool {
	if !turn.Valid {
		return true
	}
	return int64(r.turn) > turn.Int64 || (int64(r.turn) == turn.Int64 && item.Valid && int64(r.index) > item.Int64)
}

// beats is the preview and tray tie rule against a stored row: newer, or
// at the same position with a smaller id.
func (r subagentClaimRow) beats(turn, item sql.NullInt64, id sql.NullString) bool {
	if r.newerThan(turn, item) {
		return true
	}
	return turn.Int64 == int64(r.turn) && item.Valid && item.Int64 == int64(r.index) && id.Valid && r.id < id.String
}

func (r subagentClaimRow) betterPick(s subagentStampValues) bool {
	return r.previewable() && r.beats(s.PickTurn, s.PickItem, s.PickID)
}

func (r subagentClaimRow) betterTool(s subagentStampValues) bool {
	return r.toolable() && r.beats(s.ToolTurn, s.ToolItem, s.ToolID)
}

func (r subagentClaimRow) pick(v *subagentStampValues) {
	v.Summary = sql.NullString{String: r.summary, Valid: true}
	v.PickTurn, v.PickItem, v.PickID = validInt(r.turn), validInt(r.index), sql.NullString{String: r.id, Valid: true}
}

func (r subagentClaimRow) tool(v *subagentStampValues) {
	v.ToolSummary = sql.NullString{String: strings.Trim(r.summary, subagentPreviewBlank), Valid: true}
	v.ToolTurn, v.ToolItem, v.ToolID = validInt(r.turn), validInt(r.index), sql.NullString{String: r.id, Valid: true}
}

// subagentClaimStamp is a row a keyed rule reads or writes.
type subagentClaimStamp struct {
	id         string
	anchorable bool
	// root is the row's transcript root as aggTranscriptRootSQL reads it.
	root    sql.NullString
	stamped bool
	stamp   subagentStampValues
}

func (s *subagentClaimStamp) clean() bool { return s.stamped && s.stamp.State == aggStateClean }

// carrier is aggCarrierSQL.
func (s *subagentClaimStamp) carrier() bool {
	return s.root.Valid && s.root.String != "" && s.root.String != s.id
}

// subagentClaimLevel is one row on the written row's parent chain.
type subagentClaimLevel struct {
	subagentClaimStamp
	depth    int
	parentID string
	visible  bool
	// promptID is the latest resume prompt directly under the row at or
	// before the written row's position; carrierID is the carrier it
	// names, or "" for none.
	promptID  sql.NullString
	carrierID string
}

// subagentClaimLevelSQL reads one chain row by primary key with its stamp
// and its round's prompt, found by one backwards probe of
// idx_items_subagent_resume_prompt. ?3 and ?4 are the written row's
// position.
var subagentClaimLevelSQL = `SELECT a.parent_id, ` + aggAnchorableSQL("a.") + `, ` + visibleItemsFilterFor("a.") + `,
       ` + aggKeyedTranscriptRootSQL("a.") + `,
       COALESCE((SELECT history_bulk_load FROM threads WHERE id = ?1), 0),
       s.item_id IS NOT NULL, COALESCE(s.state, 0), s.` + strings.Join(subagentAggregateValueColumns, ", s.") + `,
       p.id, ` + aggPromptCarrierSQL("p.") + `
  FROM items a
  LEFT JOIN subagent_aggregates s ON s.thread_id = a.thread_id AND s.item_id = a.id
  LEFT JOIN items p ON p.thread_id = a.thread_id AND p.id = (
        SELECT q.id FROM items q
         WHERE q.thread_id = ?1 AND q.parent_id = a.id AND ` + aggPromptSQL("q.") + `
           AND (q.turn_index, q.item_index) <= (?3, ?4)
         ORDER BY q.turn_index DESC, q.item_index DESC LIMIT 1)
 WHERE a.thread_id = ?1 AND a.id = ?2`

// subagentClaimStampSQL reads a row a rule names that is not on the
// chain: a round's carrier, or a carrier a prompt under a dirty anchor
// names.
var subagentClaimStampSQL = `SELECT ` + aggAnchorableSQL("c.") + `, ` + aggKeyedTranscriptRootSQL("c.") + `,
       s.item_id IS NOT NULL, COALESCE(s.state, 0), s.` + strings.Join(subagentAggregateValueColumns, ", s.") + `
  FROM items c
  LEFT JOIN subagent_aggregates s ON s.thread_id = c.thread_id AND s.item_id = c.id
 WHERE c.thread_id = ?1 AND c.id = ?2`

// subagentClaimRowSQL reads what a claimed update's rules need of the row
// before the write.
const subagentClaimRowSQL = `SELECT parent_id, turn_index, item_index, kind, tool_name, summary
  FROM items WHERE thread_id = ? AND id = ?`

var (
	subagentHasOtherChildSQL  = `SELECT ` + aggHasChildSQL("?1", "?2", "?3")
	subagentHasLocalChildSQL  = `SELECT ` + aggHasLocalChildSQL("?1", "?2", "")
	subagentPromptCarriersSQL = `SELECT ` + aggPromptCarrierSQL("p.") + ` FROM items p
 WHERE p.thread_id = ?1 AND p.parent_id = ?2 AND ` + aggPromptSQL("p.")
	subagentClaimWriteSQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state, ` +
		strings.Join(subagentAggregateValueColumns, ", ") + `)
VALUES (?, ?, ?` + strings.Repeat(", ?", len(subagentAggregateValueColumns)) + `)
` + subagentAggregateUpsertSQL("")
)

// readSubagentClaimRowTx reads a row before a claimed update.
func readSubagentClaimRowTx(tx *sql.Tx, threadID, id string) (subagentClaimRow, error) {
	row := subagentClaimRow{threadID: threadID, id: id}
	if err := tx.QueryRow(subagentClaimRowSQL, threadID, id).Scan(
		&row.parentID, &row.turn, &row.index, &row.kind, &row.toolName, &row.summary,
	); err != nil {
		return subagentClaimRow{}, fmt.Errorf("store: read claimed item %s/%s: %w", threadID, id, err)
	}
	return row, nil
}

// subagentClaim is one claimed write's keyed stamp work.
type subagentClaim struct {
	tx  *sql.Tx
	row subagentClaimRow
	// chain is the parent chain the triggers' marks would walk: the
	// parent, then each ancestor above a visible row, up to and including
	// the first hidden one.
	chain []*subagentClaimLevel
	// stamps holds every row read, by id; nil for an id with no local row.
	stamps map[string]*subagentClaimStamp
}

// readSubagentClaimTx checks a claimed row's anchor and reads its chain.
// It returns nil when the write changes no stamp: the row is hidden, or
// the thread is bulk loading.
func readSubagentClaimTx(tx *sql.Tx, row subagentClaimRow, anchor string) (*subagentClaim, error) {
	c := &subagentClaim{tx: tx, row: row, stamps: make(map[string]*subagentClaimStamp)}
	idle, found, walking := false, false, true
	for id, depth := row.parentID, 1; id != "" && depth <= 64; depth++ {
		level := &subagentClaimLevel{depth: depth}
		level.id = id
		var bulk int64
		dest := append([]any{&level.parentID, &level.anchorable, &level.visible, &level.root, &bulk,
			&level.stamped, &level.stamp.State}, level.stamp.scanTargets()...)
		dest = append(dest, &level.promptID, &level.carrierID)
		err := tx.QueryRow(subagentClaimLevelSQL, row.threadID, id, row.turn, row.index).Scan(dest...)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("store: read subagent chain %s/%s: %w", row.threadID, id, err)
		}
		if depth == 1 {
			idle = bulk == 0
		}
		if walking {
			c.chain = append(c.chain, level)
			c.stamps[id] = &level.subagentClaimStamp
		}
		if id == anchor {
			if !level.anchorable {
				return nil, fmt.Errorf("%w: %s/%s names %s, which anchors no subagent card", ErrSubagentAnchor, row.threadID, row.id, anchor)
			}
			found = true
		}
		walking = walking && level.visible
		if found && !walking {
			break
		}
		id = level.parentID
	}
	if !found {
		return nil, fmt.Errorf("%w: %s/%s names %s, which is not its parent %q or an ancestor of it",
			ErrSubagentAnchor, row.threadID, row.id, anchor, row.parentID)
	}
	if !idle || !row.visible() {
		return nil, nil
	}
	return c, nil
}

// stampOf reads a row a rule names, once per claim.
func (c *subagentClaim) stampOf(id string) (*subagentClaimStamp, error) {
	if s, ok := c.stamps[id]; ok {
		return s, nil
	}
	s := &subagentClaimStamp{id: id}
	dest := append([]any{&s.anchorable, &s.root, &s.stamped, &s.stamp.State}, s.stamp.scanTargets()...)
	err := c.tx.QueryRow(subagentClaimStampSQL, c.row.threadID, id).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		s = nil
	} else if err != nil {
		return nil, fmt.Errorf("store: read subagent stamp %s/%s: %w", c.row.threadID, id, err)
	}
	c.stamps[id] = s
	return s, nil
}

// subagentClaimRound is the written row's round under one chain anchor.
type subagentClaimRound struct {
	level *subagentClaimLevel
	// carrierOK is false when the carrier the round's prompt names is a
	// local anchorable row stamped as another root's carrier. Its card
	// still shows this round, but the row stamp, which moves the carriers
	// of the chain's anchors, does not reach it: a write in the round
	// marks it dirty, and the recompute serves it at a new revision.
	carrierOK bool
	// carrier is the named carrier when it is a local anchorable row.
	carrier *subagentClaimStamp
	role    subagentClaimRole
}

// rounds resolves the round under every chain anchor that is not a
// carrier: nothing under a carrier counts toward its card.
func (c *subagentClaim) rounds() ([]*subagentClaimRound, error) {
	var out []*subagentClaimRound
	for _, level := range c.chain {
		if !level.anchorable || level.carrier() {
			continue
		}
		round := &subagentClaimRound{level: level, carrierOK: true}
		if level.carrierID != "" {
			carrier, err := c.stampOf(level.carrierID)
			if err != nil {
				return nil, err
			}
			if carrier != nil && carrier.anchorable {
				round.carrier = carrier
				round.carrierOK = carrier.root.Valid && carrier.root.String == level.id
			}
		}
		out = append(out, round)
	}
	return out, nil
}

func (c *subagentClaim) exists(query string, args ...any) (bool, error) {
	var found bool
	if err := c.tx.QueryRow(query, args...).Scan(&found); err != nil {
		return false, fmt.Errorf("store: probe subagent children in %s: %w", c.row.threadID, err)
	}
	return found, nil
}

// subagentClaimRole is what a rule does to one stamp.
type subagentClaimRole string

const (
	claimInit        subagentClaimRole = "init"
	claimInitRoot    subagentClaimRole = "initRoot"
	claimCarrierInit subagentClaimRole = "carrierInit"
	claimRound       subagentClaimRole = "round"
	claimTranscript  subagentClaimRole = "transcript"
	claimPromptRoot  subagentClaimRole = "promptRoot"
	claimTray        subagentClaimRole = "tray"
	claimPick        subagentClaimRole = "pick"
	claimTool        subagentClaimRole = "tool"
	claimDirty       subagentClaimRole = "dirty"
)

// subagentClaimOps collects the roles each stamp takes, in first-seen
// order.
type subagentClaimOps struct {
	order []string
	roles map[string][]subagentClaimRole
}

func (o *subagentClaimOps) add(id string, role subagentClaimRole) {
	if id == "" || role == "" {
		return
	}
	if o.roles == nil {
		o.roles = make(map[string][]subagentClaimRole)
	}
	if _, ok := o.roles[id]; !ok {
		o.order = append(o.order, id)
	}
	o.roles[id] = append(o.roles[id], role)
}

// write applies each stamp's roles through values, which returns the
// stamp's new values and whether the roles write it at all. A stamp is
// written only when a local anchorable row carries it and its values
// change, and never dirty again.
func (c *subagentClaim) write(ops *subagentClaimOps, values func(roles []subagentClaimRole, old subagentStampValues) (subagentStampValues, bool)) error {
	for _, id := range ops.order {
		target, err := c.stampOf(id)
		if err != nil {
			return err
		}
		if target == nil || !target.anchorable {
			continue
		}
		old := target.stamp
		if !target.stamped {
			old = subagentStampValues{}
		}
		next, ok := values(ops.roles[id], old)
		if !ok || (next.State == aggStateDirty && target.stamped && target.stamp.State == aggStateDirty) {
			continue
		}
		if target.stamped && target.stamp == next {
			continue
		}
		args := append([]any{c.row.threadID, id, next.State}, next.args()...)
		if _, err := c.tx.Exec(subagentClaimWriteSQL, args...); err != nil {
			return fmt.Errorf("store: write subagent stamp %s/%s: %w", c.row.threadID, id, err)
		}
		target.stamped, target.stamp = true, next
	}
	return nil
}

var subagentDirtyStamp = subagentStampValues{State: aggStateDirty}

// claimSubagentInsertTx applies a claimed insert's effect on the stamps.
// row is the inserted row, at its stored position.
func claimSubagentInsertTx(tx *sql.Tx, row subagentClaimRow, anchor string) error {
	c, err := readSubagentClaimTx(tx, row, anchor)
	if err != nil || c == nil {
		return err
	}
	rounds, err := c.rounds()
	if err != nil {
		return err
	}
	var ops subagentClaimOps
	for _, round := range rounds {
		level := round.level
		opens := level.promptID.Valid && level.promptID.String == row.id
		switch {
		case !level.stamped:
			round.role = claimDirty
			if level.depth == 1 && (!level.promptID.Valid || opens) {
				other, err := c.exists(subagentHasOtherChildSQL, row.threadID, level.id, row.id)
				if err != nil {
					return err
				}
				switch {
				case other:
				case !level.promptID.Valid:
					round.role = claimInit
				case level.carrierID != level.id && round.carrierOK:
					round.role = claimInitRoot
				}
			}
		case level.stamp.State != aggStateClean:
		case !round.carrierOK:
			round.role = claimDirty
		case !level.promptID.Valid:
			round.role = claimRound
		case opens:
			round.role = claimPromptRoot
			var named *subagentClaimStamp
			if level.carrierID != "" {
				if named, err = c.stampOf(level.carrierID); err != nil {
					return err
				}
			}
			// A prompt stored before rows already written moves them
			// between rounds. A carrier that already carries a stamp was
			// named by another prompt, or is this anchor itself: shapes
			// the recompute serves readTime.
			if row.storedAfter(level.stamp.NewestTurn, level.stamp.NewestItem) ||
				row.storedAfter(level.stamp.TranscriptNewestTurn, level.stamp.TranscriptNewestItem) ||
				(named != nil && named.stamped) {
				round.role = claimDirty
			}
		default:
			round.role = claimTranscript
		}
		ops.add(level.id, round.role)
	}
	for _, round := range rounds {
		level, carrier := round.level, round.carrier
		if carrier == nil || level.carrierID == level.id {
			continue
		}
		if !round.carrierOK {
			ops.add(carrier.id, claimDirty)
			continue
		}
		var role subagentClaimRole
		switch {
		case level.promptID.Valid && level.promptID.String == row.id:
			role = claimDirty
			if round.role == claimPromptRoot || round.role == claimInitRoot {
				has, err := c.exists(subagentHasLocalChildSQL, row.threadID, carrier.id)
				if err != nil {
					return err
				}
				if !has {
					role = claimCarrierInit
				}
			}
		case carrier.clean():
			role = claimRound
		case carrier.stamped:
		default:
			role = claimDirty
		}
		ops.add(carrier.id, role)
	}
	// A prompt that opens a round the rules do not keep may cut an earlier
	// round short: rows stored after it move to its round. Every carrier a
	// prompt under the anchor names goes dirty, so a carrier whose card
	// the row stamp does not move (another root's) is served at a new
	// revision.
	for _, round := range rounds {
		level := round.level
		opens := level.promptID.Valid && level.promptID.String == row.id
		if !opens || (round.role != claimDirty && (!level.stamped || level.clean())) {
			continue
		}
		carriers, err := subagentAnchorIDs(tx, subagentPromptCarriersSQL, row.threadID, round.level.id)
		if err != nil {
			return fmt.Errorf("store: read prompts under %s/%s: %w", row.threadID, round.level.id, err)
		}
		for _, id := range carriers {
			ops.add(id, claimDirty)
		}
	}
	if len(c.chain) > 0 && c.chain[0].clean() && row.toolable() {
		ops.add(c.chain[0].id, claimTray)
	}
	return c.write(&ops, func(roles []subagentClaimRole, old subagentStampValues) (subagentStampValues, bool) {
		return claimInsertValues(roles, old, row)
	})
}

// claimInsertValues is one stamp's values after an insert's roles. A
// stamp takes one role besides the tray at most: a round role as a chain
// anchor that is not a carrier, or a carrier role as the carrier of a
// chain anchor's round, which is never itself a round's anchor.
func claimInsertValues(roles []subagentClaimRole, old subagentStampValues, row subagentClaimRow) (subagentStampValues, bool) {
	var op subagentClaimRole
	tray := false
	for _, role := range roles {
		switch role {
		case claimDirty:
			return subagentDirtyStamp, true
		case claimTray:
			tray = true
		default:
			op = role
		}
	}
	if op == "" && !tray {
		return subagentStampValues{}, false
	}
	v := subagentStampValues{State: aggStateClean}
	switch op {
	case claimInit:
		v.Count = validInt(1)
		if row.previewable() {
			row.pick(&v)
		}
		if row.toolable() {
			row.tool(&v)
		}
		v.NewestTurn, v.NewestItem = validInt(row.turn), validInt(row.index)
	case claimInitRoot:
		v.Count, v.TranscriptCount = validInt(0), validInt(1)
		v.TranscriptNewestTurn, v.TranscriptNewestItem = validInt(row.turn), validInt(row.index)
	case claimCarrierInit:
		v.Count = validInt(1)
		v.NewestTurn, v.NewestItem = validInt(row.turn), validInt(row.index)
	default:
		v = old
		v.State = aggStateClean
		switch op {
		case claimRound:
			v.Count = validInt(int(old.Count.Int64) + 1)
			if old.TranscriptCount.Valid {
				v.TranscriptCount = validInt(int(old.TranscriptCount.Int64) + 1)
			}
			if row.betterPick(old) {
				row.pick(&v)
			}
			if row.newerThan(old.NewestTurn, old.NewestItem) {
				v.NewestTurn, v.NewestItem = validInt(row.turn), validInt(row.index)
			}
		case claimTranscript:
			v.TranscriptCount = validInt(int(old.TranscriptCount.Int64) + 1)
		case claimPromptRoot:
			v.Count = validInt(int(old.Count.Int64))
			base := old.Count.Int64
			if old.TranscriptCount.Valid {
				base = old.TranscriptCount.Int64
			}
			v.TranscriptCount = validInt(int(base) + 1)
		}
		if op == claimTranscript || op == claimPromptRoot || (op == claimRound && old.TranscriptCount.Valid) {
			if row.newerThan(old.TranscriptNewestTurn, old.TranscriptNewestItem) {
				v.TranscriptNewestTurn, v.TranscriptNewestItem = validInt(row.turn), validInt(row.index)
			}
		}
		if tray && row.betterTool(old) {
			row.tool(&v)
		}
	}
	return v, true
}

// claimSubagentContentTx applies a claimed update's effect on the stamps:
// old is the row before the write, row after it. Only a change to a
// preview-kind child's summary, kind or tool moves a stamp. A write that
// also moved the row found its stamps marked dirty by the update trigger
// (subagentMarkUpdateSQL) and changes nothing here, since every rule
// reads a clean stamp.
func claimSubagentContentTx(tx *sql.Tx, old, row subagentClaimRow, anchor string) error {
	c, err := readSubagentClaimTx(tx, row, anchor)
	if err != nil || c == nil {
		return err
	}
	if old.summary == row.summary && old.kind == row.kind && old.toolName == row.toolName {
		return nil
	}
	if !previewKind(old.kind) && !previewKind(row.kind) {
		return nil
	}
	rounds, err := c.rounds()
	if err != nil {
		return err
	}
	var ops subagentClaimOps
	for _, round := range rounds {
		level := round.level
		var target *subagentClaimStamp
		switch {
		case !level.promptID.Valid:
			target = &level.subagentClaimStamp
		case round.carrierOK && level.carrierID != "":
			if target, err = c.stampOf(level.carrierID); err != nil {
				return err
			}
		case round.carrier != nil:
			ops.add(round.carrier.id, claimDirty)
			continue
		}
		if target == nil || !target.clean() {
			continue
		}
		switch {
		case target.stamp.PickID.Valid && target.stamp.PickID.String == row.id:
			if row.previewable() {
				ops.add(target.id, claimPick)
			} else {
				ops.add(target.id, claimDirty)
			}
		case row.betterPick(target.stamp):
			ops.add(target.id, claimPick)
		}
	}
	if len(c.chain) > 0 && c.chain[0].clean() {
		parent := c.chain[0]
		switch {
		case parent.stamp.ToolID.Valid && parent.stamp.ToolID.String == row.id:
			if row.toolable() {
				ops.add(parent.id, claimTool)
			} else {
				ops.add(parent.id, claimDirty)
			}
		case row.betterTool(parent.stamp):
			ops.add(parent.id, claimTool)
		}
	}
	// A stamp is one round's target at most (as for an insert) and the
	// parent's tray.
	return c.write(&ops, func(roles []subagentClaimRole, old subagentStampValues) (subagentStampValues, bool) {
		v := old
		for _, role := range roles {
			switch role {
			case claimDirty:
				return subagentDirtyStamp, true
			case claimPick:
				row.pick(&v)
			case claimTool:
				row.tool(&v)
			}
		}
		return v, true
	})
}
