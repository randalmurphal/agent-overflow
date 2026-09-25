// History repair on upgrade: the harness boots on a v118 database the store's
// own fixture test writes (TestHistoryRepairHarnessFixture, compiled into
// bin/ao-store-test by `make harness-build`), and v119's deferred phase runs
// in the background of that boot. Covers: sealed chunks fold back into their
// thread's rows, an empty payload inside a sealed chunk comes back as an
// empty blob, a leaked payload is pruned, a legacy background-agent
// transcript copy is emptied while its meta stays and a Monitor output keeps
// its data, a row the finished agent left running is settled by v124's phase
// with the live path's agent end rule, the watermark advances through v119
// and the later phases with no failure recorded, and the repaired thread
// renders its rows.
import { execFile } from 'node:child_process';
import { access, mkdir, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { promisify } from 'node:util';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { test, expect } from './fixtures.js';

const run = promisify(execFile);

// Siblings of the backend binary, for the same reason harness-bench resolves
// ao-harness that way: AO_HARNESS_BIN names the backend of this build.
function sibling(name: string): string {
  const repoRoot = path.resolve(import.meta.dirname, '..', '..');
  const backend = process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow');
  return path.join(path.dirname(backend), name);
}

async function writeFixture(dbPath: string): Promise<void> {
  await run(sibling('ao-store-test'), ['-test.run', '^TestHistoryRepairHarnessFixture$', '-test.count=1'], {
    env: { ...process.env, AO_TEST_HISTORY_REPAIR_FIXTURE: dbPath },
    timeout: 120_000,
  });
  // The test skips, and exits 0, when it is not handed a path.
  await access(dbPath);
}

async function query<T extends Record<string, unknown>>(dbPath: string, sql: string): Promise<T[]> {
  const { stdout } = await run(sibling('ao-harness'), ['-o', 'json', 'db', '--file', dbPath, sql], { timeout: 30_000 });
  return JSON.parse(stdout) as T[];
}

const count = async (dbPath: string, sql: string) => (await query<{ n: number }>(dbPath, sql))[0].n;
const watermark = async (dbPath: string) => (await query<{ user_version: number }>(dbPath, 'PRAGMA user_version'))[0].user_version;

test('an upgrade from v118 repairs stored history in the background', async ({ page }) => {
  const root = await mkdtemp(path.join(tmpdir(), 'ao-history-repair-'));
  const dbPath = path.join(root, 'agent-overflow', 'agent-overflow.db');
  let host: HarnessApp | undefined;
  try {
    await mkdir(path.dirname(dbPath), { recursive: true });
    await writeFixture(dbPath);
    expect(await watermark(dbPath)).toBe(118);
    expect(await count(dbPath, `SELECT count(*) AS n FROM import_history_chunks WHERE id LIKE 'sealed:%'`)).toBe(3);
    expect(await count(dbPath, `SELECT count(*) AS n FROM sqlite_master WHERE name = 'deferred_migration_failures'`)).toBe(0);

    host = await launchHarness({ dataDir: root });
    expect(host.bootstrap.dataDir).toBe(path.dirname(dbPath));
    await expect.poll(() => watermark(dbPath), { timeout: 60_000 }).toBe(124);

    expect(await count(dbPath, `SELECT count(*) AS n FROM deferred_migration_failures`)).toBe(0);
    expect(await count(dbPath, `SELECT count(*) AS n FROM subagent_aggregate_backfill`)).toBe(0);
    expect(await count(dbPath, `SELECT count(*) AS n FROM agent_end_backfill`)).toBe(0);
    expect(await query(dbPath, `SELECT status, summary LIKE 'Read: README.md%turn ended with tool unresolved' AS unresolved
      FROM items WHERE thread_id = 'fixture-transcript' AND id = 'agent-read'`)).toEqual([{ status: 'errored', unresolved: 1 }]);
    expect(await count(dbPath, `SELECT count(*) AS n FROM import_history_chunks`)).toBe(0);
    expect(await count(dbPath, `SELECT count(*) AS n FROM items WHERE thread_id = 'fixture-sealed'`)).toBe(31);
    expect(await query(dbPath, `SELECT typeof(data) AS type, length(data) AS bytes FROM payloads
      WHERE thread_id = 'fixture-sealed' AND id = 'p-empty'`)).toEqual([{ type: 'blob', bytes: 0 }]);
    expect(await count(dbPath, `SELECT count(*) AS n FROM payloads WHERE thread_id = 'fixture-sealed' AND id = 'orphan'`)).toBe(0);
    expect(await query(dbPath, `SELECT id, length(data) AS bytes, json_extract(meta, '$.outputFileState') AS state
      FROM payloads WHERE thread_id = 'fixture-transcript' ORDER BY id`)).toEqual([
      { id: 'p-agent', bytes: 0, state: 'loaded' },
      { id: 'p-monitor', bytes: 'monitor output'.length, state: 'loaded' },
    ]);

    await host.open(page);
    await page.getByTestId('thread-row').filter({ hasText: 'Thread fixture-sealed' }).first().click();
    await expect(page.getByText('restorable history 29', { exact: true })).toBeVisible();
  } finally {
    await page.goto('about:blank');
    await host?.close();
    await rm(root, { recursive: true, force: true });
  }
});
