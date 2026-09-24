package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// subagentStampValues is a stamp as stored, without its generation: what
// a recompute derives and compares. Its fields after State are
// subagentAggregateValueColumns, in order.
type subagentStampValues struct {
	State                                      int64
	Count                                      sql.NullInt64
	Summary                                    sql.NullString
	TranscriptCount                            sql.NullInt64
	ToolSummary                                sql.NullString
	ToolTurn, ToolItem                         sql.NullInt64
	ToolID                                     sql.NullString
	PickTurn, PickItem                         sql.NullInt64
	PickID                                     sql.NullString
	NewestTurn, NewestItem                     sql.NullInt64
	TranscriptNewestTurn, TranscriptNewestItem sql.NullInt64
}

// scanTargets are pointers to the value columns, for a scan.
func (v *subagentStampValues) scanTargets() []any {
	return []any{
		&v.Count, &v.Summary, &v.TranscriptCount,
		&v.ToolSummary, &v.ToolTurn, &v.ToolItem, &v.ToolID,
		&v.PickTurn, &v.PickItem, &v.PickID,
		&v.NewestTurn, &v.NewestItem, &v.TranscriptNewestTurn, &v.TranscriptNewestItem,
	}
}

// args are the value columns, for a write.
func (v subagentStampValues) args() []any {
	return []any{
		v.Count, v.Summary, v.TranscriptCount,
		v.ToolSummary, v.ToolTurn, v.ToolItem, v.ToolID,
		v.PickTurn, v.PickItem, v.PickID,
		v.NewestTurn, v.NewestItem, v.TranscriptNewestTurn, v.TranscriptNewestItem,
	}
}

func validInt(n int) sql.NullInt64           { return sql.NullInt64{Int64: int64(n), Valid: true} }
func nonEmptyString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// subagentStampTarget is a local row a recompute may write, with its
// stored stamp.
type subagentStampTarget struct {
	id, kind, toolName string
	// root is the transcript root a carrier's round is counted under, or
	// "" for a row that is not a carrier (aggCarrierSQL).
	root     string
	rev, gen int64
	stamped  bool
	stored   subagentStampValues
}

func (t subagentStampTarget) anchorable() bool {
	return SubagentAnchorable(t.kind, t.toolName)
}

// SubagentAnchorable reports whether a row of this kind and tool anchors
// a subagent card: a tool call other than a Codex spawn row.
// aggAnchorableSQL is the same rule.
func SubagentAnchorable(kind, toolName string) bool {
	return kind == "tool_call" && toolName != "collab_agent"
}

// subagentStampWrite is one recomputed stamp. rev is the revision the
// computation read, for the optimistic write; gen is the generation the
// write stores (newSubagentGen).
type subagentStampWrite struct {
	id       string
	rev, gen int64
	values   subagentStampValues
}

// subagentFamilyMember is one local anchor a recompute derived, with what
// a card accumulator needs to go on from it.
type subagentFamilyMember struct {
	id string
	// root is a carrier's transcript root, or "".
	root     string
	rev, gen int64
	stamped  bool
	stored   subagentStampValues
	values   subagentStampValues
	// rounds reports a clean root with resume rounds: lo is its last
	// round's prompt position, and roundID that round's carrier, or ""
	// when the prompt names none.
	rounds  bool
	lo      TimelineCursor
	roundID string
	// foreign reports a carrier a prompt under another root names. The
	// item triggers stamp only the carriers whose transcript root is on a
	// written row's chain, so rows under the naming root do not reach it.
	foreign bool
	// written reports that the recompute stored values, at generation gen.
	written bool
}

// changed reports a member whose stored stamp is not its values.
func (m subagentFamilyMember) changed() bool { return !m.stamped || m.stored != m.values }

// restamp reports a member a recompute writes: one that changed, and a
// foreign carrier, whose card the rows the recompute covers may have
// changed with no trigger stamping it.
func (m subagentFamilyMember) restamp() bool { return m.changed() || m.foreign }

// computeSubagentStamps is computeSubagentFamilies reduced to the stamps
// it writes (restamp), each with a new generation.
func computeSubagentStamps(q sqlQueryer, threadID string, seedIDs []string) ([]subagentStampWrite, error) {
	members, err := computeSubagentFamilies(q, threadID, seedIDs)
	if err != nil {
		return nil, err
	}
	var writes []subagentStampWrite
	for _, m := range members {
		if m.restamp() {
			writes = append(writes, subagentStampWrite{id: m.id, rev: m.rev, gen: newSubagentGen(), values: m.values})
		}
	}
	return writes, nil
}

