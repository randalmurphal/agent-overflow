package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
)

const createMigrationVersionsTableSQL = `CREATE TABLE IF NOT EXISTS migration_versions (
	version  INTEGER PRIMARY KEY,
	name     TEXT    NOT NULL,
	applied  INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
)`

// DEPRECATED for new migrations — mustReplaceOnce, mustReplaceEvery, and
// mustCutFrom below exist only for the derivations already in the
// migrate_sql_*.go files.
//
// Deriving a rebuild from an earlier migration's text makes that earlier text
// live source code: editing it silently rewrites every migration downstream of
// it, including ones that have already run on user databases. Two stores at the
// same version number then hold different schemas, and nothing in the chain can
// see it — the version row says the migration ran.
//
// A NEW rebuild-style migration RESTATES its SQL in full, as a const of its
// own. Copy the previous rebuild's text, apply the change, and let the two
// diverge; the duplication is the point. The shipped derivations are pinned by
// sha256 in migrate_freeze_test.go, so an edit to any text they derive from
// fails there rather than in a user's database.
//
// mustReplaceOnce derives one migration's SQL from an earlier, already-shipped
// rebuild by substituting exactly one fragment. It panics unless `old` occurs
// exactly once.
//
// strings.Replace(..., 1) silently no-ops when the fragment is absent, which
// would ship a rebuild migration that recreates the table without the change
// it exists to make — a schema that looks migrated and isn't. A mismatch here
// is always a source-edit mistake caught on the first test run, so failing at
// package init is the loud, early failure.
func mustReplaceOnce(source, old, replacement string) string {
	switch n := strings.Count(source, old); n {
	case 1:
		return strings.Replace(source, old, replacement, 1)
	default:
		panic(fmt.Sprintf("store: migration derivation expected exactly 1 occurrence of %q, found %d", old, n))
	}
}

// mustReplaceEvery is part of the DEPRECATED derivation family described above:
// not for new migrations, which restate their SQL in full.
//
// mustReplaceEvery replaces every occurrence of old, panicking unless there
// were exactly want of them. The count is the point: it lets a rebuild derive
// from a multi-table text where the same constraint appears once per table,
// without the derivation silently becoming a no-op (or a partial one) if a
// table is added to or removed from that text later.
func mustReplaceEvery(source, old, replacement string, want int) string {
	if n := strings.Count(source, old); n != want {
		panic(fmt.Sprintf("store: migration derivation expected exactly %d occurrences of %q, found %d", want, old, n))
	}
	return strings.ReplaceAll(source, old, replacement)
}

// mustCutFrom is part of the DEPRECATED derivation family described above: not
// for new migrations, which restate their SQL in full.
//
// mustCutFrom returns source from the first occurrence of marker onward,
// panicking if the marker is absent. Used to slice one table's statement group
// out of a multi-table rebuild so a later migration can re-derive that table
// without retyping its columns and indexes.
func mustCutFrom(source, marker string) string {
	idx := strings.Index(source, marker)
	if idx < 0 {
		panic(fmt.Sprintf("store: migration derivation could not find marker %q", marker))
	}
	return source[idx:]
}

// Migration represents a versioned schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
	// Fix is an optional Go-side data fixup that runs inside the same
	// transaction as SQL (after it, when both are set) and the version
	// bump. Use it when the row transformation needs logic SQL can't
	// express (JSON reshaping, per-row Go helpers). A migration must set
	// SQL, Fix, or both. Not supported on Rebuild migrations — those are
	// table-shape changes by definition — and applyRebuildMigration
	// REFUSES the combination by name rather than dropping the Fix on the
	// floor.
	Fix func(tx *sql.Tx) error
	// Rebuild marks a migration whose SQL performs a full table rebuild
	// (CREATE new / copy / DROP old / RENAME) to change a CHECK or drop a
	// NOT NULL that SQLite can't alter in place. Such a migration MUST run
	// with foreign_keys disabled so DROP TABLE doesn't fire ON DELETE
	// CASCADE against child tables — and foreign_keys can only be toggled
	// outside a transaction, so these run through applyRebuildMigration.
	Rebuild bool
}

