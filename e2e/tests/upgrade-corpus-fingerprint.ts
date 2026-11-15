// The release upgrade check's fingerprint (upgrade-corpus.manual.spec.ts,
// docs/architecture/release-upgrade-check.md): every row of the launcher's
// pre-migration snapshot must still be in the store the update trial left,
// unchanged on the columns both schemas share, unless a migration exception
// or a boot recovery rule (upgrade-corpus-config.ts) explains the change.
//
// The trial both migrates the store and boots on it, and a boot settles
// what a dead process left live. The two causes are told apart by
// migrating a copy of the snapshot alone: a row that copy changed is the
// migrations' doing, a row it kept and the trial changed is the boot's.
// The copy is made only when some row changed. Row content never leaves
// SQLite: the queries return keys, column names and flags.
import { execFile } from 'node:child_process';
import { copyFile, mkdir, readdir } from 'node:fs/promises';
import * as path from 'node:path';
import { promisify } from 'node:util';
import { fingerprintIgnoredColumns, fingerprintIgnoredTables, recoveryRules, type RecoveryRule } from './upgrade-corpus-config.js';
import { CONFIG_FILE, exceptionsFor } from './upgrade-corpus-helpers.js';
import { dbQuery, harnessTool, tableNames } from './upgrade-corpus-tools.js';

const run = promisify(execFile);

/** What a recovery rule explained in one run. */
export interface RecoveryCount {
  rule: string;
  writer: string;
  table: string;
  rows: number;
}

/** Rows a rule removed: by table, top-level items by thread, and the threads removed whole. */
export interface RecoveryRemoved {
  tables: Record<string, number>;
  threadItems: Record<string, number>;
  threads: string[];
}

/** A change to an old row no exception or rule explains, named by its key. */
export interface RowChange {
  table: string;
  key: Record<string, unknown>;
  removed: boolean;
  /** The columns that changed, empty for a removed row. */
  columns: string[];
}

export interface Fingerprint {
  /** Table to its pre-migration rows the trial left missing or changed. */
  changedRows: Record<string, number>;
  /** Tables of the snapshot the migrated store no longer has. */
  droppedTables: string[];
  /** Tables whose comparison an exception narrowed or skipped. */
  excepted: string[];
  /** Table to the rows the migrations alone changed; set once any row changed. */
  migrationChangedRows?: Record<string, number>;
  /** What the boot changed after the migrations, by rule. */
  recovery: RecoveryCount[];
  /** What the boot changed that no rule explains (at most 50 per table). */
  bootUnexplained: RowChange[];
  /** Table to the stamps (row revisions, thread stamps) those unexplained writes moved in their threads. */
  stampsOfUnexplained: Record<string, number>;
  /** The old rows the boot removed under a rule, which the census allows for. */
  recoveryRemoved: RecoveryRemoved;
  /** One message per cause: what failed and which entry would declare an intent. */
  failures: string[];
}

interface Column {
  name: string;
  blob: boolean;
}

interface Compared {
  table: string;
  columns: Column[];
  projection: string;
}

const quote = (name: string) => `"${name.replaceAll('"', '""')}"`;
const same = (column: Column, a: string, b: string) => column.blob
  ? `length(${a}.${quote(column.name)}) IS length(${b}.${quote(column.name)})`
  : `${a}.${quote(column.name)} IS ${b}.${quote(column.name)}`;

/**
 * migrateAlone copies the snapshot database next to the sandbox and runs
 * the candidate's migration chain on the copy, through the store test
 * binary `make harness-build` writes (TestMigrateHarnessDatabase), and
 * returns the copy's path. The caller removes `dir`.
 */
export async function migrateAlone(snapshotDB: string, dir: string): Promise<string> {
  await mkdir(dir, { recursive: true });
  const base = path.basename(snapshotDB);
  for (const name of await readdir(path.dirname(snapshotDB))) {
    if (name === base || name === `${base}-wal`) await copyFile(path.join(path.dirname(snapshotDB), name), path.join(dir, name));
  }
  const copy = path.join(dir, base);
  const { stdout } = await run(harnessTool('ao-store-test'),
    ['-test.run', '^TestMigrateHarnessDatabase$', '-test.count=1', '-test.timeout=0', '-test.v'], {
      env: { ...process.env, AO_TEST_MIGRATE_DATABASE: copy },
      maxBuffer: 16 * 1024 * 1024,
    });
  // The test skips, and exits 0, when it is not handed a path.
  if (!/--- PASS: TestMigrateHarnessDatabase/.test(stdout)) throw new Error(`migrating the snapshot copy did not run:\n${stdout}`);
  return copy;
}