// computeSubagentFamilies recomputes the stamps of the given anchors and
// of every anchor whose stamp depends on the same resume prompts: a root and
// the carriers of its rounds are one family, because a prompt cuts both.
// Values come from the read-time aggregator, so a stamp is by
// construction what decorateSubagentAnchors returns for the row.
//
// A carrier's family is its transcript root's. When the root it names is
// itself a carrier (a round resumed from a carrier), that carrier's own
// root joins too, so the named carrier is stamped from its family rather
// than written readTime as a root it is not.
//
// A family is marked readTime when its rounds take a shape the card rules
// do not maintain: a prompt in the immutable history arm (the round probe
// reads local rows only), a prompt naming the root itself, one carrier
// named by two prompts, a named carrier stamped as another root's (a
// foreign carrier, which every recompute of the family re-stamps), a root
// that is itself a carrier, or a root outside the local overlay. A carrier
// no prompt names shows the whole transcript, which no incremental rule
// keeps, and is readTime on its own.
func computeSubagentFamilies(q sqlQueryer, threadID string, seedIDs []string) ([]subagentFamilyMember, error) {
	seeds, err := subagentStampTargets(q, threadID, seedIDs)
	if err != nil {
		return nil, err
	}
	rootSet := make(map[string]struct{}, len(seeds))
	var roots []string
	addRoot := func(id string) bool {
		if _, ok := rootSet[id]; ok {
			return false
		}
		rootSet[id] = struct{}{}
		roots = append(roots, id)
		return true
	}
	var carrierRoots []string
	for _, seed := range seeds {
		if !seed.anchorable() {
			continue
		}
		if seed.root == "" {
			addRoot(seed.id)
		} else if addRoot(seed.root) {
			carrierRoots = append(carrierRoots, seed.root)
		}
	}
	for depth := 0; len(carrierRoots) > 0 && depth < 64; depth++ {
		named, err := subagentStampTargets(q, threadID, carrierRoots)
		if err != nil {
			return nil, err
		}
		carrierRoots = nil
		for _, row := range named {
			if row.anchorable() && row.root != "" && addRoot(row.root) {
				carrierRoots = append(carrierRoots, row.root)
			}
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	slices.Sort(roots)

	rounds, err := subagentResumeRounds(q, threadID, roots)
	if err != nil {
		return nil, err
	}
	carriersByRoot, err := subagentCarriersOf(q, threadID, roots)
	if err != nil {
		return nil, err
	}
	memberIDs := slices.Clone(roots)
	named := make(map[string]int, len(rounds))
	for _, round := range rounds {
		if round.anchorID != round.promptID {
			named[round.anchorID]++
			memberIDs = append(memberIDs, round.anchorID)
		}
	}
	for _, carriers := range carriersByRoot {
		memberIDs = append(memberIDs, carriers...)
	}
	for _, seed := range seeds {
		memberIDs = append(memberIDs, seed.id)
	}
	slices.Sort(memberIDs)
	memberIDs = slices.Compact(memberIDs)
	members, err := subagentStampTargets(q, threadID, memberIDs)
	if err != nil {
		return nil, err
	}

	readTimeRoot := make(map[string]bool, len(roots))
	for _, root := range roots {
		if row, ok := members[root]; !ok || !row.anchorable() || row.root != "" {
			readTimeRoot[root] = true
		}
	}
	foreign := make(map[string]bool)
	for _, round := range rounds {
		if round.imported || round.anchorID == round.rootID || named[round.anchorID] > 1 {
			readTimeRoot[round.rootID] = true
		}
		if round.anchorID == round.promptID {
			continue
		}
		if carrier, ok := members[round.anchorID]; ok && carrier.anchorable() && carrier.root != round.rootID {
			readTimeRoot[round.rootID] = true
			foreign[round.anchorID] = true
		}
	}

	var cleanRoots []string
	var cleanRounds []subagentRound
	for _, root := range roots {
		if !readTimeRoot[root] {
			cleanRoots = append(cleanRoots, root)
		}
	}
	for _, round := range rounds {
		if !readTimeRoot[round.rootID] {
			cleanRounds = append(cleanRounds, round)
		}
	}
	accumulators, transcripts, err := subagentAccumulatorsByRound(
		q, threadID, cleanRoots, subagentRoundBoundsFor(cleanRoots, cleanRounds, nil))
	if err != nil {
		return nil, err
	}
	// A clean root's last round takes the rows a card writes after it.
	lastRound := make(map[string]subagentRound, len(cleanRoots))
	for _, round := range cleanRounds {
		lastRound[round.rootID] = round
	}

	// Every local anchorable member gets a stamp: its round's values when
	// its family is clean and a bound covers it, readTime otherwise.
	var writeIDs []string
	for id, row := range members {
		if row.anchorable() {
			writeIDs = append(writeIDs, id)
		}
	}
	slices.Sort(writeIDs)
	tools, err := latestDirectSubagentTools(q, threadID, writeIDs)
	if err != nil {
		return nil, err
	}
	out := make([]subagentFamilyMember, 0, len(writeIDs))
	for _, id := range writeIDs {
		row := members[id]
		member := subagentFamilyMember{id: id, root: row.root, rev: row.rev, gen: row.gen, stamped: row.stamped,
			stored: row.stored, foreign: foreign[id]}
		values := subagentStampValues{State: aggStateReadTime}
		clean := false
		if row.root == "" {
			_, isRoot := rootSet[id]
			clean = isRoot && !readTimeRoot[id]
		} else {
			_, bounded := accumulators[id]
			clean = !readTimeRoot[row.root] && bounded && named[id] == 1
		}
		if clean {
			values = subagentStampValues{State: aggStateClean}
			var acc subagentAggregateAccumulator
			if found := accumulators[id]; found != nil {
				acc = *found
			}
			transcript := transcripts[id]
			if acc.aggregate.descendantCount > 0 || transcript != nil {
				values.Count = validInt(acc.aggregate.descendantCount)
			}
			values.Summary = nonEmptyString(acc.aggregate.latestChildSummary)
			if acc.hasPick {
				values.PickTurn, values.PickItem = validInt(acc.preview.turnIndex), validInt(acc.preview.itemIndex)
				values.PickID = nonEmptyString(acc.preview.id)
			}
			if acc.hasNewest {
				values.NewestTurn, values.NewestItem = validInt(acc.newest.TurnIndex), validInt(acc.newest.ItemIndex)
			}
			if transcript != nil {
				values.TranscriptCount = validInt(transcript.aggregate.descendantCount)
				if transcript.hasNewest {
					values.TranscriptNewestTurn = validInt(transcript.newest.TurnIndex)
					values.TranscriptNewestItem = validInt(transcript.newest.ItemIndex)
				}
			}
			if tool, ok := tools[id]; ok {
				values.ToolSummary = nonEmptyString(tool.summary)
				values.ToolTurn, values.ToolItem = validInt(tool.turnIndex), validInt(tool.itemIndex)
				values.ToolID = nonEmptyString(tool.id)
			}
			if round, ok := lastRound[id]; ok && row.root == "" {
				member.rounds = true
				member.lo = TimelineCursor{TurnIndex: round.turnIndex, ItemIndex: round.itemIndex}
				if round.anchorID != round.promptID {
					member.roundID = round.anchorID
				}
			}
		}
		member.values = values
		out = append(out, member)
	}
	return out, nil
}

// subagentStampTargetsSQL reads local rows by primary key with their
// stamps; ?2 is a JSON array of ids. Only a tool call's meta is read,
// for its transcript root.
var subagentStampTargetsSQL = `SELECT i.id, i.kind, i.tool_name,
       CASE WHEN i.kind = 'tool_call' THEN COALESCE(` + aggTranscriptRootSQL("i.") + `, '') ELSE '' END,
       i.rev, COALESCE(s.gen, 0), s.item_id IS NOT NULL, COALESCE(s.state, 0),
       s.` + strings.Join(subagentAggregateValueColumns, ", s.") + `
  FROM items i
  LEFT JOIN subagent_aggregates s ON s.thread_id = i.thread_id AND s.item_id = i.id
 WHERE i.thread_id = ?1 AND i.id IN (SELECT value FROM json_each(?2))`

// subagentStampTargets loads local rows by id.
func subagentStampTargets(q sqlQueryer, threadID string, ids []string) (map[string]subagentStampTarget, error) {
	out := make(map[string]subagentStampTarget, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(subagentStampTargetsSQL, threadID, list)
	if err != nil {
		return nil, fmt.Errorf("store: read subagent stamp targets for %s: %w", threadID, err)
	}
	for rows.Next() {
		var row subagentStampTarget
		dest := append([]any{&row.id, &row.kind, &row.toolName, &row.root, &row.rev, &row.gen, &row.stamped, &row.stored.State},
			row.stored.scanTargets()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan subagent stamp target: %w", err), rows.Close())
		}
		if row.root == row.id {
			row.root = ""
		}
		out[row.id] = row
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate subagent stamp targets for %s: %w", threadID, err)
	}
	return out, nil
}

// subagentCarriersSQL reads the local carriers stamped with the roots in
// the JSON array ?2 through idx_items_transcript_root.
var subagentCarriersSQL = `SELECT id, ` + transcriptRootExpr + ` FROM items
 WHERE thread_id = ?1 AND ` + transcriptRootExpr + ` IN (SELECT value FROM json_each(?2))`

// subagentCarriersOf lists the local carriers stamped with each root.
func subagentCarriersOf(q sqlQueryer, threadID string, roots []string) (map[string][]string, error) {
	out := make(map[string][]string)
	if len(roots) == 0 {
		return out, nil
	}
	list, err := jsonList(roots)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(subagentCarriersSQL, threadID, list)
	if err != nil {
		return nil, fmt.Errorf("store: read subagent carriers for %s: %w", threadID, err)
	}
	for rows.Next() {
		var id, root string
		if err := rows.Scan(&id, &root); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan subagent carrier: %w", err), rows.Close())
		}
		out[root] = append(out[root], id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate subagent carriers for %s: %w", threadID, err)
	}
	return out, nil
}

// bumpSubagentAggregateRevSQL advances the thread stamp for a Go write
// that changes cards outside an item write: subagent_aggregates' triggers
// stamp the anchors it writes with the thread's history_rev, which must
// be one no reader has seen. Under bulk load the thread stamp is frozen,
// as the item triggers leave it.
const bumpSubagentAggregateRevSQL = `UPDATE threads SET history_rev = history_rev + 1
 WHERE id = ? AND history_bulk_load = 0`

// writeSubagentStampSQL replaces a row's stamp with state ?4 at
// generation ?5. ?3 < 0 writes unconditionally; otherwise the item must
// still be at the revision the values were computed from.
var writeSubagentStampSQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state, gen, ` +
	strings.Join(subagentAggregateValueColumns, ", ") + `)
SELECT ?1, ?2, ?4, ?5` + strings.Repeat(", ?", len(subagentAggregateValueColumns)) + `
 WHERE EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND id = ?2 AND (?3 < 0 OR rev = ?3))
` + subagentAggregateUpsertSQL("gen = excluded.gen")

// writeSubagentStampsTx writes recomputed stamps and returns how many
// landed. An optimistic write skips a row that moved since it was read;
// that row is still dirty or unstamped and the next recompute takes it.
// The caller has advanced the thread stamp in this transaction.
func writeSubagentStampsTx(tx *sql.Tx, threadID string, writes []subagentStampWrite, optimistic bool) (landed int, err error) {
	if len(writes) == 0 {
		return 0, nil
	}
	stmt, err := tx.Prepare(writeSubagentStampSQL)
	if err != nil {
		return 0, fmt.Errorf("store: prepare subagent stamp write for %s: %w", threadID, err)
	}
	defer func() {
		if closeErr := stmt.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("store: close subagent stamp write for %s: %w", threadID, closeErr))
		}
	}()
	for _, write := range writes {
		rev := int64(-1)
		if optimistic {
			rev = write.rev
		}
		args := append([]any{threadID, write.id, rev, write.values.State, write.gen}, write.values.args()...)
		result, err := stmt.Exec(args...)
		if err != nil {
			return landed, fmt.Errorf("store: write subagent stamp %s/%s: %w", threadID, write.id, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return landed, fmt.Errorf("store: count subagent stamp %s/%s: %w", threadID, write.id, err)
		}
		landed += int(n)
	}
	return landed, nil
}

// subagentDirtyAnchorsSQL finds a thread's dirty anchors by
// idx_subagent_aggregates_dirty.
var subagentDirtyAnchorsSQL = `SELECT item_id FROM subagent_aggregates
 WHERE thread_id = ? AND state = ` + aggDirtyLiteral + ` LIMIT ?`

// subagentLegacyAnchorsSQL finds a listed thread's anchors that predate
// the stamps: unstamped local anchorable rows that a read decorates,
// because they are carriers or have a visible child in either arm. The
// candidates are the thread's distinct parent ids, read from the covering
// parent indexes of both arms, and its carriers (idx_items_transcript_root);
// each candidate costs two primary-key probes and a one-row child probe.
var subagentLegacyAnchorsSQL = `SELECT a.id FROM (
    SELECT DISTINCT parent_id AS id FROM items WHERE thread_id = ?1 AND parent_id <> ''
    UNION
    SELECT DISTINCT items.parent_id FROM thread_import_chunks refs
      CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id
     WHERE refs.thread_id = ?1 AND items.parent_id <> ''
    UNION
    SELECT id FROM items WHERE thread_id = ?1 AND ` + transcriptRootExpr + ` IS NOT NULL
  ) AS p CROSS JOIN items a
 WHERE a.thread_id = ?1 AND a.id = p.id AND ` + aggAnchorableSQL("a.") + `
   AND NOT EXISTS (SELECT 1 FROM subagent_aggregates s WHERE s.thread_id = a.thread_id AND s.item_id = a.id)
   AND (` + aggCarrierSQL("a.") + ` OR ` + aggHasChildSQL("a.thread_id", "a.id", "") + `)
 LIMIT ?2`

func subagentAnchorIDs(q sqlQueryer, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}

// recomputeSubagentFamiliesTx recomputes the given anchors with their
// families inside a write transaction and writes each stamp it restamps
// at a new generation. bump advances the thread stamp before the first
// write; nil bumps it once here. It returns every member, written or
// not, for the card accumulators.
func recomputeSubagentFamiliesTx(tx *sql.Tx, threadID string, seeds []string, bump func() error) ([]subagentFamilyMember, error) {
	members, err := computeSubagentFamilies(tx, threadID, seeds)
	if err != nil {
		return nil, err
	}
	var writes []subagentStampWrite
	var written []int
	for i := range members {
		if !members[i].restamp() {
			continue
		}
		members[i].gen = newSubagentGen()
		writes = append(writes, subagentStampWrite{id: members[i].id, gen: members[i].gen, values: members[i].values})
		written = append(written, i)
	}
	if len(writes) == 0 {
		return members, nil
	}
	if bump == nil {
		bump = subagentBumpOnce(tx, threadID)
	}
	if err := bump(); err != nil {
		return nil, err
	}
	landed, err := writeSubagentStampsTx(tx, threadID, writes, false)
	if err != nil {
		return nil, err
	}
	if landed != len(writes) {
		return nil, fmt.Errorf("store: write subagent stamps in %s: %d of %d found their row", threadID, landed, len(writes))
	}
	for _, i := range written {
		m := &members[i]
		m.written, m.stamped, m.stored = true, true, m.values
	}
	return members, nil
}

// recomputeSubagentChainsTx recomputes, inside a write transaction, what
// the write changed that the card rules do not follow: every local anchor
// on the logical parent chain from each of fromIDs upward (the id itself
// included, the walk ending above a hidden row, under which nothing
// counts), and the anchors seeds names, each with its family. It drops
// the stamps of unanchor, rows that stopped anchoring. bump advances the
// thread stamp before a stamp write; for an item write, whose trigger
// already has, it does nothing. It returns every stamp
// it covered, whose card accumulators the caller retires (cardWrite.finish).
func (s *Store) recomputeSubagentChainsTx(tx *sql.Tx, threadID string, fromIDs, seeds, unanchor []string, bump func() error) ([]string, error) {
	ids := make(map[string]struct{}, len(seeds))
	for _, id := range seeds {
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	walked := make(map[string]bool, len(fromIDs))
	for _, from := range fromIDs {
		if from == "" || walked[from] {
			continue
		}
		chain, err := subagentChainTx(tx, threadID, from, true)
		if err != nil {
			return nil, err
		}
		for _, row := range chain {
			walked[row.id] = true
			if !row.imported && SubagentAnchorable(row.kind, row.toolName) {
				ids[row.id] = struct{}{}
			}
		}
	}
	stale := slices.Sorted(maps.Keys(ids))
	if len(unanchor) > 0 {
		list, err := jsonList(unanchor)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?1 AND item_id IN (SELECT value FROM json_each(?2))`,
			threadID, list); err != nil {
			return nil, fmt.Errorf("store: drop subagent stamps of former anchors in %s: %w", threadID, err)
		}
	}
	members, err := recomputeSubagentFamiliesTx(tx, threadID, stale, bump)
	if err != nil {
		return nil, err
	}
	stale = append(stale, unanchor...)
	for _, m := range members {
		stale = append(stale, m.id)
	}
	return stale, nil
}

