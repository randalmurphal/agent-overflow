// Update trial and the no-live-migration gate against the real App boot
// (docs/specs/app-update.md, rule 7). The database is the v118 fixture the
// store's own test writes (TestHistoryRepairHarnessFixture, compiled into
// bin/ao-store-test by `make harness-build`); bin/agent-overflow runs the
// update commands and the headless boot as the Windows launcher runs them
// through wsl.exe. Covers: a headless boot started with
// --refuse-pending-migrations answers /bootstrap.json with the 409
// migrations-pending refusal and leaves the database files as they were;
// __update-snapshot reports the schema version the refusal named, which keys
// the launcher's failure memory; __update-snapshot and __update-trial-run
// migrate it in the real App trial
// boot, which reports prepared; the next gated boot finds nothing pending and
// becomes ready; and __update-restore after a prepared trial puts the
// fixture's bytes back. Every process runs with a temporary home and fake
// claude, codex, gh and glab first on PATH, which record each call: the
// refused boot and the parked trial start no provider, and the live boot's
// provider probes reach the fakes.
import { createHash } from 'node:crypto';
import { access, copyFile, mkdtemp, readdir, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { execProduct, productBinary } from '../src/product-process.js';
import { test, expect } from './fixtures.js';
import {
  buildSandbox,
  cliCalls,
  killGroup,
  newUpdateID,
  runUpdateCommand,
  settledBootstrap,
  startHeadless,
  stopHeadless,
  updateResult,
  type HeadlessBackend,
  type Sandbox,
} from './update-launcher-helpers.js';
import { buildVersion } from './upgrade-corpus-tools.js';

const FIXTURE_VERSION = 118;
// The launcher's own limits are the release upgrade check's concern; here
// every wait is a generous test timeout.
const COMMAND_TIMEOUT_MS = 120_000;
const BOOTSTRAP_TIMEOUT_MS = 30_000;
const BOOT_TIMEOUT_MS = 60_000;
const STOP_TIMEOUT_MS = 30_000;

async function writeFixture(dbPath: string): Promise<void> {
  await execProduct(productBinary('ao-store-test'), ['-test.run', '^TestHistoryRepairHarnessFixture$', '-test.count=1'], {
    env: { AO_TEST_HISTORY_REPAIR_FIXTURE: dbPath },
    timeout: 120_000,
  });
  // The test skips, and exits 0, when it is not handed a path.
  await access(dbPath);
}

// The digest of each file of the database triple that exists.
async function databaseFiles(dbPath: string): Promise<Record<string, string>> {
  const dir = path.dirname(dbPath);
  const base = path.basename(dbPath);
  const out: Record<string, string> = {};
  for (const name of (await readdir(dir)).sort()) {
    if (name !== base && name !== `${base}-wal` && name !== `${base}-shm`) continue;
    out[name] = createHash('sha256').update(await readFile(path.join(dir, name))).digest('hex');
  }
  return out;
}

// The highest applied migration, read from a copy so the query cannot
// change the files the test compares.
async function schemaVersion(sb: Sandbox): Promise<number> {
  const copy = await mkdtemp(path.join(sb.root, 'query-'));
  const dir = path.dirname(sb.dbPath);
  for (const name of await readdir(dir)) {
    if (name === 'agent-overflow.db' || name === 'agent-overflow.db-wal') {
      await copyFile(path.join(dir, name), path.join(copy, name));
    }
  }
  const { stdout } = await execProduct(productBinary('ao-harness'), ['-o', 'json', 'db', '--file', path.join(copy, 'agent-overflow.db'),
    'SELECT MAX(version) AS v FROM migration_versions'], { timeout: 30_000 });
  await rm(copy, { recursive: true, force: true });
  return (JSON.parse(stdout) as Array<{ v: number }>)[0].v;
}

test('a gated boot refuses a v118 database, a real trial migrates it, and the next boot finds nothing pending', async () => {
  test.setTimeout(240_000);
  const sb = await buildSandbox(await mkdtemp(path.join(tmpdir(), 'ao-update-trial-')));
  const backends: HeadlessBackend[] = [];
  try {
    await writeFixture(sb.dbPath);
    expect(await schemaVersion(sb)).toBe(FIXTURE_VERSION);
    const fixture = await databaseFiles(sb.dbPath);

    // The gate: the boot refuses before anything writes the database.
    const refused = await startHeadless(sb, BOOTSTRAP_TIMEOUT_MS);
    backends.push(refused);
    const refusal = await settledBootstrap(refused, BOOT_TIMEOUT_MS, BOOT_TIMEOUT_MS);
    expect(refusal.status).toBe(409);
    expect(refusal.cacheControl).toContain('no-store');
    const pending = JSON.parse(refusal.body) as { reason: string; database: number; build: number; pending: number };
    expect(pending).toMatchObject({ reason: 'migrations-pending', database: FIXTURE_VERSION });
    // The count is the chain's: at least one, and never more than the
    // version span, which versions need not fill contiguously.
    expect(pending.pending).toBeGreaterThan(0);
    expect(pending.pending).toBeLessThanOrEqual(pending.build - FIXTURE_VERSION);
    await stopHeadless(refused, STOP_TIMEOUT_MS);
    expect(await databaseFiles(sb.dbPath)).toEqual(fixture);

    // The migration the launcher runs: snapshot, then the real App trial boot.
    const id = newUpdateID();
    const snapshot = await runUpdateCommand(sb, ['__update-snapshot', '--id', id], COMMAND_TIMEOUT_MS);
    expect(updateResult(snapshot.events), snapshot.stderr).toMatchObject({ type: 'result', outcome: 'ok', schema: pending.database });
    const trial = await runUpdateCommand(sb, ['__update-trial-run', '--id', id, '--to', await buildVersion(), '--attempt', '1'], COMMAND_TIMEOUT_MS);
    expect(updateResult(trial.events), trial.stderr).toMatchObject({ type: 'result', outcome: 'prepared' });
    expect(trial.events.some((event) => event.progress?.phase === 'store.migrate'), trial.stderr).toBe(true);
    expect(await schemaVersion(sb)).toBe(pending.build);
    // Provider probes are unattended work, which a trial parks.
    expect(await cliCalls(sb)).toEqual([]);

    // The live boot after the trial finds nothing pending and starts, and
    // its provider probes resolve to the fakes.
    const live = await startHeadless(sb, BOOTSTRAP_TIMEOUT_MS);
    backends.push(live);
    const ready = await settledBootstrap(live, BOOT_TIMEOUT_MS, BOOT_TIMEOUT_MS);
    expect(ready.status, ready.body).toBe(200);
    await expect.poll(() => cliCalls(sb), { timeout: 20_000 }).toEqual(
      expect.arrayContaining(['claude --version', 'codex --version']));
    await stopHeadless(live, STOP_TIMEOUT_MS);

    const discard = await runUpdateCommand(sb, ['__update-discard', '--id', id], COMMAND_TIMEOUT_MS);
    expect(updateResult(discard.events), discard.stderr).toMatchObject({ type: 'result', outcome: 'ok' });
    await expect(access(path.join(sb.dataRoot, 'agent-overflow', 'runtime', 'app-update', 'snapshot'))).rejects.toThrow();
    test.info().annotations.push({ type: 'cli-calls', description: (await cliCalls(sb)).join(' | ') });
  } finally {
    for (const b of backends) {
      await stopHeadless(b, STOP_TIMEOUT_MS).catch(() => killGroup(b.child));
    }
    await rm(sb.root, { recursive: true, force: true });
  }
});

test('a restore after a prepared trial puts back the v118 fixture byte for byte', async () => {
  test.setTimeout(180_000);
  const sb = await buildSandbox(await mkdtemp(path.join(tmpdir(), 'ao-update-trial-')));
  try {
    await writeFixture(sb.dbPath);
    const fixture = await databaseFiles(sb.dbPath);

    const id = newUpdateID();
    const snapshot = await runUpdateCommand(sb, ['__update-snapshot', '--id', id], COMMAND_TIMEOUT_MS);
    expect(updateResult(snapshot.events), snapshot.stderr).toMatchObject({ type: 'result', outcome: 'ok', schema: FIXTURE_VERSION });
    const trial = await runUpdateCommand(sb, ['__update-trial-run', '--id', id, '--to', await buildVersion(), '--attempt', '1'], COMMAND_TIMEOUT_MS);
    expect(updateResult(trial.events), trial.stderr).toMatchObject({ type: 'result', outcome: 'prepared' });
    expect(await schemaVersion(sb)).toBeGreaterThan(FIXTURE_VERSION);
    expect(await databaseFiles(sb.dbPath)).not.toEqual(fixture);

    const restore = await runUpdateCommand(sb, ['__update-restore', '--id', id, '--reason', 'the test rolls back'], COMMAND_TIMEOUT_MS);
    expect(updateResult(restore.events), restore.stderr).toMatchObject({ type: 'result', outcome: 'ok' });
    expect(await databaseFiles(sb.dbPath)).toEqual(fixture);
    expect(await schemaVersion(sb)).toBe(FIXTURE_VERSION);

    const discard = await runUpdateCommand(sb, ['__update-discard', '--id', id], COMMAND_TIMEOUT_MS);
    expect(updateResult(discard.events), discard.stderr).toMatchObject({ type: 'result', outcome: 'ok' });
    expect(await cliCalls(sb)).toEqual([]);
  } finally {
    await rm(sb.root, { recursive: true, force: true });
  }
});
