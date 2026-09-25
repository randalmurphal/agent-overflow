// A boot sweep that fails is shown: the harness boots on a database the
// store's own fixture test writes (TestBootSweepFailureHarnessFixture,
// compiled into bin/ao-store-test by `make harness-build`), whose
// crashed-turn sweep a trigger refuses. Covers: the boot goes on past the
// failed phase, the hello carries it, the transport strip names the phase
// and its error with the retry, a dismiss clears it, a reload shows it
// again, and the turn stays in flight for the next start.
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

async function inFlightTurns(dbPath: string): Promise<number> {
  const { stdout } = await run(sibling('ao-harness'), ['-o', 'json', 'db', '--file', dbPath,
    'SELECT count(*) AS n FROM turns WHERE completed_at IS NULL'], { timeout: 30_000 });
  return (JSON.parse(stdout) as Array<{ n: number }>)[0].n;
}

test('a failed boot sweep is shown on the transport strip', async ({ page }) => {
  const root = await mkdtemp(path.join(tmpdir(), 'ao-boot-sweep-failure-'));
  const dbPath = path.join(root, 'agent-overflow', 'agent-overflow.db');
  let host: HarnessApp | undefined;
  try {
    await mkdir(path.dirname(dbPath), { recursive: true });
    await run(sibling('ao-store-test'), ['-test.run', '^TestBootSweepFailureHarnessFixture$', '-test.count=1'], {
      env: { ...process.env, AO_TEST_BOOT_SWEEP_FAILURE_FIXTURE: dbPath },
      timeout: 120_000,
    });
    // The test skips, and exits 0, when it is not handed a path.
    await access(dbPath);

    host = await launchHarness({ dataDir: root });
    await host.open(page);
    const strip = page.getByTestId('transport-status-banner');
    await expect(strip.locator('p')).toHaveText(
      /^Settling interrupted turns failed at startup: triage: recover crashed turns: .*fixture: turns are read-only[^.]*\. Retrying on next start\.$/,
    );
    await expect(strip).toHaveAttribute('data-status', 'connected');

    await page.getByTestId('transport-status-dismiss').click();
    await expect(strip).toBeHidden();
    await page.reload();
    await expect(strip).toContainText('Settling interrupted turns failed at startup:');
    expect(await inFlightTurns(dbPath)).toBe(1);
  } finally {
    await page.goto('about:blank');
    await host?.close();
    await rm(root, { recursive: true, force: true });
  }
});
