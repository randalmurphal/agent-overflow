package store

// Rebuild DDL for the work_items table and the shared fragments its rebuilds
// substitute into one another (the reason CHECKs and copy-column lists). Newest
// rebuild first, which is also derivation order: v57 derives from v56, v56 from
// v44, v44 from v43, v43 from v39, v39 from v38.
//
// The chain driver, the derivation helpers, and the migrations slice are in
// migrate.go.

var rebuildWorkItemProviderUsageLimitedV57SQL = mustReplaceOnce(
	rebuildWorkItemRetryReasonsV56SQL,
	workItemsReasonCheckV56, workItemsReasonCheckV57,
)

const workItemsReasonCheckV57 = `reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','provider-retries-exhausted','provider-usage-limited','loop-limit-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed','child-failed','paused','checkpoint')),`

var rebuildWorkItemRetryReasonsV56SQL = mustReplaceOnce(
	mustReplaceOnce(
		mustReplaceOnce(
			mustReplaceOnce(
				rebuildWorkItemsSoftStopV44SQL,
				workItemsReasonCheckV44, workItemsReasonCheckV56,
			),
			"    soft_stop        INTEGER NOT NULL DEFAULT 0 CHECK(soft_stop IN (0,1)),",
			"    soft_stop        INTEGER NOT NULL DEFAULT 0 CHECK(soft_stop IN (0,1)),\n"+
				"    wake_signature   TEXT    NOT NULL DEFAULT '',\n"+
				"    pending_guidance  TEXT    NOT NULL DEFAULT '',\n"+
				"    auto_resume_at    INTEGER NOT NULL DEFAULT 0,",
		),
		workItemsCopyColumnsV44+"\n)",
		workItemsCopyColumnsV56+"\n)",
	),
	workItemsCopyColumnsV44+"\nFROM work_items;",
	workItemsCopyColumnsV56+"\nFROM work_items;",
)

const workItemsCopyColumnsV56 = workItemsCopyColumnsV44 + `,
    soft_stop, wake_signature, pending_guidance, auto_resume_at`

const workItemsReasonCheckV56 = `reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','provider-retries-exhausted','loop-limit-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed','child-failed','paused','checkpoint')),`

// rebuildWorkItemsSoftStopV44SQL adds `soft_stop` — a standing request to stop
// this run tree at its next call boundary — and widens the typed park reason
// set with `checkpoint`, the reason a run takes when that boundary fires (D36).
//
// The two ship together because they are one feature and one of them forces a
// rebuild regardless: SQLite cannot alter a CHECK in place, so widening `reason`
// costs a table rewrite that `soft_stop` may as well ride along with rather
// than paying for a second one.
//
// `soft_stop` is deliberately NOT constrained to root runs by a CHECK, unlike
// the call-linkage columns above. "Is this run a root" is readable from
// `parent_item_id`, so a CHECK would be expressible — but the rule it would
// encode is about who may *ask*, which the engine enforces before it writes,
// and a column-level refusal would surface as a corrupt-write error rather than
// as the refusal a human needs to read. The linkage CHECKs exist because a
// half-written linkage makes the tree unreadable; a soft stop on a child would
// merely never fire.
//
// Derived from the shipped v43 text, and the derivation extends v43's copy list
// with `parent_unit_id` — v43's own new column, absent from the list it
// inherited from v39. Same failure mode the v43 comment names: a rebuild
// derived from an earlier text silently drops whatever shipped in between.
// `soft_stop` itself is not copied; it is new here and defaults to disarmed.
var rebuildWorkItemsSoftStopV44SQL = mustReplaceOnce(
	mustReplaceOnce(
		mustReplaceOnce(
			mustReplaceOnce(
				rebuildWorkItemsUnitCallLinkageV43SQL,
				workItemsReasonCheckV39, workItemsReasonCheckV44,
			),
			"    step_mode        INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),",
			"    step_mode        INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),\n"+
				"    soft_stop        INTEGER NOT NULL DEFAULT 0 CHECK(soft_stop IN (0,1)),",
		),
		workItemsCopyColumnsV43+"\n)",
		workItemsCopyColumnsV44+"\n)",
	),
	workItemsCopyColumnsV43+"\nFROM work_items;",
	workItemsCopyColumnsV44+"\nFROM work_items;",
)

// workItemsCopyColumnsV44 is workItemsCopyColumnsV43 plus `parent_unit_id`, the
// column v43 itself created and therefore did not copy.
const workItemsCopyColumnsV44 = `    source, source_ref, triage_thread_id, origin_thread_id, disposition, digest,
    parent_item_id, parent_phase_id, parent_unit_id, parent_attempt, call_depth,
    created_at, started_at, ended_at`

