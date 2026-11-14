// A thread error AO cannot save to history is still shown. A trigger on the
// live harness database refuses the thread's error rows (what a full disk
// does to the insert), and the mock Claude session reports an error.
// Covers: the error reaches the open thread live, marked as not saved and
// with the write's failure; no error row is written; a reload shows it again
// from GetThreadLiveState; a dismiss clears it, also across a reload.
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { test, expect, type SeedResult } from './fixtures.js';
import { openThread } from './worktree-removal-helpers.js';
import { mockSessions } from './workflows-helpers.js';
import type { HarnessApp } from '../src/harness.js';

const seededTurn = { userText: 'hello', items: [{ kind: 'assistant_text', summary: 'Ready.' }] };

function withDatabase<T>(harness: HarnessApp, use: (db: DatabaseSync) => T): T {
  const db = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'));
  try {
    db.exec('PRAGMA busy_timeout = 5000');
    return use(db);
  } finally {
    db.close();
  }
}

test('an error AO cannot save is shown live as not saved, until dismissed', async ({ harness, page }) => {
  const title = 'Claude on a full disk';
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'unsaved-error', repo: {}, threads: [{ title, provider: 'claude', turns: [seededTurn] }] }],
  });
  const [threadId] = seed.projects[0].threadIds;
  const trigger = `fail_error_rows_${threadId.replaceAll('-', '_')}`;
  // Thread ids are harness-generated UUIDs, so they are safe to inline.
  withDatabase(harness, (db) =>
    db.exec(
      `CREATE TRIGGER ${trigger} BEFORE INSERT ON items WHEN NEW.thread_id = '${threadId}' AND NEW.kind = 'error' ` +
        `BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`,
    ),
  );
  try {
    const before = await mockSessions(harness, 'claude', 0);
    await harness.rpc('StartSession', threadId);
    let mockId = '';
    await expect
      .poll(async () => {
        const mocks = await mockSessions(harness, 'claude', 0);
        mockId = mocks.find((mock) => !before.some((known) => known.mockId === mock.mockId))?.mockId ?? '';
        return mockId;
      })
      .not.toBe('');

    await harness.open(page);
    await openThread(page, title);
    const banner = page.getByTestId('unsaved-error-banner');
    await expect(banner).toHaveCount(0);

    await harness.rpc('HarnessMockCommand', mockId, {
      type: 'emit',
      lines: [
        JSON.stringify({
          type: 'system',
          subtype: 'model_refusal_no_fallback',
          original_model: 'claude-opus-4-7',
          api_refusal_explanation: 'Disk test refusal.',
        }),
      ],
    });

    await expect(banner).toBeVisible();
    await expect(banner).toContainText("Error not saved to this thread's history");
    await expect(banner).toContainText('claude-opus-4-7 refused this request and no fallback model was available.');
    await expect(banner).toContainText('Could not save:');
    await expect(banner).toContainText('database or disk is full');
    const errorRows = withDatabase(harness, (db) =>
      db.prepare(`SELECT count(*) AS n FROM items WHERE thread_id = ? AND kind = 'error'`).get(threadId),
    ) as { n: number };
    expect(errorRows.n).toBe(0);

    // A client that opens the thread later reads the retained list.
    await harness.open(page);
    await openThread(page, title);
    await expect(banner).toBeVisible();
    await expect(banner).toContainText('Disk test refusal.');

    await banner.getByTestId('unsaved-error-dismiss').click();
    await expect(banner).toHaveCount(0);
    await harness.open(page);
    await openThread(page, title);
    await expect(page.getByText('Ready.')).toBeVisible();
    await expect(banner).toHaveCount(0);
  } finally {
    withDatabase(harness, (db) => db.exec(`DROP TRIGGER IF EXISTS ${trigger}`));
    await harness.rpc('StopSession', threadId).catch(() => undefined);
  }
});
