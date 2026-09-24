package store

// The `timeline_items` VIEW is a `UNION ALL` of two PHYSICAL sources: the
// thread-owned `items` overlay and the shared immutable import history
// (`thread_import_chunks` JOIN `import_history_items`, minus this thread's
// `thread_import_item_overrides`). It is the right way to express a
// SET of logical rows, and the WRONG way to express an ORDERED, LIMITED
// one.
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
// their plans.

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

// importedItemRevExpr is the imported arm's `items.rev`. Imported history
// rows live in shared immutable chunks keyed by chunk id, so there is no
// thread-scoped place to stamp a per-row revision on them the way the item
// triggers stamp `items.rev` (docs/architecture/thread-replica-sync.md
// §3.1). -1 is the honest answer rather than a fabricated number: it can
// never equal a real stamp, and window verification refuses any window that
// contains one instead of treating an unstampable row as proof of freshness.
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
	// reference owns it for imported rows) and the row revision
	// (`items.rev` locally, importedItemRevExpr for imported history) —
	// most callers ignore both. Every other column is written against
	// the `items` alias, which BOTH arms carry.
	//
	// The projection MUST name every ORDER BY key: a compound's ORDER BY
	// can only reference its own result columns, and it is precisely
	// ordering by a column the subquery does not return that forces the
	// temp b-tree this helper exists to avoid.
	Columns func(threadIDExpr, revExpr string) string

	// Source, when non-empty, is a row source CROSS JOINed AHEAD of the
	// timeline table in both arms (`rel` for the subagent descendant
	// walk). CROSS JOIN is a planner directive: it pins the caller's
	// small driving set on the outer side instead of letting the planner
	// rescan the thread per row.
	Source string

	// KeyFirst drives the imported arm from import_history_items through
	// an index that leads with a key Where pins (items.id, parent_id,
	// completion_of, meta.task_id, meta.transcript_root_id), then probes
	// the row's chunk for membership in the thread. The arm then costs
	// the matching rows, not one probe per chunk the thread references.
	// Where MUST pin such a key: without one the arm reads every imported
	// row in the database.
	KeyFirst bool

	// Turn, when non-empty, pins the selection to one turn, or with
	// FromTurn to that turn and every later one. It is an SQL
	// expression, "?" or an outer query's column, and TurnArgs are its
	// bind values, emitted at each place the expression is rendered. The
	// imported arm reads only the chunk references whose turn range can
	// hold such a turn (idx_thread_import_chunks_turns), so a turn past
	// the thread's imported history reads no chunk at all.
	Turn     string
	TurnArgs []any
	FromTurn bool

	// Thread, when non-empty, is an outer query's column that names the
	// thread, for a selection correlated per outer row. It replaces the
	// bound thread id, and timelineArms then ignores its threadID
	// argument.
	Thread string

	// Where is the arm predicate, qualified with the `items.` alias and
	// EXCLUDING the thread-id term each arm supplies itself. The alias is
	// mandatory: the imported arm also has `refs` in scope, and an
	// unqualified column would be free to bind to it.
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

// timelineArms renders sel as `<local arm> UNION ALL <imported arm>
// ORDER BY … LIMIT …` and returns the SQL plus its bind values in wire
// order: per arm the thread id, the turn and the predicate args, then
// the limit.
//
// The rows and their order are identical to the same selection against
// `timeline_items`, because the arms ARE the view's arms: same overrides
// exclusion, same predicate, and (turn_index, item_index) is unique
// across both by the v61 overlap triggers, so nothing depends on how a
// tie breaks.
func timelineArms(threadID string, sel timelineSelection) (string, []any) {
	where := ""
	if sel.Where != "" {
		where = "\n		   AND " + sel.Where
	}
	source := ""
	if sel.Source != "" {
		source = sel.Source + "\n		  CROSS JOIN "
	}
	importedSource := source + "thread_import_chunks refs\n\t\t  JOIN import_history_items items ON items.chunk_id = refs.chunk_id"
	switch {
	case sel.Source != "" || sel.KeyFirst:
		importedSource = source + "import_history_items items\n\t\t  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id"
	case sel.Turn != "":
		importedSource = "thread_import_chunks refs\n\t\t  CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id"
	}
	localTurn, importedTurn := "", ""
	importedTurnRenders := 0
	switch {
	case sel.Turn != "" && sel.FromTurn:
		localTurn = "\n		   AND items.turn_index >= " + sel.Turn
		importedTurn = "\n		   AND refs.max_turn_index >= " + sel.Turn +
			"\n		   AND items.turn_index >= " + sel.Turn
		importedTurnRenders = 2
	case sel.Turn != "":
		localTurn = "\n		   AND items.turn_index = " + sel.Turn
		importedTurn = "\n		   AND " + importedTurnRange(sel.Turn) +
			"\n		   AND items.turn_index = " + sel.Turn
		importedTurnRenders = 3
	}
	thread := "?"
	var threadArgs []any
	if sel.Thread != "" {
		thread = sel.Thread
	} else {
		threadArgs = []any{threadID}
	}
	sql := `SELECT ` + sel.Columns("items.thread_id", "items.rev") + `
		  FROM ` + source + `items
		 WHERE items.thread_id = ` + thread + localTurn + where + `
		UNION ALL
		SELECT ` + sel.Columns("refs.thread_id", importedItemRevExpr) + `
		  FROM ` + importedSource + `
		 WHERE refs.thread_id = ` + thread + importedTurn + where + `
		   AND ` + importedNotOverridden
	if sel.OrderBy != "" {
		sql += "\n		 ORDER BY " + sel.OrderBy
	}
	turnArgs := 0
	if sel.Turn != "" {
		turnArgs = len(sel.TurnArgs)
	}
	args := make([]any, 0, 2*(len(sel.WhereArgs)+1)+(1+importedTurnRenders)*turnArgs+1)
	args = append(args, threadArgs...)
	args = append(args, sel.TurnArgs[:turnArgs]...)
	args = append(args, sel.WhereArgs...)
	args = append(args, threadArgs...)
	for range importedTurnRenders {
		args = append(args, sel.TurnArgs[:turnArgs]...)
	}
	args = append(args, sel.WhereArgs...)
	if sel.Limit > 0 {
		sql += "\n		 LIMIT ?"
		args = append(args, sel.Limit)
	}
	return sql, args
}

