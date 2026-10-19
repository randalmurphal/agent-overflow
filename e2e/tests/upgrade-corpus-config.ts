// The release upgrade check's configuration (docs/architecture/release-upgrade-check.md).
// Everything an author may need to touch when a migration lands is in this
// file; the check's failure messages name the entry to add.

// Production limits the check mirrors. Each is pinned to its Go constant by
// internal/wsllauncher/release_upgrade_mirror_test.go, so a product change
// fails that test until the mirror follows.
/** startupprogress.StallWindow: the longest a trial may go without progress. */
export const TRIAL_STALL_WINDOW_MS = 30_000;
/** supervise.TrialCeiling: the longest a trial may run however much it reports. */
export const TRIAL_CEILING_MS = 30 * 60_000;
/** wsllauncher.UpdateCommandStallWindow: the window an update command runs under. */
export const UPDATE_COMMAND_STALL_WINDOW_MS = TRIAL_STALL_WINDOW_MS + 15_000;
/** wsllauncher.UpdateCommandCeiling: the ceiling of one update command. */
export const UPDATE_COMMAND_CEILING_MS = 2 * TRIAL_CEILING_MS;
/** wsllauncher bootstrapProbeDeadline: the longest a starting boot may go without progress. */
export const BOOT_PROBE_DEADLINE_MS = 30_000;

/**
 * A MigrationException declares what one migration does to rows that
 * existed before it. The check compares every table of the pre-migration
 * snapshot with the migrated store and fails on any old row that is
 * missing or changed, on any table that shrank, and on any table that
 * disappeared, unless an entry here covers it. An entry applies only when
 * its version lies in the span the run migrates, so the list empties
 * itself as releases pass; an entry below the corpus's version is stale
 * and fails the check until it is removed.
 */
export interface MigrationException {
  /** The migration (migrations[].Version in internal/store) that makes the change. */
  version: number;
  table: string;
  /** Columns the migration rewrites in place; they drop out of the comparison. */
  columns?: string[];
  /** The migration removes or moves rows out of this table. */
  rowsMayShrink?: boolean;
  /** The migration drops the table. */
  dropped?: boolean;
  reason: string;
}

export const migrationExceptions: MigrationException[] = [
  { version: 144, table: 'channels', dropped: true, reason: 'v144 removes discussions and their channels' },
  { version: 144, table: 'channel_messages', dropped: true, reason: 'v144 removes discussions and their channel messages' },
  { version: 144, table: 'discussion_definitions', dropped: true, reason: 'v144 removes discussion definitions' },
  { version: 144, table: 'threads', columns: ['mode'], reason: 'v144 turns a former discussion thread into a chat thread' },
  { version: 144, table: 'scratch_threads', columns: ['return_mode'], reason: 'v144 returns a side chat opened from a discussion to chat mode' },
  { version: 144, table: 'chat_bar_favorites', rowsMayShrink: true, reason: 'v144 keeps only model favorites; discussion favorites are removed' },
  { version: 146, table: 'automations', dropped: true, reason: 'v146 removes the workflow scheduler and drops its automations' },
  { version: 146, table: 'automation_cursors', dropped: true, reason: "v146 drops the scheduler's per-source cursors with its automations" },
  { version: 146, table: 'work_items', columns: ['source', 'source_ref'], reason: 'v146 rewrites a run an automation started as a manual run with an empty source_ref, which named a dropped automation' },
  { version: 155, table: 'standing_agents', dropped: true, reason: 'v155 moves every standing agent to an agents record whose id is its thread id, and drops the table' },
  { version: 156, table: 'agent_wakes', columns: ['last_output', 'last_stderr'], rowsMayShrink: true, reason: 'v156 clears what a cancelled loop or watch carried and keeps each thread its 16 newest cancelled rows' },
  { version: 156, table: 'remote_watches', rowsMayShrink: true, reason: 'v156 rebuilds remote_watches to cascade from its thread and deletes the watches whose thread is already gone' },
  { version: 158, table: 'thread_requests', columns: ['lineage_only'], reason: 'v158 keeps the newest of two local spawns of one thread and makes the older one lineage-only' },
];

/**
 * Tables the old-row comparison skips, with the reason. Row counts are
 * still checked for them unless they are listed in countIgnoredTables.
 */
export const fingerprintIgnoredTables: Record<string, string> = {
  migration_versions: 'the chain records the migrations the run applied',
  deferred_migration_failures: 'written by the deferred phases under test',
};

/**
 * Tables whose row count may shrink across a run, with the reason: the
 * progress tables of work a boot finishes, which delete their rows as the
 * work completes.
 */
