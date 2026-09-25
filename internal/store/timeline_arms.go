package store

import (
	"fmt"
	"strconv"
	"strings"
)

// The `timeline_items` VIEW is a `UNION ALL` of the PHYSICAL sources of a
// thread's rows: the thread-owned `items` overlay, the shared immutable
// import history (`thread_import_chunks` JOIN `import_history_items`, minus
// this thread's `thread_import_item_overrides`) and, for a pointer fork, the
// same two sources of every ancestor in its lineage, cut and filtered by
// inheritedItemVisibleSQL. It is the right way to express a SET of logical
// rows, and the WRONG way to express an ORDERED, LIMITED one.
//
// SQLite cannot push an outer `ORDER BY … LIMIT` into a compound view
// whose ordering keys are not part of the selected result: it runs both
// arms whole, pours them into a temp b-tree, and only then takes the
// first N. On a 67k-item thread the cold-open tail window read 18,079
// pages (74 MB) and 52 ms warm that way; the same rows selected as a
// TOP-LEVEL compound — each arm pre-sorted by its own index, `ORDER BY
// … LIMIT` applied to the compound — plan as `MERGE (UNION ALL)`, read
// 151 pages, and take 1-2 ms.
//
// timelineArms renders that compound. Every ordered or limited read of a
// thread's logical timeline goes through it; `timeline_items` stays for
// unordered whole-thread set reads and thread-level `EXISTS` probes.
// `TestTimelineArmSelectionsWalkIndexes` is the tripwire that keeps the
// class from coming back.
//
// The view's imported arm also starts from the thread's chunk references,
// so a lookup by id, by another key or by turn through it probes every
// chunk the thread references. Those lookups render the arms with
// KeyFirst or Turn instead; TestImportedLookupsDoNotEnumerateChunks pins
// their plans, and TestPointerForkLookupsProbeTheLineage pins the lineage
// arms' plans for a fork.

// importedNotOverridden is the imported arm's half of the view's
// semantics: a thread hides one imported row by writing a
// thread_import_item_overrides entry for it (the local `items` row that
// replaces it is already on the other arm). Every hand-written imported
// arm in this package states it, aliasing the chunk reference `refs` and
// the imported row `items`.
const importedNotOverridden = `NOT EXISTS (
		       SELECT 1 FROM thread_import_item_overrides o
		        WHERE o.thread_id = refs.thread_id AND o.item_id = items.id
		   )`

// inheritedKeyedItemVisibleSQL is inheritedItemVisibleSQL for a lineage arm
// a key drives: ids, a payload, a parent or another lookup key. SQLite has
// no statistics here and prices the cut, a range on the ancestor's
// idx_items_thread_turn_item_unique, below an id list or a partial key
// index, and so walks the ancestor's history below the cut. The unary plus
// keeps the cut a filter on the rows the key's index finds.
const inheritedKeyedItemVisibleSQL = `(+items.turn_index, +items.item_index) < (l.cut_turn_index, l.cut_item_index)
   AND ` + inheritedItemNotHiddenSQL

// importedItemRevExpr is the imported arm's `items.rev`. Imported history
// rows live in shared immutable chunks keyed by chunk id, so there is no
// thread-scoped place to stamp a per-row revision on them the way the item
// triggers stamp `items.rev` (docs/architecture/thread-replica-sync.md
// §3.1). -1 is the honest answer rather than a fabricated number: it can
// never equal a real stamp, and window verification refuses any window that
// contains one instead of treating an unstampable row as proof of freshness.
// A fork's inherited rows carry it too: they are stamped in their owner's
// history, not the fork's.
//
// The CAST is load-bearing. A bare literal has no affinity, `items.rev` has
// INTEGER affinity, and SQLite refuses to push WHERE terms into a UNION ALL
// whose arms disagree on any result column's affinity. A correlated join
// against `timeline_items` (threadColumns' proposed-plan probe, the
// ListThreadsWithItems EXISTS) then materializes the whole view per outer
// row: a full scan of `items` for every thread in the sidebar.
// TestTimelineItemsViewJoinPushesDown pins the plan.
const importedItemRevExpr = `CAST(-1 AS INTEGER)`

