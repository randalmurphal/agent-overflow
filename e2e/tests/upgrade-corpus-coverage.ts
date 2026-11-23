// Schema coverage: what a store holds against what its schema can hold.
// readSchema lists every table, column and enumerated CHECK value from
// sqlite_master; measureCoverage counts, per table in one query, the rows,
// the rows setting each column to a non-default value, and the rows
// carrying each CHECK value; assertCoverage holds the result to the
// allowlist and fails naming what to write or declare.
import { excludedScenarios, topUpScenarios, type CoverageAllowance } from './upgrade-corpus-config.js';
import { dbQuery } from './upgrade-corpus-tools.js';

const CONFIG_FILE = 'e2e/tests/upgrade-corpus-config.ts';
const TOPUP_FILE = 'e2e/tests/upgrade-corpus-topup.ts';

export interface SchemaColumn {
  name: string;
  /** The default as SQL text (pragma_table_info.dflt_value), null when the column has none. */
  dflt: string | null;
  /** The column's position in the primary key, from 1; 0 when it is not in it. */
  pk: number;
}

export interface SchemaTable {
  name: string;
  /** A virtual (FTS) table: only its row count is measured. */
  virtual: boolean;
  columns: SchemaColumn[];
  /** Column to the values its CHECK (... IN (...)) constraint enumerates. */
  checks: Record<string, Array<string | number>>;
}

/** readSchema lists the tables of `dbPath`, FTS shadow tables folded under their virtual table. */
export async function readSchema(dbPath: string): Promise<SchemaTable[]> {
  const tables = await dbQuery<{ name: string; sql: string }>(dbPath,
    `SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`);
  const virtual = tables.filter((row) => /^CREATE VIRTUAL TABLE/i.test(row.sql)).map((row) => row.name);
  const columns = await dbQuery<{ table: string; column: string; dflt: string | null; pk: number }>(dbPath, `
    SELECT m.name AS "table", p.name AS "column", p.dflt_value AS dflt, p.pk AS pk
      FROM sqlite_master m JOIN pragma_table_info(m.name) p
     WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
     ORDER BY m.name, p.cid`);
  return tables
    .filter((row) => !virtual.some((v) => row.name.startsWith(`${v}_`)))
    .map((row) => {
      const own = columns.filter((c) => c.table === row.name).map((c) => ({ name: c.column, dflt: c.dflt, pk: c.pk }));
      const isVirtual = virtual.includes(row.name);
      return { name: row.name, virtual: isVirtual, columns: isVirtual ? [] : own, checks: enumeratedChecks(row.sql, own.map((c) => c.name)) };
    });
}

/** The `column IN (v, ...)` enumerations of a CREATE TABLE statement, by column. */
function enumeratedChecks(sql: string, columns: string[]): Record<string, Array<string | number>> {
  const checks: Record<string, Array<string | number>> = {};
  for (const match of sql.matchAll(/\b"?([A-Za-z_][A-Za-z0-9_]*)"?\s+IN\s*\(([^()]*)\)/g)) {
    const [, column, list] = match;
    if (!columns.includes(column)) continue;
    const values = list.split(',').map((raw) => raw.trim()).filter(Boolean).map((raw) =>
      /^'.*'$/.test(raw) ? raw.slice(1, -1).replace(/''/g, "'") : Number(raw));
    checks[column] = [...new Set([...(checks[column] ?? []), ...values])];
  }
  return checks;
}

export interface Coverage {
  /** Table to its row count. */
  rows: Record<string, number>;
  /** `table.column` to the rows setting it to a non-default, non-empty value. */
  columns: Record<string, number>;
  /** `table.column=value` to the rows carrying that CHECK value. */
  values: Record<string, number>;
  /**
   * An allowlisted table, `table.column` or `table.column=value` the store
   * writes after all, to the first rows that carry it, read with its count,
   * so the failure names them.
   */
  carriers: Record<string, string[]>;
}

/**
 * measureCoverage counts what `dbPath` holds of `schema`, one statement per
 * table, which also reads the first rows that carry each table, column or
 * value of `allowlist` it counts: one statement reads one snapshot, so a
 * row that settles while the store is measured cannot leave a count with no
 * row to name.
 */