// migrations is the ordered list of all schema migrations. Squashed
// for v0.0.1: the prior 51-migration chain produced this schema; old
// databases were rebaked into a single (1, 'initial_schema') row by
// cmd/db-rebake. New columns / indexes / CHECKs from this point on
// append a new Migration entry — never edit v1.
var migrations = []Migration{
	{
		Version: 1,
		Name:    "initial_schema",
		SQL:     initialSchemaSQL,
	},
	{
		Version: 2,
		Name:    "channel_messages_meta",
		SQL:     `ALTER TABLE channel_messages ADD COLUMN meta TEXT NULL;`,
	},
	{
		Version: 3,
		Name:    "thread_drafts_has_content",
		SQL: `ALTER TABLE thread_drafts ADD COLUMN has_content INTEGER NOT NULL DEFAULT 0 CHECK(has_content IN (0,1));

UPDATE thread_drafts SET has_content = 1
WHERE TRIM(content) <> ''
   OR COALESCE(attachments, '[]') NOT IN ('', '[]', 'null')
   OR COALESCE(terminal_chips, '[]') NOT IN ('', '[]', 'null')
   OR pending_plan_implementation IS NOT NULL;

CREATE INDEX idx_thread_drafts_has_content
  ON thread_drafts(thread_id) WHERE has_content = 1;`,
	},
	{
		Version: 4,
		Name:    "thread_disabled_mcp_servers",
		SQL:     `ALTER TABLE threads ADD COLUMN disabled_mcp_servers TEXT NULL CHECK(disabled_mcp_servers IS NULL OR json_valid(disabled_mcp_servers));`,
	},
	{
		Version: 5,
		Name:    "thread_terminal_mode",
		SQL:     rebuildThreadsV5SQL,
		Rebuild: true,
	},
	{
		Version: 6,
		Name:    "new_thread_mcp_defaults",
		SQL: `CREATE TABLE new_thread_mcp_defaults (
    provider         TEXT NOT NULL CHECK(provider IN ('claude','codex')),
    workspace_path   TEXT NOT NULL DEFAULT '',
    disabled_servers TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(disabled_servers)),
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (provider, workspace_path)
);`,
	},
	{
		Version: 7,
		Name:    "payload_append_chunks",
		SQL: `CREATE TABLE payload_chunks (
    payload_id   TEXT    NOT NULL REFERENCES payloads(id) ON DELETE CASCADE,
    chunk_index  INTEGER NOT NULL,
    start_offset INTEGER NOT NULL CHECK(start_offset >= 0),
    data         BLOB    NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (payload_id, chunk_index)
);

CREATE INDEX idx_payload_chunks_payload_start
  ON payload_chunks(payload_id, start_offset);`,
	},
	{
		Version: 8,
		Name:    "trim_tool_result_echo_meta",
		Fix:     trimToolResultEchoMetaFixup,
	},
	{
		Version: 9,
		Name:    "trim_collab_agent_state_messages_meta",
		Fix:     trimCollabAgentStateMessagesMetaFixup,
	},
	{
		Version: 10,
		Name:    "claude_tui_provider",
		SQL:     rebuildProvidersV10SQL,
		Rebuild: true,
	},
	{
		Version: 11,
		Name:    "compaction_reasoning_item_kind",
		SQL:     rebuildItemsV11SQL,
		Rebuild: true,
	},
	{
		Version: 12,
		Name:    "channel_max_turns",
		// The DEFAULT 8 literal mirrors discussion.DefaultMaxTurns (and
		// the frontend's DEFAULT_MAX_TURNS in
		// frontend/src/lib/types/discussion.ts), but is frozen by
		// migration semantics — shipped migrations never change. If the
		// shared default ever moves, this SQL stays 8 and a new
		// migration carries the new value.
		SQL: `ALTER TABLE channels ADD COLUMN max_turns INTEGER NOT NULL DEFAULT 8;`,
	},
	{
		Version: 13,
		Name:    "turns_inflight_partial_index",
		// Backs the boot-time crashed-turn sweep (RecoverCrashedTurns).
		// Partial on completed_at IS NULL so it only ever holds the
		// handful of in-flight rows — the sweep's SELECT is O(crashed)
		// instead of a full turns scan, and steady-state maintenance
		// cost is one insert + one delete per turn.
		SQL: `CREATE INDEX idx_turns_inflight
  ON turns(thread_id, turn_index)
  WHERE completed_at IS NULL;`,
	},
	{
		Version: 14,
		Name:    "usage_ledger",
		// Append-only per-turn token/cost accounting (one row per model
		// per settled turn). DELIBERATELY no foreign keys: lifetime
		// usage totals must survive thread and project deletion, so
		// thread_id / project_id / turn_id are plain attribution columns
		// and provider/model are denormalized at write time. cost_usd is
		// wire-reported only (Claude); 0 with no wire cost means
		// "unpriced", which is what cost_source records.
		SQL: `CREATE TABLE usage_ledger (
    id                          INTEGER PRIMARY KEY,
    created_at                  INTEGER NOT NULL,
    thread_id                   TEXT    NOT NULL,
    project_id                  TEXT    NOT NULL DEFAULT '',
    turn_id                     TEXT    NOT NULL DEFAULT '',
    provider                    TEXT    NOT NULL DEFAULT '',
    model                       TEXT    NOT NULL DEFAULT '',
    input_tokens                INTEGER NOT NULL DEFAULT 0,
    output_tokens               INTEGER NOT NULL DEFAULT 0,
    cache_read_input_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_output_tokens     INTEGER NOT NULL DEFAULT 0,
    cost_usd                    REAL    NOT NULL DEFAULT 0,
    cost_source                 TEXT    NOT NULL DEFAULT 'none'
        CHECK(cost_source IN ('wire','none'))
);

CREATE INDEX idx_usage_ledger_created ON usage_ledger(created_at);

CREATE INDEX idx_usage_ledger_thread ON usage_ledger(thread_id, created_at);`,
	},
	{
		Version: 15,
		Name:    "ui_state",
		// Persisted per-client UI view state (sidebar width, pane
		// layout, collapsed sections). scope is an opaque namespace
		// string — "client:<uuid>" today, "user:<id>" reserved for
		// when identities exist. This table exists because webview
		// localStorage is not durable here: the transport binds an
		// ephemeral port, so the webview origin (and its per-origin
		// storage) changes every launch. Frontend $state still owns
		// in-session reactivity; these rows are the restart-surviving
		// copy, hydrated once at boot.
		SQL: `CREATE TABLE ui_state (
    scope      TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (scope, key)
);`,
	},
	{
		Version: 16,
		Name:    "diff_review_comment_local_scopes",
		SQL:     rebuildDiffReviewCommentsV16SQL,
		Rebuild: true,
	},
	{
		Version: 17,
		Name:    "thread_pr_ref",
		Fix:     addThreadPRRefColumn,
	},
	{
		Version: 18,
		Name:    "diff_review_comment_pr_scope",
		SQL:     rebuildDiffReviewCommentsV18SQL,
		Rebuild: true,
	},
	{
		Version: 19,
		Name:    "codex_max_ultra_reasoning",
		SQL:     rebuildCodexReasoningEffortsV19SQL,
		Rebuild: true,
	},
	{
		Version: 20,
		Name:    "turns_provider_turn_id",
		// Provider-assigned turn id, decoupled from the PRIMARY KEY so
		// forked threads can carry copies of their source's turns (a
		// Codex fork keeps the source's turn ids, so the copy is what
		// makes a cloned turn usable as a `thread/fork` lastTurnId
		// anchor — see Store.CloneThreadTurns). Backfill: Codex rows
		// store the wire id verbatim in turn_id; Claude rows use the
		// synthesized `<threadID>:<turnIndex>` form, recognizable by
		// the ':' that never appears in a Codex turn id.
		SQL: `ALTER TABLE turns ADD COLUMN provider_turn_id TEXT NOT NULL DEFAULT '';

UPDATE turns SET provider_turn_id = turn_id WHERE turn_id NOT LIKE '%:%';`,
	},
	{
		Version: 21,
		Name:    "trim_codex_v2_encrypted_collab_prompts",
		Fix:     trimCodexV2EncryptedCollabPromptsFixup,
	},
	{
		Version: 22,
		Name:    "payload_highlight_spans",
		// Version-stamped, content-addressed highlight span blobs
		// persisted beside the payload they describe (JSON, shape owned
		// by the app layer — PersistedPatchSpans). preview_spans covers
		// the per-file inline-diff preview patches and rides item list
		// reads via the itemColumns join; spans covers the full payload
		// data and is read only by the on-demand payload loads. Empty
		// string means "not computed" — readers fall back to the
		// highlight RPC path.
		SQL: `ALTER TABLE payloads ADD COLUMN preview_spans TEXT NOT NULL DEFAULT '';
ALTER TABLE payloads ADD COLUMN spans TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 23,
		Name:    "message_anchors_replace_checkpoints",
		// The per-message git-checkpoint machinery is gone. What fork-
		// from-message and revert-on-interrupt still need from the old
		// thread_checkpoints row is pure provider correlation — turn
		// index + Claude wire uuids — so the table slims into
		// message_anchors and the git-side columns (ref_name,
		// baseline_sha, status, files, workspace_path) drop with the
		// snapshots themselves. thread_tracked_files backed only the
		// removed session-diff/files-revert paths. Review comments
		// scoped to the removed 'turn'/'session' diff sources are
		// orphaned (their diffs can never load again) and go too; the
		// scope CHECK constraint keeps the legacy values because
		// rebuilding the table to tighten it buys nothing. Leftover
		// refs/agent-overflow/* refs in workspaces are cleaned up
		// manually if at all (see docs/architecture/schema.md) —
		// migrations don't run subprocesses.
		SQL: `CREATE TABLE message_anchors (
    thread_id                TEXT    NOT NULL,
    user_item_id             TEXT    NOT NULL,
    turn_index               INTEGER NOT NULL,
    provider_user_message_id TEXT    NOT NULL DEFAULT '',
    provider_parent_uuid     TEXT    NOT NULL DEFAULT '',
    created_at               INTEGER NOT NULL,
    PRIMARY KEY (thread_id, user_item_id),
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (thread_id, user_item_id) REFERENCES items(thread_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_message_anchors_thread_turn
    ON message_anchors(thread_id, turn_index);

INSERT INTO message_anchors (
	thread_id, user_item_id, turn_index,
	provider_user_message_id, provider_parent_uuid, created_at
)
SELECT thread_id, user_item_id, turn_index,
       provider_user_message_id, provider_parent_uuid, captured_at
  FROM thread_checkpoints;

DROP TABLE thread_checkpoints;

DROP TABLE thread_tracked_files;

DELETE FROM diff_review_comments WHERE scope IN ('turn', 'session');`,
	},
	{
		Version: 24,
		Name:    "edit_file_snapshots",
		// Per-edit new-side file snapshots backing the review pane's
		// Edits scope: gzip-compressed full file content captured at
		// diff persist time, the one moment the just-edited workspace
		// file provably matches the patch. Hunk-gap expansion and span
		// priming resolve against the snapshot first, so a historical
		// edit stays expandable after later turns drift the workspace
		// file. Rows ride the payload lifecycle (the payload GC
		// triggers cascade here); absent rows — pre-feature history or
		// size-capped writes — degrade to the workspace-verify
		// fallback. No backfill: old edits simply have no snapshot.
		SQL: `CREATE TABLE edit_file_snapshots (
    payload_id TEXT    NOT NULL REFERENCES payloads(id) ON DELETE CASCADE,
    path       TEXT    NOT NULL,
    content    BLOB    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (payload_id, path)
);`,
	},
	{
		Version: 25,
		Name:    "project_slugs",
		SQL:     `ALTER TABLE projects ADD COLUMN slug TEXT NOT NULL DEFAULT '';`,
		Fix:     backfillProjectSlugsFixup,
	},
	{
		Version: 26,
		Name:    "workflow_persistence",
		SQL: `CREATE TABLE work_items (
    id             TEXT    PRIMARY KEY,
    project_id     TEXT    NOT NULL,
    goal           TEXT    NOT NULL,
    workflow_id    TEXT    NOT NULL,
    workflow_scope TEXT    NOT NULL CHECK(workflow_scope IN ('project','shared')),
    snapshot       TEXT    NOT NULL DEFAULT '' CHECK(snapshot = '' OR json_valid(snapshot)),
    state          TEXT    NOT NULL CHECK(state IN ('queued','running','needs-human','done','failed','cancelled')),
    reason         TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted')),
    sort_position  INTEGER NOT NULL,
    seeds          TEXT    NOT NULL DEFAULT '' CHECK(seeds = '' OR json_valid(seeds)),
    step_mode      INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),
    worktree_path  TEXT    NOT NULL DEFAULT '',
    branch         TEXT    NOT NULL DEFAULT '',
    base_branch    TEXT    NOT NULL DEFAULT '',
    budget         TEXT    NOT NULL DEFAULT '' CHECK(budget = '' OR json_valid(budget)),
    source         TEXT    NOT NULL CHECK(source IN ('manual','agent','automation')),
    source_ref     TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    started_at     INTEGER NOT NULL DEFAULT 0,
    ended_at       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_work_items_project_state_sort
  ON work_items(project_id, state, sort_position, created_at);

CREATE INDEX idx_work_items_project_sort
  ON work_items(project_id, sort_position, created_at);

CREATE TABLE work_item_phases (
    item_id          TEXT    NOT NULL,
    phase_id         TEXT    NOT NULL,
    attempt          INTEGER NOT NULL CHECK(attempt >= 1),
    thread_id        TEXT    NOT NULL DEFAULT '',
    input_envelope   TEXT    NOT NULL DEFAULT '' CHECK(input_envelope = '' OR json_valid(input_envelope)),
    output_envelope  TEXT    NOT NULL DEFAULT '' CHECK(output_envelope = '' OR json_valid(output_envelope)),
    gate_trace       TEXT    NOT NULL DEFAULT '' CHECK(gate_trace = '' OR json_valid(gate_trace)),
    intervention     TEXT    NOT NULL DEFAULT '' CHECK(intervention = '' OR json_valid(intervention)),
    narrative_path   TEXT    NOT NULL DEFAULT '',
    status           TEXT    NOT NULL CHECK(status IN ('running','completed','parked','failed','cancelled','superseded')),
    started_at       INTEGER NOT NULL,
    ended_at         INTEGER NOT NULL DEFAULT 0,
    UNIQUE(item_id, phase_id, attempt)
);

CREATE INDEX idx_work_item_phases_item_started
  ON work_item_phases(item_id, started_at, phase_id, attempt);

CREATE TABLE work_item_effects (
    item_id      TEXT    NOT NULL,
    phase_id     TEXT    NOT NULL,
    tool         TEXT    NOT NULL,
    payload_hash TEXT    NOT NULL,
    payload      TEXT    NOT NULL CHECK(json_valid(payload)),
    created_at   INTEGER NOT NULL,
    UNIQUE(item_id, phase_id, tool, payload_hash)
);

CREATE TABLE automations (
    id             TEXT    PRIMARY KEY,
    project_id     TEXT    NOT NULL,
    workflow_id    TEXT    NOT NULL,
    workflow_scope TEXT    NOT NULL CHECK(workflow_scope IN ('project','shared')),
    name           TEXT    NOT NULL,
    enabled        INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
    trigger        TEXT    NOT NULL CHECK(json_valid(trigger)),
    condition      TEXT    NOT NULL DEFAULT '' CHECK(condition = '' OR json_valid(condition)),
    seeds          TEXT    NOT NULL DEFAULT '' CHECK(seeds = '' OR json_valid(seeds)),
    notes          TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE INDEX idx_automations_project ON automations(project_id, created_at);

CREATE TABLE automation_cursors (
    automation_id TEXT    NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
    source_key    TEXT    NOT NULL,
    cursor        TEXT    NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY(automation_id, source_key)
);

ALTER TABLE usage_ledger ADD COLUMN work_item_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_usage_ledger_work_item
  ON usage_ledger(work_item_id, created_at);`,
	},
	{
		Version: 27,
		Name:    "thread_workflow_mode",
		SQL:     rebuildThreadsWorkflowModeV27SQL,
		Rebuild: true,
	},
	{
		Version: 28,
		Name:    "thread_workflow_modes_takeover_triage",
		SQL:     rebuildThreadsWorkflowModesV28SQL + rebuildWorkItemsTakeoverTriageV28SQL,
		Rebuild: true,
	},
	{
		Version: 29,
		Name:    "work_item_disposition_digest",
		SQL: `ALTER TABLE work_items ADD COLUMN disposition TEXT NOT NULL DEFAULT ''
CHECK(disposition = '' OR json_valid(disposition));

ALTER TABLE work_items ADD COLUMN digest TEXT NOT NULL DEFAULT ''
CHECK(digest = '' OR json_valid(digest));

CREATE INDEX idx_work_items_state_sort
  ON work_items(state, sort_position, created_at, id);`,
	},
	{
		Version: 30,
		Name:    "usage_ledger_project_work_item",
		SQL: `CREATE INDEX idx_usage_ledger_project_work_item
  ON usage_ledger(project_id, work_item_id)
  WHERE work_item_id <> '';`,
	},
	{
		Version: 31,
		Name:    "workflow_proposal_item_kind",
		SQL: rebuildItemsWorkflowProposalV31SQL + `
CREATE UNIQUE INDEX idx_work_items_agent_source_ref
  ON work_items(source_ref)
  WHERE source = 'agent' AND source_ref <> '';`,
		Rebuild: true,
	},
	{
		Version: 32,
		Name:    "project_workflow_queue_settings",
		SQL: `ALTER TABLE projects ADD COLUMN workflow_queue_paused INTEGER NOT NULL DEFAULT 0
CHECK(workflow_queue_paused IN (0,1));

ALTER TABLE projects ADD COLUMN workflow_concurrency INTEGER NOT NULL DEFAULT 0
CHECK(workflow_concurrency BETWEEN 0 AND 32);`,
	},
	{
		Version: 33,
		Name:    "work_items_direct_start",
		SQL:     rebuildWorkItemsDirectStartV33SQL,
		Rebuild: true,
	},
	{
		Version: 34,
		Name:    "runtime_mode_read_only",
		SQL:     rebuildRuntimeModeReadOnlyV34SQL,
		Rebuild: true,
	},
	{
		Version: 35,
		Name:    "work_item_units",
		// One row per fan-out unit of one phase attempt, written when the unit is
		// created rather than when it finishes: a sub-worktree and branch must be
		// registered the moment they exist so a crash can never strand them and
		// the discard preview can list them. Like work_item_phases this carries no
		// foreign keys — run history outlives the project, thread, and item rows
		// it references.
		SQL: `CREATE TABLE work_item_units (
    item_id        TEXT    NOT NULL,
    phase_id       TEXT    NOT NULL,
    attempt        INTEGER NOT NULL CHECK(attempt >= 1),
    unit_id        TEXT    NOT NULL,
    unit_index     INTEGER NOT NULL CHECK(unit_index >= 0),
    kind           TEXT    NOT NULL CHECK(kind IN ('unit','join')),
    provider       TEXT    NOT NULL DEFAULT '',
    model          TEXT    NOT NULL DEFAULT '',
    thread_id      TEXT    NOT NULL DEFAULT '',
    branch         TEXT    NOT NULL DEFAULT '',
    worktree_path  TEXT    NOT NULL DEFAULT '',
    narrative_path TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL CHECK(status IN ('pending','running','done','failed','dropped','taken-over')),
    unit_attempt   INTEGER NOT NULL DEFAULT 1 CHECK(unit_attempt >= 1),
    envelope       TEXT    NOT NULL DEFAULT '' CHECK(envelope = '' OR json_valid(envelope)),
    feedback       TEXT    NOT NULL DEFAULT '',
    started_at     INTEGER NOT NULL DEFAULT 0,
    ended_at       INTEGER NOT NULL DEFAULT 0,
    UNIQUE(item_id, phase_id, attempt, unit_id)
);

CREATE INDEX idx_work_item_units_attempt
  ON work_item_units(item_id, phase_id, attempt, unit_index);

CREATE INDEX idx_work_item_units_worktree
  ON work_item_units(item_id, worktree_path)
  WHERE worktree_path <> '';`,
	},
	{
		Version: 36,
		Name:    "work_item_unit_failed_reason",
		SQL:     rebuildWorkItemsUnitFailedReasonV36SQL,
		Rebuild: true,
	},
	{
		Version: 37,
		Name:    "work_item_unit_thread_index",
		// Steering a taken-over fan-out unit starts from its thread, not from its
		// run: the human is looking at a thread and sends into it. That lookup runs
		// on every send to a workflow-mode thread, so it gets an index. Partial,
		// because a unit only has a thread once its runner created one — pending
		// units and every tool unit carry the empty default.
		SQL: `CREATE INDEX idx_work_item_units_thread
  ON work_item_units(thread_id)
  WHERE thread_id <> '';`,
	},
	{
		Version: 38,
		Name:    "work_item_call_linkage",
		SQL:     rebuildWorkItemsCallLinkageV38SQL,
		Rebuild: true,
	},
	{
		Version: 39,
		Name:    "work_item_thread_binding",
		SQL:     rebuildWorkItemsThreadBindingV39SQL,
		Rebuild: true,
	},
	{
		Version: 40,
		Name:    "automation_fire_records",
		// What the §11 scheduler did with each trigger, on the automation's own
		// row: the last fire (when, and the run it started) and the skip record
		// the overlap policy, a false or unevaluable condition, a self-chain, or a
		// failed start produces. A skipped fire is never silently dropped — it is
		// the record that makes a starving park or a wrong condition visible.
		//
		// Plain ADD COLUMNs: nothing here changes a CHECK or a column list, so no
		// table rebuild is warranted.
		//
		// The index backs the overlap check itself, which runs on every fire:
		// "does this automation still have a non-terminal run". Partial on the
		// automation source, because every other run's source_ref means something
		// else entirely (an agent proposal id, a call site) and none of them are
		// ever queried this way.
		SQL: `ALTER TABLE automations ADD COLUMN last_fired_at INTEGER NOT NULL DEFAULT 0;

ALTER TABLE automations ADD COLUMN last_run_item_id TEXT NOT NULL DEFAULT '';

ALTER TABLE automations ADD COLUMN skip_count INTEGER NOT NULL DEFAULT 0;

ALTER TABLE automations ADD COLUMN last_skip_at INTEGER NOT NULL DEFAULT 0;

ALTER TABLE automations ADD COLUMN last_skip_reason TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_work_items_automation_source_ref
  ON work_items(source_ref, state)
  WHERE source = 'automation' AND source_ref <> '';`,
	},
	{
		Version: 41,
		Name:    "items_running_bg_tool_calls_index",
		// Startup's orphaned-background-task sweep
		// (ListRecoverableClaudeBackgroundLaunches) filters items on exactly
		// this predicate with no thread scope. Without a matching partial
		// index it full-scans items — measured 12s of cold I/O on a multi-GB
		// history DB (2026-07-28), spent inside ServiceStartup while the SPA
		// is still gated on readiness. The existing idx_items_live_background
		// can't serve it: that index additionally requires parent_id = '',
		// and subagent-scoped background launches carry a parent. The index
		// stays small (settled launches keep their completion sibling but the
		// launch row stays `running`, so entries accumulate slowly — a few
		// thousand rows across months of history).
		SQL: `CREATE INDEX idx_items_running_bg_tool_calls
    ON items(thread_id, id)
 WHERE kind = 'tool_call'
   AND status = 'running'
   AND is_background = 1
   AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0;`,
	},
	{
		Version: 42,
		Name:    "items_running_fg_tool_calls_index",
		// HasRunningTopLevelForegroundToolCall runs at every flush-queue
		// boundary and matches at most a handful of rows (a foreground tool
		// call is transient), but without a matching partial index it walks
		// the thread's whole slice of items — 11ms per probe on a 38k-item
		// thread. The predicate mirrors the query textually so the partial
		// index qualifies.
		SQL: `CREATE INDEX idx_items_running_fg_tool_calls
    ON items(thread_id)
 WHERE kind = 'tool_call'
   AND status = 'running'
   AND is_background = 0
   AND parent_id = '';`,
	},
	{
		Version: 43,
		Name:    "work_item_unit_call_linkage",
		SQL:     rebuildWorkItemsUnitCallLinkageV43SQL,
		Rebuild: true,
	},
	{
		Version: 44,
		Name:    "work_item_soft_stop",
		SQL:     rebuildWorkItemsSoftStopV44SQL,
		Rebuild: true,
	},
	{
		Version: 45,
		Name:    "runtime_mode_auto",
		SQL:     rebuildRuntimeModeAutoV45SQL,
		Rebuild: true,
	},
	{
		Version: 46,
		Name:    "project_worktree_setup",
		// The per-project worktree setup recipe (files copied from the main
		// checkout, argv commands run at worktree creation). It used to live in
		// the hand-authored profile.yaml, where only the workflow engine could
		// reach it; it moves here so chat-thread worktree creation can run the
		// same recipe and so it has a settings UI.
		//
		// One TEXT column holding the whole recipe as JSON, rather than three
		// tables: it is one editor's worth of state, written whole and read
		// whole, and nothing queries inside it. Empty means unconfigured —
		// deliberately NOT nullable, so there is one representation of "no
		// recipe" instead of two indistinguishable ones.
		//
		// A plain ADD COLUMN: nothing here changes a CHECK or a column list, so
		// the FK-parent projects table is not rebuilt.
		SQL: `ALTER TABLE projects ADD COLUMN worktree_setup TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 47,
		Name:    "thread_worktree_setup_state",
		// The DURABLE half of the chat-thread worktree setup run: whether the
		// recipe is running for this thread's worktree, or failed there. The
		// streaming panel is in-memory and dies with the process; this column
		// is what survives a restart, so the sidebar can still say "Setup
		// failed" and the retry affordance stays reachable.
		//
		// Three states, CHECK-enforced: '' (nothing to say — never ran,
		// succeeded, cancelled, or the thread left the worktree), 'running',
		// 'failed'. A separate boolean pair would admit "running AND failed",
		// which is not a state this can be in.
		//
		// A plain ADD COLUMN: SQLite permits a CHECK on an added column, and
		// nothing here changes a column list, so the FK-parent threads table is
		// not rebuilt.
		SQL: `ALTER TABLE threads ADD COLUMN worktree_setup_state TEXT NOT NULL DEFAULT ''
		    CHECK(worktree_setup_state IN ('', 'running', 'failed'));`,
	},
	{
		Version: 48,
		Name:    "command_result_item_kind",
		// The persisted row for a slash command the provider CLI ran itself
		// (`/usage`, a skill, a plugin command): no API call, no model output,
		// so it must not share `assistant_text`. SQLite cannot alter a CHECK
		// in place, so this is a table rewrite — mechanically derived from the
		// v31 rebuild, which was derived from v11.
		SQL:     rebuildItemsCommandResultV48SQL,
		Rebuild: true,
	},
	{
		Version: 49,
		Name:    "drop_per_thread_mcp_state",
		// Provider-native MCP config replaced AO's per-thread MCP snapshot
		// model: Claude's `disabledMcpServers` (per-workspace in
		// ~/.claude.json, written live by the CLI's own mcp_toggle) and
		// Codex's global `enabled` flag in ~/.codex/config.toml are the only
		// durable toggle state now, so the AO-side copies are dead data.
		// `disabled_mcp_servers`' CHECK references only the column itself, so
		// SQLite drops the constraint with the column — no threads rebuild.
		SQL: `ALTER TABLE threads DROP COLUMN disabled_mcp_servers;
DROP TABLE new_thread_mcp_defaults;`,
	},
	{
		Version: 50,
		Name:    "session_import_state",
		// Imported provider sessions. Two halves that only make sense
		// together: the thread's provenance, and the cursor a refresh
		// continues from.
		//
		// `threads.import_source` is set once, at import, and never
		// rewritten — it is what the "Check for Provider Updates" menu item
		// is gated on. `session_ref` cannot gate it: every thread that has
		// run a turn has one. A plain ADD COLUMN with a CHECK (SQLite allows
		// a CHECK on an added column), so the FK-parent threads table is not
		// rebuilt.
		//
		// `(last_turn_index, last_item_index)` is the divergence guard, and
		// it is a PAIR because `items.item_index` restarts at 0 in every
		// turn (store's own nextItemIndexTx allocates per turn). A single
		// item index therefore names no position in a thread: item 3 of
		// turn 1 and item 3 of turn 9 share it. The pair is the same
		// (turn_index, item_index) ordering every timeline read sorts by, so
		// "the thread grew past the import" is exactly "an item exists
		// lexicographically after the pair" — one index range scan on
		// idx_items_thread. Both default to -1 so "imported nothing yet"
		// sits below every real position rather than colliding with turn 0 /
		// item 0.
		SQL: `ALTER TABLE threads ADD COLUMN import_source TEXT NOT NULL DEFAULT ''
    CHECK(import_source IN ('', 'claude', 'codex'));
CREATE TABLE thread_import_state (
    thread_id          TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    provider           TEXT NOT NULL CHECK(provider IN ('claude','codex')),
    source_path        TEXT NOT NULL,
    source_session_id  TEXT NOT NULL,
    leaf_uuid          TEXT NOT NULL DEFAULT '',
    last_source_uuid   TEXT NOT NULL DEFAULT '',
    last_source_offset INTEGER NOT NULL DEFAULT 0,
    last_turn_index    INTEGER NOT NULL DEFAULT -1,
    last_item_index    INTEGER NOT NULL DEFAULT -1,
    imported_at        INTEGER NOT NULL,
    refreshed_at       INTEGER NOT NULL DEFAULT 0
);`,
	},
	{
		Version: 51,
		Name:    "work_item_phase_park_cause",
		// Why the ENGINE parked this attempt, in its own words. It is a
		// separate column from `output_envelope` because the envelope is the
		// AGENT's artifact: a phase that parked because a worktree could not be
		// cut ran no turn, and writing engine prose into the envelope makes
		// every reader — the history binding, the crash rebuild's terminal
		// check, the wake's detail line — treat it as something a model said.
		//
		// Empty means "no engine-diagnosed cause": either the attempt rested on
		// its own envelope, or the reason names its own cause (`interrupted`,
		// `paused`, `taken-over`) and a sentence would add nothing.
		//
		// A plain ADD COLUMN with no CHECK — the text is free-form and this
		// table is nobody's FK parent, so it is not rebuilt.
		SQL: `ALTER TABLE work_item_phases ADD COLUMN park_cause TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 52,
		Name:    "work_item_wake_signature",
		// The signature of the last wake DELIVERED into this run's bound thread
		// (K2). A wake is deduplicated by what it says, never by a time window:
		// the same ask arriving twice with nothing having happened in between is
		// noise, and a timer would either suppress a genuinely new state or let a
		// slow duplicate through.
		//
		// It is durable rather than in-memory because the app restarting is one
		// of the ways the same ask gets re-composed: a crash rebuild parks every
		// interrupted run, and a supervising agent that already read that message
		// should not read it again on every launch.
		//
		// Empty means "nothing delivered yet, or a human/agent has acted since",
		// which is the state every wake delivers from. The column is deliberately
		// absent from `workItemColumns`: it is wake bookkeeping, never needed by a
		// listing, and its own narrow accessors are the only readers and writers.
		//
		// A plain ADD COLUMN with no CHECK — free-form text, and this table is
		// nobody's FK parent. A future `work_items` REBUILD has to carry it, like
		// every other column added since v39.
		SQL: `ALTER TABLE work_items ADD COLUMN wake_signature TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 53,
		Name:    "work_item_pending_guidance",
		// Operator guidance waiting for this run's next fresh phase entry — the
		// mirror of `notify:`, which carries a run's progress out to a thread;
		// this carries a person's steering in without parking the run.
		//
		// A JSON array of `{text, at, by, byRun}` objects, appended by
		// `run guide` and cleared by the entry that delivers it. Empty (the
		// default, and what a cleared slot is set back to) means nothing is
		// pending. The engine bounds both the per-entry size and the entry count
		// before it writes, so the column stays small enough to read on every
		// phase entry.
		//
		// Like `wake_signature` it is deliberately absent from
		// `workItemColumns`: no listing, overlay, or run projection carries it,
		// and its own narrow accessors are the only readers and writers.
		//
		// A plain ADD COLUMN with no CHECK — the content is engine-written JSON
		// and this table is nobody's FK parent. A future `work_items` REBUILD has
		// to carry it, like every other column added since v39.
		SQL: `ALTER TABLE work_items ADD COLUMN pending_guidance TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 54,
		Name:    "work_item_auto_resume_at",
		// Unix milliseconds at which a parked run resumes ITSELF, or 0 for the
		// runs that never will — which is almost all of them.
		//
		// It is written by `agent-overflow run resume --at`, an explicit request
		// to continue a parked attempt at a future moment. Provider usage-limit
		// handling no longer arms it (D75): a typed refusal parks and waits for an
		// ordinary explicit action.
		//
		// It is DURABLE rather than an in-memory timer for the reason the wake
		// signature is: a five-day stall outlives any process, so a restart has
		// to be able to re-arm what it finds. The app owns the timer — the engine
		// holds none by boundary — and every path that moves the run out of that
		// park clears the column, so an opt-out clears what the opt-in stored.
		//
		// Deliberately absent from `workItemColumns`, exactly like
		// `wake_signature` and `pending_guidance`: no listing, overlay, or run
		// projection reads it, and every row those reads carry would pay for it.
		// Its own narrow accessors are the only readers and writers.
		//
		// A plain ADD COLUMN with no CHECK — the value is engine-adjacent app
		// state and this table is nobody's FK parent. A future `work_items`
		// REBUILD has to carry it, like every other column added since v39.
		SQL: `ALTER TABLE work_items ADD COLUMN auto_resume_at INTEGER NOT NULL DEFAULT 0;`,
	},
	{
		Version: 55,
		Name:    "thread_history_stamps",
		// The client-replica invalidation contract
		// (docs/architecture/thread-replica-sync.md §3): two counters on every
		// thread that any persisted item mutation provably advances, plus
		// the store's identity row.
		//
		// `history_rev` advances on every write that can change what a
		// windowed item read returns; `history_epoch` additionally advances
		// when a cached ORDERING is no longer safe to paint (a row was
		// deleted or moved). Every epoch bump also bumps rev, so a rev match
		// alone means "byte-identical window read".
		//
		// The item-side bumps are TRIGGERS, not a helper store functions are
		// asked to remember: no writer — present or future — can put a row
		// into `items` without advancing the contract. The payload side
		// cannot be a trigger (`payloads` has no thread_id, and routing via a
		// subquery over items.payload_id would put an unindexed scan on the
		// streaming append path), so those mutators take a threadID and bump
		// explicitly; the signature is that half's enforcement.
		//
		// Cost is one extra dirty page per COMMIT, not per statement — WAL
		// writes pages per commit — so a 500-row retention delete or a 10 Hz
		// streaming flush pays one page image on a transaction it already had.
		//
		// `store_meta` is the identity half (§3.3). `backend_id` keys the
		// client's replica database per backend; `replica_generation` is
		// re-minted whenever rev/epoch continuity breaks for reasons the
		// counters cannot express (today: RestoreFrom replacing the whole
		// database), and a mismatch tells a client to drop its replica
		// wholesale rather than migrate it. Both are minted in Go (see
		// mintStoreIdentity) because SQLite has no uuid generator worth
		// spelling in a migration.
		//
		// Every pre-existing thread is lifted off rev 0 by the trailing
		// UPDATE, which makes rev 0 UNREACHABLE for any thread that
		// predates the contract. That matters because (0, 0) is also the
		// JSON zero value of the wire request's stamp pair: a client that
		// omits the fields, or a bug that drops them, asks "is rev 0 still
		// current?" — and against an untouched pre-migration thread with
		// 400 items of history that would have matched, answering `fresh`
		// with no page over a replica the client does not have. After the
		// lift, (0, 0) can only describe a thread with zero item writes
		// since v55 — a brand-new row, for which a page-less `fresh` is
		// the truthful answer to an empty window. The column DEFAULT stays
		// 0 for exactly that reason.
		SQL: `ALTER TABLE threads ADD COLUMN history_rev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN history_epoch INTEGER NOT NULL DEFAULT 0;

UPDATE threads SET history_rev = 1;

CREATE TABLE store_meta (
    id                 INTEGER PRIMARY KEY CHECK(id = 1),
    backend_id         TEXT NOT NULL,
    replica_generation TEXT NOT NULL
);

` + historyRevTriggersLegacySQL,
		Fix: mintStoreIdentity,
	},
	{
		Version: 56,
		Name:    "work_item_retry_reasons",
		// The old `retries-exhausted` reason represented two unrelated limits:
		// the provider retry ladder and a workflow loop/reject bound. They have
		// different recovery edges, so new runs need distinct persisted reasons.
		//
		// Existing rows keep the legacy value. The phase cause is optional and
		// free-form, so rewriting those rows would guess at their history. Keeping
		// the old spelling in the CHECK also preserves their shipped bare-resume
		// behavior while all new engine transitions write a specific reason.
		//
		// SQLite cannot widen a CHECK in place. This rebuild is derived from v44,
		// the latest work_items rebuild, and explicitly carries the four columns
		// added since that schema text: soft_stop, wake_signature,
		// pending_guidance, and auto_resume_at.
		SQL:     rebuildWorkItemRetryReasonsV56SQL,
		Rebuild: true,
	},
	{
		Version: 57,
		Name:    "workflow_provider_usage_limits",
		// A provider usage refusal is neither an exhausted retry ladder nor a
		// model-specific fact the workflow layer can safely infer. The run keeps
		// a distinct typed reason, while the phase/unit that actually failed
		// records a durable provider-account scope. Cross-run attention state is
		// keyed by that scope and the thread watching the runs; it coalesces an
		// outage storm without becoming an admission gate.
		//
		// The scope intentionally includes the credential generation. A managed
		// account switch (and the equivalent unmanaged credential replacement)
		// therefore starts a fresh scope through the same identity boundary normal
		// sends already use. The attention generation is separate: starting or
		// resuming work re-arms notification delivery without claiming the provider
		// is healthy, and a real send is always still attempted.
		SQL: rebuildWorkItemProviderUsageLimitedV57SQL + `

ALTER TABLE work_item_phases ADD COLUMN provider_usage_scope_id INTEGER NOT NULL DEFAULT 0 CHECK(provider_usage_scope_id >= 0);
ALTER TABLE work_item_units ADD COLUMN provider_usage_scope_id INTEGER NOT NULL DEFAULT 0 CHECK(provider_usage_scope_id >= 0);

CREATE TABLE workflow_provider_usage_scopes (
    id                    INTEGER PRIMARY KEY,
    provider              TEXT    NOT NULL CHECK(provider <> ''),
    account_id            TEXT    NOT NULL DEFAULT '',
    credential_generation INTEGER NOT NULL CHECK(credential_generation >= 0),
    first_seen_at         INTEGER NOT NULL,
    last_seen_at          INTEGER NOT NULL,
    UNIQUE(provider, account_id, credential_generation)
);

CREATE TABLE workflow_provider_usage_attention (
    scope_id             INTEGER NOT NULL CHECK(scope_id > 0),
    thread_id            TEXT    NOT NULL CHECK(thread_id <> ''),
    generation           INTEGER NOT NULL DEFAULT 1 CHECK(generation >= 1),
    delivered_generation INTEGER NOT NULL DEFAULT 0 CHECK(delivered_generation >= 0),
    queued_generation    INTEGER NOT NULL DEFAULT 0 CHECK(queued_generation >= 0),
    queued_token         TEXT    NOT NULL DEFAULT '',
    source_item_id       TEXT    NOT NULL DEFAULT '',
    updated_at           INTEGER NOT NULL,
    UNIQUE(scope_id, thread_id),
    CHECK((queued_generation = 0) = (queued_token = '')),
    CHECK((queued_generation = 0) = (source_item_id = '')),
    CHECK(queued_generation = 0 OR queued_generation = generation),
    CHECK(delivered_generation <= generation)
);

CREATE INDEX idx_workflow_provider_usage_attention_thread
  ON workflow_provider_usage_attention(thread_id);
`,
		Rebuild: true,
	},
	{
		Version: 58,
		Name:    "payload_thread_scope",
		SQL:     payloadThreadScopeMigrationSQL,
		Rebuild: true,
	},
	{
		Version: 59,
		Name:    "bulk_import_history_revision",
		SQL: `ALTER TABLE threads ADD COLUMN history_bulk_load INTEGER NOT NULL DEFAULT 0
    CHECK(history_bulk_load IN (0, 1));

` + dropHistoryRevTriggersSQL + historyRevTriggersSQL,
	},
	{
		Version: 60,
		Name:    "drop_redundant_items_timeline_index",
		// idx_items_thread_turn_item_unique has the exact same key columns in
		// the exact same order. Keeping both makes every item write maintain two
		// interchangeable B-trees; the unique one already serves timeline/range
		// reads and is the invariant that prevents duplicate positions.
		SQL: `DROP INDEX idx_items_thread;`,
	},
	{
		Version: 61,
		Name:    "shared_import_history",
		SQL:     sharedImportHistoryMigrationSQL,
	},
	{
		Version: 62,
		Name:    "turn_thread_scope",
		SQL:     turnThreadScopeMigrationSQL,
	},
	{
		Version: 63,
		Name:    "imported_session_lineage",
		// Provider forks are separate resumable sessions even though their
		// histories share a prefix. Keep the provider parent id alongside the
		// import cursor so the AO thread link can be resolved after either side
		// arrives (and restored after a parent is deleted and re-imported).
		//
		// Existing installations may contain duplicate source identities from
		// the old one-thread-per-Claude-leaf importer. A UNIQUE index would make
		// this forward migration fail and strand those users, so the triggers
		// preserve legacy rows while making every NEW claim structurally unique.
		// The UPDATE trigger permits cursor refreshes and rejects only an actual
		// identity change to a source another thread already owns.
		SQL: `ALTER TABLE thread_import_state
    ADD COLUMN source_parent_session_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_thread_import_state_parent
    ON thread_import_state(provider, source_parent_session_id)
    WHERE source_parent_session_id <> '';

CREATE INDEX idx_thread_import_state_source
    ON thread_import_state(provider, source_session_id);

CREATE TRIGGER thread_import_state_unique_source_insert
BEFORE INSERT ON thread_import_state
WHEN NOT EXISTS (
    SELECT 1 FROM thread_import_state AS own
     WHERE own.thread_id = NEW.thread_id
       AND own.provider = NEW.provider
       AND own.source_session_id = NEW.source_session_id
)
 AND (EXISTS (
    SELECT 1 FROM thread_import_state AS existing
     WHERE existing.provider = NEW.provider
       AND existing.source_session_id = NEW.source_session_id
       AND existing.thread_id <> NEW.thread_id
) OR EXISTS (
    SELECT 1 FROM threads AS existing
     WHERE existing.id <> NEW.thread_id
       AND CASE existing.provider
             WHEN 'claude-tui' THEN 'claude'
             ELSE existing.provider
           END = NEW.provider
       AND (existing.session_ref = NEW.source_session_id
            OR existing.pending_fork_session_ref = NEW.source_session_id)
))
BEGIN
    SELECT RAISE(ABORT, 'provider session is already claimed by another thread');
END;

CREATE TRIGGER thread_import_state_unique_source_update
BEFORE UPDATE OF provider, source_session_id ON thread_import_state
WHEN (OLD.provider <> NEW.provider OR OLD.source_session_id <> NEW.source_session_id)
 AND (EXISTS (
    SELECT 1 FROM thread_import_state AS existing
     WHERE existing.provider = NEW.provider
       AND existing.source_session_id = NEW.source_session_id
       AND existing.thread_id <> NEW.thread_id
) OR EXISTS (
    SELECT 1 FROM threads AS existing
     WHERE existing.id <> NEW.thread_id
       AND CASE existing.provider
             WHEN 'claude-tui' THEN 'claude'
             ELSE existing.provider
           END = NEW.provider
       AND (existing.session_ref = NEW.source_session_id
            OR existing.pending_fork_session_ref = NEW.source_session_id)
))
BEGIN
    SELECT RAISE(ABORT, 'provider session is already claimed by another thread');
END;`,
	},
	{
		Version: 64,
		Name:    "work_item_phase_feedback_delivered",
		// When this attempt's persisted `input_envelope.feedback` stopped being
		// owed to a turn — either a provider session that renders it started, or a
		// later attempt of the same phase took the note over. 0 means "still owed".
		//
		// It exists because feedback was write-only durable state: an operator
		// answered a parked question, the engine persisted the answer as the new
		// attempt's feedback, the runner start wedged, and nothing ever read a
		// superseded attempt's feedback again. The answer was on disk and lost all
		// the same. This column is what a later attempt of the same phase consults
		// to redeliver it (see `internal/workflow/engine/feedback.go`), and it is
		// the exact mirror of `work_items.pending_guidance`'s ordering rule: the
		// attempt row carrying the note is written first and the stamp lands only
		// when a session that renders it has actually started, so every way an
		// attempt can end without a turn is a redelivery rather than a loss.
		//
		// THE BACKFILL IS THE POINT OF THE SECOND STATEMENT. A bare ADD COLUMN
		// leaves every historical row at 0, which reads as "owed" — so the next
		// attempt of every phase any run ever entered would prepend feedback from
		// a round that finished months ago. Existing rows are stamped with their
		// own time (ended_at for a settled attempt, started_at for one still
		// running), and the trailing `1` guarantees the stamp is non-zero even for
		// a row whose timestamps were never written: "delivered" has to be
		// structural here, not conditional on data this migration cannot verify.
		//
		// 0-means-unset rather than NULL, matching `ended_at`, `auto_resume_at`,
		// and `provider_usage_scope_id`: NULL and 0 would be two spellings of one
		// state that every reader would have to agree about. A plain ADD COLUMN
		// with no CHECK — this table is nobody's FK parent, so no rebuild.
		SQL: `ALTER TABLE work_item_phases ADD COLUMN feedback_delivered_at INTEGER NOT NULL DEFAULT 0;

UPDATE work_item_phases SET feedback_delivered_at = MAX(started_at, ended_at, 1);`,
	},
	{
		Version: 65,
		Name:    "thread_live_todo",
		// The activity rail's todo list (Claude TodoWrite / the Task* family,
		// Codex update_plan) as the provider last reported it. It used to live
		// only in a triage map, so an app restart or a session teardown erased
		// a list the user was still working through — the work was not finished,
		// only the process that happened to be holding the note.
		//
		// It is durable state about the conversation rather than history: the
		// column is rewritten in place by each report and cleared when the
		// provider empties the list, so there is exactly one row per thread and
		// no timeline row is ever written for a todo tick.
		//
		// `''` is the only spelling of "no list" — the writer refuses an empty
		// step array so a cleared list cannot also be stored as `[]`. The CHECK
		// admits `''` or valid JSON, which is what stops a half-written blob
		// from being discovered later by a reader that can only report it as an
		// error. A plain ADD COLUMN: SQLite permits a CHECK on one, and nothing
		// here changes a column list, so the FK-parent threads table is not
		// rebuilt.
		SQL: `ALTER TABLE threads ADD COLUMN live_todo TEXT NOT NULL DEFAULT ''
    CHECK(live_todo = '' OR json_valid(live_todo));`,
	},
	{
		Version: 66,
		Name:    "provider_thread_cost",
		// The PROVIDER's own cumulative cost estimate for a thread, as opposed
		// to usage_ledger's per-turn token rows priced from AO's rate table.
		// Today's only producer is Codex >= 0.148
		// (`account/usage/read {threadId}` -> `threadUsage`), which answers with
		// a lifetime-to-date figure for the whole thread rather than a delta.
		//
		// WHY NOT usage_ledger. That table's contract is "values are per-turn
		// deltas; summing any slice of rows is safe", and the workflow budget
		// check enforces spend limits by summing it. A cumulative figure stored
		// as a row would be added to every turn it already contains — silently
		// inflating every dollar total in the app, budget enforcement included.
		// A cumulative fact needs a table whose grain is the thread.
		//
		// ONE ROW PER THREAD, rewritten in place: each read restates the same
		// total, so this is a cache of the newest answer, not history. It is
		// cache content in the core-principle-3 sense — droppable and
		// re-derivable from the provider — which is also why it is a separate
		// table rather than three Codex-shaped columns on the hot threads row.
		//
		// cost_usd_micros stores the wire integer verbatim (millionths of a
		// dollar) rather than a float: the value crosses no arithmetic on its
		// way in, so there is no reason to round-trip it through binary
		// floating point before it is displayed. Only rows the provider PRICED
		// are written, so the column is NOT NULL and there is no "unknown"
		// sentinel to disambiguate — an absent estimate is an absent row.
		//
		// cost_source names the provenance for a reader that will one day see
		// more than one kind. 'provider-estimate' is the only legal value now,
		// and the CHECK is what forces a future producer to declare itself.
		//
		// Cascades with the thread, unlike usage_ledger: this row is only ever
		// read to render one thread's own cost, so it has nothing to say once
		// that thread is gone. Lifetime account aggregates keep coming from the
		// ledger.
		SQL: `CREATE TABLE provider_thread_cost (
    thread_id       TEXT    PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    provider        TEXT    NOT NULL,
    cost_source     TEXT    NOT NULL CHECK(cost_source IN ('provider-estimate')),
    cost_usd_micros INTEGER NOT NULL,
    credits_micros  INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL
);`,
	},
	{
		Version: 67,
		Name:    "import_source_identity",
		// The fingerprint a session-import REFRESH compares before it resumes
		// reading a provider session file from a byte offset.
		//
		// Until now the only divergence test was "is the file smaller than the
		// cursor" (rollout.ErrSourceShrank). Codex 0.147 made that
		// insufficient: a thread can be MIGRATED from `legacy` to `paginated`
		// history in place — codex-rs rewrites the whole rollout into a
		// canonical form and atomically publishes it over the same path
		// (thread-store/src/local/rollout_migration.rs). The rewritten file is
		// usually the same size or LARGER, so the size check passes, while
		// every byte offset in it now addresses a different record. Resuming
		// there splices unrelated content into the thread as if the
		// conversation had continued.
		//
		// source_meta_hash is sha256 of the file's FIRST LINE (its header
		// record), not of the whole file: a rollout is appended to constantly,
		// so a whole-file digest would have to be recomputed over every
		// candidate on every refresh, while the header is rewritten by exactly
		// the events that invalidate a cursor. source_history_mode records the
		// declared mode alongside it, so a migration is named as the cause
		// rather than reported as a generic mismatch.
		//
		// Both default to '' and '' means "recorded before this migration" —
		// NOT "no header". A blank stored value therefore cannot be compared
		// and must not be treated as a mismatch, or every thread imported
		// before this column existed would report itself diverged on its next
		// refresh. Two plain ADD COLUMNs; no column list changes, so no table
		// is rebuilt.
		SQL: `ALTER TABLE thread_import_state ADD COLUMN source_meta_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_import_state ADD COLUMN source_history_mode TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 68,
		Name:    "provider_thread_cost_session_ref",
		// Records WHICH provider thread each cost row describes, so a row that
		// no longer matches the AO thread's session ref is ignored on read.
		//
		// v66's row was keyed by the AO thread id alone and named no provider
		// thread. That made its correctness depend on a DELETE: a Codex
		// rollback that forks into a new provider thread, or a rollback to turn
		// 0 that clears the session ref entirely, had to remove the row before
		// anything read it again. A delete that failed (a locked database, a
		// crash between the fence and the statement) left a row describing the
		// OLD provider thread's lifetime total sitting against a thread whose
		// history is now shorter, and the only thing hiding it was an
		// in-memory mark that died with the process.
		//
		// session_ref is the provider thread identity the figure was read from
		// — for Codex, the app-server root thread id echoed back on
		// `account/usage/read`, the same value `threads.session_ref` holds
		// while the thread still points at that provider thread. The overlay
		// join compares them, so a stale row answers "not found" by
		// construction and the next successful read overwrites it. Deleting on
		// rollback is still done, because a row nothing can match is dead
		// weight; it is no longer what makes the answer correct.
		//
		// DROPPED rather than converted. This table is cache content in the
		// core-principle-3 sense: every row is one `account/usage/read` away
		// from being re-derived, and no existing row can be told which provider
		// thread it came from — backfilling from today's `threads.session_ref`
		// would assert exactly the identity the column exists to verify, and
		// would re-bless the stale rows this migration is here to neutralise.
		//
		// The CHECK forbids '' because a blank identity would match every
		// rolled-back thread's cleared session ref, which is the one comparison
		// that must never succeed. The writer refuses to store a row it cannot
		// name.
		SQL: `DROP TABLE IF EXISTS provider_thread_cost;
CREATE TABLE provider_thread_cost (
    thread_id       TEXT    PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    session_ref     TEXT    NOT NULL CHECK(session_ref <> ''),
    provider        TEXT    NOT NULL,
    cost_source     TEXT    NOT NULL CHECK(cost_source IN ('provider-estimate')),
    cost_usd_micros INTEGER NOT NULL,
    credits_micros  INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL
);`,
	},
	{
		Version: 69,
		Name:    "pending_fork_resume_at",
		// The transcript cut a lazy Claude fork start must pin. A mid-turn
		// tail fork stores (pending_fork_session_ref = the SOURCE session,
		// pending_fork_resume_at = the leaf uuid captured when Fork was
		// clicked); the fork's first session start then spawns
		// `--resume <source> --resume-session-at <repaired leaf>
		// --fork-session`, which makes the CLI's own fork cut exactly where
		// the timeline was cloned instead of wherever the source's
		// transcript has grown to by first send (the 2026-08-22 44s-skew
		// incident). Empty means "no pin": a plain lazy fork of an idle
		// source cuts at the CLI's own tail, which is correct there.
		// Cleared together with pending_fork_session_ref by both
		// session-ref writers — a committed fork must not re-pin on a later
		// restart. NOT NULL DEFAULT '' so "no pin" has one spelling; a
		// plain ADD COLUMN, so the FK-parent threads table is not rebuilt.
		SQL: `ALTER TABLE threads ADD COLUMN pending_fork_resume_at TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 70,
		Name:    "trim_unverified_codex_v2_profiles",
		// Older MultiAgentV2 rows stored the parent session profile or the
		// requested spawn arguments as if they were the child's effective
		// model and effort. Codex applies role and default configuration after
		// those inputs, and its activity item does not report the result. Drop
		// the unverified cache fields once. The current adapter repopulates
		// active rows from a metadata-only child thread/resume response.
		Fix: trimUnverifiedCodexV2ProfilesFixup,
	},
	{
		Version: 71,
		Name:    "thread_pin_groups",
		// NULL keeps every existing pinned row on the front burner without a
		// data rewrite. A non-NULL group requires an active pin, which makes
		// latent group state on an unpinned row structurally impossible.
		SQL: `ALTER TABLE threads ADD COLUMN pin_group INTEGER
    CHECK(pin_group IS NULL OR (pinned_at IS NOT NULL AND pin_group IN (0, 1)));`,
	},
	{
		Version: 72,
		Name:    "remove_design_mode",
		SQL:     removeDesignModeV72SQL,
		Rebuild: true,
	},
	{
		Version: 73,
		Name:    "thread_created_by_device",
		// Which screen started this thread. One backend now serves several
		// clients, and a thread carries no record of where it came from.
		//
		// Creation only, never "last touched": a single column can hold one
		// answer, and overwriting it on every mutation would destroy the
		// provenance it exists to keep while still not being an audit trail
		// (that needs a log, not a slot).
		//
		// NOT NULL DEFAULT '' so "unattributed" has one spelling, which is
		// the honest value for every existing row and for every thread the
		// backend creates on its own. A plain ADD COLUMN, so the FK-parent
		// threads table is not rebuilt.
		SQL: `ALTER TABLE threads ADD COLUMN created_by_device TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 74,
		Name:    "thread_created_git_origin",
		// The git coordinates of the workspace at the moment the thread was
		// created: branch, remote URL, head commit.
		//
		// Recorded because they are unrecoverable later. A fork or a transfer
		// needs to know which repository and which commit a thread grew from,
		// and by then the branch has moved, the commit may have been rebased
		// away, and the workspace may hold something else entirely. The live
		// `branch` column answers a different question — it tracks the
		// CHECKOUT and is rewritten whenever the working tree moves.
		//
		// Empty is a first-class value: a non-git workspace, a detached HEAD,
		// a repository with no remote, and every pre-migration row all read
		// as "not known", and no caller may treat empty as an error. Three
		// plain ADD COLUMNs, no rebuild.
		SQL: `ALTER TABLE threads ADD COLUMN created_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN created_remote_url TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN created_head_commit TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 75,
		Name:    "identity_core",
		// Users, devices, sessions, signing keys, recovery codes, and the
		// credential audit log. The first authoritative (non-cache) rows in
		// this database — see the const's doc comment for why they are one
		// migration and what each table's non-obvious columns decide.
		SQL: identityCoreV75SQL,
	},
}

// runMigrations sets PRAGMAs, creates the version tracking table, and applies
// any unapplied migrations in order.
func runMigrations(db *sql.DB) error {
	if err := configureDatabase(db); err != nil {
		return err
	}
	if err := ensureMigrationTable(db); err != nil {
		return err
	}

	applied, err := currentMigrationVersion(db)
	if err != nil {
		return err
	}

	return applyPendingMigrations(db, applied)
}

func configureDatabase(db *sql.DB) error {
	// PRAGMA journal_mode=WAL returns the resulting mode even on
	// success; SQLite silently falls back to the previous journal mode
	// when WAL can't be enabled (NFS filesystems, read-only mounts,
	// shared-cache databases). We don't treat this as fatal — rollback
	// journaling keeps the app correct — but we log a warning so the
	// user can see why checkpointing is not happening.
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("set WAL mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		log.Printf("store: journal_mode=WAL returned %q; SQLite fell back to rollback journaling (often caused by NFS or read-only mount)", journalMode)
	}
	// foreign_keys, busy_timeout and synchronous are connection-scoped, so
	// setting them here would only configure whichever connection served
	// this call — the writer DSN carries them instead (see dsn.go), which
	// is what makes them survive a recycled connection. What's left to do
	// is prove the DSN actually applied them: modernc.org/sqlite runs
	// _pragma values verbatim and SQLite ignores an unknown PRAGMA name
	// without complaining, so a typo would open cleanly and run the whole
	// app with foreign keys off.
	return verifyConnPragmas(db, writerConnPragmas)
}

func ensureMigrationTable(db *sql.DB) error {
	if _, err := db.Exec(createMigrationVersionsTableSQL); err != nil {
		return fmt.Errorf("create migration_versions table: %w", err)
	}
	return nil
}

func currentMigrationVersion(db *sql.DB) (int, error) {
	var maxVersion sql.NullInt64
	if err := db.QueryRow("SELECT MAX(version) FROM migration_versions").Scan(&maxVersion); err != nil {
		return 0, fmt.Errorf("query max migration version: %w", err)
	}
	if !maxVersion.Valid {
		return 0, nil
	}
	return int(maxVersion.Int64), nil
}

var validTableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// tableColumns returns the set of column names defined on the given
// table. Kept exported-within-package because items_lifecycle_test.go
// and items_parent_test.go use it as a schema-existence probe.
func tableColumns(db sqlQueryer, table string) (map[string]bool, error) {
	if !validTableName.MatchString(table) {
		return nil, fmt.Errorf("invalid table name: %q", table)
	}
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate table_info(%s): %w", table, err)
	}
	return columns, nil
}

func applyPendingMigrations(db *sql.DB, applied int) error {
	for _, m := range migrations {
		if m.Version <= applied {
			continue
		}
		apply := applyMigration
		if m.Rebuild {
			apply = applyRebuildMigration
		}
		if err := apply(db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(db *sql.DB, m Migration) error {
	log.Printf("store: applying migration v%d: %s", m.Version, m.Name)
	if strings.TrimSpace(m.SQL) == "" && m.Fix == nil {
		return fmt.Errorf("migration v%d (%s) has neither SQL nor Fix", m.Version, m.Name)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration v%d: %w", m.Version, err)
	}

	if strings.TrimSpace(m.SQL) != "" {
		if _, err := tx.Exec(m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration v%d (%s) failed: %w", m.Version, m.Name, err)
		}
	}
	if m.Fix != nil {
		if err := m.Fix(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration v%d (%s) fixup failed: %w", m.Version, m.Name, err)
		}
	}
	if _, err := tx.Exec(
		"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
		m.Version,
		m.Name,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record migration v%d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration v%d: %w", m.Version, err)
	}

	log.Printf("store: migration v%d applied", m.Version)
	return nil
}

func addThreadPRRefColumn(tx *sql.Tx) error {
	columns, err := tableColumns(tx, "threads")
	if err != nil {
		return err
	}
	if columns["pr_ref"] {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE threads ADD COLUMN pr_ref TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add threads.pr_ref: %w", err)
	}
	return nil
}

// applyRebuildMigration runs a table-rebuild migration with foreign keys
// disabled. PRAGMA foreign_keys is a no-op inside a transaction, so the
// toggle must happen on a connection with no transaction open — we pin a
// single *sql.Conn for the whole operation rather than leaning on
// SetMaxOpenConns(1), keeping the FK-off window scoped to exactly this
// connection. Sequence: disable FK, run the rebuild + version bump in one
// transaction, verify integrity with foreign_key_check, commit, re-enable FK.
func applyRebuildMigration(db *sql.DB, m Migration) error {
	// Refuse rather than run the Fix: this path used to ignore it
	// silently, so a rebuild that also needed a Go-side data pass would
	// record itself as applied with half its work never done — a forward-
	// only chain has no second chance at that. Running it here instead
	// would look like the kinder choice, but it would quietly grant
	// Rebuild migrations a capability the Migration doc says they do not
	// have, and the fixups this chain carries all reshape ROWS of a table
	// whose shape a rebuild is concurrently replacing. Split the two into
	// consecutive migrations, which is what every existing pair does.
	if m.Fix != nil {
		return fmt.Errorf("migration v%d (%s): a Rebuild migration cannot carry a Fix; split the data fixup into its own migration", m.Version, m.Name)
	}
	log.Printf("store: applying rebuild migration v%d: %s", m.Version, m.Name)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection for rebuild v%d: %w", m.Version, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("disable foreign keys for rebuild v%d: %w", m.Version, err)
	}
	// Always restore enforcement before the connection returns to the pool,
	// even on failure — a leaked foreign_keys=OFF would silently disable
	// cascade integrity for the rest of the process. Deferred after
	// conn.Close so it runs first (LIFO).
	defer func() {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			log.Printf("store: WARNING failed to re-enable foreign_keys after rebuild v%d: %v", m.Version, err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rebuild v%d: %w", m.Version, err)
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("rebuild v%d (%s) failed: %w", m.Version, m.Name, err)
	}
	if err := assertForeignKeysIntact(ctx, tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("rebuild v%d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
		m.Version, m.Name,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record rebuild v%d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rebuild v%d: %w", m.Version, err)
	}

	log.Printf("store: rebuild migration v%d applied", m.Version)
	return nil
}

// assertForeignKeysIntact runs PRAGMA foreign_key_check and returns an error
// if any row references a missing parent. After a rebuild that copies ids
// verbatim this is always clean; the check is cheap insurance that turns a
// botched rebuild (which would otherwise silently strand child rows) into a
// loud migration failure + rollback. foreign_key_check works regardless of
// whether enforcement is currently enabled.
func assertForeignKeysIntact(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		// Columns: child table, child rowid, referenced table, fk index.
		var table, referred sql.NullString
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &referred, &fkid); err != nil {
			return fmt.Errorf("foreign_key_check scan: %w", err)
		}
		return fmt.Errorf("foreign key violation after rebuild: table %q row %d references missing %q",
			table.String, rowid.Int64, referred.String)
	}
	return rows.Err()
}
