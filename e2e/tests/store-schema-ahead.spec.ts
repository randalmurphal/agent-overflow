// A database a newer build migrated: the harness boots on a file the store's
// own fixture test stamps two migrations ahead (TestSchemaAheadHarnessFixture,
// compiled into bin/ao-store-test by `make harness-build`). Covers: boot
// fails with the store's sentence, as written, on the harness bootstrap's
// startup failure, and the database is left as the newer build wrote it.
import { execFile } from 'node:child_process';
import { access, mkdir, mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { promisify } from 'node:util';
import { launchHarness } from '../src/harness.js';
import { test, expect } from './fixtures.js';

const run = promisify(execFile);

// Siblings of the backend binary, for the same reason harness-bench resolves
// ao-harness that way: AO_HARNESS_BIN names the backend of this build.
function sibling(name: string): string {
  const repoRoot = path.resolve(import.meta.dirname, '..', '..');
  const backend = process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow');
  return path.join(path.dirname(backend), name);
}

async function topVersion(dbPath: string): Promise<number> {
  const { stdout } = await run(sibling('ao-harness'), ['-o', 'json', 'db', '--file', dbPath, 'SELECT MAX(version) AS v FROM migration_versions'], { timeout: 30_000 });
  return (JSON.parse(stdout) as Array<{ v: number }>)[0].v;
}

test('a database from a newer build fails boot with the version sentence', async () => {
  const root = await mkdtemp(path.join(tmpdir(), 'ao-schema-ahead-'));
  const dbPath = path.join(root, 'agent-overflow', 'agent-overflow.db');
  try {
    await mkdir(path.dirname(dbPath), { recursive: true });
    await run(sibling('ao-store-test'), ['-test.run', '^TestSchemaAheadHarnessFixture$', '-test.count=1'], {
      env: { ...process.env, AO_TEST_SCHEMA_AHEAD_FIXTURE: dbPath },
      timeout: 120_000,
    });
    // The test skips, and exits 0, when it is not handed a path.
    await access(dbPath);
    const ahead = await topVersion(dbPath);
    const before = await readFile(dbPath);

    const failure = await launchHarness({ dataDir: root }).then(
      async (host) => {
        await host.close();
        return undefined;
      },
      (error: Error) => error.message,
    );
    expect(failure).toBe(
      `harness backend failed to start: database is at schema v${ahead}; this build knows v${ahead - 2}; install the newer version`,
    );
    expect(await topVersion(dbPath)).toBe(ahead);
    expect((await readFile(dbPath)).equals(before)).toBe(true);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