/**
 * fingerprint compares the snapshot with the store the trial left, table
 * by table over the columns both schemas share (BLOBs by length), and
 * names every change no migrationExceptions entry for (from, to] or
 * recoveryRules entry explains. Rows the trial added are not its concern.
 * `migrated` returns a copy of the snapshot the migrations alone ran on;
 * it is called only when a row changed.
 */
export async function fingerprint(
  dbPath: string, snapshotPath: string, from: number, to: number, migrated: () => Promise<string>,
): Promise<Fingerprint> {
  const exceptions = exceptionsFor(from, to);
  const span = to <= from ? 'the store open (no migration was pending)'
    : from + 1 === to ? `migration v${to}` : `the migrations v${from + 1}..v${to}`;
  const out: Fingerprint = {
    changedRows: {}, droppedTables: [], excepted: [], recovery: [], bootUnexplained: [], stampsOfUnexplained: {},
    recoveryRemoved: { tables: {}, threadItems: {}, threads: [] }, failures: [],
  };
  const before = await tableNames(dbPath, 'snap', snapshotPath);
  const after = new Set(await tableNames(dbPath, 'main', snapshotPath));
  // Every table's columns on both sides in one read each.
  const columnsOf = async (schema: 'snap' | 'main') => {
    const rows = await dbQuery<{ tbl: string; name: string; type: string }>(dbPath,
      `SELECT m.name AS tbl, p.name AS name, p.type AS type FROM ${schema}.sqlite_master m, pragma_table_info(m.name, '${schema}') p
        WHERE m.type = 'table' AND m.sql NOT LIKE 'CREATE VIRTUAL TABLE%' ORDER BY m.name, p.cid`, snapshotPath);
    const out = new Map<string, Array<{ name: string; type: string }>>();
    for (const row of rows) out.set(row.tbl, [...(out.get(row.tbl) ?? []), row]);
    return out;
  };
  const snapColumns = await columnsOf('snap');
  const mainColumns = await columnsOf('main');
  const candidates: Compared[] = [];
  for (const table of before) {
    const forTable = exceptions.filter((entry) => entry.table === table);
    if (!after.has(table)) {
      out.droppedTables.push(table);
      if (forTable.some((entry) => entry.dropped)) {
        out.excepted.push(table);
      } else {
        out.failures.push(`${span} dropped the table ${table}. If that is the migration's intent, add a migrationExceptions entry ` +
          `{ version, table: '${table}', dropped: true, reason } to ${CONFIG_FILE}; otherwise the migration lost user data.`);
      }
      continue;
    }
    // A virtual table's rows live in its shadow tables, which tableNames folds away.
    if (table in fingerprintIgnoredTables || !snapColumns.has(table)) continue;
    const skipColumns = new Set(forTable.flatMap((entry) => entry.columns ?? []));
    const shrink = forTable.some((entry) => entry.rowsMayShrink);
    if (skipColumns.size > 0 || shrink) out.excepted.push(table);
    if (shrink) continue;
    const shared = new Set((mainColumns.get(table) ?? []).map((column) => column.name));
    const columns = snapColumns.get(table)!
      .filter((column) => shared.has(column.name) && !skipColumns.has(column.name)
        && !(`${table}.${column.name}` in fingerprintIgnoredColumns))
      .map((column) => ({ name: column.name, blob: /BLOB/i.test(column.type) }));
    if (columns.length === 0) continue;
    const projection = columns
      .map((column) => (column.blob ? `length(${quote(column.name)}) AS ${quote(column.name)}` : quote(column.name)))
      .join(', ');
    candidates.push({ table, columns, projection });
  }
  const counts = await perTable<{ n: number }>(dbPath, snapshotPath, candidates, ({ table, projection }) =>
    `(SELECT COUNT(*) FROM (SELECT ${projection} FROM snap.${quote(table)} EXCEPT SELECT ${projection} FROM main.${quote(table)})) AS n`);
  const compared: Compared[] = [];
  for (const entry of candidates) {
    const { n } = counts.get(entry.table)!;
    out.changedRows[entry.table] = n;
    if (n > 0) compared.push(entry);
  }
  if (compared.length === 0) return out;

  // Something changed: tell the migrations' changes from the boot's.
  const alone = await migrated();
  const schemas = { snap: snapshotPath, mig: alone };
  out.migrationChangedRows = {};
  const boot: Compared[] = [];
  const causes = await perTable<{ migration: number; booted: number }>(dbPath, schemas, compared, ({ table, projection }) => `
      (SELECT COUNT(*) FROM (SELECT ${projection} FROM snap.${quote(table)} EXCEPT SELECT ${projection} FROM mig.${quote(table)})) AS migration,
      (SELECT COUNT(*) FROM (SELECT ${projection} FROM snap.${quote(table)} INTERSECT SELECT ${projection} FROM mig.${quote(table)}
                             EXCEPT SELECT ${projection} FROM main.${quote(table)})) AS booted`);
  for (const entry of compared) {
    const { table } = entry;
    const { migration, booted } = causes.get(table)!;
    out.migrationChangedRows[table] = migration;
    if (migration > 0) {
      out.failures.push(`${span} left ${migration} pre-migration row(s) missing or changed in ${table} (the snapshot migrated alone shows it). ` +
        `If that is the migration's intent, add a migrationExceptions entry { version, table: '${table}', ... reason } to ${CONFIG_FILE}; ` +
        'otherwise the migration lost or altered user data.');
    }
    if (booted > 0) boot.push(entry);
  }
  if (boot.length > 0) await classifyBootChanges(dbPath, schemas, boot, out);
  return out;
}

