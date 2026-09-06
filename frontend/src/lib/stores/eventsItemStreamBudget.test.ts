import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { buildPane, makeItem } from '../../test/helpers/chat';
import { FakeSmoothingClock, installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { applyItemStreamEvent, flushItemEventQueue, resetItemEventQueue } from './eventsItemStream';
import { resetPanesForTest } from './panes.svelte';
import { getSettings } from './settings.svelte';
import { __setSmoothingClockForTest } from './threadPaneShared';

beforeEach(installThreadPaneTestEnv);
afterEach(() => {
  resetItemEventQueue();
  resetPanesForTest();
  vi.useRealTimers();
  getSettings().lowPowerMode = false;
  __setSmoothingClockForTest(undefined);
});

it.each([100, 140_000, 400_000])('bounds batches without losing large events (%i chars)', async (size) => {
  const pane = await buildPane();
  const apply = vi.spyOn(pane, 'applyProviderItemUpserts');
  const expected = Array.from({ length: 12 }, (_, i) => makeItem({
    id: `command-${i}`, itemIndex: i, kind: 'tool_call', summary: 'x'.repeat(size),
  }));
  for (const item of expected) applyItemStreamEvent({ action: 'upsert', threadId: item.threadId, item });
  if (size === 400_000) expect(apply.mock.calls.length).toBeGreaterThan(0);
  for (let i = 0; i < expected.length; i++) flushItemEventQueue();
  expect(pane.items.map(item => item.id)).toEqual(expected.map(item => item.id));
  expect(apply.mock.calls.flatMap(([items]) => items.map(item => item.id))).toEqual(expected.map(item => item.id));
  for (const [items] of apply.mock.calls) {
    expect(items.length === 1 || items.reduce((n, item) => n + item.summary.length, 0) <= 256 * 1024).toBe(true);
  }
  if (size === 100) expect(apply).toHaveBeenCalledOnce();
  if (size > 256 * 1024) expect(apply).toHaveBeenCalledTimes(expected.length);
});

it('reset discards pending payloads and resets the pressure budget', async () => {
  const pane = await buildPane();
  const apply = vi.spyOn(pane, 'applyProviderItemUpserts');
  const stale = makeItem({ id: 'stale', summary: 'x'.repeat(1_900_000) });
  applyItemStreamEvent({ action: 'upsert', threadId: stale.threadId, item: stale });
  resetItemEventQueue();
  const current = makeItem({ id: 'current', summary: 'y'.repeat(200_000) });
  applyItemStreamEvent({ action: 'upsert', threadId: current.threadId, item: current });
  expect(apply).not.toHaveBeenCalled();
  flushItemEventQueue();
  expect(pane.items.map(item => item.id)).toEqual(['current']);
});

it('continues the untouched queue tail after a failed batch', async () => {
  const pane = await buildPane();
  vi.useFakeTimers();
  vi.spyOn(pane, 'applyProviderItemUpserts').mockImplementationOnce(() => {
    throw new Error('failed row commit');
  });
  for (const id of ['first', 'second']) {
    const item = makeItem({ id, summary: 'x'.repeat(140_000) });
    applyItemStreamEvent({ action: 'upsert', threadId: item.threadId, item });
  }
  expect(() => flushItemEventQueue()).toThrow('failed row commit');
  await vi.advanceTimersByTimeAsync(60);
  expect(pane.items.map(item => item.id)).toEqual(['second']);
});

it('preserves delta, correction, metadata and completion order across text-budget splits', async () => {
  const clock = new FakeSmoothingClock();
  __setSmoothingClockForTest(clock);
  const pane = await buildPane();
  getSettings().lowPowerMode = true;
  const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
  const base = { threadId: item.threadId, itemId: item.id, kind: item.kind };
  const suffix = 'b'.repeat(140_000);
  applyItemStreamEvent({ action: 'upsert', threadId: item.threadId, item });
  applyItemStreamEvent({ action: 'delta', ...base, delta: 'a'.repeat(140_000), updatedAt: 2 });
  applyItemStreamEvent({ action: 'patch', ...base, patch: { summary: 'corrected ', updatedAt: 3 } });
  applyItemStreamEvent({ action: 'delta', ...base, delta: suffix, updatedAt: 4 });
  applyItemStreamEvent({ action: 'meta', ...base, meta: '{}', updatedAt: 5 });
  applyItemStreamEvent({ action: 'patch', ...base, patch: { status: 'completed', updatedAt: 6 } });
  for (let i = 0; i < 6; i++) flushItemEventQueue();
  clock.tickFrame(16);
  expect(pane.getItemById(item.id)).toMatchObject({
    summary: 'corrected ' + suffix, status: 'completed', meta: '{}', updatedAt: 6,
  });
  expect(pane.revealBoundary).toBeNull();
  expect(pane.__itemSmootherCountForTest()).toBe(0);
});
