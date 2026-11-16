package store

// Migration v156 refills the thread-find candidate text of the top-level
// user rows. A user row's candidate text now also holds what its row shows
// that its summary does not hold as written (find_candidate.go): an agent
// message's header line, read from the row's meta by the rule the timeline
// renders it by, a provider's agent message's first line with its white
// space collapsed, and the decoded data of a report a person's message was
// joined after. v150's backfill filled these rows from their summary alone,
// so a header line that collapsed a white space the loose form keeps, such
// as a no-break or an ideographic space, was no candidate.
//
// The SQL unsets the candidate text of those rows, which idx_items_user_text
// lists; a find computes it from the row until it is filled
// (findMatchSQL), and the deferred phase fills it again with v150's
// backfill, which finds the rows through its pending indexes.
const userFindTextMigrationVersion = 156

const userFindTextV156SQL = `
UPDATE items SET find_text = NULL
 WHERE kind = 'user_text' AND parent_id = '' AND find_text IS NOT NULL;
`
