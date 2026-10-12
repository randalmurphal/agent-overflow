// Helpers for the release upgrade check (upgrade-corpus.manual.spec.ts):
// the read-only census of the database the check compares before and after
// the migration, the fingerprint of the launcher's snapshot against the
// migrated store, the version-keyed exceptions that narrow both, the
// deferred-phase wait, and the sidebar and timeline reads the migrated boot
// must answer.
import { expect, type Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import {
  countIgnoredTables,
  fingerprintIgnoredTables,
  migrationExceptions,
  type MigrationException,
} from './upgrade-corpus-config.js';
import { dbQuery, tableNames } from './upgrade-corpus-tools.js';

const CONFIG_FILE = 'e2e/tests/upgrade-corpus-config.ts';

/** The exceptions that apply to a run migrating from one version to another. */
function exceptionsFor(from: number, to: number): MigrationException[] {
  return migrationExceptions.filter((entry) => entry.version > from && entry.version <= to);
}

/**
 * assertExceptionsCurrent fails on an entry the lineage has already
 * passed, so the list cannot accumulate: an entry below the corpus's
 * version describes a migration no run will apply again.
 */
export function assertExceptionsCurrent(corpusVersion: number): void {
  const stale = migrationExceptions.filter((entry) => entry.version <= corpusVersion);
  if (stale.length > 0) {
    throw new Error(`${CONFIG_FILE} migrationExceptions has entries for v${stale.map((e) => e.version).join(', v')}, ` +
      `which the corpus (v${corpusVersion}) has already crossed; remove them`);
  }
}

export interface Fingerprint {
  /** Table to the number of its pre-migration rows now missing or changed. */
  changedRows: Record<string, number>;
  /** Tables of the snapshot the migrated store no longer has. */
  droppedTables: string[];
  /** Tables whose comparison an exception narrowed or skipped. */
  excepted: string[];
}

/**
 * fingerprint compares every table of the pre-migration snapshot with the
 * migrated store, row by row over the columns both schemas share (BLOBs by
 * length), and fails on the first old row that is missing or changed, or
 * table that is gone, unless a migrationExceptions entry for a migration in
 * (from, to] covers it. Rows a migration adds are not its concern; a row a
 * migration took away or altered is user data, and the failure names the
 * entry an intentional change must declare.
 */
export async function fingerprint(dbPath: string, snapshotPath: string, from: number, to: number): Promise<Fingerprint> {
  const exceptions = exceptionsFor(from, to);
  const span = from + 1 === to ? `migration v${to}` : `the migrations v${from + 1}..v${to}`;
  const instruction = (table: string, what: string) =>
    `${span} ${what} in ${table}. If that is the migration's intent, add a migrationExceptions entry ` +
    `{ version, table: '${table}', ... reason } to ${CONFIG_FILE}; otherwise the migration lost or altered user data.`;
  const before = await tableNames(dbPath, 'snap', snapshotPath);
  const after = new Set(await tableNames(dbPath, 'main', snapshotPath));
  const out: Fingerprint = { changedRows: {}, droppedTables: [], excepted: [] };
  for (const table of before) {
    const forTable = exceptions.filter((entry) => entry.table === table);
    if (!after.has(table)) {
      out.droppedTables.push(table);
      if (!forTable.some((entry) => entry.dropped)) throw new Error(instruction(table, 'dropped the table'));
      out.excepted.push(table);
      continue;
    }
    if (table in fingerprintIgnoredTables) continue;
    const virtual = await dbQuery<{ n: number }>(dbPath,
      `SELECT COUNT(*) AS n FROM snap.sqlite_master WHERE name = '${table}' AND sql LIKE 'CREATE VIRTUAL TABLE%'`, snapshotPath);
    if (virtual[0].n > 0) continue;
    const skipColumns = new Set(forTable.flatMap((entry) => entry.columns ?? []));
    const shrink = forTable.some((entry) => entry.rowsMayShrink);
    if (skipColumns.size > 0 || shrink) out.excepted.push(table);
    if (shrink) continue;
    const snapColumns = await dbQuery<{ name: string; type: string }>(dbPath,
      `SELECT name, type FROM pragma_table_info('${table}', 'snap')`, snapshotPath);
    const mainColumns = new Set((await dbQuery<{ name: string }>(dbPath,
      `SELECT name FROM pragma_table_info('${table}')`, snapshotPath)).map((column) => column.name));
    const projection = snapColumns
      .filter((column) => mainColumns.has(column.name) && !skipColumns.has(column.name))
      .map((column) => (/BLOB/i.test(column.type) ? `length("${column.name}")` : `"${column.name}"`))
      .join(', ');
    if (!projection) continue;
    const [{ n }] = await dbQuery<{ n: number }>(dbPath,
      `SELECT COUNT(*) AS n FROM (SELECT ${projection} FROM snap."${table}" EXCEPT SELECT ${projection} FROM main."${table}")`, snapshotPath);
    out.changedRows[table] = n;
    if (n > 0) throw new Error(instruction(table, `left ${n} pre-migration row(s) missing or changed`));
  }
  return out;
}

/**
 * Census is what the check compares across the migration: the entity
 * counts a user would notice missing, every thread's top-level item count,
 * and every table's row count so the report can name the tables the corpus
 * leaves empty.
 */
export interface Census {
  schemaVersion: number;
  /** Non-archived projects, what ListProjects and the sidebar show. */
  projects: number;
  archivedProjects: number;
  threads: number;
  archived: number;
  /** Pinned non-archived threads, what HarnessListThreadRows carries. */
  pinned: number;
  groups: number;
  turns: number;
  items: number;
  childItems: number;
  payloads: number;
  payloadBytes: number;
  /** Streaming append chunks beside payloads (payload_chunks). */
  chunks: number;
  chunkBytes: number;
  /** Thread id to its top-level item count, archived threads included. */
  threadItems: Record<string, number>;
  /** Every table with its row count, FTS shadow tables folded under their virtual table. */
  tables: Record<string, number>;
}

export async function census(dbPath: string): Promise<Census> {
  const [counts] = await dbQuery<Omit<Census, 'threadItems' | 'tables'>>(dbPath, `
    SELECT (SELECT MAX(version) FROM migration_versions) AS schemaVersion,
           (SELECT COUNT(*) FROM projects WHERE archived = 0) AS projects,
           (SELECT COUNT(*) FROM projects WHERE archived = 1) AS archivedProjects,
           (SELECT COUNT(*) FROM threads WHERE archived = 0) AS threads,
           (SELECT COUNT(*) FROM threads WHERE archived = 1) AS archived,
           (SELECT COUNT(*) FROM threads WHERE archived = 0 AND pinned_at IS NOT NULL) AS pinned,
           (SELECT COUNT(*) FROM thread_groups) AS groups,
           (SELECT COUNT(*) FROM turns) AS turns,
           (SELECT COUNT(*) FROM items) AS items,
           (SELECT COUNT(*) FROM items WHERE parent_id <> '') AS childItems,
           (SELECT COUNT(*) FROM payloads) AS payloads,
           (SELECT COALESCE(SUM(LENGTH(data)), 0) FROM payloads) AS payloadBytes,
           (SELECT COUNT(*) FROM payload_chunks) AS chunks,
           (SELECT COALESCE(SUM(LENGTH(data)), 0) FROM payload_chunks) AS chunkBytes`);
  const perThread = await dbQuery<{ threadId: string; n: number }>(dbPath,
    `SELECT thread_id AS threadId, COUNT(*) AS n FROM items WHERE parent_id = '' GROUP BY thread_id`);
  // FTS shadow tables are folded under their virtual table: their segment
  // rows merge and split on their own, the virtual table's row count is
  // the one that means anything.
  const names = await tableNames(dbPath, 'main');
  const union = names.map((name) => `SELECT '${name}' AS name, COUNT(*) AS n FROM "${name}"`).join(' UNION ALL ');
  const rows = await dbQuery<{ name: string; n: number }>(dbPath, union);
  return {
    ...counts,
    threadItems: Object.fromEntries(perThread.map((row) => [row.threadId, row.n])),
    tables: Object.fromEntries(rows.map((row) => [row.name, row.n])),
  };
}

/**
 * compareCensus is the rule a run must keep from one census to the next:
 * the entities a user sees (projects, threads, archived, pinned, groups)
 * are exactly what they were plus what `growth` declares; no table shrinks
 * or disappears; no thread loses rows. Rows may be added, by a repair
 * migration, by boot recovery of in-flight turns, by the top-up. Returns
 * every count's delta for the report; throws naming the first violation
 * and the exception entry an intentional change must declare.
 */
export type CensusEntity = 'projects' | 'archivedProjects' | 'threads' | 'archived' | 'pinned' | 'groups';

/** What a phase is expected to add to the entities a user sees. */
export interface CensusGrowth {
  /** Entities that must grow by exactly this much. */
  exact?: Partial<Record<CensusEntity, number>>;
  /** Entities that must grow by at least this much. */
  atLeast?: Partial<Record<CensusEntity, number>>;
}

export function compareCensus(before: Census, after: Census, from: number, to: number, growth: CensusGrowth = {}): Record<string, number> {
  const exceptions = exceptionsFor(from, to);
  const exact: CensusEntity[] = ['projects', 'archivedProjects', 'threads', 'archived', 'pinned', 'groups'];
  const deltas: Record<string, number> = {};
  for (const key of [...exact, 'turns', 'items', 'childItems', 'payloads', 'payloadBytes', 'chunks', 'chunkBytes'] as const) {
    deltas[key] = after[key] - before[key];
  }
  for (const key of exact) {
    const floor = growth.atLeast?.[key];
    if (floor !== undefined) {
      if (after[key] < before[key] + floor) throw new Error(`${key}: ${before[key]} before, ${after[key]} after; at least ${floor} more expected`);
      continue;
    }
    const expected = before[key] + (growth.exact?.[key] ?? 0);
    if (after[key] !== expected) throw new Error(`${key}: ${before[key]} before, ${after[key]} after; ${expected} expected`);
  }
  const bytesBefore = before.payloadBytes + before.chunkBytes;
  const bytesAfter = after.payloadBytes + after.chunkBytes;
  if (bytesAfter < bytesBefore) throw new Error(`payload bytes shrank: ${bytesBefore} before the migration, ${bytesAfter} after`);
  for (const [table, n] of Object.entries(before.tables)) {
    const forTable = exceptions.filter((entry) => entry.table === table);
    const now = after.tables[table];
    if (now === undefined) {
      if (forTable.some((entry) => entry.dropped)) continue;
      throw new Error(`table ${table} (${n} rows) is gone after the migration; a migration that drops it declares ` +
        `{ version, table: '${table}', dropped: true, reason } in ${CONFIG_FILE}`);
    }
    deltas[`table:${table}`] = now - n;
    if (now < n && !(table in countIgnoredTables) && !forTable.some((entry) => entry.rowsMayShrink)) {
      throw new Error(`table ${table} shrank from ${n} to ${now} rows; a migration that removes or moves its rows declares ` +
        `{ version, table: '${table}', rowsMayShrink: true, reason } in ${CONFIG_FILE}`);
    }
  }
  for (const [threadID, n] of Object.entries(before.threadItems)) {
    const now = after.threadItems[threadID];
    if (now === undefined) throw new Error(`thread ${threadID} had ${n} rows before the migration and none after`);
    if (now < n) throw new Error(`thread ${threadID} had ${n} rows before the migration, ${now} after`);
  }
  return deltas;
}

/** The thread with the most top-level items, from a census. */
export async function largestThread(dbPath: string): Promise<{ id: string; title: string; items: number }> {
  const [row] = await dbQuery<{ id: string; title: string; items: number }>(dbPath, `
    SELECT t.id AS id, t.title AS title, COUNT(i.id) AS items
      FROM threads t JOIN items i ON i.thread_id = t.id AND i.parent_id = ''
     WHERE t.archived = 0
     GROUP BY t.id ORDER BY items DESC, t.id LIMIT 1`);
  if (!row) throw new Error('the database has no non-archived thread with items');
  return row;
}

/** HarnessStoreStatus on the wire. */
export interface StoreStatus {
  schemaVersion: number;
  buildSchemaVersion: number;
  deferredWatermark: number;
  deferredLatest: number;
  deferredPending: boolean;
  deferredFailure: { version: number; title: string; failures: number; firstError: string } | null;
}

/**
 * awaitDeferredSettled polls the candidate until its deferred migration
 * phases have finished, and fails on the first recorded failure: a phase
 * that skipped an item is a migration defect, not a slow one.
 */
export async function awaitDeferredSettled(candidate: HarnessApp, timeoutMs: number): Promise<StoreStatus> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const status = await candidate.rpc<StoreStatus>('HarnessStoreStatus');
    if (status.deferredFailure) {
      throw new Error(`deferred migration v${status.deferredFailure.version} (${status.deferredFailure.title}) failed ` +
        `${status.deferredFailure.failures} time(s): ${status.deferredFailure.firstError}`);
    }
    if (!status.deferredPending) return status;
    if (Date.now() > deadline) {
      throw new Error(`deferred migrations still pending after ${timeoutMs}ms: watermark ${status.deferredWatermark}, latest ${status.deferredLatest}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 2_000));
  }
}

/**
 * openThreadByTitle finds a thread through the sidebar search, opens it,
 * and returns how long the timeline took to show its first row. The
 * `#n of N` token of a corpus title is unique; a real database's titles
 * are whatever the user named them, so the caller passes the whole title.
 */
export async function openThreadByTitle(page: Page, title: string, token: string, timeoutMs: number): Promise<number> {
  await page.getByTestId('sidebar-thread-search').fill(token);
  const row = page.getByTestId('thread-row').filter({ hasText: token }).first();
  await expect(row).toBeVisible({ timeout: timeoutMs });
  const started = Date.now();
  await row.click();
  await expect(page.getByTestId('message-timeline-node').first()).toBeVisible({ timeout: timeoutMs });
  const ms = Date.now() - started;
  await expect(page.getByTestId('chat-header')).toContainText(title.slice(0, 40), { timeout: timeoutMs });
  await page.getByTestId('sidebar-thread-search').fill('');
  return ms;
}
