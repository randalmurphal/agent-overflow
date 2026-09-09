package store

// Earlier estimates were calculated at read time. Pin their initial pricing
// basis here; new rows stamp the catalog version used when they are recorded.
const usagePricingV93SQL = `
ALTER TABLE usage_ledger ADD COLUMN pricing_version TEXT NOT NULL DEFAULT '2026-09-09';
DROP VIEW usage_records;
CREATE VIEW usage_records AS
    SELECT created_at, thread_id, project_id, work_item_id, turn_id, provider, model,
           input_tokens, output_tokens, cache_read_input_tokens,
           cache_creation_input_tokens, reasoning_output_tokens, cost_usd, cost_source,
           0 AS pending, pricing_version
    FROM usage_ledger
    UNION ALL
    SELECT created_at, thread_id, project_id, work_item_id, turn_id, provider, model,
           input_tokens, output_tokens, cache_read_input_tokens,
           cache_creation_input_tokens, reasoning_output_tokens, 0 AS cost_usd,
           'pending' AS cost_source, 1 AS pending, '' AS pricing_version
    FROM usage_pending;
`
