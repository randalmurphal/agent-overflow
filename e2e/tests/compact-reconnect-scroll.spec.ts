import type { WebSocketRoute } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import { RESULT_LINE, advance, claudeScenario, emit, startMock, textLines, waitForGate } from './agent-visibility-helpers.js';

for (const mode of ['large', 'small', 'reading', 'gesture', 'gap'] as const) {
  test(`reconnect scrolling: ${mode}`, async ({ harness, page }) => {
    let online = true;
    let recovering = false;
    let releaseReplay: (() => void) | undefined;
    let connection: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
    await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
      if (!online) { void socket.close({ code: 1012 }); return; }
      const server = socket.connectToServer();
      connection = { page: socket, server };
      socket.onMessage((message) => server.send(message));
      server.onMessage((message) => {
        if (recovering && JSON.parse(String(message)).type === 'replay') {
          releaseReplay = () => socket.send(message);
        } else socket.send(message);
      });
    });
    const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{
      name: 'reconnect-scroll', repo: {}, threads: [{ title: 'Catch up', provider: 'claude',
        turns: Array.from({ length: 20 }, (_, i) => ({ userText: `Earlier question ${i}`,
          items: [{ kind: 'assistant_text', summary: `Earlier answer ${i}.\n\nMore history to read.` }],
        })),
      }],
    }] });
    const thread = seed.projects[0].threadIds[0];
    const lines = mode === 'small' ? textLines('end', 'Done.') : Array.from(
      { length: mode === 'gap' ? 180 : 30 }, (_, i) => textLines(`missed-${i}`,
        `Recovered paragraph ${i}.\n\nThis arrived while the phone was disconnected.\n\nEnd of recovered paragraph ${i}.`),
    ).flat();
    await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('scroll-reconnect', [
      emit(textLines('start', 'Working before the disconnect.')),
      { waitSignal: { name: 'finish' } }, emit([...lines, RESULT_LINE]),
    ]) });
    await harness.open(page);
    await page.getByTestId('thread-row').click();
    const mock = await startMock(harness, thread);
    await harness.rpc('SendMessage', thread, 'Continue', null);
    await waitForGate(harness, 'finish');
    const scroll = page.getByTestId('message-timeline-scroll');
    await expect(page.getByText('Working before the disconnect.', { exact: true })).toBeVisible();
    const distance = () => scroll.evaluate((el) => el.scrollHeight - el.clientHeight - el.scrollTop);
    await expect.poll(distance).toBeLessThanOrEqual(2);
    if (mode === 'reading') {
      await scroll.hover();
      await page.mouse.wheel(0, -400);
      await expect.poll(distance).toBeGreaterThan(200);
    }
    const before = await scroll.evaluate((el) => el.scrollTop);
    online = false;
    await connection!.page.close({ code: 1012 });
    await connection!.server.close();
    await advance(harness, mock, 'finish');
    await harness.waitForEvent('provider:turn_completed');

    // Frame samples distinguish one placement from a long scroll through
    // recovered history. They observe the real controller and virtualizer.
    await scroll.evaluate((el) => {
      const state = { samples: [el.scrollTop], done: false };
      (window as any).__reconnectScroll = state;
      const sample = () => {
        state.samples.push(el.scrollTop);
        if (!state.done) requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
    });
    recovering = true;
    online = true;
    await expect.poll(() => !!releaseReplay).toBe(true);
    let readingTop = before;
    if (mode === 'gesture') {
      await scroll.hover();
      await page.mouse.wheel(0, -200);
      await expect.poll(() => scroll.evaluate((el) => el.scrollTop)).toBeLessThan(before - 50);
      readingTop = await scroll.evaluate((el) => el.scrollTop);
    }
    releaseReplay!();
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);
    if (mode === 'reading' || mode === 'gesture') {
      await page.waitForTimeout(300);
      expect(await scroll.evaluate((el) => el.scrollTop)).toBeCloseTo(readingTop, 0);
      expect(await distance()).toBeGreaterThan(200);
    } else {
      await expect.poll(distance).toBeLessThanOrEqual(2);
      const samples = await page.evaluate(() => {
        const state = (window as any).__reconnectScroll;
        state.done = true;
        return state.samples as number[];
      });
      const largestStep = Math.max(0, ...samples.slice(1).map((top, i) => top - samples[i]));
      const viewport = await scroll.evaluate((el) => el.clientHeight);
      if (mode === 'small') expect(largestStep).toBeLessThan(viewport);
      else expect(largestStep).toBeGreaterThan(viewport);
    }
    await page.evaluate(() => { (window as any).__reconnectScroll.done = true; });
  });
}
