// A press on the compact list or the compact thread header, as the sync the
// transport banner reports ends under the finger, lands where it was
// pressed: the banner's row is held until the tap's click, which the engine
// hit-tests after the release. The sync is kept open by holding back the
// server's replay-complete frame, and the press and the release are
// separate touch events, so the sync ends between them on every run. A
// press that ends in no click (a long press, a drag) lets the row go at its
// own end.
import type { CDPSession, Locator, Page, WebSocketRoute } from '@playwright/test';
import { test, expect } from './fixtures.js';

type Point = { x: number; y: number };

// Routes the page's socket so a reconnect's replay-complete frame can be
// held back, which keeps the banner reporting the sync until released.
async function routeReplay(page: Page) {
  let holding = false;
  const held: Array<() => void> = [];
  let connection: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    connection = { page: socket, server };
    server.onMessage((message) => {
      if (holding && (JSON.parse(String(message)) as { type?: string }).type === 'replay') {
        held.push(() => socket.send(message));
        return;
      }
      socket.send(message);
    });
  });
  return {
    // Reconnects with the replay held: the banner reports the sync.
    async startSync() {
      holding = true;
      await connection!.page.close({ code: 1012 });
      await connection!.server.close();
      await expect(page.getByTestId('transport-status-banner')).toContainText('Syncing');
      await expect.poll(() => held.length).toBe(1);
    },
    // Ends the sync: the banner goes.
    async endSync() {
      holding = false;
      for (const send of held.splice(0)) send();
      await expect(page.getByTestId('transport-status-banner')).toHaveCount(0);
    },
  };
}

async function seed(harness: { rpc: (method: string, body: unknown) => Promise<unknown> }, provider: string) {
  await harness.rpc('HarnessSeed', {
    projects: [
      {
        name: `banner-press-${provider}`,
        repo: {},
        threads: [
          { title: 'Upper thread', provider, turns: [{ userText: 'plan the release', items: [{ kind: 'assistant_text', summary: 'Planned.' }] }] },
          { title: 'Lower thread', provider, turns: [{ userText: 'check the build', items: [{ kind: 'assistant_text', summary: 'Checked.' }] }] },
        ],
      },
    ],
  });
}

async function center(target: Locator): Promise<Point> {
  const box = (await target.boundingBox())!;
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
}

async function touchStart(page: Page, point: Point): Promise<CDPSession> {
  const touch = await page.context().newCDPSession(page);
  await touch.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [point] });
  return touch;
}

async function touchEnd(touch: CDPSession): Promise<void> {
  await touch.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await touch.detach();
}

// The window the app gives a tap's click after its release
// (COMPAT_GRACE_MS in frontend/src/lib/utils/longPressContextMenu.ts). A
// hold that ends this long after the release ended on that window, not on
// the press's own end.
const CLICK_WINDOW_MS = 700;

// Releases the touch and reports how long after its `pointerup` the held
// row left the DOM, measured in the page. The release is read from the
// event's own timestamp: the hold ends inside `pointerup` dispatch, before a
// listener added here would run.
async function releaseAndTimeHeldRow(page: Page, touch: CDPSession): Promise<number> {
  await page.evaluate(() => {
    const record = window as unknown as { heldRowGone?: Promise<number> };
    record.heldRowGone = new Promise<number>((resolve) => {
      let releasedAt = 0;
      let removedAt = 0;
      const settle = () => {
        if (releasedAt !== 0 && removedAt !== 0) resolve(removedAt - releasedAt);
      };
      const gone = () => document.querySelector('[data-testid="transport-status-held-row"]') === null;
      window.addEventListener(
        'pointerup',
        (event) => {
          releasedAt = event.timeStamp;
          settle();
        },
        { capture: true, once: true },
      );
      if (gone()) {
        removedAt = performance.now();
        return;
      }
      const observer = new MutationObserver(() => {
        if (!gone()) return;
        observer.disconnect();
        removedAt = performance.now();
        settle();
      });
      observer.observe(document.body, { childList: true, subtree: true });
    });
  });
  await touchEnd(touch);
  return page.evaluate(() => (window as unknown as { heldRowGone: Promise<number> }).heldRowGone);
}