// workItemsReasonCheckV44 is workItemsReasonCheckV39 widened with `checkpoint`:
// the park a soft stop produces when a run reaches the call boundary it was
// asked to stop at. It is deliberately not `paused` — both resume the same way,
// and the reason is what tells a human (and the wake message) that the run
// stopped exactly where they asked rather than being interrupted.
const workItemsReasonCheckV44 = `reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed','child-failed','paused','checkpoint')),`

// rebuildWorkItemsUnitCallLinkageV43SQL adds `parent_unit_id`: the fan-out unit
// whose call created a run. v38's linkage identifies an invocation by (item,
// phase, attempt), which is unique for a `shape: call` phase — it makes exactly
// one call — but not for a fan-out phase, where every call-bound unit of one
// attempt would otherwise be indistinguishable from its siblings and a parent
// could not tell which child it was waiting on.
//
// The fifth CHECK keeps the linkage all-or-nothing in the same direction the
// other four do: a unit id without a parent item would name a unit of no run.
// The column is empty for a phase call, which is what makes "this child came
// from a call phase" and "this child came from a fan-out unit" distinguishable
// without a second nullable column.
//
// SQLite can alter neither a CHECK nor a column list in place, so this follows
// the established table-rebuild pattern, derived from the shipped v39 text
// rather than retyped. The derivation EXTENDS v39's copy list with
// `origin_thread_id` — v39's own new column, absent from the list it inherited
// from v38 — because re-running that text against a v39 table would otherwise
// unbind every run from the thread it reports into.
var rebuildWorkItemsUnitCallLinkageV43SQL = mustReplaceOnce(
	mustReplaceOnce(
		mustReplaceOnce(
			mustReplaceOnce(
				rebuildWorkItemsThreadBindingV39SQL,
				"    parent_phase_id  TEXT    NOT NULL DEFAULT '',",
				"    parent_phase_id  TEXT    NOT NULL DEFAULT '',\n    parent_unit_id   TEXT    NOT NULL DEFAULT '',",
			),
			"    CHECK(parent_item_id = '' OR origin_thread_id = '')",
			"    CHECK(parent_item_id = '' OR origin_thread_id = ''),\n    CHECK(parent_unit_id = '' OR parent_item_id <> '')",
		),
		workItemsCopyColumnsV39+"\n)",
		workItemsCopyColumnsV43+"\n)",
	),
	workItemsCopyColumnsV39+"\nFROM work_items;",
	workItemsCopyColumnsV43+"\nFROM work_items;",
) + `
CREATE INDEX idx_work_items_automation_source_ref
  ON work_items(source_ref, state)
  WHERE source = 'automation' AND source_ref <> '';
`

// The v39 text this rebuild is derived from recreates the indexes that existed
// when *it* shipped, so every work_items index added after v39 has to be
// restated here or the rebuild silently drops it — v40's automation-overlap
// index above is the one that qualifies. No index is added for `parent_unit_id`
// itself: every read of it is already scoped to one parent through
// `idx_work_items_parent`, which bounds the row set to one run's children.

// workItemsCopyColumnsV43 is workItemsCopyColumnsV39 plus `origin_thread_id`,
// the column v39 itself created and therefore did not copy.
const workItemsCopyColumnsV43 = `    source, source_ref, triage_thread_id, origin_thread_id, disposition, digest,
    parent_item_id, parent_phase_id, parent_attempt, call_depth,
    created_at, started_at, ended_at`

// workItemsReasonCheckV38 is the typed park-reason CHECK shipped by v38;
// workItemsReasonCheckV39 is the same constraint widened with `paused`, the
// reason a deliberate pause (human or graceful quit) parks under. It is
// distinct from `interrupted` on purpose: both resume identically, and the
// reason is what tells a human whether the run stopped by intent or by death.
const (
	workItemsReasonCheckV38 = `reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed','child-failed')),`
	workItemsReasonCheckV39 = `reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed','child-failed','paused')),`
)

