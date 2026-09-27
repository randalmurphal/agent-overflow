package store

// Migration v135 indexes the tool_result payloads whose inline diff is
// summary-only. A Codex turn diff arrives after every file change and can
// upgrade only those rows (ListTurnSummaryOnlyDiffItems). Without the index
// each turn diff read the whole turn, so a long turn's diffs cost the square
// of its length.
const summaryOnlyDiffIndexMigrationVersion = 135

// summaryOnlyDiffPayloadSQL is the index's predicate over the `payloads`
// alias. A query repeats it verbatim for the planner to use the index.
const summaryOnlyDiffPayloadSQL = `payloads.kind = 'tool_result'
   AND CASE WHEN json_valid(payloads.meta) THEN json_extract(payloads.meta, '$.inlineDiff.availability') END = 'summary_only'`

const summaryOnlyDiffIndexV135SQL = `CREATE INDEX idx_payloads_summary_only_diff
    ON payloads(thread_id, id)
 WHERE kind = 'tool_result'
   AND CASE WHEN json_valid(meta) THEN json_extract(meta, '$.inlineDiff.availability') END = 'summary_only'`