export const countIgnoredTables: Record<string, string> = {
  deferred_migration_failures: 'a retried phase clears its own failure rows',
  thread_search_build: 'the search index rebuild deletes its progress row when done',
  agent_end_backfill: "v124's deferred phase deletes each thread's row as it is settled",
  subagent_aggregate_backfill: "v121's deferred phase deletes each thread's row as it is recomputed",
  shown_history_fix: "v131's deferred phase clears its progress once every copy is fixed",
};

/**
 * The top-up (upgrade-corpus-topup.ts): what the candidate writes into
 * the corpus, natively at its own schema, after the migration. One thread
 * per library scenario here, driven with the mock provider, then the
 * sidebar and workflow operations. Every library scenario is either here
 * or in excludedScenarios with the reason; the coverage gate fails on one
 * that is in neither, so a new scenario is classified when it lands.
 */
export interface TopUpScenario {
  name: string;
  /** The turn asks for an approval, which the top-up answers this way. */
  approve?: 'allow' | 'deny';
  /** The turn ends with the mock exiting instead of completing. */
  exits?: boolean;
}

export const topUpScenarios: TopUpScenario[] = [
  { name: 'streaming-text' },
  { name: 'thinking-then-text' },
  { name: 'tool-call' },
  { name: 'tool-approval', approve: 'allow' },
  { name: 'tool-approval', approve: 'deny' },
  { name: 'file-edit' },
  { name: 'file-edit-diff' },
  { name: 'bench-mixed-turn' },
  { name: 'bench-subagent-fanout' },
  { name: 'usage-limit-claude' },
  { name: 'session-death', exits: true },
  { name: 'codex-basic' },
  { name: 'codex-approval', approve: 'allow' },
  { name: 'codex-approval', approve: 'deny' },
  { name: 'codex-collab-parallel-children' },
  { name: 'codex-revert-paginated' },
  { name: 'usage-limit-codex' },
];

export const excludedScenarios: Record<string, string> = {
  'bench-active-stream': 'paced at the reveal rate; writes the rows streaming-text and tool-call write',
  'bench-burst-stream': 'a text-delta flood; writes the rows streaming-text writes',
  'bench-giant-turn': 'seventy-five tool iterations; writes the rows bench-mixed-turn writes',
  'codex-collab-progress-message': 'needs a child message driven mid-run',
  'codex-collab-reload-after-unload': 'needs a followup_task driven mid-run',
  'codex-collab-send-message-queueonly': 'needs a send_message driven mid-run',
  'codex-collab-two-deliveries': 'writes the rows codex-collab-parallel-children writes',
  'codex-revert-legacy': 'writes the rows codex-revert-paginated writes',
  'codex-steer-while-running': 'parks mid-stream until steered',
  'soak-background-agents': 'never ends',
  'stall-forever': 'never ends',
  'step-gated': 'waits for an advance at every frame',
};

/** The corpus scale the coverage gate seeds before the top-up: seconds, not minutes. */
export const COVERAGE_CORPUS_SCALE = 0.02;

/**
 * What the corpus and the top-up do not write, with the reason. The
 * coverage gate (upgrade-corpus-coverage.spec.ts) fails on a table with
 * no rows, a column no row sets to a non-default value, or a CHECK value
 * no row carries, unless an entry here covers it; and on an entry that
 * has become covered or names something the schema no longer has, so the
 * list cannot go stale. An entry without columns or values covers the
 * whole table.
 */
export interface CoverageAllowance {
  table: string;
  columns?: string[];
  /** Column to the CHECK values no row carries. */
  values?: Record<string, Array<string | number>>;
  reason: string;
}