// timelineSelection describes one ordered/limited read of a thread's
// logical timeline. It is rendered once per physical arm, so a predicate
// or a projection is written once and cannot drift between them.
type timelineSelection struct {
	// Columns renders the arm's projection. Its arguments are the two
	// expressions an arm cannot write against the `items` alias: the
	// row's logical thread id (`items` owns it locally; the chunk
	// reference owns it for imported rows; the lineage row for a fork's
	// inherited rows) and the row revision (`items.rev` locally,
	// importedItemRevExpr for imported and inherited history) — most
	// callers ignore both. Every other column is written against the
	// `items` alias, which EVERY arm carries.
	//
	// The projection MUST name every ORDER BY key: a compound's ORDER BY
	// can only reference its own result columns, and it is precisely
	// ordering by a column the subquery does not return that forces the
	// temp b-tree this helper exists to avoid.
	Columns func(threadIDExpr, revExpr string) string

	// LocalJoin, when non-empty, is a join the own local arm's projection
	// reads, rendered after its items row: servedItemJoin for a
	// projection that serves meta. The imported and lineage arms have no
	// stamps: their rows read revision -1, which serves stored meta.
	LocalJoin string

	// Source, when non-empty, is a row source CROSS JOINed AHEAD of the
	// timeline table in every arm (`rel` for the subagent descendant
	// walk). CROSS JOIN is a planner directive: it pins the caller's
	// small driving set on the outer side instead of letting the planner
	// rescan the thread per row.
	Source string

	// KeyFirst drives the imported arms from import_history_items through
	// an index that leads with a key Where pins (items.id, parent_id,
	// completion_of, meta.task_id, meta.transcript_root_id), then probes
	// the row's chunk for membership in the thread (or, on a lineage arm,
	// in the ancestor). The arm then costs the matching rows, not one
	// probe per chunk the thread references. Where MUST pin such a key:
	// without one the arm reads every imported row in the database.
	KeyFirst bool

	// Turn, when non-empty, pins the selection to one turn, or with
	// FromTurn to that turn and every later one. It is an SQL
	// expression, "?" or an outer query's column, and TurnArgs are its
	// bind values, emitted at each place the expression is rendered. The
	// imported arms read only the chunk references whose turn range can
	// hold such a turn (idx_thread_import_chunks_turns), so a turn past
	// the thread's imported history reads no chunk at all, and a lineage
	// arm whose cut precedes the turn reads nothing.
	Turn     string
	TurnArgs []any
	FromTurn bool

	// Thread, when non-empty, is an outer query's column that names the
	// thread, for a selection correlated per outer row. It replaces the
	// bound thread id; such a selection renders through
	// correlatedTimelineArms.
	Thread string

	// Where is the arm predicate, qualified with the `items.` alias and
	// EXCLUDING the thread-id term each arm supplies itself. The alias is
	// mandatory: the imported arm also has `refs` in scope and a lineage
	// arm `l`, and an unqualified column would be free to bind to them.
	Where string

	// WhereArgs are Where's bind values. They are emitted once per arm.
	WhereArgs []any

	// OrderBy is the compound's ordering, written in the projection's
	// OUTPUT column names ("turn_index DESC, item_index DESC"). Empty
	// renders no ORDER BY — the shape a caller whose own window
	// functions do the ordering wants (the subagent aggregates), and the
	// only one that may leave it out.
	OrderBy string

	// Limit caps the compound. Non-positive renders no LIMIT clause;
	// a caller whose budget is caller-supplied must reject non-positive
	// values before it gets here.
	Limit int
}

// everyLineageLevel renders a fork's lineage as one pair of arms that reads
// every level. It is the correlated form: the outer row's thread, and so
// its depth, is not known when the SQL is written.
const everyLineageLevel = -1

