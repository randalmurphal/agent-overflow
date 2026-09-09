package store

const usagePendingV92SQL = `
CREATE TABLE usage_pending (
    thread_id TEXT NOT NULL,
    scope TEXT NOT NULL,
    segment TEXT NOT NULL,
    model TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    project_id TEXT NOT NULL DEFAULT '',
    work_item_id TEXT NOT NULL DEFAULT '',
    turn_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    input_tokens INTEGER NOT NULL CHECK(input_tokens >= 0),
    output_tokens INTEGER NOT NULL CHECK(output_tokens >= 0),
    cache_read_input_tokens INTEGER NOT NULL CHECK(cache_read_input_tokens >= 0),
    cache_creation_input_tokens INTEGER NOT NULL CHECK(cache_creation_input_tokens >= 0),
    reasoning_output_tokens INTEGER NOT NULL CHECK(reasoning_output_tokens >= 0),
    PRIMARY KEY(thread_id, scope, segment, model)
);
CREATE INDEX idx_usage_pending_reconcile ON usage_pending(thread_id, scope, model, created_at, segment);
CREATE INDEX idx_usage_pending_created ON usage_pending(created_at);
CREATE INDEX idx_usage_pending_work_item ON usage_pending(work_item_id, created_at);
CREATE INDEX idx_usage_pending_project_work_item ON usage_pending(project_id, work_item_id)
    WHERE work_item_id <> '';
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
`
