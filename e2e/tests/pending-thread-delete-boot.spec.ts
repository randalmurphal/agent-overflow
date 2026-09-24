// A thread whose delete began and did not finish (threads.deleting) is gone
// from the page from its first render, and the next boot completes the
// delete once the page has read its catalogs
// (app_pending_thread_deletes.go, started by startUnattendedWork).
//
// The Go tests stop a delete partway, or fail it, and complete it from the
// reopened database (thread_delete_pending_test.go,
// app_pending_thread_deletes_test.go). This level shows a real boot
// starting that walk. The spec marks the thread in the stopped backend's
// database, which is the state a delete a crash stopped leaves behind.
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';

const DOOMED = 'Doomed thread';
const KEPT = 'Kept thread';

test('the next boot completes a delete that did not finish, and the page never shows its thread', async ({ page }) => {
  test.setTimeout(120_000);
  const root = await mkdtemp(join(tmpdir(), 'ao-pending-delete-'));
  let harness: HarnessApp | undefined;
  try {
    harness = await launchHarness({ dataDir: root });
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        {
          name: 'pending-thread-delete',
          repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
          threads: [DOOMED, KEPT].map((title) => ({
            title,
            provider: 'claude',
            turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          })),
        },
      ],
    });
    const doomed = seed.projects[0].threadIds[0];
    const database = join(harness.bootstrap.dataDir, 'agent-overflow.db');
    const rows = page.getByTestId('thread-row');
    await harness.open(page);
    await expect(rows.filter({ hasText: DOOMED })).toHaveCount(1);
    await expect(rows.filter({ hasText: KEPT })).toHaveCount(1);
    expect(await harness.stop()).toBe(true);
    harness = undefined;

    const marking = new DatabaseSync(database);
    try {
      expect(marking.prepare('UPDATE threads SET deleting = 1 WHERE id = ?').run(doomed).changes).toBe(1);
    } finally {
      marking.close();
    }
    const stored = () => {
      const reading = new DatabaseSync(database, { readOnly: true });
      try {
        return (reading.prepare('SELECT COUNT(*) AS n FROM threads WHERE id = ?').get(doomed) as { n: number }).n;
      } finally {
        reading.close();
      }
    };

    harness = await launchHarness({ dataDir: root });
    await harness.open(page);
    await expect(rows.filter({ hasText: KEPT })).toHaveCount(1);
    await expect(rows.filter({ hasText: DOOMED })).toHaveCount(0);
    await expect.poll(stored, { timeout: 10_000 }).toBe(0);
    await expect(rows.filter({ hasText: KEPT })).toHaveCount(1);
    await expect(rows.filter({ hasText: DOOMED })).toHaveCount(0);
  } finally {
    try {
      await page.close();
      await harness?.close();
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }
});
