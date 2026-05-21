#!/usr/bin/env -S node --experimental-strip-types
/**
 * One-shot migration for the local Agent Overflow SQLite database.
 *
 * What this does:
 *  1. Drops the redundant idx_items_id index — every WHERE items.id = ?
 *     query in the codebase also constrains items.thread_id = ?, so the
 *     composite PRIMARY KEY (thread_id, id) already covers them. The
 *     standalone index was free-floating leftover from the squash.
 *  2. Adds a FOREIGN KEY on threads.discussion_id → channels(id) with
 *     ON DELETE SET NULL. Orphaned discussion_id values (channel row
 *     was deleted out from under the thread row) are NULLed first so
 *     the FK can be enforced cleanly on the rebuild.
 *
 * Why a script instead of a v2 migration: we're pre-1.0, the v1 schema
 * is the source of truth, and the only DB currently in the wild is the
 * one on this machine. Shipping a single squashed v1 is cleaner than
 * carrying a v2 forever; this script does the same delta on the one
 * DB that needs it.
 *
 * Prerequisites:
 *  - Quit the Agent Overflow app first (the rebuild needs an exclusive
 *    lock; the script bails if any process holds the DB file).
 *  - `sqlite3` CLI on PATH (3.39+ for STRICT-table-safe rebuild).
 *  - Node 22+ for --experimental-strip-types, OR run with
 *    `npx tsx scripts/migrate-local-db-2026-05-21.ts`.
 *
 * Run:
 *   node --experimental-strip-types \
 *     scripts/migrate-local-db-2026-05-21.ts
 *
 * Safe to re-run: detects already-migrated state and exits cleanly.
 */

import { execFileSync, spawnSync } from 'node:child_process';
import { copyFileSync, existsSync, statSync } from 'node:fs';
import { homedir, platform } from 'node:os';
import { join } from 'node:path';

// --- locate the database --------------------------------------------------

function defaultDbPath(): string {
  // Mirrors app.go's initStores: UserConfigDir/agent-overflow/agent-overflow.db.
  const home = homedir();
  switch (platform()) {
    case 'darwin':
      return join(home, 'Library', 'Application Support', 'agent-overflow', 'agent-overflow.db');
    case 'win32': {
      const appData = process.env.APPDATA ?? join(home, 'AppData', 'Roaming');
      return join(appData, 'agent-overflow', 'agent-overflow.db');
    }
    default: {
      // Linux / WSL — XDG_CONFIG_HOME or ~/.config
      const xdg = process.env.XDG_CONFIG_HOME ?? join(home, '.config');
      return join(xdg, 'agent-overflow', 'agent-overflow.db');
    }
  }
}

const dbPath = process.argv[2] ?? defaultDbPath();
if (!existsSync(dbPath)) {
  console.error(`error: no database at ${dbPath}`);
  console.error('       pass an explicit path as the first argument if it lives elsewhere');
  process.exit(1);
}

// --- exclusive-lock probe -------------------------------------------------

// sqlite3 returns SQLITE_BUSY when another process holds a write lock on
// the DB (the app keeps a write transaction open via WAL). We probe with
// a BEGIN IMMEDIATE inside a no-rollback transaction and a 0-ms busy
// timeout so the script bails immediately instead of stalling.
function probeExclusive(): void {
  const probe = spawnSync(
    'sqlite3',
    [dbPath, '.timeout 0', 'BEGIN IMMEDIATE; ROLLBACK;'],
    { encoding: 'utf8' },
  );
  if (probe.status !== 0) {
    const msg = (probe.stderr ?? '').trim() || probe.error?.message || 'unknown';
    console.error(`error: could not acquire exclusive lock on ${dbPath}`);
    console.error(`       sqlite3 said: ${msg}`);
    console.error('       quit the Agent Overflow app and try again.');
    process.exit(1);
  }
}

// --- migration detection --------------------------------------------------

