// The gate's own rules at run time (tests/base-test.ts): a page sleep throws,
// an event watch serves one assertion and counts for the test that made it
// and no longer, and an event wait the test abandoned is cancelled at its
// end. The watches and waits are the ones HarnessApp.watchEvents and
// waitForEvent make (watchEventsIn, waitForEventIn), handed events directly,
// so no test here needs a backend.
import type { Page } from '@playwright/test';
import { test, expect } from './base-test.js';
import { currentEventWatchScope, EventWatchScope, openEventWatchScope, waitForEventIn, watchEventsIn, type WireEvent } from '../src/harness.js';

const PROBE = 'e2e:watch-probe';

/** Event waiters as a HarnessApp keeps them, and a feed that hands each of them an event. */
function probeEvents(): { waiters: Set<(ev: WireEvent) => void>; feed: (data: unknown, channel?: string) => void } {
  const waiters = new Set<(ev: WireEvent) => void>();
  return {
    waiters,
    feed: (data, channel = PROBE) => {
      for (const waiter of [...waiters]) waiter({ channel, seq: 1, data });
    },
  };
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

test.beforeAll(() => {
  expect(() => currentEventWatchScope(), 'a watch outside a test has nothing to end it').toThrow(/scoped to a test/);
});

test('each test runs in a watch scope of its own', () => {
  expect(() => currentEventWatchScope()).not.toThrow();
  expect(() => openEventWatchScope(), 'the fixture opened none for this test').toThrow(/already open/);
});

test('a watch counts its matches and keeps only the first, cut short', async () => {
  const { waiters, feed } = probeEvents();
  const watch = watchEventsIn<{ n: number; pad: string }>(currentEventWatchScope(), waiters, PROBE, (data) => data.n > 0);
  for (const n of [1, 0, 2, 3]) feed({ n, pad: 'x'.repeat(100_000) });
  feed({ n: 9 }, 'e2e:other-probe');
  expect(watch.count()).toBe(3);
  const error = await watch.expectNoneThrough(async () => {}, 'probe').then(() => undefined, (e: Error) => e);
  expect(error?.message).toMatch(/^probe: 3 e2e:watch-probe event\(s\) arrived before the barrier finished, want 0, first \{"n":1,"pad":"x+$/);
  expect(error!.message.length).toBeLessThan(700);

  const exact = watchEventsIn(currentEventWatchScope(), waiters, PROBE);
  feed({ n: 1 });
  feed({ n: 2 });
  await exact.expectExactlyThrough(2, async () => {}, 'two probes');
});

test('a watch serves one assertion and stops counting at it', async () => {
  const { waiters, feed } = probeEvents();
  const watch = watchEventsIn(currentEventWatchScope(), waiters, PROBE);
  await watch.expectNoneThrough(async () => {}, 'none yet');
  feed({ n: 1 });
  expect(watch.count(), 'an asserted watch kept counting').toBe(0);
  await expect(watch.expectNoneThrough(async () => {}, 'again')).rejects.toThrow(/already ended; make a new watch/);
});

test('an assertion claims its watch when it is called, so an overlapping one is refused', async () => {
  const { waiters, feed } = probeEvents();
  const watch = watchEventsIn(currentEventWatchScope(), waiters, PROBE);
  let finishBarrier!: () => void;
  const barrier = new Promise<void>((resolve) => {
    finishBarrier = resolve;
  });
  const first = watch.expectExactlyThrough(1, () => barrier, 'first');
  await expect(watch.expectNoneThrough(async () => {}, 'overlap')).rejects.toThrow(/^overlap: this e2e:watch-probe watch is serving another assertion/);
  feed({ n: 1 });
  finishBarrier();
  await first;
});

test('a barrier that fails ends its watch and its error stands', async () => {
  const { waiters, feed } = probeEvents();
  const watch = watchEventsIn(currentEventWatchScope(), waiters, PROBE);
  await expect(watch.expectExactlyThrough(1, async () => { throw new Error('barrier broke'); }, 'probe')).rejects.toThrow('barrier broke');
  feed({ n: 1 });
  expect(watch.count()).toBe(0);
  expect(waiters.size, 'an ended watch kept its waiter').toBe(0);
});

test('ending the test\'s scope ends every watch it holds, asserted or not', async () => {
  const { waiters, feed } = probeEvents();
  // The scope the fixture ends at teardown, ended here by the same call.
  const scope = currentEventWatchScope();
  const left = watchEventsIn(scope, waiters, PROBE);
  const other = watchEventsIn(scope, waiters, PROBE);
  const outside = watchEventsIn(new EventWatchScope(), waiters, PROBE);
  feed({ n: 1 });
  scope.end();
  feed({ n: 2 });
  expect(left.count(), 'an ended watch kept counting').toBe(1);
  expect(other.count(), 'an ended watch kept counting').toBe(1);
  expect(outside.count(), 'a watch in another scope was ended').toBe(2);
  await expect(left.expectNoneThrough(async () => {}, 'left')).rejects.toThrow(/already ended/);
  expect(() => currentEventWatchScope(), 'the ended scope stayed open').toThrow(/scoped to a test/);
});

/** Whether `wait` has settled, read without awaiting it. */
function settledFlag(wait: Promise<unknown>): () => boolean {
  let settled = false;
  wait.then(() => { settled = true; }, () => { settled = true; });
  return () => settled;
}

test('a wait the test\'s scope ends is cancelled: it consumes nothing and never times out', async () => {
  const { waiters } = probeEvents();
  const scope = new EventWatchScope();
  const abandoned = settledFlag(waitForEventIn(scope, [], waiters, PROBE, undefined, 10));
  expect(scope.end(), 'the scope did not report the wait it cancelled').toEqual([PROBE]);
  expect(waiters.size, 'a cancelled wait kept its listener').toBe(0);
  const later: WireEvent = { channel: PROBE, seq: 1, data: {} };
  for (const waiter of [...waiters]) waiter(later);
  expect(later.consumed, 'a cancelled wait consumed a later event').toBeUndefined();
  // A wait outside any scope times out at 30 ms; the cancelled one's 10 ms
  // timer would have fired first.
  await expect(waitForEventIn(undefined, [], new Set(), PROBE, undefined, 30)).rejects.toThrow(/timeout waiting for e2e:watch-probe/);
  expect(abandoned(), 'a cancelled wait settled at its timeout').toBe(false);
});

test('a wait that settles leaves its scope, so the scope cancels nothing', async () => {
  const { waiters, feed } = probeEvents();
  const scope = new EventWatchScope();
  const answered = waitForEventIn<{ n: number }>(scope, [], waiters, PROBE, (data) => data.n === 2, 60_000);
  feed({ n: 1 });
  feed({ n: 2 });
  expect(await answered).toEqual({ n: 2 });
  await expect(waitForEventIn(scope, [], waiters, PROBE, undefined, 1)).rejects.toThrow(/timeout/);
  expect(scope.end()).toEqual([]);
});

test('a test that ends without awaiting its wait fails at its end', () => {
  test.fail(true, 'the fixture fails the test: "the test ended without awaiting its event wait on e2e:watch-probe"');
  void waitForEventIn(currentEventWatchScope(), [], new Set(), PROBE, undefined, 60_000);
});