// SubagentRecompute reports one RecomputeSubagentAggregates call.
type SubagentRecompute struct {
	// Stamped is the number of anchor rows whose stamp changed.
	Stamped int
	// Remaining reports anchors the call left for a later one: the limit
	// bound, or a row moved between the read and the write.
	Remaining bool
}

// RecomputeSubagentAggregates is the one recompute primitive: it rewrites
// up to limit of the thread's dirty anchors, then, while the thread is
// listed in subagent_aggregate_backfill, its anchors that predate the
// stamps, each with its family. The values are computed on a read
// snapshot and written in a short write transaction that skips any row
// written since the snapshot, so the aggregator's walk never holds the
// writer.
//
// A listed thread leaves the list with the batch whose snapshot selected
// fewer anchors than the limit and whose stamps all landed: no anchor
// that predates the stamps is left. The proof is the snapshot's, not a
// re-selection under the writer, which on a large thread would hold it
// for hundreds of milliseconds. A write after the snapshot cannot add
// such an anchor: a card opened on an unstamped anchor with children
// recomputes it (seedCardStamp), a write without a card recomputes the
// chain it changed (recomputeSubagentChainsTx), and a carrier, unstamped
// until its prompt lands, is walked in every thread.
//
// A stamp it writes moves to a new generation, so a card accumulator
// read before it recomputes at its next flush rather than write over it.
func (s *Store) RecomputeSubagentAggregates(ctx context.Context, threadID string, limit int) (SubagentRecompute, error) {
	if limit <= 0 {
		return SubagentRecompute{}, fmt.Errorf("store: recompute subagent aggregates for %s: limit must be positive", threadID)
	}
	var seeds []string
	var listed bool
	var writes []subagentStampWrite
	err := func() error {
		rtx, err := s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return fmt.Errorf("store: begin subagent recompute read for %s: %w", threadID, err)
		}
		defer rtx.Rollback()
		if seeds, err = subagentAnchorIDs(rtx, subagentDirtyAnchorsSQL, threadID, limit); err != nil {
			return fmt.Errorf("store: select dirty subagent anchors for %s: %w", threadID, err)
		}
		if listed, err = subagentBackfillListed(rtx, threadID); err != nil {
			return err
		}
		if listed && len(seeds) < limit {
			legacy, err := subagentAnchorIDs(rtx, subagentLegacyAnchorsSQL, threadID, limit-len(seeds))
			if err != nil {
				return fmt.Errorf("store: select legacy subagent anchors for %s: %w", threadID, err)
			}
			seeds = append(seeds, legacy...)
		}
		if len(seeds) == 0 {
			return nil
		}
		writes, err = computeSubagentStamps(rtx, threadID, seeds)
		return err
	}()
	if err != nil {
		return SubagentRecompute{}, err
	}
	if len(seeds) == 0 && !listed {
		return SubagentRecompute{}, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SubagentRecompute{}, fmt.Errorf("store: begin subagent recompute write for %s: %w", threadID, err)
	}
	defer tx.Rollback()
	if len(writes) > 0 {
		if _, err := tx.Exec(bumpSubagentAggregateRevSQL, threadID); err != nil {
			return SubagentRecompute{}, fmt.Errorf("store: bump history for subagent stamps in %s: %w", threadID, err)
		}
	}
	landed, err := writeSubagentStampsTx(tx, threadID, writes, true)
	if err != nil {
		return SubagentRecompute{}, err
	}
	out := SubagentRecompute{Stamped: landed, Remaining: len(seeds) >= limit || landed < len(writes)}
	if listed && !out.Remaining {
		if _, err := tx.Exec(`DELETE FROM subagent_aggregate_backfill WHERE thread_id = ?`, threadID); err != nil {
			return SubagentRecompute{}, fmt.Errorf("store: finish subagent backfill for %s: %w", threadID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return SubagentRecompute{}, fmt.Errorf("store: commit subagent recompute for %s: %w", threadID, err)
	}
	return out, nil
}

// subagentBackfillBatch is how many anchors one call of v121's deferred
// phase stamps. The call computes on a read snapshot and holds the writer
// only for the stamp writes.
const subagentBackfillBatch = 16

// subagentBackfillStallLimit is how many batches in a row may land no
// stamp before the phase leaves the thread to the next run: its rows moved
// under every one of them.
const subagentBackfillStallLimit = 8

// stampLegacySubagentAnchors is migration v121's deferred phase. It stamps
// the anchors of each thread subagent_aggregate_backfill lists, in paced
// RecomputeSubagentAggregates batches, and a thread leaves the list with
// its last anchor: the list is the phase's progress and an empty list its
// end. Until then reads walk a listed thread's unstamped anchors, so the
// phase changes no read. A thread whose batch fails, or whose rows move
// under subagentBackfillStallLimit batches in a row, is reported and stays
// listed for the next run; the threads after it go on.
func stampLegacySubagentAnchors(ctx context.Context, s *Store, run *deferredRun) error {
	after := ""
	batches := 0
	for {
		threadID, err := s.nextSubagentBackfillThread(ctx, after)
		if err != nil || threadID == "" {
			return err
		}
		after = threadID
		for stalled := 0; ; {
			if batches > 0 {
				run.pause()
			}
			if ctx.Err() != nil {
				return nil
			}
			batches++
			result, err := s.RecomputeSubagentAggregates(ctx, threadID, subagentBackfillBatch)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				run.fail(fmt.Errorf("stamp subagent anchors of thread %s: %w", threadID, err))
				break
			}
			if !result.Remaining {
				break
			}
			if result.Stamped > 0 {
				stalled = 0
				continue
			}
			if stalled++; stalled >= subagentBackfillStallLimit {
				run.fail(fmt.Errorf("stamp subagent anchors of thread %s: its rows moved under %d batches in a row", threadID, stalled))
				break
			}
		}
	}
}

