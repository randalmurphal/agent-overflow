package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	root    string
	rev     int64
	stamped bool
	stored  subagentStampValues
}

func (t subagentStampTarget) anchorable() bool {
	return t.kind == "tool_call" && t.toolName != "collab_agent"
}

// subagentStampWrite is one recomputed stamp. rev is the revision the
// computation read, for the optimistic write.
type subagentStampWrite struct {
	id     string
	rev    int64
	values subagentStampValues
}

// computeSubagentStamps recomputes the stamps of the given anchors and of
// every anchor whose stamp depends on the same resume prompts: a root and
// the carriers of its rounds are one family, because a prompt cuts both.
// Values come from the read-time aggregator, so a stamp is by
// construction what decorateSubagentAnchors returns for the row.
//
// A carrier's family is its transcript root's. When the root it names is
// itself a carrier (a round resumed from a carrier), that carrier's own
// root joins too, so the named carrier is stamped from its family rather
// than written readTime as a root it is not.
//
// A family is marked readTime when its rounds take a shape the triggers do
// not maintain: a prompt in the immutable history arm (the round probe
// reads local rows only), a prompt naming the root itself, one carrier
// named by two prompts, a named carrier stamped as another root's, a root
// that is itself a carrier, or a root outside the local overlay. A carrier
// no prompt names shows the whole transcript, which no incremental rule
// keeps, and is readTime on its own.
func computeSubagentStamps(q sqlQueryer, threadID string, seedIDs []string) ([]subagentStampWrite, error) {
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
	for _, round := range rounds {
		if round.imported || round.anchorID == round.rootID || named[round.anchorID] > 1 {
			readTimeRoot[round.rootID] = true
		}
		if round.anchorID == round.promptID {
			continue
		}
		if carrier, ok := members[round.anchorID]; ok && carrier.anchorable() && carrier.root != round.rootID {
			readTimeRoot[round.rootID] = true
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
	writes := make([]subagentStampWrite, 0, len(writeIDs))
	for _, id := range writeIDs {
		row := members[id]
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
		}
		if row.stamped && row.stored == values {
			continue
		}
		writes = append(writes, subagentStampWrite{id: id, rev: row.rev, values: values})
	}
	return writes, nil
}

// subagentStampTargetsSQL reads local rows by primary key with their
// stamps; ?2 is a JSON array of ids. Only a tool call's meta is read,
// for its transcript root.
var subagentStampTargetsSQL = `SELECT i.id, i.kind, i.tool_name,
       CASE WHEN i.kind = 'tool_call' THEN COALESCE(` + aggTranscriptRootSQL("i.") + `, '') ELSE '' END,
       i.rev, s.item_id IS NOT NULL, COALESCE(s.state, 0),
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
		dest := append([]any{&row.id, &row.kind, &row.toolName, &row.root, &row.rev, &row.stamped, &row.stored.State},
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

// bumpSubagentAggregateRevSQL advances the thread stamp ahead of a Go
// write to subagent_aggregates, whose triggers stamp the anchors it
// writes with the thread's history_rev. Under bulk load the thread stamp
// is frozen, as the item triggers leave it.
const bumpSubagentAggregateRevSQL = `UPDATE threads SET history_rev = history_rev + 1
 WHERE id = ? AND history_bulk_load = 0`

// writeSubagentStampSQL replaces a row's stamp and moves its generation
// by one; a new row starts at generation one. ?3 < 0 writes
// unconditionally; otherwise the item must still be at the revision the
// values were computed from.
var writeSubagentStampSQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state, gen, ` +
	strings.Join(subagentAggregateValueColumns, ", ") + `)
SELECT ?1, ?2, ?4, 1` + strings.Repeat(", ?", len(subagentAggregateValueColumns)) + `
 WHERE EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND id = ?2 AND (?3 < 0 OR rev = ?3))
` + subagentAggregateUpsertSQL("gen = subagent_aggregates.gen + 1")

// writeSubagentStampsTx writes recomputed stamps and returns how many
// landed. An optimistic write skips a row that moved since it was read;
// that row is still dirty or unstamped and the next recompute takes it.
func writeSubagentStampsTx(tx *sql.Tx, threadID string, writes []subagentStampWrite, optimistic bool) (landed int, err error) {
	if len(writes) == 0 {
		return 0, nil
	}
	if _, err := tx.Exec(bumpSubagentAggregateRevSQL, threadID); err != nil {
		return 0, fmt.Errorf("store: bump history for subagent stamps in %s: %w", threadID, err)
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
		args := append([]any{threadID, write.id, rev, write.values.State}, write.values.args()...)
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

// settleSubagentAggregatesTx recomputes the thread's dirty anchors inside
// the caller's write transaction. A write path that can leave an anchor
// dirty calls it before it reads back or commits, so the dirty state never
// outlives the write that caused it. The probe is one keyed read of an
// index that is empty almost always.
func settleSubagentAggregatesTx(tx *sql.Tx, threadID string) error {
	const batch = 256
	for pass := 0; ; pass++ {
		ids, err := subagentAnchorIDs(tx, subagentDirtyAnchorsSQL, threadID, batch)
		if err != nil {
			return fmt.Errorf("store: select dirty subagent anchors for %s: %w", threadID, err)
		}
		if len(ids) == 0 {
			return nil
		}
		if pass > 0 && pass >= len(ids)+1 {
			return fmt.Errorf("store: dirty subagent anchors in %s do not settle: %v", threadID, ids)
		}
		if err := recomputeSubagentAggregatesTx(tx, threadID, ids); err != nil {
			return err
		}
	}
}

// recomputeSubagentAggregatesTx is RecomputeSubagentAggregates for the
// given anchors inside a caller's write transaction, which already holds
// the rows still.
func recomputeSubagentAggregatesTx(tx *sql.Tx, threadID string, ids []string) error {
	writes, err := computeSubagentStamps(tx, threadID, ids)
	if err != nil {
		return err
	}
	_, err = writeSubagentStampsTx(tx, threadID, writes, false)
	return err
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
// such an anchor: the insert trigger stamps or dirties an unstamped
// parent at its child's insert, and a carrier, unstamped until its
// prompt lands, is walked in every thread.
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

// markSubagentChainsDirtyTx marks dirty the local anchors above each
// given row that are clean or unstamped. It is for writes the triggers do
// not see: imported rows leaving or joining a local anchor's subtree. An
// unstamped anchor is included because rows joining it end the "no child
// since its insert" that lets a read skip its walk. The walk follows the
// logical parent chain, imported links included, by primary key.
func markSubagentChainsDirtyTx(tx *sql.Tx, threadID string, fromIDs []string) error {
	seen := make(map[string]bool)
	var chain []string
	for _, id := range fromIDs {
		for depth := 0; id != "" && depth < 64 && !seen[id]; depth++ {
			seen[id] = true
			chain = append(chain, id)
			var parent string
			query, args := timelineArms(threadID, timelineSelection{
				Columns:  func(string, string) string { return "items.parent_id" },
				KeyFirst: true,
				Where:    "items.id = ?", WhereArgs: []any{id},
			})
			err := tx.QueryRow(query, args...).Scan(&parent)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return fmt.Errorf("store: walk subagent chain %s/%s: %w", threadID, id, err)
			}
			id = parent
		}
	}
	if len(chain) == 0 {
		return nil
	}
	list, err := jsonList(chain)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(bumpSubagentAggregateRevSQL, threadID); err != nil {
		return fmt.Errorf("store: bump history for dirty subagent anchors in %s: %w", threadID, err)
	}
	if _, err := tx.Exec(markSubagentAnchorsDirtySQL, threadID, list); err != nil {
		return fmt.Errorf("store: mark subagent anchors dirty in %s: %w", threadID, err)
	}
	return nil
}

// markSubagentAnchorsDirtySQL marks dirty the anchors among the local rows
// the JSON array ?2 names, by primary key, that are clean or unstamped.
var markSubagentAnchorsDirtySQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state)
SELECT a.thread_id, a.id, ` + aggDirtyLiteral + ` FROM items a
 WHERE a.thread_id = ?1 AND a.id IN (SELECT value FROM json_each(?2)) AND ` + aggAnchorableSQL("a.") + `
ON CONFLICT (thread_id, item_id) DO UPDATE SET state = ` + aggDirtyLiteral + `, ` +
	strings.Join(subagentAggregateValueColumns, " = NULL, ") + ` = NULL
 WHERE subagent_aggregates.state = ` + aggCleanLiteral

// restampSubagentAggregatesTx rebuilds every stamp in a thread whose rows
// were loaded with the triggers' aggregate work suspended: its stamps are
// removed, then every anchor a read decorates is recomputed. It runs
// inside the loading transaction, before the thread leaves bulk load.
func restampSubagentAggregatesTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: clear loaded subagent stamps in %s: %w", threadID, err)
	}
	ids, err := subagentAnchorIDs(tx, subagentLegacyAnchorsSQL, threadID, -1)
	if err != nil {
		return fmt.Errorf("store: select loaded subagent anchors in %s: %w", threadID, err)
	}
	return recomputeSubagentAggregatesTx(tx, threadID, ids)
}