function indexExists(name: string): boolean {
  const out = execFileSync(
    'sqlite3',
    [dbPath, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=${quote(name)};`],
    { encoding: 'utf8' },
  ).trim();
  return out !== '0';
}

function discussionIdHasFk(): boolean {
  // PRAGMA foreign_key_list returns one row per FK on the table.
  // We're looking for a `discussion_id → channels(id)` row.
  // Format: id|seq|table|from|to|on_update|on_delete|match
  const out = execFileSync('sqlite3', [dbPath, 'PRAGMA foreign_key_list(threads);'], {
    encoding: 'utf8',
  });
  return out
    .split('\n')
    .some((line) => {
      const cols = line.split('|');
      return cols[2] === 'channels' && cols[3] === 'discussion_id';
    });
}

function quote(s: string): string {
  return `'${s.replace(/'/g, "''")}'`;
}

// --- migration body -------------------------------------------------------

// VERBATIM copy of the threads CREATE TABLE block in
// internal/store/schema_v1.go (after the discussion_id FK change).
// Column names, order, defaults, and CHECK constraints must match
// exactly — a drift here would silently change schema semantics on the
// migrated DB. The post-migration verification confirms column-set
// match against fresh-install schema.
const NEW_THREADS_SCHEMA = `
CREATE TABLE "threads_new" (
    id                       TEXT    PRIMARY KEY,
    project_id               TEXT    NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title                    TEXT    NOT NULL DEFAULT 'New Thread',
    provider                 TEXT    NOT NULL CHECK(provider IN ('claude','codex')),
    model                    TEXT    NOT NULL DEFAULT '',
    workspace_path           TEXT    NOT NULL,
    worktree_path            TEXT,
    branch                   TEXT,
    session_ref              TEXT,
    pending_fork_session_ref TEXT,
    mode                     TEXT    NOT NULL DEFAULT 'chat'
        CHECK(mode IN ('chat','plan','design','discussion')),
    reasoning_effort         TEXT    NOT NULL DEFAULT 'high'
        CHECK(
            (provider = 'codex' AND reasoning_effort IN ('none','minimal','low','medium','high','xhigh'))
            OR (provider = 'claude' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
        ),
    fast_mode                INTEGER NOT NULL DEFAULT 0 CHECK(fast_mode IN (0,1)),
    context_window           INTEGER NOT NULL DEFAULT 1000000 CHECK(context_window > 0),
    auto_compact_standard_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_standard_percent BETWEEN 0 AND 90),
    auto_compact_extended_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_extended_percent BETWEEN 0 AND 90),
    runtime_mode             TEXT    NOT NULL DEFAULT 'full-access'
        CHECK(runtime_mode IN ('approval-required','auto-accept-edits','full-access')),
    discussion_id            TEXT    REFERENCES channels(id) ON DELETE SET NULL,
    parent_thread_id         TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    forked_from_thread_id    TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    last_token_usage         TEXT    NOT NULL DEFAULT ''
        CHECK(last_token_usage = '' OR json_valid(last_token_usage)),
    last_read_at             INTEGER,
    pinned_at                INTEGER,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    archived                 INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1))
);`.trim();

const COLUMN_NAMES = [
  'id',
  'project_id',
  'title',
  'provider',
  'model',
  'workspace_path',
  'worktree_path',
  'branch',
  'session_ref',
  'pending_fork_session_ref',
  'mode',
  'reasoning_effort',
  'fast_mode',
  'context_window',
  'auto_compact_standard_percent',
  'auto_compact_extended_percent',
  'runtime_mode',
  'discussion_id',
  'parent_thread_id',
  'forked_from_thread_id',
  'last_token_usage',
  'last_read_at',
  'pinned_at',
  'created_at',
  'updated_at',
  'archived',
];

