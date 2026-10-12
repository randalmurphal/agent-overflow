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

export async function dbQuery<T>(dbPath: string, sql: string, attach?: string): Promise<T[]> {
  const args = ['-o', 'json', 'db', '--file', dbPath];
  if (attach) args.push('--attach', `snap=${attach}`);
  const { stdout } = await execProduct(productBinary('ao-harness'), [...args, sql], {
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