// nextSubagentBackfillThread names the first listed thread after `after`
// whose anchors predate the stamps, or "" when none follows it.
func (s *Store) nextSubagentBackfillThread(ctx context.Context, after string) (string, error) {
	var threadID string
	err := s.reader().QueryRowContext(ctx,
		`SELECT thread_id FROM subagent_aggregate_backfill WHERE thread_id > ? ORDER BY thread_id LIMIT 1`,
		after).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: next subagent backfill thread: %w", err)
	}
	return threadID, nil
}

// subagentBackfillListed reports whether the thread's unstamped anchors
// may predate the stamps.
func subagentBackfillListed(q sqlQueryer, threadID string) (bool, error) {
	var listed bool
	if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM subagent_aggregate_backfill WHERE thread_id = ?)`, threadID).Scan(&listed); err != nil {
		return false, fmt.Errorf("store: probe subagent backfill for %s: %w", threadID, err)
	}
	return listed, nil
}

// markSubagentAnchorsDirtySQL marks dirty the anchors among the local rows
// the JSON array ?2 names, by primary key, that are clean or unstamped.
var markSubagentAnchorsDirtySQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state)
SELECT a.thread_id, a.id, ` + aggDirtyLiteral + ` FROM items a
 WHERE a.thread_id = ?1 AND a.id IN (SELECT value FROM json_each(?2)) AND ` + aggAnchorableSQL("a.") + `
ON CONFLICT (thread_id, item_id) DO UPDATE SET state = ` + aggDirtyLiteral + `, ` +
	strings.Join(subagentAggregateValueColumns, " = NULL, ") + ` = NULL
 WHERE subagent_aggregates.state = ` + aggCleanLiteral