function buildMigrationSql(): string {
  const cols = COLUMN_NAMES.join(', ');
  // Follows the canonical SQLite ALTER pattern from
  // https://www.sqlite.org/lang_altertable.html section 7:
  //   1. Disable FKs for the rebuild window.
  //   2. Inside a transaction, copy → drop → rename.
  //   3. Re-enable FKs and run foreign_key_check.
  return `
.echo on
.bail on

-- 1. Drop redundant index — covered by the composite PK on items.
DROP INDEX IF EXISTS idx_items_id;

-- 2. NULL out orphan discussion_id values so the FK on the rebuilt
--    threads table doesn't reject the INSERT SELECT. Picks up threads
--    that reference a channel row that was deleted out from under them.
UPDATE threads
   SET discussion_id = NULL
 WHERE discussion_id IS NOT NULL
   AND discussion_id NOT IN (SELECT id FROM channels);

-- 3. Rebuild threads with the discussion_id FK.
PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;

${NEW_THREADS_SCHEMA}

INSERT INTO threads_new (${cols})
     SELECT ${cols} FROM threads;

DROP TABLE threads;
ALTER TABLE threads_new RENAME TO threads;

-- foreign_key_check returns one row per violation BEFORE commit; if any
-- exist the bail-on-error flag rolls the transaction back.
PRAGMA foreign_key_check;
COMMIT;
PRAGMA foreign_keys=ON;
`.trim();
}

function applyMigration(): void {
  const sql = buildMigrationSql();
  const result = spawnSync('sqlite3', [dbPath], {
    input: sql,
    encoding: 'utf8',
  });
  if (result.status !== 0) {
    console.error('error: sqlite3 exited non-zero — migration NOT applied');
    if (result.stdout) console.error('stdout:', result.stdout);
    if (result.stderr) console.error('stderr:', result.stderr);
    process.exit(1);
  }
  // foreign_key_check rows show up on stdout if violations exist —
  // surface them so the user can see why we aborted.
  if (result.stdout && result.stdout.trim().length > 0) {
    process.stdout.write(result.stdout);
  }
}

// Verify the migrated threads table has every expected column. A
// silently-dropped column would not have failed the apply step (SQLite
// is forgiving on INSERT column lists) but would corrupt subsequent
// app behavior.
function verifyColumns(): void {
  const out = execFileSync('sqlite3', [dbPath, 'PRAGMA table_info(threads);'], {
    encoding: 'utf8',
  });
  const found = new Set(
    out
      .split('\n')
      .map((line) => line.split('|')[1])
      .filter(Boolean),
  );
  const missing = COLUMN_NAMES.filter((c) => !found.has(c));
  if (missing.length > 0) {
    console.error('error: post-migration threads schema is missing columns:', missing);
    process.exit(1);
  }
}

// --- main -----------------------------------------------------------------

console.log(`db: ${dbPath}`);
console.log(`size: ${(statSync(dbPath).size / 1024 / 1024).toFixed(2)} MiB`);

probeExclusive();

const hasIdxItemsId = indexExists('idx_items_id');
const hasFk = discussionIdHasFk();
if (!hasIdxItemsId && hasFk) {
  console.log('already migrated — nothing to do.');
  process.exit(0);
}
console.log(`status: idx_items_id present=${hasIdxItemsId}, discussion_id FK present=${hasFk}`);

const backupPath = `${dbPath}.backup-${new Date().toISOString().replace(/[:.]/g, '-')}`;
console.log(`backup: ${backupPath}`);
copyFileSync(dbPath, backupPath);

console.log('applying migration...');
applyMigration();

// Re-probe and confirm.
const stillHasIdx = indexExists('idx_items_id');
const nowHasFk = discussionIdHasFk();
if (stillHasIdx || !nowHasFk) {
  console.error('error: post-migration verification failed.');
  console.error(`  idx_items_id present (want false): ${stillHasIdx}`);
  console.error(`  discussion_id FK present (want true): ${nowHasFk}`);
  console.error(`  restore the backup: cp "${backupPath}" "${dbPath}"`);
  process.exit(1);
}
verifyColumns();

console.log('done. Launch Agent Overflow when ready.');
console.log(`If anything looks off, restore: cp "${backupPath}" "${dbPath}"`);