/** perTable runs one row of `select` per table in a single read. */
async function perTable<T>(dbPath: string, attach: string | Record<string, string>, tables: Compared[],
  select: (entry: Compared) => string): Promise<Map<string, T>> {
  if (tables.length === 0) return new Map();
  const rows = await dbQuery<T & { tbl: string }>(dbPath,
    tables.map((entry) => `SELECT '${entry.table}' AS tbl, ${select(entry)}`).join(' UNION ALL '), attach);
  return new Map(rows.map((row) => [row.tbl, row]));
}

interface BootRow {
  table: string;
  key: Record<string, unknown>;
  thread: string | null;
  removed: boolean;
  /** A top-level `items` row, which the census counts per thread. */
  top: boolean;
  columns: string[];
  /** The first snapshot-scoped rule that explains it. */
  rule?: RecoveryRule;
}

/**
 * classifyBootChanges names, for every old row the migrations kept and the
 * boot changed, the recovery rule that explains it, and fails on the rest.
 * A rule's selection runs on the snapshot row; its allowed change on the
 * booted row. 'thread-write' rules run last, over the threads the boot
 * wrote rows in.
 */
async function classifyBootChanges(dbPath: string, schemas: Record<string, string>, boot: Compared[], out: Fingerprint): Promise<void> {
  const rows: BootRow[] = [];
  const unkeyed: Record<string, number> = {};
  for (const { table, columns, projection } of boot) {
    const rules = recoveryRules.filter((rule) => rule.table === table || rule.table === '*');
    const pk = (await dbQuery<{ name: string }>(dbPath,
      `SELECT name FROM pragma_table_info('${table}', 'snap') WHERE pk > 0 ORDER BY pk`, schemas)).map((row) => row.name);
    const key = pk.length > 0 ? pk : rules.find((rule) => rule.key)?.key ?? [];
    const changed = `SELECT ${projection} FROM snap.${quote(table)} INTERSECT SELECT ${projection} FROM mig.${quote(table)}
                     EXCEPT SELECT ${projection} FROM main.${quote(table)}`;
    if (key.length === 0) {
      const [{ n }] = await dbQuery<{ n: number }>(dbPath, `SELECT COUNT(*) AS n FROM (${changed})`, schemas);
      unkeyed[table] = n;
      continue;
    }
    const names = new Set(columns.map((column) => column.name));
    const thread = names.has('thread_id') ? 's."thread_id"' : table === 'threads' ? 's."id"' : 'NULL';
    const join = (a: string, b: string) => key.map((name) => `${a}.${quote(name)} IS ${b}.${quote(name)}`).join(' AND ');
    const removed = `NOT EXISTS (SELECT 1 FROM main.${quote(table)} x WHERE ${join('x', 's')})`;
    const flags = columns.map((column, i) => `NOT (${same(column, 's', 'b')}) AS c${i}`);
    const matches = rules.filter((rule) => rule.rows !== 'thread-write')
      .map((rule, i) => `(${ruleMatch(rule, columns, removed, thread)}) AS r${i}`);
    const top = table === 'items' ? `s."parent_id" = ''` : '0';
    const sql = `SELECT ${key.map((name, i) => `s.${quote(name)} AS k${i}`).join(', ')}, ${thread} AS thread,
        ${removed} AS removed, ${top} AS top, ${[...flags, ...matches].join(', ')}
      FROM (${changed}) d
      JOIN snap.${quote(table)} s ON ${join('s', 'd')}
      LEFT JOIN main.${quote(table)} b ON ${join('b', 's')}`;
    const scoped = rules.filter((rule) => rule.rows !== 'thread-write');
    for (const row of await dbQuery<Record<string, unknown>>(dbPath, sql, schemas)) {
      const isRemoved = row.removed === 1;
      rows.push({
        table,
        key: Object.fromEntries(key.map((name, i) => [name, row[`k${i}`]])),
        thread: (row.thread as string | null) ?? null,
        removed: isRemoved,
        top: row.top === 1,
        columns: isRemoved ? [] : columns.filter((_, i) => row[`c${i}`] === 1).map((column) => column.name),
        rule: scoped.find((_, i) => row[`r${i}`] === 1),
      });
    }
  }

  // The threads the boot wrote in, for the stamps and cards that follow a
  // write. A write a rule explains, or a row the boot added, explains them;
  // the stamps of a write no rule explains are named with that write.
  const isWrite = (row: BootRow) => row.table === 'items' && (row.removed || row.columns.some((c) => c !== 'rev'));
  const explainedWrites = new Set(rows.filter((row) => isWrite(row) && row.rule).map((row) => row.thread!));
  const unexplainedWrites = new Set(rows.filter((row) => isWrite(row) && !row.rule).map((row) => row.thread!));
  const pending = rows.filter((row) => !row.rule && row.thread && !row.removed
    && recoveryRules.some((rule) => rule.table === row.table && rule.rows === 'thread-write'));
  if (pending.some((row) => !explainedWrites.has(row.thread!))) {
    for (const { thread } of await dbQuery<{ thread: string }>(dbPath,
      `SELECT DISTINCT b.thread_id AS thread FROM main.items b
        WHERE NOT EXISTS (SELECT 1 FROM mig.items m WHERE m.thread_id = b.thread_id AND m.id = b.id)`, schemas)) {
      explainedWrites.add(thread);
    }
  }
  const threadWrite = (row: BootRow, writes: Set<string>) => recoveryRules.find((rule) => rule.table === row.table
    && rule.rows === 'thread-write' && writes.has(row.thread!)
    && (rule.columns?.includes('*') || row.columns.every((column) => rule.columns?.includes(column))));
  for (const row of pending.filter((r) => r.table === 'subagent_aggregates')) row.rule = threadWrite(row, explainedWrites);
  const cardWrites = rows.filter((row) => row.table === 'subagent_aggregates' && row.rule).map((row) => row.thread!);
  const stampWrites = new Set([...explainedWrites, ...cardWrites]);
  const followers: BootRow[] = [];
  for (const row of pending.filter((r) => r.table !== 'subagent_aggregates')) {
    row.rule = threadWrite(row, stampWrites);
    if (!row.rule && threadWrite(row, unexplainedWrites)) followers.push(row);
  }

  // By rule, then table: a '*' rule's count is one entry per table.
  const counts = new Map<RecoveryRule, Map<string, number>>();
  const removed = out.recoveryRemoved;
  for (const row of rows) {
    if (!row.rule) continue;
    const byTable = counts.get(row.rule) ?? new Map<string, number>();
    counts.set(row.rule, byTable.set(row.table, (byTable.get(row.table) ?? 0) + 1));
    if (!row.removed) continue;
    removed.tables[row.table] = (removed.tables[row.table] ?? 0) + 1;
    if (row.top) removed.threadItems[row.thread!] = (removed.threadItems[row.thread!] ?? 0) + 1;
    if (row.table === 'threads') removed.threads.push(row.thread!);
  }
  out.recovery = recoveryRules.filter((rule) => counts.has(rule)).flatMap((rule) => [...counts.get(rule)!.entries()]
    .map(([table, n]) => ({ rule: rule.name, writer: rule.writer, table, rows: n })));
  out.stampsOfUnexplained = Object.fromEntries([...new Set(followers.map((row) => row.table))]
    .map((table) => [table, followers.filter((row) => row.table === table).length]));

  const unexplained = rows.filter((row) => !row.rule && !followers.includes(row));
  const byTable = new Map<string, BootRow[]>();
  for (const row of unexplained) byTable.set(row.table, [...(byTable.get(row.table) ?? []), row]);
  out.bootUnexplained = [...byTable.values()].flatMap((list) => list.slice(0, 50))
    .map(({ table, key, removed, columns }) => ({ table, key, removed, columns }));
  const parts = [...byTable.entries()].map(([table, list]) => {
    const removed = list.filter((row) => row.removed).length;
    const shown = list.slice(0, 10).map((row) => `${Object.values(row.key).join('/')} ${row.removed ? 'removed' : `(${row.columns.join(', ')})`}`);
    return `${table}: ${removed} removed, ${list.length - removed} changed [${shown.join('; ')}${list.length > 10 ? '; ...' : ''}]`;
  });
  parts.push(...Object.entries(unkeyed).map(([table, n]) => `${table}: ${n} removed or changed (the table has no key to name them by)`));
  if (parts.length > 0) {
    const stamps = Object.entries(out.stampsOfUnexplained).map(([table, n]) => `${n} in ${table}`).join(', ');
    out.failures.push(`the candidate's boot, after the migrations, removed or changed rows the previous process had settled, which no ` +
      `recoveryRules entry explains: ${parts.join('; ')}${stamps ? ` (the stamps those writes moved: ${stamps})` : ''}. A boot may change only what the previous process left live. If a boot writer ` +
      `settles live state no entry selects, add a recoveryRules entry naming it to ${CONFIG_FILE}; otherwise the boot lost or altered user data.`);
  }
}