// liveSubagentAgentsSQL lists the agents that were running when the
// previous process stopped: running tool calls in the foreground, at the
// top level and nested, and live background launches, each set read
// through its partial index (idx_items_running_fg_tool_calls,
// idx_items_running_nested_fg_tool_calls, idx_items_running_bg_tool_calls),
// with the transcript root a resumed round writes under.
var liveSubagentAgentsSQL = `SELECT thread_id, id, COALESCE(` + aggTranscriptRootSQL("") + `, '') FROM items
 WHERE kind = 'tool_call' AND status = 'running' AND is_background = 0 AND parent_id = ''
   AND tool_name <> 'collab_agent'
UNION ALL
SELECT thread_id, id, COALESCE(` + aggTranscriptRootSQL("") + `, '') FROM items
 WHERE kind = 'tool_call' AND status = 'running' AND is_background = 0 AND parent_id <> ''
   AND tool_name <> 'collab_agent'
UNION ALL
SELECT thread_id, id, COALESCE(` + aggTranscriptRootSQL("") + `, '') FROM items
 WHERE kind = 'tool_call' AND status = 'running' AND is_background = 1
   AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
   AND tool_name <> 'collab_agent'`

// recoverSubagentCardsBatch is how many dirty anchors one recompute of
// the boot pass takes.
const recoverSubagentCardsBatch = 64