// timelineArms renders sel as the UNION ALL of every physical source of
// threadID's logical timeline with `ORDER BY … LIMIT …` applied to the
// compound, and returns the SQL plus its bind values in wire order: per arm
// the thread id, the turn and the predicate args, then the limit.
//
// The sources are the thread's own `items`, its imported history and, for a
// pointer fork, one local and one imported arm per lineage level
// (lineageArms). The rows and their order are identical to the same
// selection against `timeline_items`, because the arms ARE the view's arms:
// same override and hidden-row exclusions, same predicate, and
// (turn_index, item_index) is unique across all of them by the v61 overlap
// triggers and the fork position triggers, so nothing depends on how a tie
// breaks.
//
// q reads the lineage depth; pass the transaction or pool that runs the
// rendered SQL. A thread that is not a fork renders exactly its own two
// arms. The arms join the lineage rows themselves, so a depth read in an
// earlier snapshot can only add a level that returns nothing.
func timelineArms(q sqlQueryer, threadID string, sel timelineSelection) (string, []any, error) {
	if sel.Thread != "" {
		return "", nil, fmt.Errorf("store: timeline selection correlated on %s renders through correlatedTimelineArms", sel.Thread)
	}
	depth, err := forkLineageDepth(q, threadID)
	if err != nil {
		return "", nil, err
	}
	sql, args := renderTimelineArms(threadID, depth, sel)
	return sql, args, nil
}

// correlatedTimelineArms renders a selection correlated on sel.Thread. Every
// lineage level reads through one pair of arms, so the selection is exact
// for any outer row; it is for existence probes and key lookups, not for an
// ordered page, which could not merge the levels by index.
func correlatedTimelineArms(sel timelineSelection) (string, []any) {
	return renderTimelineArms("", everyLineageLevel, sel)
}

// renderTimelineArms renders sel with levels lineage levels, or with
// everyLineageLevel as one pair over all of them.
func renderTimelineArms(threadID string, levels int, sel timelineSelection) (string, []any) {
	r := newArmRenderer(threadID, sel)
	r.ownArms()
	if levels == everyLineageLevel {
		r.lineageArms("")
	}
	// With a literal depth each level is its own pair, because a nearer
	// level can hold rows that replace inherited ones at the same
	// positions: one arm per level stays ordered by its own index, the
	// lineage probe is a one-row primary key lookup, and the compound
	// merges them.
	for level := 1; level <= levels; level++ {
		r.lineageArms("\n		   AND l.depth = " + strconv.Itoa(level))
	}
	return r.finish()
}

// inheritedTimelineArms renders only sel's lineage arms for threadID: the
// rows it shows from its ancestors, on the levels levelFilter (a predicate
// on the lineage row `l`, or "") selects. A fork's own writes to rows it
// inherits (fork_retract.go, snapshotRowsTx) use it, so they find
// inherited rows through the same sources and visibility rule as every
// timeline read.
func inheritedTimelineArms(threadID, levelFilter string, sel timelineSelection) (string, []any) {
	r := newArmRenderer(threadID, sel)
	r.lineageArms(levelFilter)
	return r.finish()
}

// ownTimelineArms renders only sel's own arms for threadID: its local rows
// and its imported history, without the rows it inherits.
func ownTimelineArms(threadID string, sel timelineSelection) (string, []any) {
	r := newArmRenderer(threadID, sel)
	r.ownArms()
	return r.finish()
}

// armRenderer is one selection rendered into arms: the row sources, turn
// bounds and bind values every arm of the selection shares.
type armRenderer struct {
	b                     armBuilder
	sel                   timelineSelection
	thread                string
	threadArgs            []any
	where                 string
	source                string
	importedSource        string
	lineageImportedSource string
	turnArgs              []any
	localTurn             string
	importedTurn          string
	lineageCut            string
	localTurnRenders      int
	importedTurnRenders   int
}

