// The corpus archive: the data root a passing release check leaves behind,
// packed for the next release to migrate. A release publishes it as the
// `upgrade-corpus.tar.zst` asset; the next release's check restores it, so
// the corpus a release migrates is the one every release before it wrote
// into, at the previous release's schema.
//
// The archive is relocatable only to the root it was made at: a project's
// path is absolute in the store, so the rig root is fixed (prepareRig) and
// the manifest records it. A restore at another root fails naming both.
import { execFile } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rename, rm, stat, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { promisify } from 'node:util';
import type { Rig } from './upgrade-corpus-launcher.js';
import { dbQuery } from './upgrade-corpus-tools.js';

const run = promisify(execFile);
const MANIFEST = 'upgrade-corpus.json';
const DATA = 'data';

/** Runtime state of the instance that wrote the root; never part of the corpus. */
const EXCLUDED = [
  'agent-overflow/bin',
  'agent-overflow/logs',
  'agent-overflow/runtime',
  'agent-overflow/backend.lock',
  'agent-overflow/harness.lock',
  'agent-overflow/control.json',
  'agent-overflow/transport-port.json',
  'agent-overflow/harness-instance.json',
  'agent-overflow/agent-overflow.db-shm',
];

export interface CorpusManifest {
  format: 1;
  /** The rig root the archive was made at and must be restored to. */
  root: string;
  /** The release whose check wrote the archive. */
  release: string;
  schemaVersion: number;
  createdAt: string;
  threads: number;
  items: number;
  dbBytes: number;
}

/** archiveCorpus packs `rig.dataRoot` and the manifest into `out` (tar + zstd). */
export async function archiveCorpus(rig: Rig, out: string, manifest: Omit<CorpusManifest, 'format' | 'root' | 'createdAt'>): Promise<CorpusManifest> {
  const full: CorpusManifest = { format: 1, root: rig.root, createdAt: new Date().toISOString(), ...manifest };
  await writeFile(path.join(rig.root, MANIFEST), JSON.stringify(full, null, 2) + '\n');
  await mkdir(path.dirname(out), { recursive: true });
  await rm(out, { force: true });
  await run('tar', [
    '--zstd', '-cf', out, '-C', rig.root,
    ...EXCLUDED.map((entry) => `--exclude=${DATA}/${entry}`),
    MANIFEST, DATA,
  ], { maxBuffer: 16 * 1024 * 1024 });
  return full;
}

/**
 * restoreCorpus unpacks `archive` into `rig.dataRoot`, which must be the
 * empty root prepareRig made, and returns the manifest. Fails when the
 * archive was made at another root.
 */
export async function restoreCorpus(rig: Rig, archive: string): Promise<CorpusManifest> {
  await stat(archive);
  const staging = await mkdtemp(path.join(rig.root, 'restore-'));
  await run('tar', ['--zstd', '-xf', archive, '-C', staging], { maxBuffer: 16 * 1024 * 1024 });
  const manifest = JSON.parse(await readFile(path.join(staging, MANIFEST), 'utf8')) as CorpusManifest;
  if (manifest.format !== 1) throw new Error(`${archive}: manifest format ${String(manifest.format)} is not 1`);
  if (manifest.root !== rig.root) {
    throw new Error(`${archive} was made at ${manifest.root} and the check runs at ${rig.root}; ` +
      'run it with the same root (AO_RELEASE_UPGRADE_ROOT is its parent) or bootstrap a new corpus');
  }
  await rm(rig.dataRoot, { recursive: true, force: true });
  await rename(path.join(staging, DATA), rig.dataRoot);
  await rm(staging, { recursive: true, force: true });
  await stat(rig.dbPath);
  return manifest;
}

/**
 * assertRelocated proves the restored store points at this rig: every
 * project path is under its workspaces, so the candidate's git and
 * worktree operations find real repositories.
 */
export async function assertRelocated(rig: Rig): Promise<void> {
  const rows = await dbQuery<{ path: string }>(rig.dbPath, 'SELECT DISTINCT path FROM projects ORDER BY path');
  const prefix = path.join(rig.dataRoot, 'workspaces') + path.sep;
  const elsewhere = rows.map((row) => row.path).filter((p) => !p.startsWith(prefix));
  if (elsewhere.length > 0) {
    throw new Error(`the corpus has ${elsewhere.length} project(s) outside ${prefix}: ${elsewhere.slice(0, 3).join(', ')}`);
  }
}
