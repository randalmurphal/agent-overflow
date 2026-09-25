package store

// Payload arms: a thread's logical payloads read from their physical
// sources, as timeline_arms.go reads its rows.

// timelinePayloadArms renders a thread's logical payloads whose id
// matches idWhere (a predicate on `p.id` that pins it: `p.id = ?` or an IN
// list), the way the timeline_payloads view resolves them: the thread's
// own row, else the imported row, else, for a fork, the nearest ancestor's
// (inheritedPayloadVisibleSQL). The imported arms start from
// idx_import_history_payloads_id and probe the chunk's membership in the
// thread or the ancestor, where the view would probe every chunk the thread
// references. The lineage arms read every level: a payload lookup is not
// ordered. Only a fork renders them, as resolveSelectedTimelineSQL does its
// lineage branches; q reads the depth. columns is written against alias
// `p` with the arm's thread id and data length expressions.
func timelinePayloadArms(q sqlQueryer, threadID string, columns func(threadIDExpr, dataLengthExpr string) string, idWhere string, idArgs []any) (string, []any, error) {
	depth, err := forkLineageDepth(q, threadID)
	if err != nil {
		return "", nil, err
	}
	arms := 2
	sql := `SELECT ` + columns("p.thread_id", "length(p.data)") + `
		  FROM payloads p
		 WHERE p.thread_id = ? AND ` + idWhere + `
		UNION ALL
		SELECT ` + columns("refs.thread_id", "length(p.data)") + `
		  FROM import_history_payloads p
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = p.chunk_id
		 WHERE refs.thread_id = ? AND ` + idWhere + `
		   AND NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = refs.thread_id AND local.id = p.id)`
	if depth > 0 {
		arms = 4
		sql += `
		UNION ALL
		SELECT ` + columns("l.thread_id", "length(p.data)") + `
		  FROM thread_fork_lineage l
		  CROSS JOIN payloads p ON p.thread_id = l.ancestor_id
		 WHERE l.thread_id = ? AND ` + idWhere + `
		   AND ` + inheritedPayloadVisibleSQL("p.id") + `
		UNION ALL
		SELECT ` + columns("l.thread_id", "length(p.data)") + `
		  FROM thread_fork_lineage l
		  CROSS JOIN import_history_payloads p
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = p.chunk_id AND refs.thread_id = l.ancestor_id
		 WHERE l.thread_id = ? AND ` + idWhere + `
		   AND NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = refs.thread_id AND local.id = p.id)
		   AND ` + inheritedPayloadVisibleSQL("p.id")
	}
	return sql, repeatArgs(arms, append([]any{threadID}, idArgs...)), nil
}

// payloadRowArmCount is the number of arms ownPayloadRowArms and
// inheritedPayloadRowArms each render; every arm binds (thread, payload).
const payloadRowArmCount = 3

// ownPayloadRowArms renders a thread's own rows that name a payload as
// their payload or input payload, projected by columns (written against
// `items`). Local rows are read through the two partial payload indexes,
// one arm each: an OR of the two sends SQLite to the thread's whole index.
// Imported rows start from the payload's chunk rows
// (idx_import_history_payloads_id), keep the chunks the thread references,
// and scan only those chunks' rows. A row that names the payload in both
// columns appears twice.
func ownPayloadRowArms(columns string) string {
	return `SELECT ` + columns + `
		  FROM items
		 WHERE items.thread_id = ? AND items.payload_id = ?
		UNION ALL
		SELECT ` + columns + `
		  FROM items
		 WHERE items.thread_id = ? AND items.input_payload_id = ?
		UNION ALL
		SELECT ` + columns + `
		  FROM import_history_payloads ref_payload
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
		  CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
		 WHERE refs.thread_id = ? AND ref_payload.id = ?
		   AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
		   AND ` + importedNotOverridden
}

// inheritedPayloadRowArms is ownPayloadRowArms for the rows a fork shows
// from its ancestors on the lineage levels `levels` (a predicate on `l`, or
// "") selects, under the lineage arms' visibility rule. columns may also
// name the lineage row `l`. thread and payload are SQL expressions, each
// arm's viewer and payload id: "?" binds them per arm. The ancestor's rows
// are found through the payload keys, never through the ancestor's whole
// thread.
func inheritedPayloadRowArms(columns, levels, thread, payload string) string {
	return `SELECT ` + columns + `
		  FROM thread_fork_lineage l
		  CROSS JOIN items ON items.thread_id = l.ancestor_id
		 WHERE l.thread_id = ` + thread + levels + ` AND items.payload_id = ` + payload + `
		   AND ` + inheritedKeyedItemVisibleSQL + `
		UNION ALL
		SELECT ` + columns + `
		  FROM thread_fork_lineage l
		  CROSS JOIN items ON items.thread_id = l.ancestor_id
		 WHERE l.thread_id = ` + thread + levels + ` AND items.input_payload_id = ` + payload + `
		   AND ` + inheritedKeyedItemVisibleSQL + `
		UNION ALL
		SELECT ` + columns + `
		  FROM thread_fork_lineage l
		  CROSS JOIN import_history_payloads ref_payload
		  CROSS JOIN thread_import_chunks refs
		     ON refs.chunk_id = ref_payload.chunk_id AND refs.thread_id = l.ancestor_id
		  CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
		 WHERE l.thread_id = ` + thread + levels + ` AND ref_payload.id = ` + payload + `
		   AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
		   AND ` + importedNotOverridden + `
		   AND ` + inheritedKeyedItemVisibleSQL
}

// logicalPayloadReferenceSQL is true while a logical timeline row of
// thread (an SQL expression) names payload (an SQL expression) as its
// payload or input payload. Local rows answer through the two partial
// payload indexes on items. An imported row's payload lives in its own
// chunk, so the imported probe starts from the payload's chunk rows
// (idx_import_history_payloads_id), keeps chunks the thread references,
// and scans only those chunks' rows. A fork's inherited rows never name
// one of the fork's own payloads: the fork takes a payload together with
// the rows that reference it (shadowInheritedPayloadTx, snapshotRowsTx).
func logicalPayloadReferenceSQL(thread, payload string) string {
	return `(EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = ` + thread + ` AND ref.payload_id = ` + payload + `)
	      OR EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = ` + thread + ` AND ref.input_payload_id = ` + payload + `)
	      OR ` + importedPayloadReferenceSQL(thread, payload) + `)`
}

// importedPayloadReferenceSQL is logicalPayloadReferenceSQL's imported
// probe: a row of the thread's imported history names the payload.
func importedPayloadReferenceSQL(thread, payload string) string {
	return `EXISTS (SELECT 1 FROM import_history_payloads ref_payload
	                  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
	                  CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
	                 WHERE ref_payload.id = ` + payload + ` AND refs.thread_id = ` + thread + `
	                   AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
	                   AND ` + importedNotOverridden + `)`
}
