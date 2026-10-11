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
  {
    version: 141,
    table: 'projects',
    columns: ['remote_url', 'root_commit', 'identity_error'],
    reason: 'v141 moves repository identity to repository_id: the legacy remote and root commit are cleared and an unverified identity is marked',
  },
  {
    version: 141,
    table: 'threads',
    columns: ['created_remote_url'],
    reason: 'v141 clears the remote a thread was created from along with the project columns it mirrored',
  },
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