// RecoverSubagentCards is the boot pass for the card accumulators a
// process lost: rows written under an agent after the last flush are in
// no stamp. It marks dirty the stamps of every agent running when the
// previous process stopped, with every anchor above it and above the
// transcript root it writes under, and recomputes them. The work is
// bounded by the running agents, not the history. A clean shutdown
// flushed first (FlushAllSubagentCards), so the recompute writes back the
// values the stamps held. A dirty stamp the recompute leaves is
// read through the aggregator until a later write recomputes it. It must
// run before the crash sweeps settle the running rows, and before any
// card is opened.
func (s *Store) RecoverSubagentCards(ctx context.Context) (int, error) {
	marked, err := s.markLiveSubagentChainsDirty()
	if err != nil {
		return 0, err
	}
	stamped := 0
	var errs []error
	for _, threadID := range marked {
		for stalled := 0; ; {
			if ctx.Err() != nil {
				return stamped, ctx.Err()
			}
			result, err := s.RecomputeSubagentAggregates(ctx, threadID, recoverSubagentCardsBatch)
			if err != nil {
				errs = append(errs, err)
				break
			}
			stamped += result.Stamped
			if !result.Remaining {
				break
			}
			if result.Stamped > 0 {
				stalled = 0
				continue
			}
			if stalled++; stalled >= subagentBackfillStallLimit {
				errs = append(errs, fmt.Errorf("store: recover subagent cards of %s: its rows moved under %d batches in a row", threadID, stalled))
				break
			}
		}
	}
	return stamped, errors.Join(errs...)
}

