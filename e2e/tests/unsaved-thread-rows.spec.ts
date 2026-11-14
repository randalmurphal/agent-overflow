// Thread errors and notices AO cannot save to history are still shown. A
// trigger on the live harness database refuses the thread's rows of one kind
// (what a full disk does to the insert), and the mock Claude session reports
// an error or dies.
// Covers: an error reaches the open thread live in the error tone, marked as
// not saved and with the write's failure; no error row is written; a reload
// shows it again from GetThreadUnsavedRows; a dismiss removes the rows it
// names, also across a reload. A session death whose notice is refused
// still lights the session Reconnect row, and its notice is listed in the
// warning tone. A view-only device reads the list, cannot dismiss it, and
// sees another client's dismiss live.
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { test as pwTest, type BrowserContext, type Locator } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import { openThread } from './worktree-removal-helpers.js';
import { mockSessions } from './workflows-helpers.js';
import {
  PAIRED_APP_MOUNT_MS,
  confirmOnHost,
  instrument,
  mintInvite,
  nonLoopbackIPv4,
  redeemOnScreen,
} from './offhost-helpers.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';

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

function rowCount(harness: HarnessApp, threadId: string, kind: string): number {
  const row = withDatabase(harness, (db) =>
    db.prepare(`SELECT count(*) AS n FROM items WHERE thread_id = ? AND kind = ?`).get(threadId, kind),
  ) as { n: number };
  return row.n;
}

// Seeds a Claude thread, refuses its rows of `kind`, starts its session and
// returns the mock's id. The returned cleanup drops the trigger and stops
// the session.
async function liveThreadRefusing(
  harness: HarnessApp,
  title: string,
  kind: string,
): Promise<{ threadId: string; mockId: string; cleanup: () => Promise<void> }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `unsaved-${kind}`, repo: {}, threads: [{ title, provider: 'claude', turns: [seededTurn] }] }],
  });
  const [threadId] = seed.projects[0].threadIds;
  const trigger = `refuse_${kind}_${threadId.replaceAll('-', '_')}`;
  // Thread ids are harness-generated UUIDs, so they are safe to inline.
  withDatabase(harness, (db) =>
    db.exec(
      `CREATE TRIGGER ${trigger} BEFORE INSERT ON items WHEN NEW.thread_id = '${threadId}' AND NEW.kind = '${kind}' ` +
        `BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`,
    ),
  );
  const cleanup = async () => {
    withDatabase(harness, (db) => db.exec(`DROP TRIGGER IF EXISTS ${trigger}`));
    await harness.rpc('StopSession', threadId).catch(() => undefined);
  };
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
    return { threadId, mockId, cleanup };
  } catch (err) {
    await cleanup();
    throw err;
  }
}

// The mock session reports a provider error: a model refusal with no
// fallback, whose row is an `error` row.
async function emitRefusal(harness: HarnessApp, mockId: string, model: string, explanation: string): Promise<void> {
  await harness.rpc('HarnessMockCommand', mockId, {
    type: 'emit',
    lines: [
      JSON.stringify({
        type: 'system',
        subtype: 'model_refusal_no_fallback',
        original_model: model,
        api_refusal_explanation: explanation,
      }),
    ],
  });
}

function entries(banner: Locator): Locator {
  return banner.getByTestId('unsaved-row');
}

test('an error AO cannot save is shown live as not saved, until dismissed',async ({ harness, page }) => {
  const title = 'Claude on a full disk';
  const { threadId, mockId, cleanup } = await liveThreadRefusing(harness, title, 'error');
  try {
    await harness.open(page);
    await openThread(page, title);
    const banner = page.getByTestId('unsaved-rows-banner');
    await expect(banner).toHaveCount(0);

    await emitRefusal(harness, mockId, 'claude-opus-4-7', 'Disk test refusal.');

    await expect(banner).toBeVisible();
    await expect(banner).toContainText("Not saved to this thread's history");
    const entry = banner.getByTestId('unsaved-row');
    await expect(entry).toHaveAttribute('data-severity', 'error');
    await expect(entry).toContainText('claude-opus-4-7 refused this request and no fallback model was available.');
    await expect(entry).toContainText('Could not save:');
    await expect(entry).toContainText('database or disk is full');
    expect(rowCount(harness, threadId, 'error')).toBe(0);

    // A client that opens the thread later reads the retained list.
    await harness.open(page);
    await openThread(page, title);
    await expect(banner).toBeVisible();
    await expect(banner).toContainText('Disk test refusal.');

    // A dismiss removes only the rows it names: a client that displayed
    // only the first leaves the second for every screen, also across a
    // reload.
    await emitRefusal(harness, mockId, 'claude-sonnet-4-6', 'Second refusal.');
    await expect(entries(banner)).toHaveCount(2);
    const kept = await harness.rpc<{ rows: Array<{ id: string }> }>('GetThreadUnsavedRows', threadId);
    expect(new Set(kept.rows.map((row) => row.id)).size).toBe(2);
    await harness.rpc('DismissUnsavedThreadRows', threadId, [kept.rows[0].id]);
    await expect(entries(banner)).toHaveCount(1);
    await expect(entries(banner)).toContainText('Second refusal.');
    await harness.open(page);
    await openThread(page, title);
    await expect(entries(banner)).toHaveCount(1);
    await expect(entries(banner)).toContainText('Second refusal.');

    await banner.getByTestId('unsaved-rows-dismiss').click();
    await expect(banner).toHaveCount(0);
    await harness.open(page);
    await openThread(page, title);
    await expect(page.getByText('Ready.')).toBeVisible();
    await expect(banner).toHaveCount(0);
  } finally {
    await cleanup();
  }
});