// rebuildWorkItemsThreadBindingV39SQL records a root run's bound origin thread
// (§5, D17) and widens the typed park reason set with `paused` (D23).
//
// `origin_thread_id` is the thread a resting transition wakes. Only a *root*
// run may carry one — a called run surfaces through its caller's tree and never
// binds — and the fourth CHECK below makes that structural rather than a rule
// callers have to remember. There is no threads foreign key, matching
// `triage_thread_id`: run history outlives the thread it was started from, and
// a deleted bound thread degrades to the unbound surface at wake time.
//
// SQLite can alter neither a CHECK nor a column list in place, so this follows
// the established table-rebuild pattern. It is derived from the shipped v38
// text rather than retyped, which makes accidental column/index drift
// impossible. The derivation also has to EXTEND v38's copy lists with the call
// linkage v38 itself introduced: v38's INSERT copies the columns that existed
// at v37, so re-running that text against a v38 table would silently drop every
// run's parent linkage. `origin_thread_id` is not copied — it is new here and
// defaults empty.
var rebuildWorkItemsThreadBindingV39SQL = mustReplaceOnce(
	mustReplaceOnce(
		mustReplaceOnce(
			mustReplaceOnce(
				mustReplaceOnce(
					rebuildWorkItemsCallLinkageV38SQL,
					workItemsReasonCheckV38, workItemsReasonCheckV39,
				),
				"    triage_thread_id TEXT    NOT NULL DEFAULT '',",
				"    triage_thread_id TEXT    NOT NULL DEFAULT '',\n    origin_thread_id TEXT    NOT NULL DEFAULT '',",
			),
			"    CHECK((parent_item_id = '') = (call_depth = 0))",
			"    CHECK((parent_item_id = '') = (call_depth = 0)),\n    CHECK(parent_item_id = '' OR origin_thread_id = '')",
		),
		workItemsCopyColumnsV38+"\n)",
		workItemsCopyColumnsV39+"\n)",
	),
	workItemsCopyColumnsV38+"\nFROM work_items;",
	workItemsCopyColumnsV39+"\nFROM work_items;",
) + `
CREATE INDEX idx_work_items_origin_thread
  ON work_items(origin_thread_id)
  WHERE origin_thread_id <> '';
`

// workItemsCopyColumnsV38 is the column list v38's rebuild copies (the v37
// column set); workItemsCopyColumnsV39 adds the linkage columns v38 created, so
// a rebuild derived from v38's text carries them instead of resetting them.
const (
	workItemsCopyColumnsV38 = `    source, source_ref, triage_thread_id, disposition, digest,
    created_at, started_at, ended_at`
	workItemsCopyColumnsV39 = `    source, source_ref, triage_thread_id, disposition, digest,
    parent_item_id, parent_phase_id, parent_attempt, call_depth,
    created_at, started_at, ended_at`
)

// rebuildWorkItemsCallLinkageV38SQL links a run to the call phase that invoked
// it: the parent item, the parent phase attempt, and the depth of the tree it
// sits at. `call` joins the source set (nobody enqueued the run — a phase did)
// and `child-failed` joins the typed reason set (the parent's call phase failed
// because its child run did). SQLite cannot alter a CHECK in place, so this
// follows the established table-rebuild pattern; every other column, value, and
// index is carried over unchanged, plus one partial index for the children-of
// lookup the run tree is read through.
const rebuildWorkItemsCallLinkageV38SQL = `
CREATE TABLE work_items_new (
    id               TEXT    PRIMARY KEY,
    project_id       TEXT    NOT NULL,
    goal             TEXT    NOT NULL,
    workflow_id      TEXT    NOT NULL,
    workflow_scope   TEXT    NOT NULL CHECK(workflow_scope IN ('project','shared')),
    snapshot         TEXT    NOT NULL DEFAULT '' CHECK(snapshot = '' OR json_valid(snapshot)),
    state            TEXT    NOT NULL CHECK(state IN ('running','needs-human','done','failed','cancelled')),
    reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed','child-failed')),
    seeds            TEXT    NOT NULL DEFAULT '' CHECK(seeds = '' OR json_valid(seeds)),
    step_mode        INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),
    worktree_path    TEXT    NOT NULL DEFAULT '',
    branch           TEXT    NOT NULL DEFAULT '',
    base_branch      TEXT    NOT NULL DEFAULT '',
    budget           TEXT    NOT NULL DEFAULT '' CHECK(budget = '' OR json_valid(budget)),
    source           TEXT    NOT NULL CHECK(source IN ('manual','agent','automation','call')),
    source_ref       TEXT    NOT NULL DEFAULT '',
    triage_thread_id TEXT    NOT NULL DEFAULT '',
    disposition      TEXT    NOT NULL DEFAULT '' CHECK(disposition = '' OR json_valid(disposition)),
    digest           TEXT    NOT NULL DEFAULT '' CHECK(digest = '' OR json_valid(digest)),
    parent_item_id   TEXT    NOT NULL DEFAULT '',
    parent_phase_id  TEXT    NOT NULL DEFAULT '',
    parent_attempt   INTEGER NOT NULL DEFAULT 0 CHECK(parent_attempt >= 0),
    call_depth       INTEGER NOT NULL DEFAULT 0 CHECK(call_depth >= 0),
    created_at       INTEGER NOT NULL,
    started_at       INTEGER NOT NULL DEFAULT 0,
    ended_at         INTEGER NOT NULL DEFAULT 0,
    CHECK((parent_item_id = '') = (parent_phase_id = '')),
    CHECK((parent_item_id = '') = (parent_attempt = 0)),
    CHECK((parent_item_id = '') = (call_depth = 0))
);

INSERT INTO work_items_new (
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, worktree_path, branch, base_branch, budget,
    source, source_ref, triage_thread_id, disposition, digest,
    created_at, started_at, ended_at
)
SELECT
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, worktree_path, branch, base_branch, budget,
    source, source_ref, triage_thread_id, disposition, digest,
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
  WHERE parent_item_id <> '';`