export const coverageAllowlist: CoverageAllowance[] = [
  // Bookkeeping a finished boot leaves empty.
  { table: 'agent_end_backfill', reason: "v124's deferred phase deletes each thread's row as it settles it" },
  { table: 'subagent_aggregate_backfill', reason: "v121's deferred phase deletes each thread's row as it recomputes it" },
  { table: 'shown_history_fix', reason: "v131's deferred phase clears its progress once every copy is fixed" },
  { table: 'deferred_migration_failures', reason: 'written only when a deferred phase fails an item, which the release check treats as a defect' },
  { table: 'flush_queue_items', reason: 'pending-send rows a turn flushes before it rests; empty once every turn is at rest' },
  { table: 'pending_background_task_terminals', reason: 'a background task whose terminal notification outran its tool row; cleared when the row lands' },

  // Identity: paired devices, browser sign-ins and passkeys, which the
  // harness page does not go through.
  { table: 'auth_audit', columns: ['reason', 'device_id', 'session_id', 'peer'], values: { outcome: ['refused'] }, reason: 'a refused or device-bound sign-in; the harness page signs in locally' },
  { table: 'devices', columns: ['key_thumbprint', 'passkey_credential_id', 'last_seen_at', 'revoked_at', 'proof_kind'], values: { class: ['browser', 'phone', 'cli', 'backend-peer'] }, reason: 'rows the pairing flow writes for a browser, phone, CLI or peer backend' },
  { table: 'own_devices', reason: 'this backend seen as a device by a peer it paired with' },
  { table: 'own_device_sessions', reason: 'sessions a peer backend opened on this one' },
  { table: 'pairing_links', reason: 'a minted pairing link (MintDevicePairing)' },
  { table: 'passkeys', reason: 'a WebAuthn registration from a browser' },
  { table: 'push_sender', reason: 'the push credential a paired phone registers' },
  { table: 'push_tokens', reason: 'push tokens paired phones register' },
  { table: 'refresh_secrets', reason: 'refresh secrets of device-bound sessions' },
  { table: 'recovery_codes', columns: ['consumed_at', 'consumed_by'], reason: 'a recovery code a sign-in consumed' },
  { table: 'sessions', columns: ['expires_at', 'revoked_at', 'last_seen_at'], values: { binding_class: ['device-bound', 'public'] }, reason: 'sessions a paired device or a public link opens' },
  { table: 'users', columns: ['disabled_at', 'webauthn_user_handle'], values: { role: ['member'] }, reason: 'a second (member) user, a disabled user or a passkey holder' },

  // Connected computers and the agent thread tools, which need a second
  // backend or an agent calling the tools.
  { table: 'remote_jobs', reason: 'a job on a connected computer' },
  { table: 'remote_watches', reason: 'a watch on a connected computer' },
  { table: 'thread_transfers', reason: 'a conversation moved or copied to a connected computer' },
  { table: 'thread_transfer_sessions', reason: 'a transfer session with a connected computer' },
  { table: 'thread_requests', reason: "an agent's thread_send or thread_ask request" },
  { table: 'thread_request_receipts', reason: "an agent's thread_send or thread_ask request" },
  { table: 'async_questions', reason: "an agent's thread_ask question" },
  { table: 'agents', columns: ['creator_agent_id'], reason: "an assistant another assistant made with thread_new_assistant, which is an agent's thread request" },
  {
    table: 'agent_wakes',
    columns: ['last_output', 'undelivered', 'missed'],
    values: { state: ['running'] },
    reason: "a wake that never reached the agent (last_output, undelivered), a skipped fire (missed) or a live run (state running): the top-up's one fire reaches the agent before another is due, and a run left live would keep its script running through every later boot of the corpus",
  },
  { table: 'scratch_threads', reason: 'a side chat not yet promoted; the next boot sweeps every one, so the corpus keeps none' },
  { table: 'work_item_effects', reason: "an agent's workflow tool calls on a run" },
  { table: 'session_start_failures', reason: "a session start that failed for a message the app queued for an agent (a wake, a remote completion, a thread request's answer); a later start that succeeds deletes it" },

  // Imports from a provider home.
  { table: 'import_history_items', reason: 'a provider session imported from a provider home' },
  { table: 'import_history_chunks', reason: 'a provider session imported from a provider home' },
  { table: 'import_history_payloads', reason: 'a provider session imported from a provider home' },
  { table: 'thread_import_chunks', reason: 'a provider session imported from a provider home' },
  { table: 'thread_import_item_overrides', reason: 'a repaired row of an imported session' },
  { table: 'thread_import_state', reason: 'a provider session imported from a provider home' },
  { table: 'threads', columns: ['import_source'], reason: 'a thread imported from a provider home' },

  // Plans and reviews.
  { table: 'threads', values: { mode: ['terminal', 'workflow-studio', 'workflow-triage', 'holder', 'scratch'], provider: ['claude-tui'] }, reason: 'threads their owning saga creates (a terminal, the workflow studio and triage, a fork holder, an unpromoted side chat) and the claude-tui provider, which no mock speaks' },
  { table: 'proposed_plans', reason: 'a plan a Claude turn proposes through ExitPlanMode, which no library scenario emits' },
  { table: 'proposed_plan_comments', reason: 'review comments on a proposed plan' },
  { table: 'thread_drafts', columns: ['pending_plan_implementation'], values: { has_content: [0] }, reason: 'a draft seeded from an accepted plan, and an empty draft that only holds attachments' },
  { table: 'diff_review_comments', reason: 'review comments on a pull request diff through the forge' },
  { table: 'thread_pull_requests', reason: "an open pull request a forge reports for a thread's linked worktree; the top-up seeds no forge pull request" },
  { table: 'chat_bar_favorites', values: { provider: ['codex', 'claude-tui'] }, reason: 'favorites of other providers; one favorite proves the table' },
  { table: 'chat_model_profiles', values: { provider: ['claude-tui'], reasoning_effort: ['none', 'minimal', 'low', 'high', 'xhigh'], runtime_mode: ['read-only', 'approval-required', 'auto-accept-edits'] }, reason: 'one remembered profile per provider and model holds the settings used last; the claude-tui provider has no mock' },

  // Provider accounting the mock does not report.
  { table: 'usage_ledger', reason: 'token usage the provider reports; the mock reports none' },
  { table: 'usage_pending', reason: 'token usage the provider reports; the mock reports none' },
  { table: 'provider_thread_cost', reason: 'cost the provider reports; the mock reports none' },
  { table: 'turns', columns: ['token_usage_json'], reason: 'token usage the provider reports; the mock reports none' },
  { table: 'threads', columns: ['last_token_usage', 'live_todo'], reason: 'token usage and a TodoWrite list, which no library scenario emits' },
  { table: 'workflow_provider_usage_scopes', reason: 'a workflow run parked on a provider usage limit' },
  { table: 'workflow_provider_usage_attention', reason: 'a workflow run parked on a provider usage limit' },

  // Transient states no run is left in, and item shapes no library
  // scenario emits.
  { table: 'items', values: { kind: ['compaction_reasoning', 'workflow_proposal', 'command_result', 'terminal_interaction', 'api_retry'], status: ['streaming', 'killed', 'parked'], decision: ['approved', 'amended', 'lost'] }, reason: 'compaction reasoning, API retry, terminal and workflow-proposal rows need scenarios no library entry provides; the approval scenarios raise their request without a toolUseId, so an allowed decision is not matched to the tool row that follows, and an amended or lost one needs an edited or abandoned request; streaming, killed and parked are mid-turn states' },
  { table: 'threads', columns: ['history_bulk_load', 'fork_preparing', 'fork_cut_turn_index', 'deleting', 'created_remote_url', 'worktree_setup_state'], reason: 'fork-in-progress, delete-in-progress and worktree-setup-in-progress markers, which rest at their defaults; the remote a thread was created from needs a forge remote' },
  { table: 'threads', values: { reasoning_effort: ['none', 'minimal'] }, reason: 'efforts no catalog model declares' },
  { table: 'subagent_aggregates', columns: ['state', 'transcript_count', 'transcript_newest_turn', 'transcript_newest_item'], reason: 'a subagent still running or failed when its thread rested, and transcript cursors of Codex children' },
  { table: 'work_items', columns: ['wake_signature', 'pending_guidance', 'auto_resume_at', 'budget', 'triage_thread_id', 'origin_thread_id', 'disposition', 'parent_item_id', 'parent_phase_id', 'parent_unit_id', 'parent_attempt', 'call_depth', 'soft_stop', 'source_ref'], values: { state: ['running', 'needs-human', 'failed'], workflow_scope: ['shared'], source: ['agent', 'call'], reason: ['gate', 'question', 'stuck', 'stalled', 'budget-exhausted', 'retries-exhausted', 'provider-retries-exhausted', 'provider-usage-limited', 'loop-limit-exhausted', 'check-failed-genuine', 'agent-error', 'wiring-error', 'disposition', 'setup-failed', 'taken-over', 'unit-failed', 'child-failed', 'paused', 'checkpoint'] }, reason: 'running and needs-human are states a run passes through; the rest are failures, budgets, child runs, shared-scope and agent-started runs the top-up does not stage; source_ref is set only on a run a phase started or a call phase invoked' },
  { table: 'work_item_phases', columns: ['park_cause', 'provider_usage_scope_id'], values: { status: ['running', 'failed', 'superseded'] }, reason: 'running is a state a phase passes through; failed, superseded and parked phases need a failing or looping run' },
  { table: 'work_item_units', reason: 'a phase fanned out into units (a fan-out workflow definition)' },
  { table: 'projects', columns: ['color', 'workflow_queue_paused', 'workflow_concurrency', 'remote_url', 'root_commit', 'repository_id'], reason: 'a verified repository_id needs a repository the forge serves (the top-up only writes a failed read); remote_url and root_commit are retired; the queue pause and concurrency are project settings the top-up leaves default; color is set by no App method' },

  // Fork internals.
  { table: 'edit_file_snapshots', reason: 'a file checkpoint of an Edit the mock performs for real; the library scenarios edit through frames only' },
  { table: 'thread_fork_hidden', reason: 'rows a fork hides from its source after the cut' },
  { table: 'thread_fork_walked', reason: 'the walked anchors of a fork whose source kept writing' },
  { table: 'payload_snapshots', reason: 'a payload snapshot of a fork whose source payload changed after the cut' },
  { table: 'payload_snapshot_chunks', reason: 'a payload snapshot of a fork whose source payload changed after the cut' },
  { table: 'payload_snapshot_edits', reason: 'a payload snapshot of a fork whose source payload changed after the cut' },
  { table: 'payload_snapshot_refs', reason: 'a payload snapshot of a fork whose source payload changed after the cut' },
  { table: 'thread_draft_recoveries', reason: 'the draft a revert-and-resend keeps while the resent turn runs' },
];