// importedTurnRange limits an imported arm's chunk references (`refs`)
// to the ones whose turn range can hold turn, an SQL expression rendered
// twice. idx_thread_import_chunks_turns ranges over the leading
// max_turn_index term, so a turn past the imported history matches no
// reference.
func importedTurnRange(turn string) string {
	return "refs.max_turn_index >= " + turn + " AND refs.min_turn_index <= " + turn
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
func timelineIDSelection(threadID string, sel timelineSelection) (string, []any) {
	sel.Columns = timelineIDColumns
	sql, args := timelineArms(threadID, sel)
	return "SELECT id FROM (\n" + sql + "\n)", args
}

// turnIDSelection selects the ids of one turn's logical rows.
func turnIDSelection(threadID string, turnIndex int) (string, []any) {
	return timelineIDSelection(threadID, timelineSelection{Turn: "?", TurnArgs: []any{turnIndex}})
}

// timelineKeyedIDSelection selects the ids of the few rows a key pins,
// ordered by orderBy (over result columns of project) and cut to limit.
// The materialized key rows are ordered afterwards: ordering the arms
// themselves would invite the local arm to walk its ordering index and
// test the key per row instead.
func timelineKeyedIDSelection(threadID string, project, where string, whereArgs []any, orderBy string, limit int) (string, []any) {
	sql, args := timelineArms(threadID, timelineSelection{
		Columns:   func(string, string) string { return "items.id AS id, " + project },
		KeyFirst:  true,
		Where:     where,
		WhereArgs: whereArgs,
	})
	return "WITH keyed AS MATERIALIZED (\n" + sql + "\n) SELECT id FROM keyed ORDER BY " + orderBy + " LIMIT ?", append(args, limit)
}

// turnAggregateQuery renders aggregate(column) over one turn's logical
// rows, such as MAX(item_index); NULL when the turn has none.
func turnAggregateQuery(threadID string, turnIndex int, aggregate, column string) (string, []any) {
	sql, args := timelineArms(threadID, timelineSelection{
		Columns:  func(string, string) string { return "items." + column + " AS " + column },
		Turn:     "?",
		TurnArgs: []any{turnIndex},
	})
	return "SELECT " + aggregate + "(" + column + ") FROM (\n" + sql + "\n)", args
}

// timelinePayloadArms renders a thread's logical payloads whose id
// matches idWhere (a predicate on `p.id` that pins it: `p.id = ?` or an IN
// list), the way the timeline_payloads view resolves them: the thread's
// own row, resolved through payload snapshots, or else the imported row.
// The imported arm starts from idx_import_history_payloads_id and probes
// the chunk's membership in the thread, where the view would probe every
// chunk the thread references. columns is written against alias `p`
// with the arm's thread id and data length expressions.
func timelinePayloadArms(threadID string, columns func(threadIDExpr, dataLengthExpr string) string, idWhere string, idArgs []any) (string, []any) {
	sql := `SELECT ` + columns("p.thread_id", "p.data_length") + `
		  FROM resolved_payloads p
		 WHERE p.thread_id = ? AND ` + idWhere + `
		UNION ALL
		SELECT ` + columns("refs.thread_id", "length(p.data)") + `
		  FROM import_history_payloads p
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = p.chunk_id
		 WHERE refs.thread_id = ? AND ` + idWhere + `
		   AND NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = refs.thread_id AND local.id = p.id)`
	args := make([]any, 0, 2+2*len(idArgs))
	args = append(args, threadID)
	args = append(args, idArgs...)
	args = append(args, threadID)
	args = append(args, idArgs...)
	return sql, args
}

// logicalPayloadReferenceSQL is true while a logical timeline row of
// thread (an SQL expression) names payload (an SQL expression) as its
// payload or input payload. Local rows answer through the two partial
// payload indexes on items. An imported row's payload lives in its own
// chunk, so the imported probe starts from the payload's chunk rows
// (idx_import_history_payloads_id), keeps chunks the thread references,
// and scans only those chunks' rows.
func logicalPayloadReferenceSQL(thread, payload string) string {
	return `(EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = ` + thread + ` AND ref.payload_id = ` + payload + `)
	      OR EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = ` + thread + ` AND ref.input_payload_id = ` + payload + `)
	      OR EXISTS (SELECT 1 FROM import_history_payloads ref_payload
	                  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
	                  CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
	                 WHERE ref_payload.id = ` + payload + ` AND refs.thread_id = ` + thread + `
	                   AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
	                   AND ` + importedNotOverridden + `))`
}