func newArmRenderer(threadID string, sel timelineSelection) *armRenderer {
	r := &armRenderer{sel: sel, thread: "?"}
	if sel.Thread != "" {
		r.thread = sel.Thread
	} else {
		r.threadArgs = []any{threadID}
	}
	if sel.Where != "" {
		r.where = "\n		   AND " + sel.Where
	}
	if sel.Source != "" {
		r.source = sel.Source + "\n		  CROSS JOIN "
	}
	r.importedSource = r.source + "thread_import_chunks refs\n\t\t  JOIN import_history_items items ON items.chunk_id = refs.chunk_id"
	r.lineageImportedSource = r.source + "thread_fork_lineage l\n\t\t  CROSS JOIN thread_import_chunks refs ON refs.thread_id = l.ancestor_id\n\t\t  JOIN import_history_items items ON items.chunk_id = refs.chunk_id"
	switch {
	case sel.Source != "" || sel.KeyFirst:
		r.importedSource = r.source + "import_history_items items\n\t\t  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id"
		r.lineageImportedSource = r.source + "thread_fork_lineage l\n\t\t  CROSS JOIN import_history_items items\n\t\t  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id AND refs.thread_id = l.ancestor_id"
	case sel.Turn != "":
		r.importedSource = "thread_import_chunks refs\n\t\t  CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id"
		r.lineageImportedSource = "thread_fork_lineage l\n\t\t  CROSS JOIN thread_import_chunks refs ON refs.thread_id = l.ancestor_id\n\t\t  CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id"
	}
	switch {
	case sel.Turn != "" && sel.FromTurn:
		r.localTurn = "\n		   AND items.turn_index >= " + sel.Turn
		r.importedTurn = "\n		   AND refs.max_turn_index >= " + sel.Turn +
			"\n		   AND items.turn_index >= " + sel.Turn
		r.localTurnRenders, r.importedTurnRenders = 1, 2
	case sel.Turn != "":
		r.localTurn = "\n		   AND items.turn_index = " + sel.Turn
		r.importedTurn = "\n		   AND " + importedTurnRange(sel.Turn) +
			"\n		   AND items.turn_index = " + sel.Turn
		r.localTurnRenders, r.importedTurnRenders = 1, 3
	}
	if sel.Turn != "" {
		r.turnArgs = sel.TurnArgs
		// An ancestor shows the fork nothing at or after its cut turn's
		// end, so a turn past the cut reads no ancestor row at all.
		r.lineageCut = "\n		   AND l.cut_turn_index >= " + sel.Turn
	}
	return r
}

// ownArms renders the thread's own local and imported arms.
func (r *armRenderer) ownArms() {
	sel := r.sel
	r.b.arm(`SELECT `+sel.Columns("items.thread_id", "items.rev")+`
		  FROM `+r.source+`items`+sel.LocalJoin+`
		 WHERE items.thread_id = `+r.thread+r.localTurn+r.where,
		r.threadArgs, repeatArgs(r.localTurnRenders, r.turnArgs), sel.WhereArgs)
	r.b.arm(`SELECT `+sel.Columns("refs.thread_id", importedItemRevExpr)+`
		  FROM `+r.importedSource+`
		 WHERE refs.thread_id = `+r.thread+r.importedTurn+r.where+`
		   AND `+importedNotOverridden,
		r.threadArgs, repeatArgs(r.importedTurnRenders, r.turnArgs), sel.WhereArgs)
}

// lineageArms renders the local and imported arms of the lineage levels
// level selects. A lookup (KeyFirst, or rows from Source) finds the
// ancestor's rows by its key and filters them by the cut; any other
// selection walks the ancestor's timeline index up to the cut.
func (r *armRenderer) lineageArms(level string) {
	sel := r.sel
	visible := inheritedItemVisibleSQL
	if sel.KeyFirst || sel.Source != "" {
		visible = inheritedKeyedItemVisibleSQL
	}
	r.b.arm(`SELECT `+sel.Columns("l.thread_id", importedItemRevExpr)+`
		  FROM `+r.source+`thread_fork_lineage l
		  CROSS JOIN items ON items.thread_id = l.ancestor_id
		 WHERE l.thread_id = `+r.thread+level+r.lineageCut+r.localTurn+r.where+`
		   AND `+visible,
		r.threadArgs, repeatArgs(1+r.localTurnRenders, r.turnArgs), sel.WhereArgs)
	r.b.arm(`SELECT `+sel.Columns("l.thread_id", importedItemRevExpr)+`
		  FROM `+r.lineageImportedSource+`
		 WHERE l.thread_id = `+r.thread+level+r.lineageCut+r.importedTurn+r.where+`
		   AND `+importedNotOverridden+`
		   AND `+visible,
		r.threadArgs, repeatArgs(1+r.importedTurnRenders, r.turnArgs), sel.WhereArgs)
}

// finish applies the compound's ordering and limit.
func (r *armRenderer) finish() (string, []any) {
	sql, args := r.b.sql.String(), r.b.args
	if r.sel.OrderBy != "" {
		sql += "\n		 ORDER BY " + r.sel.OrderBy
	}
	if r.sel.Limit > 0 {
		sql += "\n		 LIMIT ?"
		args = append(args, r.sel.Limit)
	}
	return sql, args
}

// armBuilder joins compound arms with UNION ALL and collects their bind
// values in the order the arms render them.
type armBuilder struct {
	sql  strings.Builder
	args []any
}