for (const provider of ['claude', 'codex'] as const) {
  test.describe(provider, () => {
    test('a row pressed as the sync ends stays put and opens its thread', async ({ harness, page }) => {
      const replay = await routeReplay(page);
      await seed(harness, provider);
      await harness.open(page);
      const upper = page.getByTestId('thread-row').filter({ hasText: 'Upper thread' });
      await expect(upper).toBeVisible();
      await replay.startSync();

      const pressed = (await upper.boundingBox())!;
      const touch = await touchStart(page, await center(upper));
      await replay.endSync();
      await expect(page.getByTestId('transport-status-held-row')).toHaveCount(1);
      expect(await upper.boundingBox(), 'the pressed row stays where it was pressed').toEqual(pressed);
      await touchEnd(touch);

      await expect(page.getByTestId('chat-header-title')).toHaveText('Upper thread');
      await expect(page.getByTestId('transport-status-held-row')).toHaveCount(0);
    });

    test('the Back control pressed as the sync ends stays put and returns to the list', async ({ harness, page }) => {
      const replay = await routeReplay(page);
      await seed(harness, provider);
      await harness.open(page);
      await page.getByTestId('thread-row').filter({ hasText: 'Upper thread' }).click();
      await expect(page.getByTestId('chat-header-title')).toHaveText('Upper thread');
      await replay.startSync();

      const back = page.getByTestId('compact-back');
      const pressed = (await back.boundingBox())!;
      const touch = await touchStart(page, await center(back));
      await replay.endSync();
      await expect(page.getByTestId('transport-status-held-row')).toHaveCount(1);
      expect(await back.boundingBox(), 'the pressed control stays where it was pressed').toEqual(pressed);
      await touchEnd(touch);

      await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
      await expect(page.getByTestId('transport-status-held-row')).toHaveCount(0);
    });

    test('a long press as the sync ends opens the row menu and lets the row go at its release', async ({ harness, page }) => {
      const replay = await routeReplay(page);
      await seed(harness, provider);
      await harness.open(page);
      const upper = page.getByTestId('thread-row').filter({ hasText: 'Upper thread' });
      await expect(upper).toBeVisible();
      await replay.startSync();

      const touch = await touchStart(page, await center(upper));
      await replay.endSync();
      await expect(page.getByTestId('transport-status-held-row')).toHaveCount(1);
      const menu = page.locator('[data-popover]');
      await expect(menu.getByRole('menu', { name: 'Thread Actions' })).toBeVisible();
      await expect(page.getByTestId('transport-status-held-row')).toHaveCount(1);

      // No click follows a long press, and the row is let go at the release,
      // not after the window a tap's click is given.
      expect(await releaseAndTimeHeldRow(page, touch)).toBeLessThan(CLICK_WINDOW_MS);
      await expect(menu.getByRole('menu', { name: 'Thread Actions' })).toBeVisible();
      await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
    });

    test('a drag as the sync ends opens nothing and lets the row go', async ({ harness, page }) => {
      const replay = await routeReplay(page);
      await seed(harness, provider);
      await harness.open(page);
      const upper = page.getByTestId('thread-row').filter({ hasText: 'Upper thread' });
      await expect(upper).toBeVisible();
      await replay.startSync();

      const start = await center(upper);
      const touch = await touchStart(page, start);
      await replay.endSync();
      await expect(page.getByTestId('transport-status-held-row')).toHaveCount(1);
      for (let step = 1; step <= 6; step++) {
        await touch.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: start.x, y: start.y + step * 15 }] });
      }

      // The engine takes the drag as a scroll and cancels the pointer, so
      // the row goes while the finger is still down.
      await expect(page.getByTestId('transport-status-held-row')).toHaveCount(0);
      await touchEnd(touch);
      await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
    });
  });
}