// rebuildWorkItemsUnitFailedReasonV36SQL widens the typed park reason set with
// `unit-failed`: a fan-out unit that exhausted its retries stops the launch of
// not-yet-started units and parks the run with the failed unit's record. SQLite
// cannot alter a CHECK in place, so the established table-rebuild pattern
// applies; every other column, value, and index is carried over unchanged.
const rebuildWorkItemsUnitFailedReasonV36SQL = `
CREATE TABLE work_items_new (
    id               TEXT    PRIMARY KEY,
    project_id       TEXT    NOT NULL,
    goal             TEXT    NOT NULL,
    workflow_id      TEXT    NOT NULL,
    workflow_scope   TEXT    NOT NULL CHECK(workflow_scope IN ('project','shared')),
    snapshot         TEXT    NOT NULL DEFAULT '' CHECK(snapshot = '' OR json_valid(snapshot)),
    state            TEXT    NOT NULL CHECK(state IN ('running','needs-human','done','failed','cancelled')),
    reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed')),
    seeds            TEXT    NOT NULL DEFAULT '' CHECK(seeds = '' OR json_valid(seeds)),
    step_mode        INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),
    worktree_path    TEXT    NOT NULL DEFAULT '',
    branch           TEXT    NOT NULL DEFAULT '',
    base_branch      TEXT    NOT NULL DEFAULT '',
    budget           TEXT    NOT NULL DEFAULT '' CHECK(budget = '' OR json_valid(budget)),
    source           TEXT    NOT NULL CHECK(source IN ('manual','agent','automation')),
    source_ref       TEXT    NOT NULL DEFAULT '',
    triage_thread_id TEXT    NOT NULL DEFAULT '',
    disposition      TEXT    NOT NULL DEFAULT '' CHECK(disposition = '' OR json_valid(disposition)),
    digest           TEXT    NOT NULL DEFAULT '' CHECK(digest = '' OR json_valid(digest)),
    created_at       INTEGER NOT NULL,
    started_at       INTEGER NOT NULL DEFAULT 0,
    ended_at         INTEGER NOT NULL DEFAULT 0
);

INSERT INTO work_items_new (
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, worktree_path, branch, base_branch, budget,
    source, source_ref, triage_thread_id, disposition, digest,
    created_at, started_at, ended_at
)
SELECT
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, worktree_path, branch, base_branch, budget,
    source, source_ref, triage_thread_id, disposition, digest,
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
`