export async function measureCoverage(dbPath: string, schema: SchemaTable[], allowlist: CoverageAllowance[]): Promise<Coverage> {
  const coverage: Coverage = { rows: {}, columns: {}, values: {}, carriers: {} };
  for (const table of schema) {
    const terms = [`COUNT(*) AS "rows"`];
    for (const column of table.columns) {
      const q = `"${column.name}"`;
      terms.push(`COALESCE(SUM(${nonDefaultSQL(column)}), 0) AS "c:${column.name}"`);
      for (const value of table.checks[column.name] ?? []) {
        terms.push(`COALESCE(SUM(${q} = ${sqlLiteral(value)}), 0) AS "v:${column.name}=${value}"`);
      }
    }
    for (const [key, where] of allowedHere(table, allowlist)) {
      terms.push(`CASE WHEN COALESCE(SUM(${where}), 0) > 0 THEN (${carriersSQL(table, where)}) END AS "w:${key}"`);
    }
    const [row] = await dbQuery<Record<string, number | string | null>>(dbPath, `SELECT ${terms.join(', ')} FROM "${table.name}"`);
    coverage.rows[table.name] = row.rows as number;
    for (const [key, n] of Object.entries(row)) {
      if (key.startsWith('c:')) coverage.columns[`${table.name}.${key.slice(2)}`] = n as number;
      else if (key.startsWith('v:')) coverage.values[`${table.name}.${key.slice(2)}`] = n as number;
      else if (key.startsWith('w:') && typeof n === 'string') coverage.carriers[key.slice(2)] = describeCarriers(n);
    }
  }
  return coverage;
}

/** The keys `allowlist` declares on `table` that the schema has, each with the SQL that holds on a row carrying it. */
function allowedHere(table: SchemaTable, allowlist: CoverageAllowance[]): Array<[string, string]> {
  const out: Array<[string, string]> = [];
  for (const entry of allowlist) {
    if (entry.table !== table.name) continue;
    if (!entry.columns && !entry.values) out.push([table.name, '1']);
    for (const name of entry.columns ?? []) {
      const column = table.columns.find((c) => c.name === name);
      if (column) out.push([`${table.name}.${name}`, nonDefaultSQL(column)]);
    }
    for (const [name, list] of Object.entries(entry.values ?? {})) {
      for (const value of list) {
        if ((table.checks[name] ?? []).includes(value)) out.push([`${table.name}.${name}=${value}`, `"${name}" = ${sqlLiteral(value)}`]);
      }
    }
  }
  return out;
}

/** The SQL that holds where `column` is set to a non-default, non-empty value. */
function nonDefaultSQL(column: SchemaColumn): string {
  const q = `"${column.name}"`;
  return column.dflt === null
    ? `${q} IS NOT NULL AND ${q} != ''`
    : `${q} IS NOT NULL AND ${q} IS NOT (${column.dflt}) AND ${q} != ''`;
}

/**
 * A subquery answering, as a JSON array, the first rows of `table` where
 * `where` holds, each by its primary key and, when it has one, its
 * thread's title.
 */
function carriersSQL(table: SchemaTable, where: string): string {
  const key = table.columns.filter((c) => c.pk > 0).sort((a, b) => a.pk - b.pk).map((c) => c.name);
  const fields = (key.length > 0 ? key : ['rowid']).map((c) => `'${c}', t."${c}"`);
  if (table.columns.some((c) => c.name === 'thread_id') && table.name !== 'threads') {
    fields.push(`'thread title', (SELECT title FROM threads WHERE id = t.thread_id)`);
  }
  return `SELECT json_group_array(w) FROM (SELECT json_object(${fields.join(', ')}) AS w FROM "${table.name}" AS t WHERE ${where} LIMIT 3)`;
}

/** The rows a carriersSQL answer names, one line each. */
function describeCarriers(json: string): string[] {
  return (JSON.parse(json) as Array<string | Record<string, unknown>>).map((row) => {
    const fields = typeof row === 'string' ? (JSON.parse(row) as Record<string, unknown>) : row;
    return Object.entries(fields).map(([column, value]) => `${column} ${JSON.stringify(value)}`).join(', ');
  });
}

function sqlLiteral(value: string | number): string {
  return typeof value === 'number' ? String(value) : `'${value.replace(/'/g, "''")}'`;
}

export interface CoverageReport {
  /** What the store never writes and no entry declares. */
  uncovered: string[];
  /** Entries the store now covers or the schema no longer has. */
  stale: string[];
}

/**
 * assertCoverage holds `coverage` to the allowlist: every table, column
 * and CHECK value is written or declared, and every declaration is still
 * true. Throws with both lists and the instruction each needs.
 */