test('a session death whose notice AO cannot save still shows, as a warning', async ({ harness, page }) => {
  const title = 'Claude dies on a full disk';
  const { threadId, mockId, cleanup } = await liveThreadRefusing(harness, title, 'notification');
  try {
    await harness.open(page);
    await openThread(page, title);
    const banner = page.getByTestId('unsaved-rows-banner');
    await expect(banner).toHaveCount(0);

    await harness.rpc('HarnessMockCommand', mockId, { type: 'exit', code: 1 });

    // provider:session_died still fired: the session row offers Reconnect.
    await expect(page.locator('[data-testid="pane-error-banner"][data-kind="session"]')).toBeVisible();
    await expect(banner).toBeVisible();
    const notice = banner.locator('[data-testid="unsaved-row"][data-severity="warning"]');
    await expect(notice).toHaveCount(1);
    await expect(notice).toContainText('Could not save:');
    await expect(notice).toContainText('database or disk is full');
    expect(rowCount(harness, threadId, 'notification')).toBe(0);
  } finally {
    await cleanup();
  }
});

// A view-only device holds threads:read, the grant of the push and its
// snapshot, but not threads:operate. It owns its backend because the LAN
// bind persists to the settings file, which `harness.reset()` does not undo.
pwTest.describe('on a view-only device', () => {
  const lanIP = nonLoopbackIPv4();
  pwTest.skip(lanIP === null, 'outside the test network namespace, so no off-host peer can be produced without LAN traffic');

  pwTest('unsaved rows show from the snapshot and clear live, with dismiss inert', async ({ browser }) => {
    const harness = await launchHarness();
    let phoneContext: BrowserContext | undefined;
    let cleanup = async () => {};
    try {
      const network = await harness.rpc<{ bindAll: boolean }>('SetNetworkSettings', { bindAll: true });
      expect(network.bindAll).toBe(true);
      const title = 'Claude on a full disk, watched';
      const live = await liveThreadRefusing(harness, title, 'error');
      cleanup = live.cleanup;
      await emitRefusal(harness, live.mockId, 'claude-opus-4-7', 'Watched refusal.');
      await expect
        .poll(async () => (await harness.rpc<{ rows: unknown[] }>('GetThreadUnsavedRows', live.threadId)).rows.length)
        .toBe(1);

      phoneContext = await browser.newContext();
      const phone = await phoneContext.newPage();
      const surfaced = await instrument(phone);
      const invite = await mintInvite(harness, 'view-only');
      await confirmOnHost(harness, await redeemOnScreen(phone, invite, 'Watching phone'));
      await expect(phone.getByTestId('view-only-indicator')).toBeVisible({ timeout: PAIRED_APP_MOUNT_MS });

      // Kept before this device opened the thread, so only the snapshot
      // read can show it.
      await phone.getByTestId('thread-row').filter({ hasText: title }).click();
      const banner = phone.getByTestId('unsaved-rows-banner');
      await expect(entries(banner)).toHaveCount(1);
      await expect(entries(banner)).toContainText('Watched refusal.');
      await expect(banner.getByTestId('unsaved-rows-dismiss')).toBeDisabled();

      // Another client's dismiss reaches this one as a push.
      const kept = await harness.rpc<{ rows: Array<{ id: string }> }>('GetThreadUnsavedRows', live.threadId);
      await harness.rpc('DismissUnsavedThreadRows', live.threadId, kept.rows.map((row) => row.id));
      await expect(banner).toHaveCount(0);
      expect(surfaced.refusals, 'a view-only device reads unsaved rows without a refusal').toEqual([]);
    } finally {
      await cleanup();
      await phoneContext?.close();
      await harness.rpc('SetNetworkSettings', { bindAll: false }).catch(() => undefined);
      await harness.close();
    }
  });
});