// rebuildWorkItemsTakeoverTriageV28SQL adds the durable triage-thread link and
// extends the typed reason CHECK for takeover parks. SQLite cannot alter a
// CHECK constraint in place, so the established table-rebuild pattern is
// required. Run-record tables intentionally have no work_items foreign keys.
const rebuildWorkItemsTakeoverTriageV28SQL = `
CREATE TABLE work_items_new (
    id               TEXT    PRIMARY KEY,
    project_id       TEXT    NOT NULL,
    goal             TEXT    NOT NULL,
    workflow_id      TEXT    NOT NULL,
    workflow_scope   TEXT    NOT NULL CHECK(workflow_scope IN ('project','shared')),
    snapshot         TEXT    NOT NULL DEFAULT '' CHECK(snapshot = '' OR json_valid(snapshot)),
    state            TEXT    NOT NULL CHECK(state IN ('queued','running','needs-human','done','failed','cancelled')),
    reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over')),
    sort_position    INTEGER NOT NULL,
    seeds            TEXT    NOT NULL DEFAULT '' CHECK(seeds = '' OR json_valid(seeds)),
    step_mode        INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),
    worktree_path    TEXT    NOT NULL DEFAULT '',
    branch           TEXT    NOT NULL DEFAULT '',
    base_branch      TEXT    NOT NULL DEFAULT '',
    budget           TEXT    NOT NULL DEFAULT '' CHECK(budget = '' OR json_valid(budget)),
    source           TEXT    NOT NULL CHECK(source IN ('manual','agent','automation')),
    source_ref       TEXT    NOT NULL DEFAULT '',
    triage_thread_id TEXT    NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    started_at       INTEGER NOT NULL DEFAULT 0,
    ended_at         INTEGER NOT NULL DEFAULT 0
);

INSERT INTO work_items_new (
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    sort_position, seeds, step_mode, worktree_path, branch, base_branch, budget,
    source, source_ref, triage_thread_id, created_at, started_at, ended_at
)
SELECT
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    sort_position, seeds, step_mode, worktree_path, branch, base_branch, budget,
    source, source_ref, '', created_at, started_at, ended_at
FROM work_items;

DROP TABLE work_items;

ALTER TABLE work_items_new RENAME TO work_items;

CREATE INDEX idx_work_items_project_state_sort
  ON work_items(project_id, state, sort_position, created_at);

CREATE INDEX idx_work_items_project_sort
  ON work_items(project_id, sort_position, created_at);

CREATE INDEX idx_work_items_triage_thread
  ON work_items(triage_thread_id)
  WHERE triage_thread_id <> '';

CREATE INDEX idx_work_item_phases_thread
  ON work_item_phases(thread_id, started_at DESC, attempt DESC)
  WHERE thread_id <> '';
`

// rebuildWorkItemsDirectStartV33SQL removes the work queue from the run
// record. It (a) drops `queued` from the state CHECK — a run starts running
// and contention is a phase waiting on resource capacity, not an item waiting
// in a line — and (b) drops the `sort_position` queue-order column. SQLite can
// alter neither in place, so the established table-rebuild pattern applies;
// dropping the column is free inside the rebuild the CHECK already requires.
//
// Resident `queued` rows are runs that were admitted but never started. They
// park `needs-human(interrupted)` — the same resting state and typed reason a
// crash produces — so the morning-after view explains them and the ordinary
// resume path continues them. After this migration `queued` is unrepresentable,
// which is why the engine's startup rebuild carries no legacy-queued branch.
const rebuildWorkItemsDirectStartV33SQL = `
CREATE TABLE work_items_new (
    id               TEXT    PRIMARY KEY,
    project_id       TEXT    NOT NULL,
    goal             TEXT    NOT NULL,
    workflow_id      TEXT    NOT NULL,
    workflow_scope   TEXT    NOT NULL CHECK(workflow_scope IN ('project','shared')),
    snapshot         TEXT    NOT NULL DEFAULT '' CHECK(snapshot = '' OR json_valid(snapshot)),
    state            TEXT    NOT NULL CHECK(state IN ('running','needs-human','done','failed','cancelled')),
    reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over')),
    seeds            TEXT    NOT NULL DEFAULT '' CHECK(seeds = '' OR json_valid(seeds)),
    step_mode        INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),
    worktree_path    TEXT    NOT NULL DEFAULT '',
    branch           TEXT    NOT NULL DEFAULT '',
    base_branch      TEXT    NOT NULL DEFAULT '',
    budget           TEXT    NOT NULL DEFAULT '' CHECK(budget = '' OR json_valid(budget)),
    source           TEXT    NOT NULL CHECK(source IN ('manual','agent','automation')),
    source_ref       TEXT    NOT NULL DEFAULT '',
    triage_thread_id TEXT    NOT NULL DEFAULT '',
    disposition      TEXT    NOT NULL DEFAULT '' CHECK(disposition = '' OR json_valid(disposition)),
    digest           TEXT    NOT NULL DEFAULT '' CHECK(digest = '' OR json_valid(digest)),
    created_at       INTEGER NOT NULL,
    started_at       INTEGER NOT NULL DEFAULT 0,
    ended_at         INTEGER NOT NULL DEFAULT 0
);

INSERT INTO work_items_new (
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, worktree_path, branch, base_branch, budget,
    source, source_ref, triage_thread_id, disposition, digest,
    created_at, started_at, ended_at
)
SELECT
    id, project_id, goal, workflow_id, workflow_scope, snapshot,
    CASE WHEN state = 'queued' THEN 'needs-human' ELSE state END,
    CASE WHEN state = 'queued' THEN 'interrupted' ELSE reason END,
    seeds, step_mode, worktree_path, branch, base_branch, budget,
    source, source_ref, triage_thread_id, disposition, digest,
    created_at, started_at,
    CASE WHEN state = 'queued' THEN created_at ELSE ended_at END
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
`