export function assertCoverage(schema: SchemaTable[], coverage: Coverage, allowlist: CoverageAllowance[]): CoverageReport {
  const byTable = new Map(schema.map((table) => [table.name, table]));
  const wholeTables = new Set<string>();
  const columns = new Set<string>();
  const values = new Set<string>();
  const stale: string[] = [];
  for (const entry of allowlist) {
    const table = byTable.get(entry.table);
    if (!table) {
      stale.push(`${entry.table}: the schema has no such table; remove the entry`);
      continue;
    }
    if (!entry.columns && !entry.values) {
      wholeTables.add(entry.table);
      if (coverage.rows[entry.table] > 0) stale.push(`${entry.table}: the store writes it now; remove the entry${carriedBy(coverage, entry.table)}`);
      continue;
    }
    for (const column of entry.columns ?? []) {
      const key = `${entry.table}.${column}`;
      if (!table.columns.some((c) => c.name === column)) stale.push(`${key}: the schema has no such column; remove it from the entry`);
      else if (coverage.columns[key] > 0) stale.push(`${key}: the store writes it now; remove it from the entry${carriedBy(coverage, key)}`);
      columns.add(key);
    }
    for (const [column, list] of Object.entries(entry.values ?? {})) {
      for (const value of list) {
        const key = `${entry.table}.${column}=${value}`;
        if (!(table.checks[column] ?? []).includes(value)) stale.push(`${key}: the schema no longer enumerates it; remove it from the entry`);
        else if (coverage.values[key] > 0) stale.push(`${key}: the store writes it now; remove it from the entry${carriedBy(coverage, key)}`);
        values.add(key);
      }
    }
  }
  const uncovered: string[] = [];
  for (const table of schema) {
    if (coverage.rows[table.name] === 0) {
      if (!wholeTables.has(table.name)) uncovered.push(`${table.name}: no rows`);
      continue;
    }
    for (const column of table.columns) {
      const key = `${table.name}.${column.name}`;
      if (coverage.columns[key] === 0 && !columns.has(key)) uncovered.push(`${key}: never set to a non-default value`);
      for (const value of table.checks[column.name] ?? []) {
        const vkey = `${key}=${value}`;
        if (coverage.values[vkey] === 0 && !columns.has(key) && !values.has(vkey)) uncovered.push(`${vkey}: no row carries it`);
      }
    }
  }
  if (uncovered.length > 0 || stale.length > 0) {
    const lines = [];
    if (uncovered.length > 0) {
      lines.push(`The corpus and the top-up never write ${uncovered.length} thing(s) the schema can hold. ` +
        `Add a top-up step that writes each (${TOPUP_FILE}), or declare in coverageAllowlist (${CONFIG_FILE}) why it cannot be reached:`,
        ...uncovered.map((line) => `  - ${line}`));
    }
    if (stale.length > 0) {
      lines.push(`${stale.length} coverageAllowlist entr(y/ies) in ${CONFIG_FILE} no longer hold:`, ...stale.map((line) => `  - ${line}`));
    }
    throw new Error(lines.join('\n'));
  }
  return { uncovered, stale };
}

/** The rows that carry `key`, as the failure quotes them. */
function carriedBy(coverage: Coverage, key: string): string {
  const rows = coverage.carriers[key] ?? [];
  return rows.length === 0 ? '' : `; it is on ${rows.map((row) => `(${row})`).join(', ')}`;
}

/**
 * assertScenariosClassified fails on a library scenario that is neither
 * in the top-up nor excluded with a reason, and on a listed name the
 * library no longer has.
 */
export function assertScenariosClassified(library: string[]): void {
  const listed = new Map<string, string>();
  for (const scenario of topUpScenarios) listed.set(scenario.name, 'topUpScenarios');
  for (const name of Object.keys(excludedScenarios)) {
    if (listed.has(name)) throw new Error(`${name} is in both topUpScenarios and excludedScenarios (${CONFIG_FILE})`);
    listed.set(name, 'excludedScenarios');
  }
  const problems: string[] = [];
  for (const name of library) {
    if (!listed.has(name)) {
      problems.push(`library scenario ${name} is in neither topUpScenarios nor excludedScenarios (${CONFIG_FILE}): ` +
        'add it to the top-up, or exclude it with the reason');
    }
  }
  for (const [name, list] of listed) {
    if (!library.includes(name)) problems.push(`${list} names ${name}, which the library no longer has; remove it`);
  }
  if (problems.length > 0) throw new Error(problems.join('\n'));
}
