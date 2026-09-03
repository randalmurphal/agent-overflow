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
// unordered set reads, `EXISTS` probes, and single-row lookups.
// `TestTimelineArmSelectionsWalkIndexes` is the tripwire that keeps the
// class from coming back.

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

// timelineSelection describes one ordered/limited read of a thread's
// logical timeline. It is rendered once per physical arm, so a predicate
// or a projection is written once and cannot drift between them.
type timelineSelection struct {
	// Columns renders the arm's projection. Its argument is the
	// expression that yields the row's logical thread id (`items` owns
	// it locally; the chunk reference owns it for imported rows) — most
	// callers ignore it. Every other column is written against the
	// `items` alias, which BOTH arms carry.
	//
	// The projection MUST name every ORDER BY key: a compound's ORDER BY
	// can only reference its own result columns, and it is precisely
	// ordering by a column the subquery does not return that forces the
	// temp b-tree this helper exists to avoid.
	Columns func(threadIDExpr string) string

	// Source, when non-empty, is a row source CROSS JOINed AHEAD of the
	// timeline table in both arms (`rel` for the subagent descendant
	// walk). CROSS JOIN is a planner directive: it pins the caller's
	// small driving set on the outer side instead of letting the planner
	// rescan the thread per row.
	Source string

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
// order: thread id and the predicate args once per arm, then the limit.
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
	sql := `SELECT ` + sel.Columns("items.thread_id") + `
		  FROM ` + source + `items
		 WHERE items.thread_id = ?` + where + `
		UNION ALL
		SELECT ` + sel.Columns("refs.thread_id") + `
		  FROM ` + source + `thread_import_chunks refs
		  JOIN import_history_items items ON items.chunk_id = refs.chunk_id
		 WHERE refs.thread_id = ?` + where + `
		   AND ` + importedNotOverridden
	if sel.OrderBy != "" {
		sql += "\n		 ORDER BY " + sel.OrderBy
	}
	args := make([]any, 0, 2*(len(sel.WhereArgs)+1)+1)
	args = append(args, threadID)
	args = append(args, sel.WhereArgs...)
	args = append(args, threadID)
	args = append(args, sel.WhereArgs...)
	if sel.Limit > 0 {
		sql += "\n		 LIMIT ?"
		args = append(args, sel.Limit)
	}
	return sql, args
}

// timelineIDColumns is the projection every id-selection uses: the id the
// hydrator resolves, plus the two ordering keys the compound needs to
// merge its arms instead of sorting them. Callers wrap the compound in
// `SELECT id FROM (…)` so the hydrator still sees a single `id` column.
func timelineIDColumns(string) string {
	return `items.id AS id, items.turn_index AS turn_index, items.item_index AS item_index`
}

// timelineIDSelection is the whole shape of an ordered, limited id page:
// the compound wrapped so it presents one `id` column.
func timelineIDSelection(threadID string, sel timelineSelection) (string, []any) {
	sel.Columns = timelineIDColumns
	sql, args := timelineArms(threadID, sel)
	return "SELECT id FROM (\n" + sql + "\n)", args
}