// markLiveSubagentChainsDirty is RecoverSubagentCards' first step, one
// transaction: it returns the threads it marked.
func (s *Store) markLiveSubagentChainsDirty() ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin subagent card recovery: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(liveSubagentAgentsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: select running agents: %w", err)
	}
	from := make(map[string][]string)
	for rows.Next() {
		var threadID, id, root string
		if err := rows.Scan(&threadID, &id, &root); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan running agent: %w", err), rows.Close())
		}
		from[threadID] = append(from[threadID], id)
		if root != "" && root != id {
			from[threadID] = append(from[threadID], root)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate running agents: %w", err)
	}
	threadIDs := slices.Sorted(maps.Keys(from))
	var marked []string
	for _, threadID := range threadIDs {
		var anchors []string
		seen := make(map[string]bool)
		for _, id := range from[threadID] {
			if seen[id] {
				continue
			}
			chain, err := subagentChainTx(tx, threadID, id, true)
			if err != nil {
				return nil, err
			}
			for _, row := range chain {
				seen[row.id] = true
				if !row.imported && SubagentAnchorable(row.kind, row.toolName) {
					anchors = append(anchors, row.id)
				}
			}
		}
		if len(anchors) == 0 {
			continue
		}
		list, err := jsonList(anchors)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(bumpSubagentAggregateRevSQL, threadID); err != nil {
			return nil, fmt.Errorf("store: bump history for dirty subagent anchors in %s: %w", threadID, err)
		}
		if _, err := tx.Exec(markSubagentAnchorsDirtySQL, threadID, list); err != nil {
			return nil, fmt.Errorf("store: mark subagent anchors dirty in %s: %w", threadID, err)
		}
		marked = append(marked, threadID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit subagent card recovery: %w", err)
	}
	return marked, nil
}

// restampSubagentAggregatesTx rebuilds every stamp in a thread whose rows
// were loaded with no card to keep them: its stamps are removed, then
// every anchor a read decorates is recomputed. It runs inside the loading
// transaction, before the thread leaves bulk load, and retires every
// card accumulator of the thread.
func (s *Store) restampSubagentAggregatesTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: clear loaded subagent stamps in %s: %w", threadID, err)
	}
	ids, err := subagentAnchorIDs(tx, subagentLegacyAnchorsSQL, threadID, -1)
	if err != nil {
		return fmt.Errorf("store: select loaded subagent anchors in %s: %w", threadID, err)
	}
	s.cards.invalidateThread(threadID)
	_, err = recomputeSubagentFamiliesTx(tx, threadID, ids, func() error { return nil })
	return err
}
