// The candidate build's own tools, as the upgrade corpus checks use them:
// the binary `make harness-build` wrote (or AO_HARNESS_BIN), its siblings,
// its stamped version, and a read-only query of a database through
// `ao-harness db`.
import { execFile } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import * as path from 'node:path';
import { promisify } from 'node:util';

const run = promisify(execFile);
export const repoRoot = path.resolve(import.meta.dirname, '..', '..');
/** The candidate: the binary `make harness-build` wrote. */
export const candidateBinary = process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow');
const UPDATE_PREFIX = '__AO_UPDATE__:';
const BOOTSTRAP_PREFIX = '__AO_BOOTSTRAP__:';

/** A sibling of the candidate binary (ao-harness, ao-mockprovider). */
export function candidateSibling(name: string): string {
  return path.join(path.dirname(candidateBinary), name);
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

export async function dbQuery<T>(dbPath: string, sql: string, attach?: string): Promise<T[]> {
  const args = ['-o', 'json', 'db', '--file', dbPath];
  if (attach) args.push('--attach', `snap=${attach}`);
  const { stdout } = await run(candidateSibling('ao-harness'), [...args, sql], {
    timeout: 10 * 60_000,
    maxBuffer: 256 * 1024 * 1024,
  });
  return JSON.parse(stdout) as T[];
}

/** Every table of a schema, with the FTS shadow tables folded under their virtual table. */
export async function tableNames(dbPath: string, schema: 'main' | 'snap', attach?: string): Promise<string[]> {
  const rows = await dbQuery<{ name: string; sql: string }>(dbPath,
    `SELECT name, sql FROM ${schema}.sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`, attach);
  const virtual = rows.filter((row) => /^CREATE VIRTUAL TABLE/i.test(row.sql)).map((row) => row.name);
  return rows
    .map((row) => row.name)
    .filter((name) => !virtual.some((v) => name.startsWith(`${v}_`)));
}
