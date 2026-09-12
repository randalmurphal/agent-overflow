package store

// Token-only usage is priced by the row's own date, so the pinned catalog
// version is no longer needed. Pending rows of turns that already settled
// are stale: settlement now retires them, and this clears the ones left
// behind before it did.
const usageDatedPricingV96SQL = `
DROP VIEW usage_records;
ALTER TABLE usage_ledger DROP COLUMN pricing_version;
CREATE VIEW usage_records AS
    SELECT created_at, thread_id, project_id, work_item_id, turn_id, provider, model,
           input_tokens, output_tokens, cache_read_input_tokens,
           cache_creation_input_tokens, reasoning_output_tokens, cost_usd, cost_source,
           0 AS pending
    FROM usage_ledger
    UNION ALL
    SELECT created_at, thread_id, project_id, work_item_id, turn_id, provider, model,
           input_tokens, output_tokens, cache_read_input_tokens,
           cache_creation_input_tokens, reasoning_output_tokens, 0 AS cost_usd,
           'pending' AS cost_source, 1 AS pending
    FROM usage_pending;
DELETE FROM usage_pending WHERE turn_id <> '' AND EXISTS (
    SELECT 1 FROM usage_ledger l
    WHERE l.thread_id = usage_pending.thread_id AND l.turn_id = usage_pending.turn_id);
`
