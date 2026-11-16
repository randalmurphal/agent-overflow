// The build's tools, as the upgrade corpus checks use them: the check's
// sandbox at its fixed root, the stamped version, and a read-only query of
// a database through `ao-harness db`. The harness tools themselves are
// productBinary's (src/product-process.ts).
import { mkdir, readFile, rm } from 'node:fs/promises';
import * as path from 'node:path';
import { execProduct, productBinary } from '../src/product-process.js';
import { buildSandbox, type Sandbox } from './update-launcher-helpers.js';

export const repoRoot = path.resolve(import.meta.dirname, '..', '..');

/** The rig root under its parent: fixed, because a corpus archive is relocatable only to the root it was made at. */
export const RIG_NAME = 'ao-upgrade-corpus';

/**
 * prepareRig builds the check's sandbox at `<parent>/ao-upgrade-corpus`
 * (parent defaults to /tmp), replacing what a previous run left there.
 */
export async function prepareRig(parent: string | undefined): Promise<Sandbox> {
  const root = path.join(parent ?? '/tmp', RIG_NAME);
  await rm(root, { recursive: true, force: true });
  await mkdir(root, { recursive: true });
  return buildSandbox(root);
}

/** The version `make harness-build` stamps into the candidate. */
export async function buildVersion(): Promise<string> {
  const config = await readFile(path.join(repoRoot, 'build', 'config.yml'), 'utf8');
  const match = /^ {2}version: "(.+)"$/m.exec(config);
  if (!match) throw new Error('build/config.yml names no version');
  return match[1];
}

/** Wall-clock milliseconds of one phase, kept for the report. */
export async function timed<T>(fn: () => Promise<T>): Promise<{ value: T; ms: number }> {
  const started = Date.now();
  const value = await fn();
  return { value, ms: Date.now() - started };
}

/**
 * dbQuery runs one read-only statement on dbPath. `attach` is a database
 * attached as `snap`, or schema names to database paths.
 */
export async function dbQuery<T>(dbPath: string, sql: string, attach?: string | Record<string, string>): Promise<T[]> {
  // --limit 0: every row; the default cap would cut a result short silently.
  const args = ['-o', 'json', 'db', '--file', dbPath, '--limit', '0'];
  const schemas = typeof attach === 'string' ? { snap: attach } : attach ?? {};
  for (const [name, file] of Object.entries(schemas)) args.push('--attach', `${name}=${file}`);
  const { stdout } = await execProduct(productBinary('ao-harness'), [...args, sql], {
    timeout: 10 * 60_000,
    maxBuffer: 256 * 1024 * 1024,
  });
  return JSON.parse(stdout) as T[];
}

/** The suffixes of the shadow tables FTS3, FTS4 and FTS5 create beside a virtual table. */
const shadowSuffixes = ['data', 'idx', 'content', 'docsize', 'config', 'segments', 'segdir', 'stat'];

/**
 * Every table of a schema, with the FTS shadow tables folded under their
 * virtual table. Only the shadow suffixes fold: a real table that shares the
 * prefix (thread_search_rows beside thread_search) is a table of its own.
 */
export async function tableNames(dbPath: string, schema: 'main' | 'snap', attach?: string): Promise<string[]> {
  const rows = await dbQuery<{ name: string; sql: string }>(dbPath,
    `SELECT name, sql FROM ${schema}.sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`, attach);
  const shadows = new Set(rows.filter((row) => /^CREATE VIRTUAL TABLE/i.test(row.sql))
    .flatMap((row) => shadowSuffixes.map((suffix) => `${row.name}_${suffix}`)));
  return rows.map((row) => row.name).filter((name) => !shadows.has(name));
}
