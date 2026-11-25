// The release upgrade check's configuration (docs/architecture/release-upgrade-check.md).
// Everything an author may need to touch when a migration or a boot writer
// lands is in this file; the check's failure messages name the entry to add.

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
  { version: 154, table: 'channels', dropped: true, reason: 'v154 removes discussions and their channels' },
  { version: 154, table: 'channel_messages', dropped: true, reason: 'v154 removes discussions and their channel messages' },
  { version: 154, table: 'discussion_definitions', dropped: true, reason: 'v154 removes discussion definitions' },
  { version: 154, table: 'threads', columns: ['mode'], reason: 'v154 turns a former discussion thread into a chat thread' },
  { version: 154, table: 'scratch_threads', columns: ['return_mode'], reason: 'v154 returns a side chat opened from a discussion to chat mode' },
  { version: 154, table: 'chat_bar_favorites', rowsMayShrink: true, reason: 'v154 keeps only model favorites; discussion favorites are removed' },
  { version: 154, table: 'automations', dropped: true, reason: 'v154 removes the workflow scheduler and drops its automations' },
  { version: 154, table: 'automation_cursors', dropped: true, reason: "v154 drops the scheduler's per-source cursors with its automations" },
  { version: 154, table: 'work_items', columns: ['source', 'source_ref'], reason: 'v154 rewrites a run an automation started as a manual run with an empty source_ref, which named a dropped automation' },
  { version: 154, table: 'remote_watches', rowsMayShrink: true, reason: 'v154 rebuilds remote_watches to cascade from its thread and deletes the watches whose thread is already gone' },
  { version: 158, table: 'thread_requests', columns: ['next_check'], reason: 'v158 makes the acknowledgement it owes again for an answer from a paired computer due at once' },
  { version: 165, table: 'tracker_cache_issue_details', dropped: true, reason: 'v165 drops the documents of opened tracker issues; each ticket body is read again into tracker_cache_issue_bodies at its next opening' },  // v167 empties the tracker mirror as a restore leaves it: its cards gain a title column their blob no longer carries, and every card is read again whole.
  { version: 167, table: 'tracker_cache_board_column_statuses', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_board_columns', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_board_issues', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_board_unexplained', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_boards', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_changes', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_issue_bodies', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_issue_sprints', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_issues', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_open_tickets', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_roots', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_sprints', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_statuses', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_sync', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_users', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_worklog_days', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 167, table: 'tracker_cache_worklogs', rowsMayShrink: true, reason: 'v167 empties the tracker mirror, a cache every connection reads again' },
  { version: 168, table: 'tracker_cache_worklog_days', rowsMayShrink: true, reason: 'v168 deletes logged time read before it, since nothing records which time source read it; a refresh reads it again' },
  { version: 168, table: 'tracker_cache_worklogs', rowsMayShrink: true, reason: 'v168 deletes logged time read before it, since nothing records which time source read it; a refresh reads it again' },
  { version: 169, table: 'tracker_cache_worklog_days', rowsMayShrink: true, reason: 'v169 deletes logged time read before it, which holds no ticket names; a refresh reads it again' },
  { version: 169, table: 'tracker_cache_worklogs', rowsMayShrink: true, reason: 'v169 deletes logged time read before it, which holds no ticket names; a refresh reads it again' },
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
 * Columns, as `table.column`, the old-row comparison skips, with the
 * reason: derived data no read serves, which its triggers and backfill
 * rewrite whenever the row's text does.
 */
export const fingerprintIgnoredColumns: Record<string, string> = {
  'items.find_text': "v150's thread-find candidate text, refilled by its triggers, its deferred backfill, v153 and v157",
  'import_history_items.find_text': "v150's thread-find candidate text, refilled by its triggers, its deferred backfill, v153 and v157",
};

/**
 * Tables whose row count may shrink across a run, with the reason: the
 * progress tables of work a boot finishes, which delete their rows as the
 * work completes.
 */
export const countIgnoredTables: Record<string, string> = {
  deferred_migration_failures: 'a retried phase clears its own failure rows',
  thread_search_build: 'the search index rebuild deletes its progress row when done',
  thread_search: 'the contentless FTS index of thread_search_rows, whose rows the census and fingerprint compare',
  agent_end_backfill: "v124's deferred phase deletes each thread's row as it is settled",
  subagent_aggregate_backfill: "v121's deferred phase deletes each thread's row as it is recomputed",
  shown_history_fix: "v131's deferred phase clears its progress once every copy is fixed",
};

/**
 * A RecoveryRule declares one change the candidate's boot makes to a row it
 * finds, almost always settling state the previous process left live: a
 * clone of a database whose app was running carries running rows, open
 * turns and queued messages, and the update trial's boot settles them
 * before the fingerprint reads the store. The check fails on any change the
 * boot made to an old row that no rule explains, and reports every rule's
 * count. Each entry names the Go writer and mirrors its own selection.
 */
export interface RecoveryRule {
  name: string;
  /** The Go function that makes the change. */
  writer: string;
  table: string;
  /**
   * The rows it applies to, as an SQL condition over the row `s` as the
   * migrations left it, which is what the Go writer read (the snapshot
   * migrated alone; its other tables are `mig.<table>`, the booted store's
   * `main.<table>`). A rule never reads the snapshot itself, whose schema
   * may predate the columns it names. Or 'thread-write': the
   * row's thread had a row of `items` changed, removed or added by the
   * boot, or for the `items` and `threads` stamp rules also a changed
   * `subagent_aggregates` row. Stamps move with the writes that cause them,
   * never on their own.
   */
  rows: string | 'thread-write';
  /** The columns it may change from `s`, ['*'] for any. */
  columns?: string[];
  /** Keys of `meta` it may set; the rest of `meta` must be unchanged. */
  metaKeys?: string[];
  /** A condition over the booted row `b` (and `s`) a changed row must meet. */
  to?: string;
  /** It deletes the row. */
  removes?: boolean;
  /** The columns that identify a row of a table that declares no primary key. */
  key?: string[];
}

/** A key of `meta` on the row `alias`, null when `meta` is not JSON. */
const meta = (alias: string, key: string) => `(CASE WHEN json_valid(${alias}.meta) THEN json_extract(${alias}.meta, '$.${key}') END)`;
/** The Claude background launch `x` no live agent or shell will report: RecoverOrphanedBackgroundTasks' selection. */
const orphanedClaudeLaunch = (x: string) => `${x}.kind = 'tool_call' AND ${x}.status = 'running' AND ${x}.is_background = 1
  AND COALESCE(${meta(x, 'live_background_active')}, 1) != 0
  AND NOT EXISTS (SELECT 1 FROM mig.items c WHERE c.thread_id = ${x}.thread_id AND c.completion_of = ${x}.id AND c.completion_of <> '')
  AND EXISTS (SELECT 1 FROM mig.threads t WHERE t.id = ${x}.thread_id AND t.provider IN ('claude', 'claude-tui'))`;
/**
 * The user row `x` of a message the previous process wrote to the provider
 * and never saw confirmed: still marked pendingFlush, and residue, which
 * store's markedUserRowSQL and unconfirmedUserRowResidueSQL
 * (flush_queue_items.go) define: no older than its thread, and no later turn
 * or top-level row follows it. A mark on any other row is history, which
 * v152 clears and the boot leaves alone.
 */
export const unconfirmedUserRow = (x: string) => `${x}.kind = 'user_text' AND ${x}.parent_id = '' AND ${meta(x, 'pendingFlush')} = 1
  AND ${x}.created_at >= (SELECT t.created_at FROM mig.threads t WHERE t.id = ${x}.thread_id)
  AND NOT EXISTS (SELECT 1 FROM mig.turns t WHERE t.thread_id = ${x}.thread_id AND t.turn_index > ${x}.turn_index)
  AND NOT EXISTS (SELECT 1 FROM mig.items l WHERE l.thread_id = ${x}.thread_id AND l.turn_index > ${x}.turn_index AND l.parent_id = '')`;
/**
 * The send id `sendId` names a message whose owner takes it back when the
 * boot finds it undelivered (app.ownedSendID): a request's answer or late
 * reply, a remote job's completion, or a spawner's answer to its worker's
 * async questions. None of them goes to the composer.
 */
const ownedSend = (sendId: string) => `(${sendId} GLOB 'thread-wake:?*' OR ${sendId} GLOB 'thread-wake-late:?*'
  OR ${sendId} GLOB 'remote-completion:*:*' OR ${sendId} GLOB 'agent-answer:*')`;
/** The send id of the message that carried the first answer of the thread request `x`: its carrier's (delivered_with), or its own. */
const firstWakeSend = (x: string) => `'thread-wake:' || CASE WHEN ${x}.delivered_with <> '' THEN ${x}.delivered_with ELSE ${x}.token END`;
/**
 * The thread request `x` whose wake the boot gives back to it, owed again
 * and disarmed (store.WithdrawThreadWake): its first answer or its late
 * reply is delivered and not confirmed, either still on a queue row
 * (restoreThreadQueueAtBoot) or handed to the provider
 * (giveBackUnconfirmedAtBoot), and no row carrying the provider's id
 * answers for its message (confirmEchoedAtBoot confirms that one instead).
 * A confirmed wake is never given back: a queue row that outlived its echo
 * goes alone. A lineage-only row keeps its marks.
 */
const wakeGivenBack = (x: string) => `${x}.lineage_only = 0 AND (
  (${x}.delivered_at IS NOT NULL AND ${x}.confirmed_at IS NULL
    AND NOT (${sendConfirmed(`${x}.caller_thread_id`, firstWakeSend(x))}))
  OR (${x}.late_delivered_at IS NOT NULL AND ${x}.late_confirmed_at IS NULL
    AND NOT (${sendConfirmed(`${x}.caller_thread_id`, `'thread-wake-late:' || ${x}.token`)})))`;
/**
 * The thread request `x` whose unconfirmed delivery the boot confirms from
 * the row of its echo (app.confirmEchoedAtBoot, store.ConfirmThreadWake):
 * the last run died between the echo's row write and its confirmation.
 */
const wakeEchoConfirmed = (x: string) => `${x}.lineage_only = 0 AND (
  (${x}.delivered_at IS NOT NULL AND ${x}.confirmed_at IS NULL
    AND ${sendConfirmed(`${x}.caller_thread_id`, firstWakeSend(x))})
  OR (${x}.late_delivered_at IS NOT NULL AND ${x}.late_confirmed_at IS NULL
    AND ${sendConfirmed(`${x}.caller_thread_id`, `'thread-wake-late:' || ${x}.token`)}))`;
/**
 * The confirmed thread request `x` whose wake's queue row outlived the echo:
 * the boot drops that row, and before it confirms the send from its echo's
 * row again (app.confirmEchoedAtBoot), which on a confirmed wake only names
 * that row (store.ConfirmThreadWake), as a wake v155 confirmed never has.
 */
const wakeRowNamed = (x: string) => `${x}.lineage_only = 0 AND (
  (${x}.delivered_at IS NOT NULL AND ${x}.confirmed_at IS NOT NULL
    AND EXISTS (SELECT 1 FROM mig.flush_queue_items q WHERE q.thread_id = ${x}.caller_thread_id AND q.send_id = ${firstWakeSend(x)})
    AND ${sendConfirmed(`${x}.caller_thread_id`, firstWakeSend(x))})
  OR (${x}.late_delivered_at IS NOT NULL AND ${x}.late_confirmed_at IS NOT NULL
    AND EXISTS (SELECT 1 FROM mig.flush_queue_items q WHERE q.thread_id = ${x}.caller_thread_id AND q.send_id = 'thread-wake-late:' || ${x}.token)
    AND ${sendConfirmed(`${x}.caller_thread_id`, `'thread-wake-late:' || ${x}.token`)}))`;
/** The send id of the remote watch `x`'s completion (app.remoteCompletionSendID). */
const completionSend = (x: string) => `'remote-completion:' || ${x}.computer_id || ':' || ${x}.request_id`;
/** The pairing link `x` of an approved own-device admission the last run did not finish: store.IncompleteOwnAdmissions' selection. */
const incompleteOwnAdmission = (x: string) => `${x}.purpose <> '' AND ${x}.confirmed_at IS NOT NULL AND ${x}.canceled_at IS NULL
  AND ${x}.session_id NOT IN (SELECT session_id FROM mig.own_device_sessions)
  AND ${x}.session_id IN (SELECT id FROM mig.sessions WHERE revoked_at IS NULL)`;
/**
 * The device `device` held an unrevoked session the boot ended and holds
 * none after it: a device store.endSessionsTx finds unreachable.
 */
const deviceLostItsLastSession = (device: string) => `EXISTS (SELECT 1 FROM mig.sessions k WHERE k.device_id = ${device} AND k.revoked_at IS NULL)
  AND NOT EXISTS (SELECT 1 FROM main.sessions k WHERE k.device_id = ${device} AND k.revoked_at IS NULL)`;
/**
 * The totals store.tallyFiguresSQL gives thread `t` at `now`: its tool
 * calls; its tokens with its pending usage; and its runtime with its newest
 * turn's span to `now` while that turn is open, or without that span
 * (settledTurnMs). Read from the tables as the migrations left them, which
 * is what the session ends read: identity.Bootstrap (app.initIdentity) runs
 * before every other boot writer.
 */
const tallyFigures = (t: string, now: string) => {
  const settledTurnMs = `COALESCE((SELECT turn_ms FROM mig.thread_tallies WHERE thread_id = ${t}), 0)`;
  return {
    toolCalls: `COALESCE((SELECT tool_calls FROM mig.thread_tallies WHERE thread_id = ${t}), 0)`,
    tokens: `(COALESCE((SELECT tokens FROM mig.thread_tallies WHERE thread_id = ${t}), 0)
      + COALESCE((SELECT SUM(input_tokens + cache_creation_input_tokens + output_tokens) FROM mig.usage_pending WHERE thread_id = ${t}), 0))`,
    settledTurnMs,
    turnMs: `(${settledTurnMs}
      + COALESCE((SELECT CASE WHEN nt.completed_at IS NULL THEN MAX(0, ${now} - nt.started_at) END
        FROM mig.turns nt WHERE nt.thread_id = ${t} ORDER BY nt.turn_index DESC LIMIT 1), 0))`,
  };
};
/**
 * The latest booted updated_at of the links releaseSpawnLinksTx released
 * for the run row `x`'s target, owner and source thread, over those links
 * `where` admits. The release stamps each link it releases with the moment
 * it ends the runs.
 */
const releasedLinksStamp = (x: string, where: string) => `(SELECT MAX(l.updated_at) FROM main.thread_request_receipts l
  JOIN mig.thread_request_receipts ml ON ml.token = l.token
  WHERE ml.spawn_link = 1 AND l.spawn_link = 0 AND ml.target_thread_id = ${x}.target_thread_id
    AND ml.owner_device_id = ${x}.owner_device_id AND ml.source_thread_id = ${x}.source_thread_id${where})`;
/**
 * When the release ended the run row `x`, read from a released link no
 * later boot writer touched: one the receipt sweep (open-receipts) does
 * not settle. Null when the sweep settled every one, which restamps it.
 */
const runReleasedAt = (x: string) => releasedLinksStamp(x, " AND ml.state NOT IN ('accepted', 'running')");
/** No earlier than the release: a link the sweep settled afterward carries the sweep's later moment. */
const runReleasedBy = (x: string) => releasedLinksStamp(x, '');
const releasedRunTotals = tallyFigures('s.target_thread_id', runReleasedAt('s'));
const latestRunTotals = tallyFigures('s.target_thread_id', runReleasedBy('s'));
/**
 * The open receipt `x` the boot holds for its thread's rest again
 * (app.heldReceiptAtBoot), from the rows the migrations left: running, its
 * turn ended other than interrupted or errored with no later turn open,
 * interrupted or errored, and a spawn or a send from the thread that
 * spawned its target (app.receiptAwaitsRest).
 */
const heldForRest = (x: string) => `${x}.state = 'running' AND COALESCE(${x}.target_thread_id, '') <> ''
  AND EXISTS (SELECT 1 FROM mig.turns t WHERE t.thread_id = ${x}.target_thread_id
    AND ${x}.turn_id = ${x}.target_thread_id || '#' || t.turn_index
    AND t.completed_at IS NOT NULL AND t.stop_reason NOT IN ('interrupted', 'error') AND t.error_message = ''
    AND NOT EXISTS (SELECT 1 FROM mig.turns l WHERE l.thread_id = t.thread_id AND l.turn_index > t.turn_index
      AND (l.completed_at IS NULL OR l.stop_reason IN ('interrupted', 'error') OR l.error_message <> '')))
  AND (${x}.kind = 'spawn' OR (${x}.kind = 'send' AND ${x}.source_thread_id <> ''
    AND EXISTS (SELECT 1 FROM mig.thread_request_receipts sp WHERE sp.target_thread_id = ${x}.target_thread_id
      AND sp.kind = 'spawn' AND sp.owner_device_id = ${x}.owner_device_id AND sp.source_thread_id = ${x}.source_thread_id)))`;

/**
 * Whether `json` is a JSON array, and the array or an empty one, so
 * json_each never reads a corrupt value. CASE keeps the JSON functions
 * from running on one: SQLite may evaluate both sides of an AND.
 */
const isArray = (json: string) => `(CASE WHEN json_valid(${json}) THEN json_type(${json}) END) = 'array'`;
const jsonArray = (json: string) => `(CASE WHEN ${isArray(json)} THEN ${json} ELSE '[]' END)`;
/** A user row's meta, or an empty object when it is not JSON. */
const userMeta = (x: string) => `(CASE WHEN json_valid(${x}.meta) THEN ${x}.meta ELSE '{}' END)`;
/** The provider's id on the user row `x` (usermessage.ReadProviderItemID). */
const providerItemId = (x: string) => `COALESCE(json_extract(${userMeta(x)}, '$.provider_item_id'), '')`;
/** A reader-authored user row (store.readerAuthoredUserTextFilterFor). */
const readerUserText = (x: string) => `${x}.parent_id = '' AND ${x}.kind = 'user_text'
  AND COALESCE(json_extract(${userMeta(x)}, '$.wire_only'), 0) != 1`;
/**
 * The provider confirmed the message of the send `sendId` in thread `thread`
 * (app.queuedSendDelivered): the booted store's user row
 * FindUserTextItemBySendID finds, by its own send id first and a joined one
 * second, carries the provider's id.
 */
export const sendConfirmed = (thread: string, sendId: string) => `${sendId} <> '' AND COALESCE(
  (SELECT ${providerItemId('i')} <> '' FROM main.items i WHERE i.thread_id = ${thread} AND ${readerUserText('i')}
    AND json_extract(${userMeta('i')}, '$.sendId') = ${sendId} LIMIT 1),
  (SELECT ${providerItemId('i')} <> '' FROM main.items i WHERE i.thread_id = ${thread} AND ${readerUserText('i')}
    AND ${sendId} IN (SELECT value FROM json_each(${jsonArray(`json_extract(${userMeta('i')}, '$.joinedSendIds')`)})) LIMIT 1),
  0)`;
/** The send `sendId` is recorded in thread `thread`: a user row or a queue row carries it (app.findRecordedSend). */
export const sendRecorded = (thread: string, sendId: string) => `(EXISTS (SELECT 1 FROM mig.items u WHERE u.thread_id = ${thread}
    AND ${readerUserText('u')}
    AND (json_extract(${userMeta('u')}, '$.sendId') = ${sendId}
      OR ${sendId} IN (SELECT value FROM json_each(${jsonArray(`json_extract(${userMeta('u')}, '$.joinedSendIds')`)}))))
  OR EXISTS (SELECT 1 FROM mig.flush_queue_items q WHERE q.thread_id = ${thread} AND q.send_id = ${sendId}))`;
/**
 * The booted draft of the thread of `x` is exactly what the boot's composer
 * restores write from the migrated store's rows: restoredDraftThreads
 * (upgrade-corpus-fingerprint.ts) composes it and substitutes the threads
 * whose draft matches for `{restored}` when a rule is composed. The restores
 * write the draft and delete every row they took in one transaction, so a
 * row they take may leave only when its thread's draft matches.
 */
const draftRestored = (x: string) => `${x}.thread_id IN ({restored})`;
/** MergeParts keeps a pending plan the draft already had. */
const draftPlanKept = `(COALESCE(s.pending_plan_implementation, '') = '' OR b.pending_plan_implementation IS s.pending_plan_implementation)`;
const draftColumns = ['content', 'attachments', 'terminal_chips', 'pending_plan_implementation', 'updated_at', 'has_content'];
/** A review comment a claim release returned: a sent one is a draft again, any other keeps its status; the claim is cleared and the body kept. */
const claimReturned = `b.claim_thread_id = '' AND b.claim_item_id = '' AND b.claim_settles = ''
  AND CASE WHEN s.status = 'sent' THEN b.status = 'draft' AND b.sent_at = 0 AND b.sent_turn_id = ''
    ELSE b.status = s.status AND b.sent_at = s.sent_at AND b.sent_turn_id = s.sent_turn_id END`;
/** A credential row PruneCredentials deletes: expired a day before now. */
const prunedBefore = `(CAST(strftime('%s', 'now') AS INTEGER) * 1000 - 86400000)`;
const workflowRunLive = `EXISTS (SELECT 1 FROM mig.work_items w WHERE w.id = s.item_id AND w.state IN ('running', 'needs-human'))`;

export const recoveryRules: RecoveryRule[] = [
  {
    name: 'crashed-turns', writer: 'store.(*Store).RecoverCrashedTurns', table: 'turns',
    rows: 's.completed_at IS NULL', columns: ['completed_at', 'stop_reason'],
    to: "b.completed_at IS NOT NULL AND b.stop_reason = 'interrupted'",
  },
  {
    // The crashed turn's span, to its last activity, is added to its
    // thread's runtime as an ended turn's is (tallyTurnSpanTx).
    name: 'crashed-turn-runtime', writer: 'store.(*Store).RecoverCrashedTurns (tallyTurnSpanTx)', table: 'thread_tallies',
    rows: 'EXISTS (SELECT 1 FROM mig.turns t WHERE t.thread_id = s.thread_id AND t.completed_at IS NULL)',
    columns: ['turn_ms'], to: 'b.turn_ms >= s.turn_ms',
  },
  {
    name: 'crashed-turn-rows', writer: 'store.(*Store).RecoverCrashedTurns (flipCrashedTurnItemsTx)', table: 'items',
    rows: `s.status IN ('streaming', 'running') AND NOT (s.is_background = 1 AND s.kind = 'tool_call')
      AND EXISTS (SELECT 1 FROM mig.turns t WHERE t.thread_id = s.thread_id AND t.turn_index = s.turn_index AND t.completed_at IS NULL)`,
    columns: ['status', 'summary', 'updated_at', 'rev'], to: "b.status = 'errored'",
  },
  {
    name: 'codex-background-runtime', writer: 'store.(*Store).RecoverCodexBackgroundRuntime', table: 'items',
    rows: `s.kind = 'tool_call' AND s.status = 'running' AND s.is_background = 1 AND s.tool_name <> 'collab_agent'
      AND COALESCE(${meta('s', 'live_background_active')}, 1) != 0
      AND EXISTS (SELECT 1 FROM mig.threads t WHERE t.id = s.thread_id AND t.provider = 'codex')`,
    columns: ['status', 'summary', 'decision', 'updated_at', 'rev'], to: "b.status = 'errored' AND b.decision = 'lost'",
  },
  {
    name: 'claude-background-launches',
    writer: 'triage.(*Router).RecoverOrphanedBackgroundTasks (its session_died sibling fires trg_items_settle_bg_launch_on_completion)',
    table: 'items', rows: orphanedClaudeLaunch('s'), columns: ['rev'], metaKeys: ['live_background_active'],
    to: `${meta('b', 'live_background_active')} = 0`,
  },
  {
    name: 'claude-agent-rows', writer: 'store.(*Store).UpsertAgentEnd (settleAgentSubtreeTx), from RecoverOrphanedBackgroundTasks',
    table: 'items',
    rows: `s.status IN ('running', 'streaming') AND s.parent_id <> '' AND (s.thread_id, s.id) IN (
      WITH RECURSIVE owned(thread_id, id, depth) AS (
        SELECT x.thread_id, x.id, 0 FROM mig.items x WHERE ${orphanedClaudeLaunch('x')}
        UNION
        SELECT x.thread_id, ${meta('x', 'transcript_root_id')}, 0 FROM mig.items x
         WHERE ${orphanedClaudeLaunch('x')} AND ${meta('x', 'transcript_root_id')} <> ''
        UNION
        SELECT c.thread_id, c.id, o.depth + 1 FROM owned o JOIN mig.items c ON c.thread_id = o.thread_id AND c.parent_id = o.id
         WHERE o.depth < 64
      ) SELECT thread_id, id FROM owned)`,
    columns: ['status', 'summary', 'updated_at', 'rev'], to: "b.status IN ('errored', 'completed')",
  },
  {
    // The row is the unit: it is retired whole, and the person's text in it
    // goes to the draft. A joined row splits off the members the app wrote
    // by the parts its dispatch recorded (unconfirmedRowAtBoot); one whose
    // parts no longer describe its text goes to the draft whole.
    name: 'unconfirmed-queued-messages', writer: 'app.(*App).restoreThreadQueueAtBoot (store.RestoreFlushQueueToDraft)',
    table: 'items', removes: true,
    rows: `${unconfirmedUserRow('s')} AND ${draftRestored('s')}`,
  },
  {
    name: 'unconfirmed-message-search-rows', writer: 'store.RetireUndeliveredUserRow (deleteThreadItemTx clears its search rows)',
    table: 'thread_search_rows', removes: true,
    rows: `s.item_id <> '' AND EXISTS (SELECT 1 FROM mig.items i WHERE i.thread_id = s.thread_id AND i.id = s.item_id
      AND ${unconfirmedUserRow('i')})`,
  },
  {
    name: 'payloads-of-removed-items', writer: 'trg_items_gc_payload and trg_items_gc_input_payload (store/migrate_sql_items.go)',
    table: 'payloads', removes: true,
    rows: `EXISTS (SELECT 1 FROM mig.items i WHERE i.thread_id = s.thread_id AND (i.payload_id = s.id OR i.input_payload_id = s.id))
      AND NOT EXISTS (SELECT 1 FROM main.items i WHERE i.thread_id = s.thread_id AND (i.payload_id = s.id OR i.input_payload_id = s.id))`,
  },
  {
    name: 'chunks-of-removed-payloads', writer: 'payload_chunks ON DELETE CASCADE from payloads', table: 'payload_chunks', removes: true,
    rows: 'NOT EXISTS (SELECT 1 FROM main.payloads p WHERE p.thread_id = s.thread_id AND p.id = s.payload_id)',
  },
  {
    name: 'item-stamps', writer: 'trg_items_rev_* and trg_subagent_aggregates_stamp_* (store/history_rev_triggers.go)',
    table: 'items', rows: 'thread-write', columns: ['rev'],
  },
  {
    name: 'live-agent-cards', writer: 'store.(*Store).RecoverSubagentCards', table: 'subagent_aggregates',
    rows: `EXISTS (SELECT 1 FROM mig.items x WHERE x.thread_id = s.thread_id AND x.kind = 'tool_call' AND x.status = 'running'
      AND x.tool_name <> 'collab_agent' AND (x.is_background = 0 OR COALESCE(${meta('x', 'live_background_active')}, 1) != 0))`,
    columns: ['*'],
  },
  {
    name: 'written-chain-cards', writer: 'store cardWrite.finish (subagent_card_write.go), after a recovery write',
    table: 'subagent_aggregates', rows: 'thread-write', columns: ['*'],
  },
  {
    name: 'thread-stamps', writer: 'the items triggers (history_rev_triggers.go, thread_turn_error_aggregate.go)',
    table: 'threads', rows: 'thread-write',
    columns: ['history_rev', 'history_epoch', 'newest_turn_error_at', 'newest_turn_error_turn'],
  },
  {
    name: 'worktree-setups', writer: 'store.(*Store).SweepRunningThreadWorktreeSetups', table: 'threads',
    rows: "s.worktree_setup_state = 'running'", columns: ['worktree_setup_state'], to: "b.worktree_setup_state = 'failed'",
  },
  {
    // A delete claim no delete committed: nothing of the thread was stopped or removed.
    name: 'undecided-delete-claims', writer: 'store.(*Store).ReleaseUndecidedDeleteClaims (releaseUndecidedDeletes)', table: 'threads',
    rows: "s.delete_claim <> '' AND s.deleting = 0", columns: ['delete_claim'], to: "b.delete_claim = ''",
  },
  {
    // restoreThreadQueueAtBoot takes every queue row of the thread but the
    // owned ones: a copy of a joined row's member, a message the provider
    // confirmed and a message the app wrote (discardAgentWakeAtBoot) add no
    // text, the rest do; restoredDraftThreads composes which.
    name: 'queued-messages', writer: 'app.(*App).restoreThreadQueueAtBoot (store.RestoreFlushQueueToDraft)',
    table: 'flush_queue_items', removes: true, rows: `${draftRestored('s')} AND NOT ${ownedSend('s.send_id')}`,
  },
  {
    // An answer, a reminder, a completion or a spawner's answer goes back to
    // its owner with its queue row, each in a transaction of its own.
    name: 'owned-queued-messages',
    writer: 'app.(*App).giveBackToOwners (store.WithdrawThreadWake, WithdrawRemoteCompletion, ReturnAsyncAnswer), from restoreThreadQueueAtBoot',
    table: 'flush_queue_items', removes: true, rows: ownedSend('s.send_id'),
  },
  {
    name: 'queued-message-drafts', writer: 'app.(*App).restoreQueuedDraft (composerdraft.MergeParts)', table: 'thread_drafts',
    rows: `EXISTS (SELECT 1 FROM mig.flush_queue_items q WHERE q.thread_id = s.thread_id)
      OR EXISTS (SELECT 1 FROM mig.items u WHERE u.thread_id = s.thread_id AND ${unconfirmedUserRow('u')})`,
    columns: draftColumns, to: `${draftRestored('s')} AND ${draftPlanKept}`,
  },
  {
    // The wake's own request and the requests that rode with it: the
    // delivery mark cleared and the wake disarmed, so thread_status returns
    // the answer and nothing delivers it unasked.
    name: 'wakes-given-back',
    writer: 'app.(*App).giveBackToOwners (store.WithdrawThreadWake), from restoreThreadQueueAtBoot and giveBackUnconfirmedAtBoot',
    table: 'thread_requests', rows: wakeGivenBack('s'),
    columns: ['delivered_at', 'delivered_how', 'wake_item_id', 'delivered_with',
      'late_delivered_at', 'late_delivered_how', 'late_wake_item_id', 'notify', 'updated_at'],
    to: 'b.notify = 0',
  },
  {
    // The delivery the echo's row proves is confirmed by it, and stays delivered.
    name: 'echoed-wakes-confirmed',
    writer: 'app.(*App).confirmEchoedAtBoot (store.ConfirmThreadWake), from restoreThreadQueueAtBoot and giveBackUnconfirmedAtBoot',
    table: 'thread_requests', rows: wakeEchoConfirmed('s'),
    columns: ['confirmed_at', 'wake_item_id', 'late_confirmed_at', 'late_wake_item_id', 'updated_at'],
    to: '(b.delivered_at IS NULL OR b.confirmed_at IS NOT NULL) AND (b.late_delivered_at IS NULL OR b.late_confirmed_at IS NOT NULL)',
  },
  {
    // A confirmed wake stays delivered and confirmed; its echo's row is named.
    name: 'confirmed-wakes-name-their-row',
    writer: 'app.(*App).confirmEchoedAtBoot (store.ConfirmThreadWake), from restoreThreadQueueAtBoot',
    table: 'thread_requests', rows: wakeRowNamed('s'),
    columns: ['wake_item_id', 'late_wake_item_id', 'updated_at'],
    to: 'b.confirmed_at IS s.confirmed_at AND b.late_confirmed_at IS s.late_confirmed_at',
  },
  {
    // The watch is pending again, and the watcher delivers its completion. A
    // confirmed one never goes back, and one its echo's row proves is
    // confirmed instead (below).
    name: 'completions-given-back',
    writer: 'app.(*App).giveBackToOwners (store.WithdrawRemoteCompletion), from restoreThreadQueueAtBoot and giveBackUnconfirmedAtBoot',
    table: 'remote_watches',
    rows: `s.notification = 'queued' AND s.confirmed_at IS NULL AND NOT (${sendConfirmed('s.thread_id', completionSend('s'))})`,
    columns: ['notification'], to: "b.notification = 'pending'",
  },
  {
    name: 'echoed-completions-confirmed',
    writer: 'app.(*App).confirmEchoedAtBoot (store.ConfirmRemoteCompletion), from restoreThreadQueueAtBoot and giveBackUnconfirmedAtBoot',
    table: 'remote_watches',
    rows: `s.notification = 'queued' AND s.confirmed_at IS NULL AND ${sendConfirmed('s.thread_id', completionSend('s'))}`,
    columns: ['confirmed_at'], to: 'b.confirmed_at IS NOT NULL',
  },
  {
    // The worker's questions are unanswered again, routed to their spawner anew.
    name: 'worker-answers-returned', writer: 'app.(*App).returnWorkerAnswer (store.ReturnAsyncAnswer), from restoreThreadQueueAtBoot',
    table: 'async_questions',
    rows: `s.send_id GLOB 'agent-answer:*' AND s.state IN ('submitted', 'restored')
      AND EXISTS (SELECT 1 FROM mig.flush_queue_items q WHERE q.thread_id = s.thread_id AND q.send_id = s.send_id)`,
    columns: ['state', 'answer', 'send_id', 'user_item_id', 'revision'], to: "b.state = 'unanswered'",
  },
  {
    // A loop's or watch's wake still queued, or carried by an unconfirmed
    // message, is recorded not delivered; an interval row is due again by
    // the fire it was for.
    name: 'queued-agent-wakes', writer: 'app.(*App).discardAgentWakeAtBoot (store.ReleaseQueuedAgentWake)',
    table: 'agent_wakes',
    rows: `s.state <> 'cancelled' AND (EXISTS (SELECT 1 FROM mig.flush_queue_items q
        WHERE substr(q.send_id, 1, length('agent-wake:' || s.id || ':')) = 'agent-wake:' || s.id || ':')
      OR EXISTS (SELECT 1 FROM mig.items u WHERE ${unconfirmedUserRow('u')} AND instr(u.meta, '"agent-wake:' || s.id || ':') > 0))`,
    columns: ['last_outcome', 'undelivered', 'last_output', 'next_at'],
  },
  {
    name: 'retired-message-claims', writer: 'store.(*Store).RetireUndeliveredUserRow', table: 'diff_review_comments',
    rows: `s.claim_item_id <> '' AND EXISTS (SELECT 1 FROM mig.items u WHERE u.thread_id = s.claim_thread_id AND u.id = s.claim_item_id
      AND ${unconfirmedUserRow('u')})`,
    columns: ['status', 'sent_at', 'sent_turn_id', 'claim_thread_id', 'claim_item_id', 'claim_settles'], to: claimReturned,
  },
  {
    name: 'unsettled-review-claims', writer: 'store.(*Store).ReleaseUnsettledDiffReviewClaims', table: 'diff_review_comments',
    rows: "s.claim_item_id <> '' AND s.claim_settles <> ''",
    columns: ['status', 'sent_at', 'sent_turn_id', 'claim_thread_id', 'claim_item_id', 'claim_settles'], to: claimReturned,
  },
  {
    // A recovery whose send is recorded is dropped (its message is in the
    // history or the queue); any other goes to the draft (recoverReplacementDraft).
    name: 'replacement-draft-recoveries', writer: 'app.(*App).restoreReplacementDraftsAtBoot', table: 'thread_draft_recoveries',
    removes: true, rows: draftRestored('s'),
  },
  {
    name: 'replacement-drafts', writer: 'app.(*App).recoverReplacementDraft (composerdraft.MergeParts)', table: 'thread_drafts',
    rows: 'EXISTS (SELECT 1 FROM mig.thread_draft_recoveries r WHERE r.thread_id = s.thread_id)', columns: draftColumns,
    to: `${draftRestored('s')} AND ${draftPlanKept}`,
  },
  {
    // The sweep settles every open receipt but one held for its thread's
    // rest, which it leaves running; that one settles at its thread's rest
    // once the boot recovered what the thread waits on
    // (app.settleHeldReceiptsAtRest), so either is valid.
    name: 'open-receipts', writer: 'app.(*App).sweepThreadRequestsAtBoot', table: 'thread_request_receipts',
    rows: "s.state IN ('accepted', 'running')", columns: ['*'],
    to: `b.state NOT IN ('accepted', 'running') OR (b.state IS s.state AND ${heldForRest('s')})`,
  },
  {
    name: 'open-local-requests', writer: 'app.(*App).sweepThreadRequestsAtBoot', table: 'thread_requests',
    rows: `(s.state IN ('unconfirmed', 'accepted', 'running') AND s.kind <> 'remind' AND s.target_computer_id = '')
      OR EXISTS (SELECT 1 FROM mig.thread_request_receipts r WHERE r.token = s.token AND r.state IN ('accepted', 'running'))`,
    columns: ['*'],
  },
  {
    name: 'background-terminal-stashes', writer: 'triage.(*Router).RecoverOrphanedBackgroundTasks', table: 'pending_background_task_terminals',
    rows: '1', removes: true,
  },
  {
    name: 'running-remote-jobs', writer: 'store.(*Store).RecoverRemoteJobs', table: 'remote_jobs',
    rows: "s.state = 'running'", columns: ['state', 'finished_at', 'error'], to: "b.state = 'interrupted'",
  },
  {
    name: 'workflow-runs', writer: 'workflow/engine.(*Engine).rebuild', table: 'work_items',
    rows: "s.state IN ('running', 'needs-human')", columns: ['*'],
  },
  {
    name: 'workflow-run-phases', writer: 'workflow/engine.(*Engine).rebuild', table: 'work_item_phases',
    rows: workflowRunLive, columns: ['*'], key: ['item_id', 'phase_id', 'attempt'],
  },
  {
    name: 'workflow-run-units', writer: 'workflow/engine.(*Engine).rebuild', table: 'work_item_units',
    rows: workflowRunLive, columns: ['*'], key: ['item_id', 'phase_id', 'attempt', 'unit_id'],
  },
  {
    // Reclaim takes the claim; recoverWorkflowUsageAttention then moves it to
    // a run still affected and delivers it, or releases it when none is.
    name: 'usage-attention-claims',
    writer: 'store.(*Store).ReclaimQueuedWorkflowProviderUsageAttention, then workflowapp recoverWorkflowUsageAttention (Reassign, Promote or Release)',
    table: 'workflow_provider_usage_attention', rows: 's.queued_generation > 0',
    columns: ['queued_generation', 'queued_token', 'source_item_id', 'delivered_generation', 'updated_at'],
    to: `b.queued_generation = 0 OR b.queued_token LIKE '%:' || b.scope_id || ':' || b.thread_id`,
    key: ['scope_id', 'thread_id'],
  },
  {
    name: 'local-session-scopes', writer: 'store.(*Store).EnsureLocalSession', table: 'sessions',
    rows: `s.binding_class = 'loopback-only' AND s.revoked_at IS NULL
      AND EXISTS (SELECT 1 FROM mig.devices d WHERE d.id = s.device_id AND d.channel = 'local' AND d.revoked_at IS NULL)`,
    columns: ['scopes'],
  },
  {
    name: 'own-admission-members', writer: 'identity.(*Sessions).RecoverOwnAdmissions (store.AdmitOwnPairing)', table: 'own_devices',
    rows: `EXISTS (SELECT 1 FROM mig.pairing_links l WHERE ${incompleteOwnAdmission('l')}
      AND l.purpose <> 'own-introduction' AND l.key_thumbprint = s.key_thumbprint)`,
    columns: ['member', 'generation', 'removed'], to: 'b.removed = 0',
  },
  {
    name: 'own-admission-revocations', writer: 'identity.(*Sessions).RecoverOwnAdmissions (RevokeSession when AdmitOwnPairing refuses)',
    table: 'sessions', rows: `s.id IN (SELECT l.session_id FROM mig.pairing_links l WHERE ${incompleteOwnAdmission('l')})`,
    columns: ['revoked_at'], to: 'b.revoked_at IS NOT NULL',
  },
  {
    name: 'pending-link-sessions', writer: 'identity.Bootstrap (store.RetirePendingPairings)', table: 'sessions',
    rows: `s.activated_at IS NULL AND s.revoked_at IS NULL
      AND s.id IN (SELECT l.session_id FROM mig.pairing_links l WHERE l.session_id <> '')`,
    columns: ['revoked_at'], to: 'b.revoked_at IS NOT NULL',
  },
  // A device the session ends above leave with no unrevoked session is
  // unreachable: the end (store.endSessionsTx) releases its spawn links and
  // forgets its spawn changes in the same transaction.
  {
    // A released link is stamped and stops tracking.
    name: 'ended-device-spawn-links', writer: 'store.endSessionsTx (releaseSpawnLinksTx), for the session ends above',
    table: 'thread_request_receipts',
    rows: `${deviceLostItsLastSession('s.owner_device_id')} AND s.spawn_link = 1`,
    columns: ['spawn_link', 'track_spawn', 'updated_at'], to: 'b.spawn_link = 0 AND b.track_spawn = 0',
  },
  {
    // Any other receipt of the device only stops tracking, unstamped.
    name: 'ended-device-spawn-tracking', writer: 'store.endSessionsTx, for the session ends above',
    table: 'thread_request_receipts',
    rows: `${deviceLostItsLastSession('s.owner_device_id')} AND s.spawn_link = 0 AND s.track_spawn = 1`,
    columns: ['track_spawn'], to: 'b.track_spawn = 0',
  },
  {
    // The release ends the open run of each released link's source thread
    // on its thread, with the thread's figures then (tallyFiguresSQL). The
    // tool calls and tokens are exact. The runtime is exact when a released
    // link still carries the release's moment. When the receipt sweep
    // restamped every released link afterward, the runtime is bounded, not
    // verified: nothing stored carries the release instant, so the open
    // turn's span at the release is unknown, and no pre-boot stamp bounds it
    // from below once the host clock may have been set back. It is checked
    // only to lie between the thread's runtime without its open turn and that
    // runtime with the open turn's span to the latest link stamp.
    name: 'ended-device-spawn-runs', writer: 'store.endSessionsTx (releaseSpawnLinksTx), for the session ends above',
    table: 'thread_request_receipts',
    rows: `s.run_open = 1 AND EXISTS (SELECT 1 FROM mig.thread_request_receipts r WHERE r.spawn_link = 1
      AND r.target_thread_id = s.target_thread_id AND r.owner_device_id = s.owner_device_id
      AND r.source_thread_id = s.source_thread_id AND ${deviceLostItsLastSession('r.owner_device_id')})`,
    columns: ['run_open', 'run_tool_calls', 'run_tokens', 'run_turn_ms'],
    to: `b.run_open = 0 AND b.run_tool_calls = ${releasedRunTotals.toolCalls} AND b.run_tokens = ${releasedRunTotals.tokens}
      AND CASE WHEN ${runReleasedAt('s')} IS NOT NULL THEN b.run_turn_ms = ${releasedRunTotals.turnMs}
        ELSE b.run_turn_ms BETWEEN ${latestRunTotals.settledTurnMs} AND ${latestRunTotals.turnMs} END`,
  },
  {
    name: 'ended-device-nudges', writer: 'store.endSessionsTx, for the session ends above', table: 'thread_receipt_nudges',
    rows: `EXISTS (SELECT 1 FROM mig.thread_request_receipts r WHERE r.token = s.link_token AND ${deviceLostItsLastSession('r.owner_device_id')})`,
    removes: true,
  },
  {
    name: 'ended-device-spawn-revisions', writer: 'store.endSessionsTx, for the session ends above', table: 'spawn_revisions',
    rows: deviceLostItsLastSession('s.owner_device_id'), removes: true,
  },
  {
    // The released link's thread goes standalone (trg_thread_placement_link_gone).
    name: 'ended-device-placements', writer: 'store.endSessionsTx (releaseSpawnLinksTx), for the session ends above', table: 'threads',
    rows: `s.placement = 'owned' AND EXISTS (SELECT 1 FROM mig.thread_request_receipts r
      WHERE r.target_thread_id = s.id AND r.spawn_link = 1 AND ${deviceLostItsLastSession('r.owner_device_id')})`,
    columns: ['placement'], to: "b.placement = 'standalone'",
  },
  {
    name: 'expired-sessions', writer: 'identity.(*Sessions).PruneCredentials (store.DeleteSessionsExpiredBefore)', table: 'sessions',
    rows: `s.expires_at < ${prunedBefore}
      AND NOT EXISTS (SELECT 1 FROM mig.refresh_secrets r WHERE r.session_id = s.id AND r.expires_at >= ${prunedBefore})`,
    removes: true,
  },
  {
    name: 'expired-refresh-secrets', writer: 'identity.(*Sessions).PruneCredentials (store.DeleteRefreshSecretsExpiredBefore)',
    table: 'refresh_secrets',
    rows: `s.expires_at < ${prunedBefore}
      AND NOT EXISTS (SELECT 1 FROM mig.refresh_secrets n WHERE n.secret_hash = s.next_secret_hash AND n.consumed_at IS NULL)`,
    removes: true,
  },
  {
    name: 'expired-pairing-links', writer: 'identity.(*Sessions).PruneCredentials (store.DeletePairingLinksExpiredBefore)',
    table: 'pairing_links', rows: `s.expires_at < ${prunedBefore} AND s.redeemed_at IS NULL`, removes: true,
  },
];

/**
 * A SweepRule declares threads the first ordinary boot after the trial
 * deletes, wholly or down to what a fork still reads: the steps a trial
 * defers to its commit (startBeforeClients) and the unattended work behind
 * the activation gate. The check reads the threads each rule selects from
 * the store the trial left, before any later boot, and the census and the
 * reads then allow exactly the rows and listings those threads lost; every
 * other thread must keep all of its rows. Each entry names the Go writer and
 * mirrors its selection, as an SQL query of thread ids over that store.
 */
export interface SweepRule {
  name: string;
  writer: string;
  threads: string;
}

export const sweepRules: SweepRule[] = [
  {
    name: 'terminal-threads', writer: 'app.(*App).endTerminalThreadsAtBoot (startBeforeClients)',
    threads: "SELECT id FROM owned_threads WHERE mode = 'terminal'",
  },
  {
    name: 'interrupted-forks', writer: 'app.(*App).cleanupPreparingForks (startBeforeClients)',
    threads: 'SELECT id FROM threads WHERE fork_preparing = 1',
  },
  {
    name: 'scratch-threads', writer: 'app.(*App).finishThreadRequestSweepAtBoot (startUnattendedWork)',
    threads: 'SELECT thread_id FROM scratch_threads',
  },
  {
    name: 'pending-thread-deletes', writer: 'app.(*App).startPendingThreadDeletes (completeThreadDeletes, after the first reads)',
    threads: 'SELECT id FROM threads WHERE deleting = 1',
  },
  {
    // {swept} is every thread the rules above select: a holder none of the
    // rest reads is released when they go.
    name: 'released-holders', writer: 'app.(*App).collectReleasedHolders (store.ListReleasedHolders)',
    threads: `SELECT h.id FROM threads h WHERE h.mode = 'holder'
      AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage l WHERE l.ancestor_id = h.id AND l.thread_id NOT IN ({swept}))`,
  },
];

/**
 * What the candidate answers about the root it boots on, which a
 * RowSweepRule's condition reads: ListForgeProjects, the projects whose
 * origin names a forge repository (with Error when git could not read it).
 */
export interface SweepContext {
  forgeProjects: Array<{ projectId: string; repo: unknown; error?: string }>;
}

/**
 * A RowSweepRule declares rows outside any thread that an ordinary boot
 * after the trial deletes because nothing on the root names them any more.
 * A cloned root makes these real: `ao-harness clone` relocates every project
 * to a throwaway repository with no forge remote. The check reads `read`
 * for every row of `table` from the store the trial left, and allows the
 * census to lose a row only when it is gone afterwards and `prunable` holds
 * for it, given what the candidate answered. `prunable` mirrors the Go
 * writer's condition.
 */
export interface RowSweepRule {
  name: string;
  writer: string;
  table: string;
  /** SQL select list over the table: `key` (the row's identity) and what `prunable` reads. */
  read: string;
  prunable: (row: Record<string, unknown>, live: SweepContext) => boolean;
}

/** Whether two git.RepoReference values on the wire name one repository. */
const sameRepo = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

export const rowSweepRules: RowSweepRule[] = [
  {
    // prefetchPRList and the list pump keep the repositories current
    // projects' origins name, and an errored origin's stored repository.
    name: 'unprojected-pr-lists', writer: 'app.(*App).prunePRListStore (store.DeletePRListProjectsExcept)',
    table: 'pr_list_projects', read: 'repo_key AS key, CAST(project_json AS TEXT) AS list',
    prunable: (row, live) => {
      const list = JSON.parse(String(row.list)) as { projectId?: string; repo?: unknown };
      return !live.forgeProjects.some((project) => project.error
        ? project.projectId === list.projectId
        : sameRepo(project.repo, list.repo));
    },
  },
];

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
  { name: 'agy-basic' },
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

  // Identity: browser sign-ins, passkeys and devices other than the one
  // paired computer the top-up pairs with.
  { table: 'auth_audit', columns: ['reason'], values: { outcome: ['refused'] }, reason: "a refused sign-in; the harness page signs in locally and the top-up's paired computer is admitted" },
  { table: 'devices', columns: ['passkey_credential_id', 'last_seen_at', 'revoked_at'], values: { class: ['browser', 'phone', 'cli', 'backend-peer'] }, reason: "rows the pairing flow writes for a browser, phone, CLI or a peer backend another computer introduced, and a revoked or passkey device; the top-up pairs one computer of the person's own" },
  { table: 'own_devices', values: { removed: [1] }, reason: "a paired computer of the person's own they removed; the top-up keeps its pairing" },
  {
    table: 'pairing_links', columns: ['canceled_at', 'expected_key', 'sponsor_key', 'bootstrap'],
    values: { binding_class: ['loopback-only', 'public'], device_class: ['browser', 'phone', 'cli', 'backend-peer'], purpose: ['', 'own-introduction'], bootstrap: [1] },
    reason: "a link minted for a browser, phone, CLI or peer backend, a canceled link, an introduction one computer sponsors for another and a computer pairing window's invitation (v164); the top-up redeems one link between two computers of the person's own",
  },
  { table: 'passkeys', reason: 'a WebAuthn registration from a browser' },
  { table: 'push_sender', reason: 'the push credential a paired phone registers' },
  { table: 'push_tokens', reason: 'push tokens paired phones register' },
  { table: 'recovery_codes', columns: ['consumed_at', 'consumed_by'], reason: 'a recovery code a sign-in consumed' },
  { table: 'sessions', columns: ['revoked_at', 'last_seen_at'], values: { binding_class: ['public'] }, reason: 'sessions a public link opens, and a revoked or seen session' },
  { table: 'users', columns: ['disabled_at', 'webauthn_user_handle'], values: { role: ['member'] }, reason: 'a second (member) user, a disabled user or a passkey holder' },

  // Connected computers and the agent thread tools, which need a second
  // backend or an agent calling the tools.
  { table: 'remote_jobs', reason: 'a job on a connected computer' },
  { table: 'remote_watches', reason: 'a watch on a connected computer' },
  { table: 'thread_transfers', reason: 'a conversation moved or copied to a connected computer' },
  { table: 'thread_transfer_sessions', reason: 'a transfer session with a connected computer' },
  { table: 'thread_requests', columns: ['target_computer_id', 'target_thread_title', 'next_check', 'attempts', 'issue', 'polling', 'admit_until'], values: { polling: [0] }, reason: 'the poll of a request to a thread on a connected computer, and its admission deadline there (v158)' },
  {
    table: 'thread_requests',
    columns: [
      'origin_thread_id', 'late_reply', 'late_reply_at', 'late_delivered_at', 'wake_attempts', 'wake_next_check', 'wake_issue',
      'lineage_only', 'superseded', 'late_confirmed_at', 'answer_agent', 'answer_agent_role', 'nested_open', 'target_provider',
      'late_delivered_how', 'made_in_thread_id', 'late_wake_item_id', 'open_below_0', 'open_below_1', 'open_below_2',
      'open_below_3', 'late_reply_agent', 'late_reply_agent_role', 'rest_unreported',
    ],
    values: {
      kind: ['ask'],
      state: ['unconfirmed', 'accepted', 'running', 'errored', 'cancelled', 'expired', 'refused'],
      delivered_how: ['draft'],
      late_delivered_how: ['inline', 'queued', 'draft'],
      lineage_only: [1],
      superseded: [1],
      answer_agent: [1],
      late_reply_agent: [1],
      nested_open: [1],
    },
    reason: "the top-up's requests are thread_send and thread_remind requests its caller takes as each settles, a send its caller's thread_cancel interrupts, and the assistant's helper spawn; an ask, a late reply, a failed request, a wake whose caller cannot take it (no request reads draft since a wake never enters the composer), an answer from a standing agent, a handoff's moved request, a remote spawn and a spawn the retention floor kept as lineage are not staged, and open states are ones a request passes through",
  },
  {
    table: 'thread_request_receipts',
    columns: ['late_reply', 'late_reply_at', 'answer_agent', 'answer_agent_role', 'late_reply_agent', 'late_reply_agent_role', 'lineage_only'],
    values: {
      kind: ['ask', 'remind'],
      state: ['accepted', 'errored', 'cancelled', 'expired'],
      answer_kind: ['note'],
      answer_agent: [1],
      late_reply_agent: [1],
      lineage_only: [1],
    },
    reason: "a target receipts sends and spawns only (a reminder has no target); the top-up stages no ask, late reply, failed request or answer from a standing agent, open states are ones a request passes through, and a spawn's receipt the retention floor keeps as its lineage is a month old",
  },
  {
    table: 'thread_request_receipts',
    columns: ['owner_op_seq'],
    reason: "the owner operations a paired computer's spawn admitted (v158): the top-up's paired spawns are never archived, deleted or released",
  },
  {
    table: 'thread_requests',
    columns: ['owner_op_seq', 'ack_owed', 'late_reply_expired', 'target_computer_name'],
    values: { late_reply_expired: [1] },
    reason: "the owner operations owed for a spawn on a paired computer, an acknowledgement owed to one, a late reply one dropped before it was collected, and the computer name a request there was admitted under (v158), which need a second computer",
  },
  {
    table: 'thread_requests', columns: ['link_end', 'contact_due'], values: { link_end: ['released', 'detached', 'gone'] },
    reason: 'a paired worker reports its link end and its owner schedules contact (TestOwnerRecoversReleaseAndDetachmentOfASettledWorker, TestOwnerRetriesAFailedReadOfAnOtherwiseRestingLink, TestDueOwnerContactUsesTheOriginalSpawnerAndStopsAfterRelease)',
  },
  { table: 'spawn_revisions', reason: 'unacknowledged changes to a paired computer’s spawns require a second computer' },
  { table: 'thread_owner_ops', reason: 'an archive, delete or release owed to a paired computer for a spawn there (v158), which needs a second computer' },
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
  { table: 'threads', values: { mode: ['terminal', 'workflow-studio', 'workflow-triage', 'holder', 'scratch'] }, reason: 'threads their owning saga creates (a terminal, the workflow studio and triage, a fork holder, an unpromoted side chat)' },
  { table: 'proposed_plans', reason: 'a plan a Claude turn proposes through ExitPlanMode, which no library scenario emits' },
  { table: 'proposed_plan_comments', reason: 'review comments on a proposed plan' },
  { table: 'thread_drafts', columns: ['pending_plan_implementation'], values: { has_content: [0] }, reason: 'a draft seeded from an accepted plan, and an empty draft that only holds attachments' },
  { table: 'diff_review_comments', columns: ['claim_settles'], values: { claim_settles: ['row', 'confirm'] }, reason: "a sent draft's claim waits for its message's row or the provider's confirmation, and settles before the turn rests" },
  { table: 'thread_pull_requests', columns: ['merged_at', 'handled_at', 'removal_failed_at', 'outcome'], values: { forge: ['gitlab'], outcome: ['removed', 'kept', 'gone', 'closed', 'skipped'] }, reason: "the top-up's pull request stays open, so no merge cleanup runs; one forge's pull request proves the table" },
  { table: 'pr_list_projects', reason: "the Pull requests list's stored copy of a forge project's read; the top-up's one forge-origin project names a repository the fake forge does not have, so its read fails and nothing is stored" },
  { table: 'thread_tickets', reason: 'the ticket a thread is linked to (v162, its last-seen status v167); no step of the top-up links a thread' },
  // The top-up reads the fake Jira's scrum board into the mirror through a
  // board subscription (connectTrackers); what that read leaves empty:
  { table: 'tracker_cache_roots', columns: ['sweep'], reason: 'a mirror root\'s sweep counter (v162), which a cleanup sweep that deletes an issue or a board moves; nothing the top-up reads leaves the mirror' },
  { table: 'tracker_cache_users', reason: 'a user a board\'s catalog names (v162); the fake board\'s issues name no assignee, so its read stores none' },
  {
    table: 'tracker_cache_boards',
    columns: ['estimate_unit', 'estimate_field', 'estimate_reset_at', 'retired'],
    reason: 'a board\'s estimate field and its change (v162), which the fake board does not set (it estimates by count), and a board no project lists any more; the top-up keeps both of its boards listed',
  },
  { table: 'tracker_cache_board_unexplained', reason: 'a reference a board\'s complete structure did not explain (v162); the fake board\'s structure explains all of its own' },
  {
    table: 'tracker_cache_issues',
    columns: ['type_level', 'assignee_id', 'priority_id', 'parent_id', 'resolved_at', 'history_from', 'history_wait', 'history_gen', 'history_dirty', 'body_updated_at', 'body_read_at', 'body_wall', 'body_opened_at', 'body_digest'],
    reason: 'the fields the fake board\'s issues leave empty (type level, assignee, priority, parent, resolution), an issue\'s change history, which the board read stores none of, and its body, read for a ticket a person opens; the top-up opens no ticket',
  },
  { table: 'tracker_cache_board_issues', columns: ['estimate'], reason: 'a card\'s estimate (v162); the fake board estimates by count' },
  { table: 'tracker_cache_sprints', columns: ['goal', 'completed_at'], values: { state: [1, 3] }, reason: 'a sprint\'s goal, and a future or closed sprint with its completion (v162); the fake board has one active sprint with no goal' },
  { table: 'tracker_cache_changes', reason: 'an issue\'s status, sprint, estimate or assignee change (v162); the fake board\'s read stores no change history' },
  { table: 'tracker_cache_issue_bodies', reason: 'the ticket body of an open or recently opened tracker issue (v165); the top-up opens no ticket' },
  { table: 'tracker_cache_worklogs', reason: 'time logged in a tracker (v162); the top-up reads no worklogs' },
  { table: 'tracker_cache_worklog_days', reason: 'the latest read of a day of a tracker author\'s worklogs (v162); the top-up reads no worklogs' },
  {
    table: 'tracker_cache_sync',
    columns: ['failed_at', 'failure_kind', 'failure_message', 'outcome_at', 'failed_wall', 'reconcile_failed_at', 'reconcile_failed_wall', 'reconcile_failure_kind', 'reconcile_failure_message', 'reconcile_refused'],
    reason: 'a sync scope\'s failure and its reconcile\'s failure (v162); every read the top-up makes of the fake trackers succeeds',
  },
  { table: 'tracker_cache_open_tickets', reason: 'a tracker ticket a person has open, which keeps its issue in the mirror (v165); the top-up opens no ticket' },
  { table: 'tracker_cache_issue_missing', reason: 'a read that did not return a linked issue (v167); no step of the top-up links a thread' },
  { table: 'chat_model_profiles', values: { reasoning_effort: ['none', 'minimal', 'low', 'xhigh'], runtime_mode: ['read-only', 'approval-required', 'auto-accept-edits'] }, reason: 'one remembered profile per provider and model holds the settings used last' },

  // Provider accounting the mock does not report.
  { table: 'usage_ledger', reason: 'token usage the provider reports; the mock reports none' },
  { table: 'usage_pending', reason: 'token usage the provider reports; the mock reports none' },
  { table: 'thread_tallies', columns: ['tokens'], reason: "the tokens a thread spent itself, counted from the own usage the provider reports with a turn's settlement; the mock reports none" },
  { table: 'provider_thread_cost', reason: 'cost the provider reports; the mock reports none' },
  { table: 'turns', columns: ['token_usage_json'], reason: 'token usage the provider reports; the mock reports none' },
  { table: 'threads', columns: ['live_todo'], reason: 'a TodoWrite list, which no library scenario emits' },
  { table: 'workflow_provider_usage_scopes', reason: 'a workflow run parked on a provider usage limit' },
  { table: 'workflow_provider_usage_attention', reason: 'a workflow run parked on a provider usage limit' },

  // Transient states no run is left in, and item shapes no library
  // scenario emits.
  { table: 'items', values: { kind: ['compaction_reasoning', 'workflow_proposal', 'command_result', 'terminal_interaction', 'api_retry'], status: ['streaming', 'killed', 'parked'], decision: ['approved', 'amended', 'lost'] }, reason: 'compaction reasoning, API retry, terminal and workflow-proposal rows need scenarios no library entry provides; the approval scenarios raise their request without a toolUseId, so an allowed decision is not matched to the tool row that follows, and an amended or lost one needs an edited or abandoned request; streaming is a mid-turn state, which every top-up step ends before it returns (assertAtRest); killed is the end written for a background launch or agent still running when its session ends, live or found by the boot sweep after the previous process died, and parked the paused stop of a Claude background agent; no top-up session ends with background work running, and no library scenario parks an agent' },
  { table: 'threads', columns: ['history_bulk_load', 'fork_preparing', 'fork_cut_turn_index', 'deleting', 'delete_claim', 'created_remote_url', 'worktree_setup_state'], reason: 'fork-in-progress, delete-in-progress and worktree-setup-in-progress markers, which rest at their defaults; the remote a thread was created from needs a forge remote' },
  { table: 'threads', values: { reasoning_effort: ['none', 'minimal'] }, reason: 'efforts no catalog model declares' },
  { table: 'threads', values: { attention_kind: ['decision', 'problem'] }, reason: "the other kinds of an agent's thread_attention flag; the top-up's one flag is a review" },
  { table: 'subagent_aggregates', columns: ['state', 'transcript_count', 'transcript_newest_turn', 'transcript_newest_item'], reason: 'a subagent still running or failed when its thread rested, and transcript cursors of Codex children' },
  { table: 'work_items', columns: ['wake_signature', 'pending_guidance', 'auto_resume_at', 'budget', 'triage_thread_id', 'origin_thread_id', 'disposition', 'parent_item_id', 'parent_phase_id', 'parent_unit_id', 'parent_attempt', 'call_depth', 'soft_stop', 'source_ref', 'reports_owed'], values: { state: ['running', 'needs-human', 'failed'], workflow_scope: ['shared'], source: ['agent', 'call'], reason: ['gate', 'question', 'stuck', 'stalled', 'budget-exhausted', 'retries-exhausted', 'provider-retries-exhausted', 'provider-usage-limited', 'loop-limit-exhausted', 'check-failed-genuine', 'agent-error', 'wiring-error', 'disposition', 'setup-failed', 'taken-over', 'unit-failed', 'child-failed', 'paused', 'checkpoint'] }, reason: 'running and needs-human are states a run passes through; the rest are failures, budgets, child runs, shared-scope and agent-started runs the top-up does not stage; source_ref is set only on a run a phase started or a call phase invoked; reports_owed counts rests whose wake jobs have not ended, none once a run settles' },
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