func (b *armBuilder) arm(sql string, args ...[]any) {
	if b.sql.Len() > 0 {
		b.sql.WriteString("\n		UNION ALL\n		")
	}
	b.sql.WriteString(sql)
	for _, a := range args {
		b.args = append(b.args, a...)
	}
}

// repeatArgs is n copies of args, for an expression rendered n times.
func repeatArgs(n int, args []any) []any {
	if n <= 0 || len(args) == 0 {
		return nil
	}
	out := make([]any, 0, n*len(args))
	for range n {
		out = append(out, args...)
	}
	return out
}

// importedTurnRange limits an imported arm's chunk references (`refs`)
// to the ones whose turn range can hold turn, an SQL expression rendered
// twice. idx_thread_import_chunks_turns ranges over the leading
// max_turn_index term, so a turn past the imported history matches no
// reference.
func importedTurnRange(turn string) string {
	return "refs.max_turn_index >= " + turn + " AND refs.min_turn_index <= " + turn
}

// forkLineageDepth is the number of ancestor levels threadID reads
// through: zero for every thread that is not a pointer fork.
func forkLineageDepth(q sqlQueryer, threadID string) (int, error) {
	var depth int
	if err := q.QueryRow(
		`SELECT COALESCE(MAX(depth), 0) FROM thread_fork_lineage WHERE thread_id = ?`, threadID,
	).Scan(&depth); err != nil {
		return 0, fmt.Errorf("store: read fork lineage depth for %s: %w", threadID, err)
	}
	return depth, nil
}

// timelineIDColumns is the projection every id-selection uses: the id the
// hydrator resolves, plus the two ordering keys the compound needs to
// merge its arms instead of sorting them. Callers wrap the compound in
// `SELECT id FROM (…)` so the hydrator still sees a single `id` column.
func timelineIDColumns(string, string) string {
	return `items.id AS id, items.turn_index AS turn_index, items.item_index AS item_index`
}

// timelineIDSelection is the whole shape of an ordered, limited id page:
// the compound wrapped so it presents one `id` column.
func timelineIDSelection(q sqlQueryer, threadID string, sel timelineSelection) (string, []any, error) {
	sel.Columns = timelineIDColumns
	sql, args, err := timelineArms(q, threadID, sel)
	if err != nil {
		return "", nil, err
	}
	return "SELECT id FROM (\n" + sql + "\n)", args, nil
}

// turnIDSelection selects the ids of one turn's logical rows.
func turnIDSelection(q sqlQueryer, threadID string, turnIndex int) (string, []any, error) {
	return timelineIDSelection(q, threadID, timelineSelection{Turn: "?", TurnArgs: []any{turnIndex}})
}

// timelineKeyedIDSelection selects the ids of the few rows a key pins,
// ordered by orderBy (over result columns of project) and cut to limit.
// The materialized key rows are ordered afterwards: ordering the arms
// themselves would invite the local arm to walk its ordering index and
// test the key per row instead.
func timelineKeyedIDSelection(q sqlQueryer, threadID string, project, where string, whereArgs []any, orderBy string, limit int) (string, []any, error) {
	sql, args, err := timelineArms(q, threadID, timelineSelection{
		Columns:   func(string, string) string { return "items.id AS id, " + project },
		KeyFirst:  true,
		Where:     where,
		WhereArgs: whereArgs,
	})
	if err != nil {
		return "", nil, err
	}
	return "WITH keyed AS MATERIALIZED (\n" + sql + "\n) SELECT id FROM keyed ORDER BY " + orderBy + " LIMIT ?", append(args, limit), nil
}

// turnAggregateQuery renders aggregate(column) over one turn's logical
// rows, such as MAX(item_index); NULL when the turn has none.
func turnAggregateQuery(q sqlQueryer, threadID string, turnIndex int, aggregate, column string) (string, []any, error) {
	sql, args, err := timelineArms(q, threadID, timelineSelection{
		Columns:  func(string, string) string { return "items." + column + " AS " + column },
		Turn:     "?",
		TurnArgs: []any{turnIndex},
	})
	if err != nil {
		return "", nil, err
	}
	return "SELECT " + aggregate + "(" + column + ") FROM (\n" + sql + "\n)", args, nil
}
