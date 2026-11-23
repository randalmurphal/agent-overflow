// The gate's own rules at run time (tests/fixtures.ts): a page sleep throws,
// and an event watch counts for the test that made it and no longer. Events
// are fed in as wire frames, so nothing here waits on the backend.
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import type { EventWatch, HarnessApp } from '../src/harness.js';

test.describe.configure({ mode: 'serial' });

const PROBE = 'e2e:watch-probe';

/** Hands `harness` an event frame as its socket would. */
function feed(harness: HarnessApp, data: unknown, channel = PROBE): void {
  (harness as unknown as { onFrame(raw: string): void }).onFrame(JSON.stringify({ type: 'event', channel, seq: 1, data }));
}

/** The sleep the fixture refuses; the wait scan allows it here alone (WAIT_ALLOWED). */
function attemptPageSleep(page: Page): Promise<void> {
  return page.waitForTimeout(10);
}

test('a page sleep throws and names the way to wait', async ({ page, browser }) => {
  await expect(attemptPageSleep(page)).rejects.toThrow(/is a sleep, which the gate does not run/);
  await expect(attemptPageSleep(await page.context().newPage())).rejects.toThrow(/is a sleep/);
  const context = await browser.newContext();
  try {
    await expect(attemptPageSleep(await context.newPage()), 'a context the spec makes').rejects.toThrow(/is a sleep/);
  } finally {
    await context.close();
  }
});

test.beforeAll(({ harnessWorker }) => {
  expect(() => harnessWorker.watchEvents(PROBE), 'a watch outside a test has nothing to end it').toThrow(/scoped to a test/);
});

test('a watch counts its matches and keeps only the first, cut short', async ({ harness }) => {
  const watch = harness.watchEvents<{ n: number; pad: string }>(PROBE, (data) => data.n > 0);
  for (const n of [1, 0, 2, 3]) feed(harness, { n, pad: 'x'.repeat(100_000) });
  feed(harness, { n: 9 }, 'e2e:other-probe');
  expect(watch.count()).toBe(3);
  const error = await watch.expectNoneThrough(async () => {}, 'probe').then(() => undefined, (e: Error) => e);
  expect(error?.message).toMatch(/^probe: 3 e2e:watch-probe event\(s\) arrived before the barrier finished, want 0, first \{"n":1,"pad":"x+$/);
  expect(error!.message.length).toBeLessThan(700);

  const exact = harness.watchEvents(PROBE);
  feed(harness, { n: 1 });
  feed(harness, { n: 2 });
  await exact.expectExactlyThrough(2, async () => {}, 'two probes');
});

test('a watch serves one assertion and stops counting at it', async ({ harness }) => {
  const watch = harness.watchEvents(PROBE);
  await watch.expectNoneThrough(async () => {}, 'none yet');
  feed(harness, { n: 1 });
  expect(watch.count(), 'an asserted watch kept counting').toBe(0);
  await expect(watch.expectNoneThrough(async () => {}, 'again')).rejects.toThrow(/already ended; make a new watch/);
});

test('a barrier that fails ends its watch and its error stands', async ({ harness }) => {
  const watch = harness.watchEvents(PROBE);
  await expect(watch.expectExactlyThrough(1, async () => { throw new Error('barrier broke'); }, 'probe')).rejects.toThrow('barrier broke');
  feed(harness, { n: 1 });
  expect(watch.count()).toBe(0);
});

let leftover: EventWatch | undefined;

test('a watch the test never asserts on', async ({ harness }) => {
  leftover = harness.watchEvents(PROBE);
  feed(harness, { n: 1 });
  expect(leftover.count()).toBe(1);
});

test('the next test sees none of it', async ({ harness }) => {
  feed(harness, { n: 2 });
  expect(leftover!.count(), 'the earlier test\'s watch counted into this one').toBe(1);
  await expect(leftover!.expectNoneThrough(async () => {}, 'leftover')).rejects.toThrow(/already ended/);
});
