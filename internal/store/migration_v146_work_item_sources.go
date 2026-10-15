package store

// Migration v146 removes 'automation' from work_items.source after v145
// mapped every such run to 'manual'. SQLite cannot narrow a CHECK in place,
// so the table is rebuilt with every other current column, CHECK and index
// exactly as v145 left them. work_items has no foreign key in either
// direction and no trigger or view names it.
const workItemSourcesMigrationVersion = 146

const workItemSourcesV146SQL = `CREATE TABLE work_items_new (
    id               TEXT    PRIMARY KEY,
    project_id       TEXT    NOT NULL,
    goal             TEXT    NOT NULL,
    workflow_id      TEXT    NOT NULL,
    workflow_scope   TEXT    NOT NULL CHECK(workflow_scope IN ('project','shared')),
    snapshot         TEXT    NOT NULL DEFAULT '' CHECK(snapshot = '' OR json_valid(snapshot)),
    state            TEXT    NOT NULL CHECK(state IN ('running','needs-human','done','failed','cancelled')),
    reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','provider-retries-exhausted','provider-usage-limited','loop-limit-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed','child-failed','paused','checkpoint')),
    seeds            TEXT    NOT NULL DEFAULT '' CHECK(seeds = '' OR json_valid(seeds)),
    step_mode        INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),
    soft_stop        INTEGER NOT NULL DEFAULT 0 CHECK(soft_stop IN (0,1)),
    wake_signature   TEXT    NOT NULL DEFAULT '',
    pending_guidance TEXT    NOT NULL DEFAULT '',
    auto_resume_at   INTEGER NOT NULL DEFAULT 0,
    worktree_path    TEXT    NOT NULL DEFAULT '',
    branch           TEXT    NOT NULL DEFAULT '',
    base_branch      TEXT    NOT NULL DEFAULT '',
    budget           TEXT    NOT NULL DEFAULT '' CHECK(budget = '' OR json_valid(budget)),
    source           TEXT    NOT NULL CHECK(source IN ('manual','agent','call')),
    source_ref       TEXT    NOT NULL DEFAULT '',
    triage_thread_id TEXT    NOT NULL DEFAULT '',
    origin_thread_id TEXT    NOT NULL DEFAULT '',
    disposition      TEXT    NOT NULL DEFAULT '' CHECK(disposition = '' OR json_valid(disposition)),
    digest           TEXT    NOT NULL DEFAULT '' CHECK(digest = '' OR json_valid(digest)),
    parent_item_id   TEXT    NOT NULL DEFAULT '',
    parent_phase_id  TEXT    NOT NULL DEFAULT '',
    parent_unit_id   TEXT    NOT NULL DEFAULT '',
    parent_attempt   INTEGER NOT NULL DEFAULT 0 CHECK(parent_attempt >= 0),
    call_depth       INTEGER NOT NULL DEFAULT 0 CHECK(call_depth >= 0),
    created_at       INTEGER NOT NULL,
    started_at       INTEGER NOT NULL DEFAULT 0,
    ended_at         INTEGER NOT NULL DEFAULT 0,
    CHECK((parent_item_id = '') = (parent_phase_id = '')),
    CHECK((parent_item_id = '') = (parent_attempt = 0)),
    CHECK((parent_item_id = '') = (call_depth = 0)),
    CHECK(parent_item_id = '' OR origin_thread_id = ''),
    CHECK(parent_unit_id = '' OR parent_item_id <> '')
);

INSERT INTO work_items_new (
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, soft_stop, wake_signature, pending_guidance, auto_resume_at,
    worktree_path, branch, base_branch, budget, source, source_ref,
    triage_thread_id, origin_thread_id, disposition, digest,
    parent_item_id, parent_phase_id, parent_unit_id, parent_attempt, call_depth,
    created_at, started_at, ended_at
)
SELECT
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, soft_stop, wake_signature, pending_guidance, auto_resume_at,
    worktree_path, branch, base_branch, budget, source, source_ref,
    triage_thread_id, origin_thread_id, disposition, digest,
    parent_item_id, parent_phase_id, parent_unit_id, parent_attempt, call_depth,
    created_at, started_at, ended_at
FROM work_items;

DROP TABLE work_items;

ALTER TABLE work_items_new RENAME TO work_items;

CREATE INDEX idx_work_items_project_state_created
  ON work_items(project_id, state, created_at);

CREATE INDEX idx_work_items_project_created
  ON work_items(project_id, created_at);

CREATE INDEX idx_work_items_state_created
  ON work_items(state, created_at, id);

CREATE INDEX idx_work_items_triage_thread
  ON work_items(triage_thread_id)
  WHERE triage_thread_id <> '';

CREATE UNIQUE INDEX idx_work_items_agent_source_ref
  ON work_items(source_ref)
  WHERE source = 'agent' AND source_ref <> '';

CREATE INDEX idx_work_items_parent
  ON work_items(parent_item_id, parent_phase_id, parent_attempt)
  WHERE parent_item_id <> '';

CREATE INDEX idx_work_items_origin_thread
  ON work_items(origin_thread_id)
  WHERE origin_thread_id <> '';
`
