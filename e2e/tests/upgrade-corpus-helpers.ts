// Helpers for the release upgrade check (upgrade-corpus.manual.spec.ts):
// the read-only census of the database the check compares before and after
// the migration, the version-keyed exceptions that narrow it and the
// fingerprint (upgrade-corpus-fingerprint.ts), the deferred-phase wait, and
// the sidebar and timeline reads the migrated boot must answer.
import { expect, type Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import {
  countIgnoredTables, migrationExceptions, rowSweepRules, sweepRules, type MigrationException, type SweepContext, type SweepRule,
} from './upgrade-corpus-config.js';
import { dbQuery, tableNames } from './upgrade-corpus-tools.js';

export const CONFIG_FILE = 'e2e/tests/upgrade-corpus-config.ts';

/** The exceptions that apply to a run migrating from one version to another. */
export function exceptionsFor(from: number, to: number): MigrationException[] {
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
  /**
   * Non-archived threads, what HarnessListThreadRows lists: owned_threads,
   * so a holder (the retired source a pointer fork still reads) or a thread
   * mid-delete is not counted.
   */
  threads: number;
  /** Archived threads, what ListArchivedThreads lists: hidden modes excluded. */
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

/** threadmode.HiddenModes, which ListArchivedThreads leaves out. */
const hiddenModes = ['workflow', 'workflow-studio', 'workflow-triage', 'scratch', 'holder'].map((m) => `'${m}'`).join(', ');

export async function census(dbPath: string): Promise<Census> {
  const [counts] = await dbQuery<Omit<Census, 'threadItems' | 'tables'>>(dbPath, `
    SELECT (SELECT MAX(version) FROM migration_versions) AS schemaVersion,
           (SELECT COUNT(*) FROM projects WHERE archived = 0) AS projects,
           (SELECT COUNT(*) FROM projects WHERE archived = 1) AS archivedProjects,
           (SELECT COUNT(*) FROM owned_threads WHERE archived = 0) AS threads,
           (SELECT COUNT(*) FROM owned_threads WHERE archived = 1 AND mode NOT IN (${hiddenModes})) AS archived,
           (SELECT COUNT(*) FROM owned_threads WHERE archived = 0 AND pinned_at IS NOT NULL) AS pinned,
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
 * or disappears; no thread loses rows, beyond the rows `growth.removed`
 * says a boot's recovery rules removed. Rows may be added, by a repair
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
  /**
   * Rows a boot removed that a rule declares: the trial's recovery rules
   * (Fingerprint.recoveryRemoved) and the later boots' sweeps
   * (sweepGrowth). A table or a thread may shrink by that many rows, and
   * payload bytes by `bytes`.
   */
  removed?: { tables: Record<string, number>; threadItems: Record<string, number>; bytes?: number };
}

/** One thread a sweep rule selects, as the store the trial left holds it. */
interface SweptThread {
  rule: string;
  /** Table to the thread's rows (`thread_id`, or `id` in threads). */
  tables: Record<string, number>;
  top: number;
  bytes: number;
  /** Its census entities: listed (non-archived), archived (shown), pinned. */
  threads: number;
  archived: number;
  pinned: number;
}

/** A row of a rowSweepRules table, as the store the trial left holds it. */
interface SweptRow {
  rule: string;
  key: string;
  row: Record<string, unknown>;
}

/** The threads sweepRules select, and the rowSweepRules rows, before the boots that sweep them. */
export interface SweepBaseline {
  threads: Record<string, SweptThread>;
  rows: SweptRow[];
}

/** What each sweep rule's threads lost, for the report. */
export interface SweepCount {
  rule: string;
  writer: string;
  threads: number;
  /** The threads whose row is gone. */
  gone: number;
  rows: number;
}

const sqlList = (ids: string[]) => ids.length > 0 ? ids.map((id) => `'${id.replaceAll("'", "''")}'`).join(', ') : "''";
const emptySwept = (): Omit<SweptThread, 'rule'> => ({ tables: {}, top: 0, bytes: 0, threads: 0, archived: 0, pinned: 0 });

/** sweptState reads each named thread's rows, payload bytes and census entities. */
async function sweptState(dbPath: string, ids: string[]): Promise<Record<string, Omit<SweptThread, 'rule'>>> {
  const out: Record<string, Omit<SweptThread, 'rule'>> = Object.fromEntries(ids.map((id) => [id, emptySwept()]));
  if (ids.length === 0) return out;
  const list = sqlList(ids);
  const withThread = new Set((await dbQuery<{ tbl: string }>(dbPath, `SELECT DISTINCT m.name AS tbl FROM sqlite_master m, pragma_table_info(m.name) p
      WHERE m.type = 'table' AND p.name = 'thread_id'`)).map((row) => row.tbl));
  const tables = (await tableNames(dbPath, 'main')).filter((table) => withThread.has(table));
  const counts = await dbQuery<{ tbl: string; thread: string; n: number }>(dbPath, [
    `SELECT 'threads' AS tbl, id AS thread, COUNT(*) AS n FROM threads WHERE id IN (${list}) GROUP BY id`,
    ...tables.map((table) => `SELECT '${table}' AS tbl, thread_id AS thread, COUNT(*) AS n FROM "${table}" WHERE thread_id IN (${list}) GROUP BY thread_id`),
  ].join(' UNION ALL '));
  for (const row of counts) out[row.thread].tables[row.tbl] = row.n;
  for (const row of await dbQuery<{ thread: string } & Omit<SweptThread, 'rule' | 'tables'>>(dbPath, `
      SELECT t.id AS thread,
             (SELECT COUNT(*) FROM items i WHERE i.thread_id = t.id AND i.parent_id = '') AS top,
             (SELECT COALESCE(SUM(LENGTH(data)), 0) FROM payloads p WHERE p.thread_id = t.id)
               + (SELECT COALESCE(SUM(LENGTH(data)), 0) FROM payload_chunks c WHERE c.thread_id = t.id) AS bytes,
             EXISTS (SELECT 1 FROM owned_threads o WHERE o.id = t.id AND o.archived = 0) AS threads,
             EXISTS (SELECT 1 FROM owned_threads o WHERE o.id = t.id AND o.archived = 1 AND o.mode NOT IN (${hiddenModes})) AS archived,
             EXISTS (SELECT 1 FROM owned_threads o WHERE o.id = t.id AND o.archived = 0 AND o.pinned_at IS NOT NULL) AS pinned
        FROM threads t WHERE t.id IN (${list})`)) {
    const { thread, ...rest } = row;
    Object.assign(out[thread], rest);
  }
  return out;
}


/**
 * sweepBaseline reads, from the store the trial left and before any later
 * boot, the threads each sweepRules entry selects, with their rows, and the
 * rows of every rowSweepRules table.
 */
export async function sweepBaseline(dbPath: string): Promise<SweepBaseline> {
  const rule = new Map<string, string>();
  const select = async (entry: SweepRule, sql: string) => {
    for (const row of await dbQuery<Record<string, string>>(dbPath, sql)) {
      const id = Object.values(row)[0];
      if (!rule.has(id)) rule.set(id, entry.name);
    }
  };
  for (const entry of sweepRules.filter((r) => !r.threads.includes('{swept}'))) await select(entry, entry.threads);
  for (const entry of sweepRules.filter((r) => r.threads.includes('{swept}'))) {
    await select(entry, entry.threads.replace('{swept}', sqlList([...rule.keys()])));
  }
  const state = await sweptState(dbPath, [...rule.keys()]);
  const rows: SweptRow[] = [];
  for (const entry of rowSweepRules) {
    for (const row of await dbQuery<Record<string, unknown>>(dbPath, `SELECT ${entry.read} FROM "${entry.table}"`)) {
      rows.push({ rule: entry.name, key: String(row.key), row });
    }
  }
  return { threads: Object.fromEntries([...rule.entries()].map(([id, name]) => [id, { rule: name, ...state[id] }])), rows };
}

/**
 * sweepGrowth is what the census allows once the sweeps ran: for each
 * baseline thread, the rows, top-level items, payload bytes and entities it
 * no longer has in `dbPath`, and each baseline row of a rowSweepRules table
 * that is gone and that its rule, given `live`, lets the boot delete. A
 * thread or row the sweep left alone allows nothing.
 */
export async function sweepGrowth(baseline: SweepBaseline, dbPath: string, live: SweepContext): Promise<{ growth: CensusGrowth; report: SweepCount[] }> {
  const ids = Object.keys(baseline.threads);
  const now = await sweptState(dbPath, ids);
  const exact: Partial<Record<CensusEntity, number>> = {};
  const removed = { tables: {} as Record<string, number>, threadItems: {} as Record<string, number>, bytes: 0 };
  const report = new Map<string, SweepCount>([...sweepRules, ...rowSweepRules]
    .map((r) => [r.name, { rule: r.name, writer: r.writer, threads: 0, gone: 0, rows: 0 }]));
  for (const id of ids) {
    const was = baseline.threads[id];
    const is = now[id];
    const count = report.get(was.rule)!;
    count.threads += 1;
    if (!is.tables.threads) count.gone += 1;
    for (const [table, n] of Object.entries(was.tables)) {
      const lost = n - (is.tables[table] ?? 0);
      if (lost <= 0) continue;
      removed.tables[table] = (removed.tables[table] ?? 0) + lost;
      count.rows += lost;
    }
    if (was.top > is.top) removed.threadItems[id] = was.top - is.top;
    removed.bytes += Math.max(0, was.bytes - is.bytes);
    for (const key of ['threads', 'archived', 'pinned'] as const) exact[key] = (exact[key] ?? 0) + is[key] - was[key];
  }
  for (const entry of rowSweepRules) {
    const kept = new Set((await dbQuery<{ key: unknown }>(dbPath, `SELECT ${entry.read} FROM "${entry.table}"`)).map((row) => String(row.key)));
    const count = report.get(entry.name)!;
    for (const { key, row } of baseline.rows.filter((r) => r.rule === entry.name)) {
      if (kept.has(key) || !entry.prunable(row, live)) continue;
      removed.tables[entry.table] = (removed.tables[entry.table] ?? 0) + 1;
      count.rows += 1;
    }
  }
  return { growth: { exact, removed }, report: [...report.values()].filter((count) => count.threads > 0 || count.rows > 0) };
}

/** The catalog counts a booted candidate answers. */
export interface CatalogReads {
  projects: number;
  threads: number;
  archived: number;
  groups: number;
  pinned: number;
}

/**
 * readCatalog is what the candidate lists through ListProjects,
 * HarnessListThreadRows, ListArchivedThreads and ListThreadGroups, leaving
 * out the threads a sweep rule selected: whether a sweep has run by the
 * time of the read is the boot's timing, not the root's content.
 */
export async function readCatalog(app: HarnessApp, swept: SweepBaseline): Promise<CatalogReads> {
  // An empty list crosses the wire as null (a Go nil slice).
  const list = async <T,>(method: string): Promise<T[]> => (await app.rpc<T[] | null>(method)) ?? [];
  const kept = <T extends { id: string }>(rows: T[]) => rows.filter((row) => !(row.id in swept.threads));
  const threads = kept(await list<{ id: string; pinnedAt?: number }>('HarnessListThreadRows'));
  return {
    projects: (await list<unknown>('ListProjects')).length,
    threads: threads.length,
    archived: kept(await list<{ id: string }>('ListArchivedThreads')).length,
    groups: (await list<unknown>('ListThreadGroups')).length,
    pinned: threads.filter((row) => row.pinnedAt != null).length,
  };
}

/** The reads a census promises, less the threads a sweep rule selected. */
export function expectedCatalog(c: Census, swept: SweepBaseline): CatalogReads {
  const out = { projects: c.projects, threads: c.threads, archived: c.archived, groups: c.groups, pinned: c.pinned };
  for (const thread of Object.values(swept.threads)) {
    for (const key of ['threads', 'archived', 'pinned'] as const) out[key] -= thread[key];
  }
  return out;
}

/**
 * The span two censuses bracket, which names the cause of a loss: the
 * migrations (and the trial's boot, whose own changes the fingerprint has
 * already classified), the ordinary boots after the trial, or the top-up.
 */
export type CensusPhase = 'migration' | 'boot' | 'top-up';

const phaseSpan: Record<CensusPhase, string> = {
  migration: 'across the migrations',
  boot: 'across the boots after the trial',
  'top-up': 'across the top-up',
};

/** What declares an intended loss in each phase. */
function phaseRemedy(phase: CensusPhase, table: string): string {
  if (phase === 'migration') {
    return `a migration that removes or moves its rows declares { version, table: '${table}', rowsMayShrink: true, reason } in ${CONFIG_FILE}`;
  }
  if (phase === 'boot') {
    return `a boot writer that prunes rows the last process or the root's relocation left behind declares a sweepRules entry naming it in ${CONFIG_FILE}; otherwise the boot lost user data`;
  }
  return 'the top-up removes nothing';
}

export function compareCensus(before: Census, after: Census, from: number, to: number, phase: CensusPhase,
  growth: CensusGrowth = {}): Record<string, number> {
  const exceptions = phase === 'migration' ? exceptionsFor(from, to) : [];
  const span = phaseSpan[phase];
  const exact: CensusEntity[] = ['projects', 'archivedProjects', 'threads', 'archived', 'pinned', 'groups'];
  const deltas: Record<string, number> = {};
  for (const key of [...exact, 'turns', 'items', 'childItems', 'payloads', 'payloadBytes', 'chunks', 'chunkBytes'] as const) {
    deltas[key] = after[key] - before[key];
  }
  for (const key of exact) {
    const floor = growth.atLeast?.[key];
    if (floor !== undefined) {
      if (after[key] < before[key] + floor) throw new Error(`${key} ${span}: ${before[key]} before, ${after[key]} after; at least ${floor} more expected`);
      continue;
    }
    const expected = before[key] + (growth.exact?.[key] ?? 0);
    if (after[key] !== expected) throw new Error(`${key} ${span}: ${before[key]} before, ${after[key]} after; ${expected} expected`);
  }
  const bytesBefore = before.payloadBytes + before.chunkBytes;
  const bytesAfter = after.payloadBytes + after.chunkBytes;
  if (bytesAfter < bytesBefore - (growth.removed?.bytes ?? 0)) {
    throw new Error(`payload bytes shrank ${span}: ${bytesBefore} before, ${bytesAfter} after`);
  }
  for (const [table, n] of Object.entries(before.tables)) {
    const forTable = exceptions.filter((entry) => entry.table === table);
    const now = after.tables[table];
    if (now === undefined) {
      if (forTable.some((entry) => entry.dropped)) continue;
      throw new Error(`table ${table} (${n} rows) is gone ${span}; a migration that drops it declares ` +
        `{ version, table: '${table}', dropped: true, reason } in ${CONFIG_FILE}`);
    }
    deltas[`table:${table}`] = now - n;
    if (now < n - (growth.removed?.tables[table] ?? 0) && !(table in countIgnoredTables) && !forTable.some((entry) => entry.rowsMayShrink)) {
      throw new Error(`table ${table} shrank from ${n} to ${now} rows ${span}; ${phaseRemedy(phase, table)}`);
    }
  }
  for (const [threadID, n] of Object.entries(before.threadItems)) {
    const now = after.threadItems[threadID] ?? 0;
    const floor = n - (growth.removed?.threadItems[threadID] ?? 0);
    if (now < floor) throw new Error(`thread ${threadID} had ${n} rows, ${now || 'none'} ${span}`);
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
  let status!: StoreStatus;
  await expect
    .poll(async () => {
      status = await candidate.rpc<StoreStatus>('HarnessStoreStatus');
      return status.deferredFailure !== null || !status.deferredPending;
    }, { timeout: timeoutMs, message: 'deferred migrations still pending' })
    .toBe(true);
  if (status.deferredFailure) {
    throw new Error(`deferred migration v${status.deferredFailure.version} (${status.deferredFailure.title}) failed ` +
      `${status.deferredFailure.failures} time(s): ${status.deferredFailure.firstError}`);
  }
  return status;
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
