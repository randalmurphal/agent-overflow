// Whole-thread message rail navigation across unloaded history, with real
// composer clearance, tick hit targets, and matching overflow chevrons.
import { test, expect, type SeedResult } from './fixtures.js';

test('overflow chevrons make repeated trips between first and latest user messages', async ({ harness, page }) => {
  const turns = Array.from({ length: 160 }, (_, i) => ({
    userText: `Rail question ${i}`,
    items: [{ kind: 'assistant_text', summary: `Rail answer ${i}\n\n${'A detailed answer. '.repeat(40)}` }],
  }));
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'message-rail', repo: {}, threads: [{ title: 'Rail navigation', turns }] }],
  });
  await harness.open(page);
  await page.getByText('Rail navigation', { exact: true }).click();

  const rail = page.getByTestId('message-nav-rail');
  const first = rail.getByRole('button', { name: 'Jump to first message', exact: true });
  const latest = rail.getByRole('button', { name: 'Jump to latest message', exact: true });
  const scroller = page.getByTestId('message-timeline-scroll');
  await expect(rail.locator('.nav-rail-tick')).toHaveCount(turns.length);
  await expect.poll(() => rail.locator('.nav-rail-tick').evaluateAll((ticks) => (
    ticks[1].getBoundingClientRect().top - ticks[0].getBoundingClientRect().top
  ))).toBeCloseTo(12, 1);

  for (let trip = 0; trip < 2; trip++) {
    await first.click();
    await expect(scroller.getByText('Rail question 0', { exact: true })).toBeInViewport();
    await expect(latest).toBeVisible();
    await expect(first).toBeHidden();
    await latest.click();
    await expect(scroller.getByText('Rail question 159', { exact: true })).toBeInViewport();
    await expect(latest).toBeHidden();
    await expect(first).toBeVisible();
  }
});
