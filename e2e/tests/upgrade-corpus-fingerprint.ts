// The release upgrade check's fingerprint (upgrade-corpus.manual.spec.ts,
// docs/architecture/release-upgrade-check.md): every row of the launcher's
// pre-migration snapshot must still be in the store the update trial left,
// unchanged on the columns both schemas share, unless a migration exception
// or a boot recovery rule (upgrade-corpus-config.ts) explains the change.
//
// The trial both migrates the store and boots on it, and a boot settles
// what a dead process left live. The two causes are told apart by
// migrating a copy of the snapshot alone (M): the snapshot (S) against M is
// the migrations' doing, held to migrationExceptions; M against the trial's
// store (B), over every shared column, is the boot's, held to
// recoveryRules. An exception narrows only the migrations' comparison, so a
// column a migration may rewrite is still compared for the boot. A rule
// selects over M, the store the boot's writers read, at the build's schema
// whatever the snapshot's version. The copy
// is made only when some row changed. Row content is never reported: the
// queries return keys, column names and flags, and the one composition done
// outside SQLite (restoredDraftThreads) keeps the text it reads in memory.
import { execFile } from 'node:child_process';
import { copyFile, mkdir, readdir, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { promisify } from 'node:util';
import {
  fingerprintIgnoredColumns, fingerprintIgnoredTables, recoveryRules, sendConfirmed, sendRecorded, unconfirmedUserRow, type RecoveryRule,
} from './upgrade-corpus-config.js';
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

/** Rows a rule removed: by table, top-level items by thread, and payload bytes. */
export interface RecoveryRemoved {
  tables: Record<string, number>;
  threadItems: Record<string, number>;
  bytes: number;
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
  /** Table to the rows of migrationChangedRows whose change a migrationExceptions entry declares. */
  migrationExcepted?: Record<string, number>;
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
  /** Every column both schemas share, less fingerprintIgnoredColumns. */
  columns: Column[];
  projection: string;
  /** The projection less the columns a migrationExceptions entry lets the migrations rewrite. */
  migrationProjection: string;
  /** The primary key, or a rule's declared key; empty when neither exists. */
  key: string[];
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
  await migrate(copy);
  return copy;
}

/**
 * buildSchema is the candidate's schema version: the migration chain run on
 * an empty database in `dir`, which the caller removes. The check reads it
 * before any boot touches the root, to know whether a gated boot would
 * migrate or run straight through.
 */
export async function buildSchema(dir: string): Promise<number> {
  await mkdir(dir, { recursive: true });
  const db = path.join(dir, 'schema.db');
  await writeFile(db, '');
  await migrate(db);
  const [{ version }] = await dbQuery<{ version: number }>(db, 'SELECT MAX(version) AS version FROM migration_versions');
  return version;
}

/** storeAt writes a new, empty store at `db` at schema `version`, for a test of a root older than the build. */
export async function storeAt(db: string, version: number): Promise<void> {
  await writeFile(db, '', { flag: 'wx' });
  await migrate(db, version);
}

async function migrate(db: string, through?: number): Promise<void> {
  const { stdout } = await run(harnessTool('ao-store-test'),
    ['-test.run', '^TestMigrateHarnessDatabase$', '-test.count=1', '-test.timeout=0', '-test.v'], {
      env: { ...process.env, AO_TEST_MIGRATE_DATABASE: db, ...(through ? { AO_TEST_MIGRATE_THROUGH: String(through) } : {}) },
      maxBuffer: 16 * 1024 * 1024,
    });
  // The test skips, and exits 0, when it is not handed a path.
  if (!/--- PASS: TestMigrateHarnessDatabase/.test(stdout)) throw new Error(`migrating ${db} did not run:\n${stdout}`);
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
    recoveryRemoved: { tables: {}, threadItems: {}, bytes: 0 }, failures: [],
  };
  const before = await tableNames(dbPath, 'snap', snapshotPath);
  const after = new Set(await tableNames(dbPath, 'main', snapshotPath));
  // Every table's columns on both sides in one read each.
  const columnsOf = async (schema: 'snap' | 'main') => {
    const rows = await dbQuery<{ tbl: string; name: string; type: string; pk: number }>(dbPath,
      `SELECT m.name AS tbl, p.name AS name, p.type AS type, p.pk AS pk FROM ${schema}.sqlite_master m, pragma_table_info(m.name, '${schema}') p
        WHERE m.type = 'table' AND m.sql NOT LIKE 'CREATE VIRTUAL TABLE%' ORDER BY m.name, p.cid`, snapshotPath);
    const out = new Map<string, Array<{ name: string; type: string; pk: number }>>();
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
      .filter((column) => shared.has(column.name) && !(`${table}.${column.name}` in fingerprintIgnoredColumns))
      .map((column) => ({ name: column.name, blob: /BLOB/i.test(column.type) }));
    if (columns.length === 0) continue;
    const project = (list: Column[]) => list
      .map((column) => (column.blob ? `length(${quote(column.name)}) AS ${quote(column.name)}` : quote(column.name)))
      .join(', ');
    const pk = snapColumns.get(table)!.filter((column) => column.pk > 0).sort((a, b) => a.pk - b.pk).map((column) => column.name);
    const key = pk.length > 0 ? pk : recoveryRules.find((rule) => rule.table === table && rule.key)?.key ?? [];
    // The key stays in the migrations' comparison, so a row they removed still shows.
    const migrationColumns = columns.filter((column) => !skipColumns.has(column.name) || key.includes(column.name));
    candidates.push({
      table, columns, projection: project(columns), key,
      migrationProjection: migrationColumns.length > 0 ? project(migrationColumns) : '1',
    });
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
  out.migrationExcepted = {};
  const boot: Compared[] = [];
  const causes = await perTable<{ all: number; migration: number; booted: number }>(dbPath, schemas, compared, (entry) => {
    const { table, projection, migrationProjection } = entry;
    return `
      (SELECT COUNT(*) FROM (SELECT ${projection} FROM snap.${quote(table)} EXCEPT SELECT ${projection} FROM mig.${quote(table)})) AS "all",
      (SELECT COUNT(*) FROM (SELECT ${migrationProjection} FROM snap.${quote(table)}
                             EXCEPT SELECT ${migrationProjection} FROM mig.${quote(table)})) AS migration,
      (SELECT COUNT(*) FROM (${bootChanged(entry)})) AS booted`;
  });
  for (const entry of compared) {
    const { table } = entry;
    const { all, migration, booted } = causes.get(table)!;
    out.migrationChangedRows[table] = all;
    if (all > migration) out.migrationExcepted[table] = all - migration;
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

/**
 * bootChanged selects, over the full projection, the old rows the boot
 * changed or removed: the snapshot's rows as the migrations alone left them,
 * less the trial's store. A table with no key to join by falls back to the
 * rows the migrations did not touch.
 */
function bootChanged({ table, projection, key }: Compared): string {
  if (key.length === 0) {
    return `SELECT ${projection} FROM snap.${quote(table)} INTERSECT SELECT ${projection} FROM mig.${quote(table)}
            EXCEPT SELECT ${projection} FROM main.${quote(table)}`;
  }
  const old = key.map((name) => `o.${quote(name)} IS ${quote(table)}.${quote(name)}`).join(' AND ');
  return `SELECT ${projection} FROM mig.${quote(table)} WHERE EXISTS (SELECT 1 FROM snap.${quote(table)} o WHERE ${old})
          EXCEPT SELECT ${projection} FROM main.${quote(table)}`;
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
  /** The payload bytes a removed `payloads` or `payload_chunks` row held. */
  bytes: number;
  columns: string[];
  /** The first snapshot-scoped rule that explains it. */
  rule?: RecoveryRule;
  /** The first snapshot-scoped rule that explains it once its thread's stamps may move too. */
  stamped?: RecoveryRule;
}

/**
 * classifyBootChanges names, for every old row the migrations kept and the
 * boot changed, the recovery rule that explains it, and fails on the rest.
 * A rule's selection runs on the row the migrations left (`s`, in M); its
 * allowed change on the booted row. 'thread-write' rules run last, over the threads the boot
 * wrote rows in.
 */
async function classifyBootChanges(dbPath: string, schemas: Record<string, string>, boot: Compared[], out: Fingerprint): Promise<void> {
  const restored = await restoredDraftThreads(dbPath, schemas);
  const rows: BootRow[] = [];
  const unkeyed: Record<string, number> = {};
  for (const entry of boot) {
    const { table, columns, key } = entry;
    const rules = recoveryRules.filter((rule) => rule.table === table);
    const changed = bootChanged(entry);
    if (key.length === 0) {
      const [{ n }] = await dbQuery<{ n: number }>(dbPath, `SELECT COUNT(*) AS n FROM (${changed})`, schemas);
      unkeyed[table] = n;
      continue;
    }
    const names = new Set(columns.map((column) => column.name));
    const thread = names.has('thread_id') ? 's."thread_id"' : table === 'threads' ? 's."id"' : 'NULL';
    const join = (a: string, b: string) => key.map((name) => `${a}.${quote(name)} IS ${b}.${quote(name)}`).join(' AND ');
    const removed = `NOT EXISTS (SELECT 1 FROM main.${quote(table)} x WHERE ${join('x', 's')})`;
    // What the boot changed: the row as the migrations left it (s) against the booted row (b).
    const flags = columns.map((column, i) => `NOT (${same(column, 's', 'b')}) AS c${i}`);
    const scoped = rules.filter((rule) => rule.rows !== 'thread-write');
    // A row one rule settles in a thread the boot also wrote in carries that
    // write's stamps as well: the rule may explain its columns plus them.
    const stamps = rules.filter((rule) => rule.rows === 'thread-write' && !rule.columns?.includes('*'))
      .flatMap((rule) => rule.columns ?? []);
    const matches = scoped.flatMap((rule, i) => [
      `(${ruleMatch(rule, columns, removed, restored)}) AS r${i}`,
      `(${stamps.length > 0 && rule.columns && !rule.columns.includes('*')
        ? ruleMatch({ ...rule, columns: [...rule.columns, ...stamps] }, columns, removed, restored) : '0'}) AS w${i}`,
    ]);
    const top = table === 'items' ? `s."parent_id" = ''` : '0';
    const bytes = table === 'payloads' || table === 'payload_chunks' ? 'length(s."data")' : '0';
    const sql = `SELECT ${key.map((name, i) => `s.${quote(name)} AS k${i}`).join(', ')}, ${thread} AS thread,
        ${removed} AS removed, ${top} AS top, ${bytes} AS bytes, ${[...flags, ...matches].join(', ')}
      FROM (${changed}) d
      JOIN mig.${quote(table)} s ON ${join('s', 'd')}
      LEFT JOIN main.${quote(table)} b ON ${join('b', 's')}`;
    for (const row of await dbQuery<Record<string, unknown>>(dbPath, sql, schemas)) {
      const isRemoved = row.removed === 1;
      rows.push({
        table,
        key: Object.fromEntries(key.map((name, i) => [name, row[`k${i}`]])),
        thread: (row.thread as string | null) ?? null,
        removed: isRemoved,
        top: row.top === 1,
        bytes: Number(row.bytes ?? 0),
        columns: isRemoved ? [] : columns.filter((_, i) => row[`c${i}`] === 1).map((column) => column.name),
        rule: scoped.find((_, i) => row[`r${i}`] === 1),
        stamped: scoped.find((_, i) => row[`w${i}`] === 1),
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
    row.rule = threadWrite(row, stampWrites) ?? (stampWrites.has(row.thread!) ? row.stamped : undefined);
    if (!row.rule && threadWrite(row, unexplainedWrites)) followers.push(row);
  }

  const counts = new Map<RecoveryRule, number>();
  const removed = out.recoveryRemoved;
  for (const row of rows) {
    if (!row.rule) continue;
    counts.set(row.rule, (counts.get(row.rule) ?? 0) + 1);
    if (!row.removed) continue;
    removed.tables[row.table] = (removed.tables[row.table] ?? 0) + 1;
    if (row.top) removed.threadItems[row.thread!] = (removed.threadItems[row.thread!] ?? 0) + 1;
    removed.bytes += row.bytes;
  }
  out.recovery = recoveryRules.filter((rule) => counts.has(rule))
    .map((rule) => ({ rule: rule.name, writer: rule.writer, table: rule.table, rows: counts.get(rule)! }));
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
 * ruleMatch is the SQL for "rule explains this change": the migrated row
 * `s` is one it selects, and the booted row `b` is gone when it removes
 * rows, or differs from `s` only in its columns and meta keys, and meets
 * its `to`. Every rule is composed here, and none may read the snapshot:
 * a snapshot older than a column a rule names would abort the check.
 * `{restored}` in a rule is the threads restoredDraftThreads matched.
 */
function ruleMatch(rule: RecoveryRule, columns: Column[], removed: string, restored: string[]): string {
  if (/\bsnap\./.test(`${rule.rows} ${rule.to ?? ''}`)) {
    throw new Error(`recovery rule ${rule.name} reads the snapshot (snap.); a rule selects over the migrated store (mig.), whose schema is the build's`);
  }
  const threads = restored.length > 0 ? restored.map((id) => `'${id.replaceAll("'", "''")}'`).join(', ') : 'NULL';
  rule = { ...rule, rows: rule.rows.replaceAll('{restored}', threads), to: rule.to?.replaceAll('{restored}', threads) };
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

/** The characters Go's unicode.IsSpace accepts, which strings.TrimSpace strips. */
const GO_SPACE = '\\t\\n\\v\\f\\r \\u0085\\u00A0\\u1680\\u2000-\\u200A\\u2028\\u2029\\u202F\\u205F\\u3000';
const goTrimPattern = new RegExp(`^[${GO_SPACE}]+|[${GO_SPACE}]+$`, 'g');
/** strings.TrimSpace. */
const trimSpace = (text: string) => text.replace(goTrimPattern, '');

/** A composerdraft.Part: text, attachment ids, and whether it carries a source plan. */
interface DraftPart { content: string; attachmentIds: string[]; plan: boolean }
interface Draft { content: string; attachments: string; terminalChips: string }

/** A JSON object, or null when `raw` is not one (Go's decode into a struct then fails). */
function jsonObject(raw: string | null): Record<string, unknown> | null | 'invalid' {
  if (raw === null || trimSpace(raw) === '') return null;
  try {
    const value: unknown = JSON.parse(raw);
    if (value === null) return null;
    return typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : 'invalid';
  } catch {
    return 'invalid';
  }
}

/** A JSON []string field: the strings, [] for null or absent, or 'invalid' for any other type. */
function stringList(value: unknown): string[] | 'invalid' {
  if (value === undefined || value === null) return [];
  return Array.isArray(value) && value.every((v) => typeof v === 'string') ? value : 'invalid';
}

/** draftPartFromUserItem: the row's text without the review comments it rendered, and its attachment ids; the summary alone when the meta does not decode. */
function userRowPart(summary: string, metaJSON: string): DraftPart {
  const meta = jsonObject(metaJSON);
  if (meta === 'invalid') return { content: summary, attachmentIds: [], plan: false };
  const attachments = meta?.attachments;
  const before = meta?.textBeforeReviewComments;
  if ((attachments !== undefined && attachments !== null && !Array.isArray(attachments))
    || (before !== undefined && before !== null && typeof before !== 'string')) {
    return { content: summary, attachmentIds: [], plan: false };
  }
  const ids: string[] = [];
  for (const attachment of (attachments as unknown[] | null | undefined) ?? []) {
    if (attachment === null) continue;
    if (typeof attachment !== 'object' || Array.isArray(attachment)) return { content: summary, attachmentIds: [], plan: false };
    const id = (attachment as Record<string, unknown>).id;
    if (id !== undefined && id !== null && typeof id !== 'string') return { content: summary, attachmentIds: [], plan: false };
    if (typeof id === 'string' && trimSpace(id) !== '') ids.push(trimSpace(id));
  }
  return { content: typeof before === 'string' ? before : summary, attachmentIds: ids, plan: meta?.sourceProposedPlan != null };
}

/** draftPartFromQueueRow: the queued text, and its payload's attachment ids when the payload decodes. */
function queueRowPart(message: string, payloadJSON: string | null): DraftPart {
  const payload = jsonObject(payloadJSON);
  const ids = payload === 'invalid' ? 'invalid' : stringList(payload?.attachmentIds);
  if (payload === 'invalid' || ids === 'invalid') return { content: message, attachmentIds: [], plan: false };
  return { content: message, attachmentIds: ids, plan: payload?.sourceProposedPlan != null };
}

/** userRowSendIDs: a joined row's members, or its one send id. */
function userRowSends(metaJSON: string): string[] {
  const meta = jsonObject(metaJSON);
  if (meta === null || meta === 'invalid') return [];
  const joined = stringList(meta.joinedSendIds);
  if (joined !== 'invalid' && joined.length > 0) return joined;
  return typeof meta.sendId === 'string' && meta.sendId !== '' ? [meta.sendId] : [];
}

/** emptyDraftPart. */
const emptyPart = (part: DraftPart) => trimSpace(part.content) === '' && part.attachmentIds.length === 0 && !part.plan;

/**
 * composerdraft.MergeParts over content, attachments and terminal chips:
 * each part's text trimmed, the empty ones skipped, then the draft's own,
 * joined by a blank line; attachment ids trimmed and kept once, the parts'
 * first; an empty chips value becomes '[]'. Throws where it errors (the
 * draft's attachments do not decode), so the restore writes nothing.
 */
function mergeParts(current: Draft, parts: DraftPart[]): Draft {
  const own = trimSpace(current.attachments) === '' ? [] : stringList(JSON.parse(current.attachments));
  if (own === 'invalid') throw new Error('draft attachments do not decode');
  const content = [...parts.map((part) => trimSpace(part.content)), trimSpace(current.content)].filter((text) => text !== '');
  const ids: string[] = [];
  for (const id of [...parts.flatMap((part) => part.attachmentIds), ...own].map(trimSpace)) {
    if (id !== '' && !ids.includes(id)) ids.push(id);
  }
  return {
    content: content.join('\n\n'),
    attachments: JSON.stringify(ids),
    terminalChips: trimSpace(current.terminalChips) === '' ? '[]' : current.terminalChips,
  };
}

/**
 * restoredDraftThreads lists the threads whose booted draft is exactly what
 * the boot's composer restores write from the store the migrations left,
 * for the `{restored}` of the draft rules. It mirrors, in the boot's order,
 * app.(*App).restoreReplacementDraftsAtBoot (recoverReplacementDraft, for a
 * recovery whose send findRecordedSend does not find) and
 * app.(*App).restoreDurableFlushQueueAtBoot (restoreThreadQueueAtBoot: the
 * unconfirmed rows first, whole, in timeline order, by draftPartFromUserItem;
 * then the queue rows in queue order by draftPartFromQueueRow, less the
 * copies of a send an unconfirmed row carries and the messages
 * queuedSendDelivered calls delivered), each written through
 * composerdraft.MergeParts. Content, attachments and terminal chips are
 * compared exactly. A meta or payload field of the wrong type other than
 * the ones a part reads is not mirrored: Go's decode would fail and take
 * the text alone. Row text is read into memory to compose and compare and
 * is never reported.
 */
async function restoredDraftThreads(dbPath: string, schemas: Record<string, string>): Promise<string[]> {
  const recoveries = await dbQuery<{ thread: string; content: string; attachments: string; recorded: number }>(dbPath,
    `SELECT r.thread_id AS thread, r.content, r.attachments, ${sendRecorded('r.thread_id', 'r.send_id')} AS recorded
       FROM mig.thread_draft_recoveries r ORDER BY r.rowid`, schemas);
  const unconfirmed = await dbQuery<{ thread: string; summary: string; meta: string }>(dbPath,
    `SELECT u.thread_id AS thread, u.summary, u.meta FROM mig.items u WHERE ${unconfirmedUserRow('u')}
      ORDER BY u.thread_id, u.turn_index, u.item_index`, schemas);
  const queued = await dbQuery<{ thread: string; send: string; message: string; payload: string | null; confirmed: number }>(dbPath,
    `SELECT q.thread_id AS thread, q.send_id AS send, q.message, CAST(q.payload AS TEXT) AS payload,
            ${sendConfirmed('q.thread_id', 'q.send_id')} AS confirmed
       FROM mig.flush_queue_items q ORDER BY q.thread_id, q.enqueued_at, q.rowid`, schemas);
  const threads = [...new Set([...recoveries, ...unconfirmed, ...queued].map((row) => row.thread))];
  if (threads.length === 0) return [];
  const drafts = await dbQuery<{ store: string; thread: string; content: string; attachments: string; chips: string }>(dbPath,
    ['mig', 'main'].map((store) => `SELECT '${store}' AS store, thread_id AS thread, content, attachments, terminal_chips AS chips
       FROM ${store}.thread_drafts WHERE thread_id IN (${threads.map((t) => `'${t.replaceAll("'", "''")}'`).join(', ')})`).join(' UNION ALL '),
    schemas);
  const draftOf = (store: string, thread: string): Draft | null => {
    const row = drafts.find((d) => d.store === store && d.thread === thread);
    return row ? { content: row.content, attachments: row.attachments, terminalChips: row.chips } : null;
  };
  const restored: string[] = [];
  for (const thread of threads) {
    let draft = draftOf('mig', thread);
    let wrote = false;
    try {
      const empty: Draft = { content: '', attachments: '', terminalChips: '' };
      for (const recovery of recoveries.filter((r) => r.thread === thread && !r.recorded)) {
        const ids = stringList(JSON.parse(recovery.attachments));
        if (ids === 'invalid') throw new Error('recovery attachments do not decode');
        draft = mergeParts(draft ?? empty, [{ content: recovery.content, attachmentIds: ids, plan: false }]);
        wrote = true;
      }
      const parts: DraftPart[] = [];
      const carried = new Set<string>();
      for (const row of unconfirmed.filter((r) => r.thread === thread)) {
        for (const send of userRowSends(row.meta)) carried.add(send);
        const part = userRowPart(row.summary, row.meta);
        if (!emptyPart(part)) parts.push(part);
      }
      for (const row of queued.filter((r) => r.thread === thread)) {
        if ((row.send !== '' && carried.has(row.send)) || row.confirmed) continue;
        const part = queueRowPart(row.message, row.payload);
        if (!emptyPart(part)) parts.push(part);
      }
      if (parts.length > 0) {
        draft = mergeParts(draft ?? empty, parts);
        wrote = true;
      }
    } catch {
      // The Go restore fails here and writes nothing: no row of the thread may leave.
      continue;
    }
    const booted = draftOf('main', thread);
    const same = (a: Draft | null, b: Draft | null) => a === b || (a !== null && b !== null && a.content === b.content
      && a.terminalChips === b.terminalChips && sameIds(a.attachments, b.attachments));
    if (wrote ? same(draft, booted) : same(draftOf('mig', thread), booted)) restored.push(thread);
  }
  return restored;
}

/** Two attachments values name the same ids in the same order (Go and JSON.stringify escape differently). */
function sameIds(a: string, b: string): boolean {
  try {
    return JSON.stringify(JSON.parse(a)) === JSON.stringify(JSON.parse(b));
  } catch {
    return a === b;
  }
}
