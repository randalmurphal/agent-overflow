package store

// Rebuild DDL for the items table: the v11 rebuild that established its current
// shape, plus the two later derivations that widen its kind CHECK.
//
// The chain driver, the derivation helpers, and the migrations slice are in
// migrate.go.

// rebuildItemsV11SQL widens the items.kind CHECK to admit
// 'compaction_reasoning' — the live "compact" tail row that streams the
// claudetui compaction summarizer's reasoning above the 'compaction' divider.
// SQLite cannot alter a CHECK in place, so the items table is rebuilt (CREATE
// items_new / copy / DROP / RENAME) with an explicit column list guarding
// against drift; the body is the v1 items definition verbatim with only the
// kind CHECK extended and every index recreated.
//
// Runs with foreign_keys OFF (see applyRebuildMigration) so DROP TABLE items
// does not cascade-delete thread_checkpoints, whose
// (thread_id, user_item_id) FK REFERENCES items(thread_id, id) ON DELETE
// CASCADE. The rename re-links that FK by name and assertForeignKeysIntact
// (foreign_key_check) verifies no row was stranded — always clean here because
// ids are copied verbatim. Nine indexes AND the two payload-GC triggers
// (trg_items_gc_payload / trg_items_gc_input_payload, dropped with the old
// table) ride the same window and are recreated below.
const rebuildItemsV11SQL = `
CREATE TABLE items_new (
    id                  TEXT    NOT NULL,
    thread_id           TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index          INTEGER NOT NULL,
    item_index          INTEGER NOT NULL,
    kind                TEXT    NOT NULL CHECK(kind IN (
        'user_text',
        'assistant_text',
        'thinking',
        'compaction_reasoning',
        'tool_call',
        'tool_completion',
        'error',
        'compaction',
        'terminal_interaction',
        'notification',
        'api_retry',
        'api_error'
    )),
    role                TEXT    NOT NULL CHECK(role IN ('assistant', 'user', 'system')),
    status              TEXT    NOT NULL DEFAULT 'completed' CHECK(status IN (
        'streaming',
        'running',
        'completed',
        'errored',
        'declined',
        'killed'
    )),
    summary             TEXT    NOT NULL DEFAULT '',
    payload_id          TEXT    REFERENCES payloads(id),
    parent_id           TEXT    NOT NULL DEFAULT '',
    is_background       INTEGER NOT NULL DEFAULT 0 CHECK(is_background IN (0, 1)),
    completion_of       TEXT    NOT NULL DEFAULT '',
    tool_name           TEXT    NOT NULL DEFAULT '',
    decision            TEXT    NOT NULL DEFAULT '' CHECK(decision IN (
        '',
        'approved',
        'declined',
        'amended',
        'lost'
    )),
    meta                TEXT    NOT NULL DEFAULT '{}',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    input_payload_id    TEXT REFERENCES payloads(id),
    PRIMARY KEY (thread_id, id)
);

INSERT INTO items_new (
    id, thread_id, turn_index, item_index, kind, role, status, summary,
    payload_id, parent_id, is_background, completion_of, tool_name, decision,
    meta, created_at, updated_at, input_payload_id
)
SELECT
    id, thread_id, turn_index, item_index, kind, role, status, summary,
    payload_id, parent_id, is_background, completion_of, tool_name, decision,
    meta, created_at, updated_at, input_payload_id
FROM items;

DROP TABLE items;

ALTER TABLE items_new RENAME TO items;

CREATE INDEX idx_items_thread
    ON items(thread_id, turn_index, item_index);

CREATE UNIQUE INDEX idx_items_thread_turn_item_unique
    ON items(thread_id, turn_index, item_index);

CREATE INDEX idx_items_parent
    ON items(thread_id, parent_id) WHERE parent_id <> '';

CREATE INDEX idx_items_completion_of
    ON items(thread_id, completion_of) WHERE completion_of <> '';

CREATE INDEX idx_items_payload_id
    ON items(payload_id) WHERE payload_id IS NOT NULL;

CREATE INDEX idx_items_meta_task_id
    ON items(thread_id, json_extract(meta, '$.task_id'))
 WHERE json_extract(meta, '$.task_id') IS NOT NULL;

CREATE INDEX idx_items_input_payload_id
    ON items(input_payload_id) WHERE input_payload_id IS NOT NULL;

CREATE INDEX idx_items_live_background
    ON items(thread_id, id)
 WHERE is_background = 1
   AND status = 'running'
   AND parent_id = ''
   AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0;

CREATE INDEX idx_items_live_codex_subagent
  ON items(thread_id, turn_index, item_index)
  WHERE kind = 'tool_call'
    AND status = 'completed'
    AND tool_name = 'collab_agent'
    AND is_background = 1
    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
    AND json_extract(meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent');

CREATE TRIGGER trg_items_gc_input_payload
AFTER DELETE ON items
WHEN OLD.input_payload_id IS NOT NULL
BEGIN
    DELETE FROM payloads
     WHERE id = OLD.input_payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items WHERE payload_id = OLD.input_payload_id
       )
       AND NOT EXISTS (
           SELECT 1 FROM items WHERE input_payload_id = OLD.input_payload_id
       );
END;

CREATE TRIGGER trg_items_gc_payload
AFTER DELETE ON items
WHEN OLD.payload_id IS NOT NULL
BEGIN
    DELETE FROM payloads
     WHERE id = OLD.payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items WHERE payload_id = OLD.payload_id
       );
END;
`

// rebuildItemsWorkflowProposalV31SQL widens only the items.kind CHECK from
// the proven v11 rebuild. Keeping the complete rebuild mechanically derived
// preserves every column, index, trigger, and FK-safety property of that
// migration while admitting the persisted chat confirmation card.
var rebuildItemsWorkflowProposalV31SQL = mustReplaceOnce(
	rebuildItemsV11SQL,
	"        'compaction_reasoning',",
	"        'compaction_reasoning',\n        'workflow_proposal',",
)

// rebuildItemsCommandResultV48SQL widens the items.kind CHECK once more, for
// the row that holds the output of a slash command the provider CLI executed
// itself (Claude's `<synthetic>` assistant envelope — see
// docs/references/claude-wire.md §"Slash commands"). Derived from the v31
// rebuild for the same reason v31 derived from v11: the complete rebuild stays
// mechanically inherited, so every column, index, trigger, and FK-safety
// property carries forward unchanged.
var rebuildItemsCommandResultV48SQL = mustReplaceOnce(
	rebuildItemsWorkflowProposalV31SQL,
	"        'workflow_proposal',",
	"        'workflow_proposal',\n        'command_result',",
)
