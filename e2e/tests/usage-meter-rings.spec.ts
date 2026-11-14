// Which usage rings the composer shows, through the shipped UI and the real
// backend. An account whose readings name no 5h limit gets no 5h meter, and a
// scoped weekly limit beside the account-wide one is drawn as a second ring
// inside the 7d meter, with its own threshold color and the label still clear
// of it at default zoom and at both ends of the zoom range. Driven by mock Codex account/rateLimits/updated notifications.
import { test, expect } from './fixtures.js';
import { seedAgentThread, startMock } from './agent-visibility-helpers.js';

function line(method: string, params: Record<string, unknown>): string {
  return JSON.stringify({ jsonrpc: '2.0', method, params });
}

test('a weekly-only account shows one 7d meter, with a scoped weekly limit as its inner ring', async ({ harness, page }) => {
  const weeklyReset = Math.floor(Date.now() / 1000) + 3 * 24 * 3600;
  const ids = { threadId: '${THREAD_ID}', turnId: '${TURN_ID}' };
  const turn = (label: string, limits: Record<string, unknown>[]) => ({ label, steps: [{ emit: { delayBetweenMs: 5, lines: [
    line('turn/started', { threadId: ids.threadId, turn: { id: ids.turnId } }),
    ...limits.map((rateLimits) => line('account/rateLimits/updated', { rateLimits })),
    line('item/completed', { ...ids, item: { type: 'agentMessage', id: 'msg-${TURN}', status: 'completed', text: 'Done.' } }),
    line('turn/completed', { threadId: ids.threadId, turn: { id: ids.turnId, status: 'completed' } }),
  ] } }] });
  const accountWide = {
    limitId: 'codex', limitName: 'Codex',
    secondary: { usedPercent: 40, windowDurationMins: 10080, resetsAt: weeklyReset },
  };
  const scoped = {
    limitId: 'spark', limitName: 'Spark',
    secondary: { usedPercent: 97, windowDurationMins: 10080, resetsAt: weeklyReset },
  };
  await harness.rpc('HarnessSetScenario', { scenario: {
    version: 1,
    name: 'usage-meter-rings',
    provider: 'codex',
    turns: [turn('weekly-only', [accountWide]), turn('weekly-and-scoped', [accountWide, scoped])],
    afterTurns: 'repeatLast',
  } });
  const threadId = await seedAgentThread(harness, 'usage-meter-rings', 'Usage rings', 'codex');
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await startMock(harness, threadId);

  await harness.rpc('SendMessage', threadId, 'go', null);
  await harness.waitForEvent('provider:turn_completed');

  // Weekly only: no 5h meter, and the 7d meter is today's single ring.
  const weekly = page.getByTestId('composer-rate-limit-7d');
  await expect(weekly.getByRole('button')).toHaveAttribute('aria-label', '7-day limit: 40% used');
  await expect(page.getByTestId('composer-rate-limit-5h')).toHaveCount(0);
  await expect(weekly.locator('[data-meter-arc="inner"]')).toHaveCount(0);
  await expect(weekly.locator('[data-meter-arc="main"]')).toHaveAttribute('stroke-width', '3.5');
  const singleBox = (await weekly.locator('svg').boundingBox())!;

  await harness.rpc('SendMessage', threadId, 'again', null);
  await harness.waitForEvent('provider:turn_completed');

  // A scoped weekly limit adds the inner ring; the account-wide ring keeps
  // its own color while the scoped one is in the error band.
  await expect(weekly.getByRole('button'))
    .toHaveAttribute('aria-label', '7-day limits: 40% used, Spark 97% used');
  const main = weekly.locator('[data-meter-arc="main"]');
  const inner = weekly.locator('[data-meter-arc="inner"]');
  await expect(inner).toHaveCount(1);
  await expect(main).toHaveAttribute('stroke-width', '2');
  const strokes = await weekly.locator('svg').evaluate((svg) => ({
    main: getComputedStyle(svg.querySelector('[data-meter-arc="main"]')!).stroke,
    inner: getComputedStyle(svg.querySelector('[data-meter-arc="inner"]')!).stroke,
  }));
  expect(strokes.inner).not.toBe(strokes.main);
  await expect(page.getByTestId('composer-rate-limit-5h')).toHaveCount(0);

  // The meter keeps its footprint, and the painted label sits wholly inside
  // the inner ring's inner edge (radius 9 minus half its 2-unit stroke).
  const doubleBox = (await weekly.locator('svg').boundingBox())!;
  expect(doubleBox.width).toBeCloseTo(singleBox.width, 1);
  expect(doubleBox.height).toBeCloseTo(singleBox.height, 1);
  const labelClearance = () => weekly.locator('svg').evaluate((svg) => {
    const box = svg.getBoundingClientRect();
    const unit = box.width / 28;
    const center = { x: box.left + box.width / 2, y: box.top + box.height / 2 };
    const text = svg.querySelector('text')!.getBoundingClientRect();
    const farthest = Math.max(...[text.left, text.right].flatMap((x) =>
      [text.top, text.bottom].map((y) => Math.hypot(x - center.x, y - center.y))));
    return { farthestUnits: farthest / unit, width: box.width };
  });
  expect((await labelClearance()).farthestUnits).toBeLessThan(8);

  // The same holds at both ends of the app zoom range, where the rem-sized
  // ring is a scaled svg and its root font-size is fractional.
  const defaultWidth = (await labelClearance()).width;
  for (let step = 0; step < 7; step++) await page.keyboard.press('Control+=');
  await expect.poll(async () => (await labelClearance()).width).toBeGreaterThan(defaultWidth * 1.4);
  expect((await labelClearance()).farthestUnits).toBeLessThan(8);
  await page.keyboard.press('Control+0');
  for (let step = 0; step < 3; step++) await page.keyboard.press('Control+-');
  await expect.poll(async () => (await labelClearance()).width).toBeLessThan(defaultWidth * 0.8);
  expect((await labelClearance()).farthestUnits).toBeLessThan(8);
  await page.keyboard.press('Control+0');
  await expect.poll(async () => (await labelClearance()).width).toBeCloseTo(defaultWidth, 1);

  // The hover card names both limits.
  await weekly.getByRole('button').hover();
  const tooltip = page.getByRole('tooltip');
  await expect(tooltip).toContainText('40% used');
  await expect(tooltip).toContainText('97% used');
  const names = await tooltip.locator('p[title]').allTextContents();
  expect(names).toEqual(['Codex', 'Spark']);
});