/**
 * ruleMatch is the SQL for "rule explains this change": the snapshot row
 * is one it selects, and the booted row is gone when it removes rows or
 * differs only in its columns and meta keys, and meets its `to`.
 */
function ruleMatch(rule: RecoveryRule, columns: Column[], removed: string, thread: string): string {
  if (rule.table === '*') return `${thread} IN (${rule.rows}) AND ${removed}`;
  const any = rule.columns?.includes('*') ?? false;
  const allowed = new Set([...(rule.columns ?? []), ...(rule.metaKeys ? ['meta'] : [])]);
  const kept = any ? [] : columns.filter((column) => !allowed.has(column.name)).map((column) => same(column, 's', 'b'));
  if (rule.metaKeys) {
    const keys = rule.metaKeys.map((key) => `'$.${key}'`).join(', ');
    kept.push(`(CASE WHEN json_valid(s.meta) AND json_valid(b.meta) THEN json_remove(s.meta, ${keys}) IS json_remove(b.meta, ${keys})
      ELSE s.meta IS b.meta END)`);
  }
  if (rule.to) kept.push(`(${rule.to})`);
  const change = kept.length > 0 ? kept.join(' AND ') : '1';
  return `(${rule.rows}) AND CASE WHEN ${removed} THEN ${rule.removes ? 1 : 0} ELSE ${rule.removes && !rule.columns ? 0 : `(${change})`} END`;
}
