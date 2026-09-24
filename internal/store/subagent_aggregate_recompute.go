package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// subagentStampState is subagentAggregateState as stored. Pick is
// [turn_index, item_index, id]; positions are [turn_index, item_index].
type subagentStampState struct {
	Gen              int64  `json:"gen"`
	Dirty            bool   `json:"dirty,omitempty"`
	ReadTime         bool   `json:"readTime,omitempty"`
	Pick             []any  `json:"pick,omitempty"`
	Newest           []int  `json:"newest,omitempty"`
	TranscriptNewest []int  `json:"transcriptNewest,omitempty"`
	ToolPick         string `json:"toolPick,omitempty"`
}

// subagentStampMode is how a read treats a local anchorable row.
type subagentStampMode int

const (
	// subagentUnstamped: no state. A plain tool call, an anchor in a
	// thread still listed for the backfill, or a carrier whose round the
	// triggers do not maintain.
	subagentUnstamped subagentStampMode = iota
	// subagentStampClean: the stored keys are the read.
	subagentStampClean
	// subagentStampWalk: dirty or readTime; the read-time aggregator
	// answers.
	subagentStampWalk
)

func subagentStampModeOf(meta string) subagentStampMode {
	if !strings.Contains(meta, metaKeySubagentAggregateState) {
		return subagentUnstamped
	}
	var decoded struct {
		State *subagentStampState `json:"subagentAggregateState"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil || decoded.State == nil {
		return subagentUnstamped
	}
	if decoded.State.Dirty || decoded.State.ReadTime {
		return subagentStampWalk
	}
	return subagentStampClean
}

// subagentStampValues is the part of a stamp a recompute derives: the six
// flat keys (absent ones omitted) and the state without its generation.
type subagentStampValues struct {
	Count           *int               `json:"subagentDescendantCount,omitempty"`
	Summary         string             `json:"subagentLatestChildSummary,omitempty"`
	TranscriptCount *int               `json:"subagentTranscriptDescendantCount,omitempty"`
	ToolSummary     string             `json:"subagentLatestToolSummary,omitempty"`
	ToolTurnIndex   *int               `json:"subagentLatestToolTurnIndex,omitempty"`
	ToolItemIndex   *int               `json:"subagentLatestToolItemIndex,omitempty"`
	State           subagentStampState `json:"subagentAggregateState"`
}

// storedSubagentStampValues reads the same shape back from a row's meta,
// generation zeroed, so a recompute can skip rows it would not change.
func storedSubagentStampValues(meta string) (subagentStampValues, bool) {
	var stored subagentStampValues
	var presence struct {
		State json.RawMessage `json:"subagentAggregateState"`
	}
	if json.Unmarshal([]byte(meta), &presence) != nil || len(presence.State) == 0 {
		return stored, false
	}
	if json.Unmarshal([]byte(meta), &stored) != nil {
		return stored, false
	}
	stored.State.Gen = 0
	return stored, true
}

func (v subagentStampValues) equal(other subagentStampValues) bool {
	a, errA := json.Marshal(v)
	b, errB := json.Marshal(other)
	return errA == nil && errB == nil && string(a) == string(b)
}

// subagentStampTarget is a local row a recompute may write.
type subagentStampTarget struct {
	id, kind, toolName, meta string
	rev                      int64
}

func (t subagentStampTarget) anchorable() bool {
	return t.kind == "tool_call" && t.toolName != "collab_agent"
}

// transcriptRoot is aggCarrierSQL in Go: the root a carrier's round is
// counted under, or "" for any row that is not a carrier.
func (t subagentStampTarget) transcriptRoot() string {
	if root := transcriptRootFromMeta(t.meta); root != t.id {
		return root
	}
	return ""
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
// A family is marked readTime when its rounds take a shape the triggers do
// not maintain: a prompt in the immutable history arm (the round probe
// reads local rows only), a prompt naming the root itself, one carrier
// named by two prompts, a named carrier stamped as another root's, or a
// root outside the local overlay. A carrier no prompt names shows the
// whole transcript, which no incremental rule keeps, and is readTime on
// its own.
func computeSubagentStamps(q sqlQueryer, threadID string, seedIDs []string) ([]subagentStampWrite, error) {
	seeds, err := subagentStampTargets(q, threadID, seedIDs)
	if err != nil {
		return nil, err
	}
	rootSet := make(map[string]struct{}, len(seeds))
	var roots []string
	addRoot := func(id string) {
		if _, ok := rootSet[id]; !ok {
			rootSet[id] = struct{}{}
			roots = append(roots, id)
		}
	}
	for _, seed := range seeds {
		if !seed.anchorable() {
			continue
		}
		if root := seed.transcriptRoot(); root != "" {
			addRoot(root)
		} else {
			addRoot(seed.id)
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
		if row, ok := members[root]; !ok || !row.anchorable() || row.transcriptRoot() != "" {
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
		if carrier, ok := members[round.anchorID]; ok && carrier.anchorable() && carrier.transcriptRoot() != round.rootID {
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
		values := subagentStampValues{State: subagentStampState{ReadTime: true}}
		root := row.transcriptRoot()
		clean := false
		if root == "" {
			_, isRoot := rootSet[id]
			clean = isRoot && !readTimeRoot[id]
		} else {
			_, bounded := accumulators[id]
			clean = !readTimeRoot[root] && bounded && named[id] == 1
		}
		if clean {
			values = subagentStampValues{}
			var acc subagentAggregateAccumulator
			if found := accumulators[id]; found != nil {
				acc = *found
			}
			transcript := transcripts[id]
			if acc.aggregate.descendantCount > 0 || transcript != nil {
				count := acc.aggregate.descendantCount
				values.Count = &count
			}
			values.Summary = acc.aggregate.latestChildSummary
			if acc.hasPick {
				values.State.Pick = []any{acc.preview.turnIndex, acc.preview.itemIndex, acc.preview.id}
			}
			if acc.hasNewest {
				values.State.Newest = []int{acc.newest.TurnIndex, acc.newest.ItemIndex}
			}
			if transcript != nil {
				total := transcript.aggregate.descendantCount
				values.TranscriptCount = &total
				if transcript.hasNewest {
					values.State.TranscriptNewest = []int{transcript.newest.TurnIndex, transcript.newest.ItemIndex}
				}
			}
			if tool, ok := tools[id]; ok {
				turn, item := tool.turnIndex, tool.itemIndex
				values.ToolSummary, values.ToolTurnIndex, values.ToolItemIndex = tool.summary, &turn, &item
				values.State.ToolPick = tool.id
			}
		}
		// A normalized round trip makes the comparison below exact: the
		// stored form went through the same encoding.
		values = normalizedSubagentStampValues(values)
		if stored, ok := storedSubagentStampValues(row.meta); ok && stored.equal(values) {
			continue
		}
		writes = append(writes, subagentStampWrite{id: id, rev: row.rev, values: values})
	}
	return writes, nil
}

func normalizedSubagentStampValues(v subagentStampValues) subagentStampValues {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out subagentStampValues
	if json.Unmarshal(data, &out) != nil {
		return v
	}
	return out
}

// subagentStampTargets loads local rows by id.
func subagentStampTargets(q sqlQueryer, threadID string, ids []string) (map[string]subagentStampTarget, error) {
	out := make(map[string]subagentStampTarget, len(ids))
	for start := 0; start < len(ids); start += 256 {
		clause, args := inClause("id", ids[start:min(start+256, len(ids))])
		rows, err := q.Query(`SELECT id, kind, tool_name, meta, rev FROM items WHERE thread_id = ? AND `+clause,
			append([]any{threadID}, args...)...)
		if err != nil {
			return nil, fmt.Errorf("store: read subagent stamp targets for %s: %w", threadID, err)
		}
		for rows.Next() {
			var row subagentStampTarget
			if err := rows.Scan(&row.id, &row.kind, &row.toolName, &row.meta, &row.rev); err != nil {
				return nil, errors.Join(fmt.Errorf("store: scan subagent stamp target: %w", err), rows.Close())
			}
			out[row.id] = row
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("store: iterate subagent stamp targets for %s: %w", threadID, err)
		}
	}
	return out, nil
}

// subagentCarriersOf lists the local carriers stamped with each root, by
// idx_items_transcript_root.
func subagentCarriersOf(q sqlQueryer, threadID string, roots []string) (map[string][]string, error) {
	out := make(map[string][]string)
	for start := 0; start < len(roots); start += 256 {
		clause, args := inClause(transcriptRootExpr, roots[start:min(start+256, len(roots))])
		rows, err := q.Query(`SELECT id, `+transcriptRootExpr+` FROM items WHERE thread_id = ? AND `+clause,
			append([]any{threadID}, args...)...)
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
	}
	return out, nil
}

// writeSubagentStampSQL replaces a row's stamp and moves its generation
// by one, which is how the update trigger tells an owner's write from a
// stale whole-meta write it must undo. ?4 < 0 writes unconditionally;
// otherwise the row must still be at the revision the values were
// computed from.
var writeSubagentStampSQL = `UPDATE items
   SET meta = json_set(
         json_patch(json_remove(meta, ` + aggQuotedPaths(append(slices.Clone(aggKeyPaths), aggStatePath)...) + `), ?1),
         '` + aggGenPath + `', COALESCE(` + aggJX("meta", aggGenPath) + `, 0) + 1)
 WHERE thread_id = ?2 AND id = ?3 AND (?4 < 0 OR rev = ?4)`

// writeSubagentStampsTx writes recomputed stamps and returns how many
// landed. An optimistic write skips a row that moved since it was read;
// that row is still dirty or unstamped and the next recompute takes it.
//
// The write is prepared once per call: preparing an UPDATE on items
// compiles the item triggers, which costs more than the write itself.
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
		patch, err := json.Marshal(write.values)
		if err != nil {
			return landed, fmt.Errorf("store: encode subagent stamp %s/%s: %w", threadID, write.id, err)
		}
		rev := int64(-1)
		if optimistic {
			rev = write.rev
		}
		result, err := stmt.Exec(string(patch), threadID, write.id, rev)
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
// idx_items_subagent_aggregate_dirty.
const subagentDirtyAnchorsSQL = `SELECT id FROM items WHERE thread_id = ? AND ` + subagentAggregateDirtyPredicate + ` LIMIT ?`

// subagentLegacyAnchorsSQL finds a listed thread's anchors that predate
// the stamps: unstamped local anchorable rows that a read decorates,
// because they are carriers or have a visible child in either arm. The
// candidates are the thread's distinct parent ids, read from the covering
// parent indexes of both arms, and its carriers (idx_items_transcript_root);
// each candidate costs a primary-key probe and a one-row child probe.
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
   AND ` + aggJT("a.meta", aggStatePath) + ` IS NULL
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
		if err := rtx.QueryRow(`SELECT EXISTS(SELECT 1 FROM subagent_aggregate_backfill WHERE thread_id = ?)`, threadID).Scan(&listed); err != nil {
			return fmt.Errorf("store: probe subagent backfill for %s: %w", threadID, err)
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
	encoded, err := json.Marshal(chain)
	if err != nil {
		return fmt.Errorf("store: encode subagent chain in %s: %w", threadID, err)
	}
	if _, err := tx.Exec(`UPDATE items SET meta = json_set(json_remove(meta, `+aggQuotedPaths(aggKeyPaths...)+`),
	       '`+aggStatePath+`', json_object('gen', COALESCE(`+aggJX("meta", aggGenPath)+`, 0) + 1, 'dirty', json('true')))
	 WHERE thread_id = ? AND id IN (SELECT value FROM json_each(?)) AND `+aggAnchorableSQL("items.")+`
	   AND `+aggJT("items.meta", aggDirtyPath)+` IS NULL AND `+aggJT("items.meta", aggReadTimePath)+` IS NULL`,
		threadID, string(encoded)); err != nil {
		return fmt.Errorf("store: mark subagent anchors dirty in %s: %w", threadID, err)
	}
	return nil
}

// restampSubagentAggregatesTx rebuilds every stamp in a thread whose rows
// were loaded with the triggers' aggregate work suspended: stamps the rows
// arrived with are removed, then every anchor a read decorates is
// recomputed. It runs inside the loading transaction, before the thread
// leaves bulk load.
func restampSubagentAggregatesTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(`UPDATE items SET meta = `+aggStripMetaSQL("meta")+`
	 WHERE thread_id = ? AND `+aggHasKeysSQL("meta"), threadID); err != nil {
		return fmt.Errorf("store: clear loaded subagent stamps in %s: %w", threadID, err)
	}
	ids, err := subagentAnchorIDs(tx, subagentLegacyAnchorsSQL, threadID, -1)
	if err != nil {
		return fmt.Errorf("store: select loaded subagent anchors in %s: %w", threadID, err)
	}
	return recomputeSubagentAggregatesTx(tx, threadID, ids)
}
