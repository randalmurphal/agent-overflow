import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Item } from '../types/models';
import type { SmoothingClock } from '../markdown/smoothing/PerItemSmoother';
import { makeItem } from '../../test/helpers/chat';
import { __setSmoothingClockForTest } from './threadPaneShared';
import { createThreadStreamingReveal } from './threadStreamingReveal.svelte';
import {
  PROMOTION_PRESENT_CEILING_MS,
  createThreadRowPromotions,
} from './threadRowPromotions.svelte';
import { getSettings, resetSettingsForTest } from './settings.svelte';

class FakeSmoothingClock implements SmoothingClock {
  private nextHandle = 1;
  private readonly pending = new Map<number, () => void>();
  private current = 0;

  now(): number {
    return this.current;
  }

  schedule(callback: () => void): number {
    const handle = this.nextHandle++;
    this.pending.set(handle, callback);
    return handle;
  }

  cancel(handle: number): void {
    this.pending.delete(handle);
  }

  tick(ms: number): void {
    for (let left = ms; left > 0; left -= 16) {
      this.current += Math.min(16, left);
      const pending = [...this.pending.values()];
      this.pending.clear();
      for (const callback of pending) callback();
    }
  }
}

/** One fake-timer registry the hold store can be driven from. */
function fakeTimers() {
  let nextHandle = 1;
  const pending = new Map<number, { handler: () => void; ms: number }>();
  return {
    setTimeout: (handler: () => void, ms: number) => {
      const handle = nextHandle++;
      pending.set(handle, { handler, ms });
      return handle;
    },
    clearTimeout: (handle: unknown) => {
      pending.delete(handle as number);
    },
    pendingDelays: () => [...pending.values()].map((entry) => entry.ms),
    fire: () => {
      const entries = [...pending.values()];
      pending.clear();
      for (const entry of entries) entry.handler();
    },
  };
}

const NARRATION = Array.from({ length: 48 }, (_, index) => `word${index}`).join(' ') + '.';

function thinkingRow(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'think',
    kind: 'thinking',
    status: 'streaming',
    summary: '',
    itemIndex: 0,
    rev: -1,
    ...overrides,
  });
}

function proseRow(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'think',
    kind: 'assistant_text',
    status: 'completed',
    summary: NARRATION,
    itemIndex: 0,
    meta: '{"narration":true}',
    rev: 7,
    updatedAt: 5,
    ...overrides,
  });
}

function toolRow(): Item {
  return makeItem({ id: 'tool', kind: 'tool_call', status: 'completed', summary: 'Bash', itemIndex: 1 });
}

/** A reveal over a window whose commits go through the reveal chokepoint, as a pane's do. */
function harness(initial: Item[]) {
  let items = initial;
  let indexById = new Map(items.map((item, index) => [item.id, index]));
  const installed: Item[] = [];
  let armed = 0;
  const install = (next: Item[]): void => {
    items = next;
    indexById = new Map(items.map((item, index) => [item.id, index]));
  };
  const upsert = (rows: Item[]): void => {
    reveal.withReconciledItems(rows, (prepared) => {
      const next = items.slice();
      for (const row of prepared) {
        const index = indexById.get(row.id);
        if (index === undefined) next.push(row);
        else next[index] = row;
      }
      install(next);
    }, true);
  };
  const reveal = createThreadStreamingReveal({
    getItemById: (itemId) => {
      const index = indexById.get(itemId);
      return index === undefined ? undefined : items[index];
    },
    getItemIndex: (itemId) => indexById.get(itemId),
    getItems: () => items,
    setItemAt: (index, item) => {
      const next = items.slice();
      next[index] = item;
      items = next;
    },
    appendDirectAssistantLiteral: (index, _itemId, append, updatedAt, streamEnd) => {
      const current = items[index];
      current.summary = append.next;
      current.streamEnd = streamEnd;
      current.updatedAt = Math.max(current.updatedAt, updatedAt);
    },
    stampLiveContent: () => {},
    armStructuralSpring: () => { armed += 1; },
    appendLivePayloadDeltaForItem: () => {},
    commitPromotedRow: (row) => {
      installed.push(row);
      upsert([row]);
    },
  });
  const row = (id = 'think') => items[indexById.get(id)!];
  return {
    reveal,
    row,
    upsert,
    installed,
    armedCount: () => armed,
    delta(text: string, id = 'think') {
      reveal.appendStreamingDelta(row(id), text, undefined, 2);
    },
    /** A window replacement that hands over no disposal list. */
    replaceWindow(next: Item[]) {
      reveal.withReconciledItems(next, (prepared) => install(prepared));
    },
    remove(id: string) {
      reveal.withReconciledItems([], () => {
        const dropped = items.filter((item) => item.id === id);
        install(items.filter((item) => item.id !== id));
        reveal.disposeSmoothersForItems(dropped);
      });
    },
  };
}

/** Let the settle microtask a gate pass queued run. */
async function settle(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}

/** Drain the thinking reveal to its last word, one painted frame at a time. */
function drainThinking(h: ReturnType<typeof harness>, clock: FakeSmoothingClock, text = NARRATION): void {
  for (let frame = 0; frame < 4_000; frame++) {
    if (h.reveal.liveThinkingTailFor('think') === text) return;
    clock.tick(16);
  }
  throw new Error('thinking reveal did not drain');
}

function startHeldNarration() {
  const clock = new FakeSmoothingClock();
  __setSmoothingClockForTest(clock);
  const h = harness([thinkingRow(), toolRow()]);
  h.upsert([thinkingRow()]);
  h.delta(NARRATION);
  clock.tick(48);
  expect(h.reveal.liveThinkingTailFor('think')!.length).toBeLessThan(NARRATION.length);
  h.upsert([proseRow()]);
  return { clock, h };
}

afterEach(() => {
  __setSmoothingClockForTest(undefined);
  resetSettingsForTest();
  vi.useRealTimers();
});

describe('row promotion holds', () => {
  it('asks a presenter once, then installs when it completes', () => {
    const timers = fakeTimers();
    const commits: Item[] = [];
    const promotions = createThreadRowPromotions({ commit: (row) => commits.push(row), ...timers });
    const asked: string[] = [];
    promotions.registerPresenter((itemId) => {
      asked.push(itemId);
      return true;
    });

    promotions.hold(proseRow());
    promotions.presentDrained();
    expect(asked).toEqual([]);
    expect(promotions.phase('think')).toBe('draining');

    promotions.markDrained('think');
    promotions.presentDrained();
    promotions.presentDrained();
    expect(asked).toEqual(['think']);
    expect(promotions.phase('think')).toBe('presenting');
    expect(promotions.presenting).toEqual([{ itemId: 'think', row: proseRow() }]);
    expect(timers.pendingDelays()).toEqual([PROMOTION_PRESENT_CEILING_MS]);
    expect(commits).toEqual([]);

    promotions.complete('think');
    promotions.complete('think');
    expect(commits).toEqual([proseRow()]);
    expect(promotions.presenting).toEqual([]);
    expect(promotions.size).toBe(0);
    expect(timers.pendingDelays()).toEqual([]);
  });

  it('installs at once when no presenter takes the hold', () => {
    const commits: Item[] = [];
    const promotions = createThreadRowPromotions({ commit: (row) => commits.push(row), ...fakeTimers() });
    promotions.registerPresenter(() => false);
    promotions.hold(proseRow());
    promotions.markDrained('think');
    promotions.presentDrained();
    expect(commits).toEqual([proseRow()]);
    expect(promotions.presenting).toEqual([]);
  });

  it('installs at the ceiling when the presenter never reports', () => {
    const timers = fakeTimers();
    const commits: Item[] = [];
    const promotions = createThreadRowPromotions({ commit: (row) => commits.push(row), ...timers });
    promotions.registerPresenter(() => true);
    promotions.hold(proseRow());
    promotions.markDrained('think');
    promotions.presentDrained();
    expect(commits).toEqual([]);
    timers.fire();
    expect(commits).toEqual([proseRow()]);
    expect(promotions.size).toBe(0);
  });

  it('installs the holds of a presenter that unregisters, and no others', () => {
    const commits: string[] = [];
    const promotions = createThreadRowPromotions({ commit: (row) => commits.push(row.id), ...fakeTimers() });
    const unregister = promotions.registerPresenter((itemId) => itemId === 'think');
    promotions.registerPresenter((itemId) => itemId === 'other');
    promotions.hold(proseRow());
    promotions.hold(proseRow({ id: 'other' }));
    promotions.markDrained('think');
    promotions.markDrained('other');
    promotions.presentDrained();
    unregister();
    unregister();
    expect(commits).toEqual(['think']);
    expect(promotions.presenting.map((entry) => entry.itemId)).toEqual(['other']);
  });

  it('folds later reads and patches into the held row', () => {
    const commits: Item[] = [];
    const promotions = createThreadRowPromotions({ commit: (row) => commits.push(row), ...fakeTimers() });
    promotions.registerPresenter(() => true);
    promotions.hold(proseRow());
    expect(promotions.patch('absent', { meta: '{}' })).toBe(false);
    expect(promotions.patch('think', { meta: '{"narration":true,"discardedBy":"x"}', rev: 9 })).toBe(true);
    promotions.markDrained('think');
    promotions.presentDrained();
    promotions.hold(proseRow({ summary: `${NARRATION} More.`, meta: '{"narration":true,"discardedBy":"x"}', rev: 10 }));
    expect(promotions.presenting[0].row.summary).toBe(`${NARRATION} More.`);
    promotions.patch('think', { status: 'killed', updatedAt: 11 });
    expect(promotions.presenting[0].row).toMatchObject({ status: 'killed', updatedAt: 11, rev: 10 });
    promotions.complete('think');
    expect(commits).toHaveLength(1);
    expect(commits[0]).toMatchObject({
      summary: `${NARRATION} More.`,
      status: 'killed',
      meta: '{"narration":true,"discardedBy":"x"}',
    });
  });

  it('forgets dropped and cleared holds without installing them', () => {
    const timers = fakeTimers();
    const commits: Item[] = [];
    const promotions = createThreadRowPromotions({ commit: (row) => commits.push(row), ...timers });
    promotions.registerPresenter(() => true);
    promotions.hold(proseRow());
    promotions.hold(proseRow({ id: 'other' }));
    promotions.markDrained('think');
    promotions.markDrained('other');
    promotions.presentDrained();
    promotions.drop('think');
    expect(promotions.presenting.map((entry) => entry.itemId)).toEqual(['other']);
    promotions.clear();
    expect(promotions.presenting).toEqual([]);
    expect(timers.pendingDelays()).toEqual([]);
    timers.fire();
    expect(commits).toEqual([]);
  });

  it('installs every hold, in any phase, on completeAll', () => {
    const commits: string[] = [];
    const promotions = createThreadRowPromotions({ commit: (row) => commits.push(row.id), ...fakeTimers() });
    promotions.registerPresenter(() => true);
    promotions.hold(proseRow());
    promotions.hold(proseRow({ id: 'other' }));
    promotions.markDrained('think');
    promotions.presentDrained();
    promotions.completeAll();
    expect(commits).toEqual(['think', 'other']);
    expect(promotions.size).toBe(0);
  });
});

describe('streaming reveal narration promotion', () => {
  it('keeps the thinking row revealing to its last word, then installs the prose row', async () => {
    const { clock, h } = startHeldNarration();

    // The settled read did not replace the row or cut the reveal.
    expect(h.row()).toMatchObject({ kind: 'thinking', status: 'streaming' });
    expect(h.reveal.isSmoothing('think')).toBe(true);
    expect(h.reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    await settle();
    expect(h.installed).toEqual([]);

    drainThinking(h, clock);
    // Drained, but the install is not part of the reveal frame.
    expect(h.row().kind).toBe('thinking');
    expect(h.reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

    await settle();
    expect(h.installed).toEqual([proseRow()]);
    expect(h.row()).toEqual(proseRow());
    expect(h.reveal.isSmoothing('think')).toBe(false);
    expect(h.reveal.liveThinkingTailFor('think')).toBeNull();
    expect(h.reveal.revealBoundary).toBeNull();
    expect(h.reveal.smootherCount()).toBe(0);
    // The tool row behind the gate was released by the install.
    expect(h.armedCount()).toBe(1);
  });

  it('holds the gate through a presentation and releases it when the presenter completes', async () => {
    const { clock, h } = startHeldNarration();
    const asked: Array<{ itemId: string; kind: string; smoothing: boolean }> = [];
    h.reveal.registerRowPromotionPresenter((itemId, row) => {
      asked.push({ itemId, kind: row.kind, smoothing: h.reveal.isSmoothing(itemId) });
      return true;
    });

    drainThinking(h, clock);
    await settle();

    // The presenter is asked once the smoother has settled, and the row
    // still shows the text it revealed.
    expect(asked).toEqual([{ itemId: 'think', kind: 'assistant_text', smoothing: false }]);
    expect(h.reveal.presentingRowPromotions).toEqual([{ itemId: 'think', row: proseRow() }]);
    expect(h.row()).toMatchObject({ kind: 'thinking', status: 'streaming' });
    expect(h.reveal.liveThinkingTailFor('think')).toBe(NARRATION);
    expect(h.reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    expect(h.reveal.smootherCount()).toBe(1);
    expect(h.armedCount()).toBe(0);

    clock.tick(500);
    await settle();
    expect(h.installed).toEqual([]);

    h.reveal.completeRowPromotion('think');
    expect(h.row()).toEqual(proseRow());
    expect(h.reveal.presentingRowPromotions).toEqual([]);
    expect(h.reveal.revealBoundary).toBeNull();
    expect(h.armedCount()).toBe(1);
  });

  it('installs at the ceiling when a presenter stalls', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { clock, h } = startHeldNarration();
    h.reveal.registerRowPromotionPresenter(() => true);
    drainThinking(h, clock);
    await settle();
    expect(h.row().kind).toBe('thinking');

    vi.advanceTimersByTime(PROMOTION_PRESENT_CEILING_MS - 1);
    expect(h.row().kind).toBe('thinking');
    vi.advanceTimersByTime(1);
    expect(h.row()).toEqual(proseRow());
    expect(h.reveal.revealBoundary).toBeNull();
  });

  it('reveals text that lands in the same flush after the settled read', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const h = harness([thinkingRow(), toolRow()]);
    h.upsert([thinkingRow()]);
    const head = NARRATION.slice(0, 40);
    h.delta(head);
    drainThinking(h, clock, head);
    // One flush applies its upserts before its deltas.
    h.upsert([proseRow()]);
    h.delta(NARRATION.slice(40));
    await settle();

    expect(h.installed).toEqual([]);
    expect(h.row().kind).toBe('thinking');
    drainThinking(h, clock);
    await settle();
    expect(h.row()).toEqual(proseRow());
  });

  it('folds patches and meta for the id into the held row, not the row on screen', async () => {
    const { clock, h } = startHeldNarration();
    expect(h.reveal.applyPatch('think', { rev: 8, meta: '{"narration":true,"pathRefs":[]}' })).toBeNull();
    expect(h.reveal.patchHeldRow('tool', { meta: '{}' })).toBe(false);
    expect(h.row()).toMatchObject({ kind: 'thinking', status: 'streaming' });
    expect(h.row().meta).toBeUndefined();

    drainThinking(h, clock);
    await settle();
    expect(h.row()).toEqual(proseRow({ rev: 8, meta: '{"narration":true,"pathRefs":[]}' }));
  });

  it('keeps a later read of the prose row held, and installs the latest', async () => {
    const { clock, h } = startHeldNarration();
    h.reveal.registerRowPromotionPresenter(() => true);
    drainThinking(h, clock);
    await settle();

    const later = proseRow({ rev: 12, updatedAt: 9, meta: '{"narration":true,"codeSpans":[]}' });
    h.upsert([later]);
    expect(h.row().kind).toBe('thinking');
    expect(h.reveal.presentingRowPromotions).toEqual([{ itemId: 'think', row: later }]);

    h.reveal.completeRowPromotion('think');
    expect(h.row()).toEqual(later);
  });

  it('installs held rows at once on a snap, presented or not', async () => {
    const { h } = startHeldNarration();
    h.reveal.snapAllToReceived();
    expect(h.row()).toEqual(proseRow());
    expect(h.reveal.revealBoundary).toBeNull();
    expect(h.reveal.smootherCount()).toBe(0);
    await settle();
    expect(h.installed).toHaveLength(1);

    const again = startHeldNarration();
    again.h.reveal.registerRowPromotionPresenter(() => true);
    drainThinking(again.h, again.clock);
    await settle();
    expect(again.h.reveal.presentingRowPromotions).toHaveLength(1);
    again.h.reveal.snapAllToReceived();
    expect(again.h.row()).toEqual(proseRow());
    expect(again.h.reveal.presentingRowPromotions).toEqual([]);
  });

  it('installs held rows on the test flush', () => {
    const { h } = startHeldNarration();
    h.reveal.__flushForTest();
    expect(h.row()).toEqual(proseRow());
    expect(h.reveal.__smootherCountForTest()).toBe(0);
  });

  it('drops a hold with the pane, installing nothing', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { clock, h } = startHeldNarration();
    h.reveal.registerRowPromotionPresenter(() => true);
    drainThinking(h, clock);
    await settle();

    h.reveal.disposeAll();
    expect(h.reveal.presentingRowPromotions).toEqual([]);
    expect(h.reveal.revealBoundary).toBeNull();
    vi.advanceTimersByTime(PROMOTION_PRESENT_CEILING_MS * 2);
    await settle();
    expect(h.installed).toEqual([]);
    expect(h.row().kind).toBe('thinking');
  });

  it('drops the hold of a row removed from the window', async () => {
    const { clock, h } = startHeldNarration();
    h.remove('think');
    expect(h.reveal.revealBoundary).toBeNull();
    clock.tick(2_000);
    await settle();
    expect(h.installed).toEqual([]);
    expect(h.reveal.smootherCount()).toBe(0);
  });

  it('installs nothing for a row removed while a presenter shows it', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { clock, h } = startHeldNarration();
    h.reveal.registerRowPromotionPresenter(() => true);
    drainThinking(h, clock);
    await settle();
    expect(h.reveal.presentingRowPromotions).toHaveLength(1);

    h.remove('think');
    expect(h.reveal.presentingRowPromotions).toEqual([]);
    expect(h.reveal.revealBoundary).toBeNull();
    vi.advanceTimersByTime(PROMOTION_PRESENT_CEILING_MS * 2);
    h.reveal.completeRowPromotion('think');
    expect(h.installed).toEqual([]);
  });

  it('installs nothing when a presented row left the window without disposal', async () => {
    const { clock, h } = startHeldNarration();
    h.reveal.registerRowPromotionPresenter(() => true);
    drainThinking(h, clock);
    await settle();

    h.replaceWindow([toolRow()]);
    h.reveal.completeRowPromotion('think');
    expect(h.installed).toEqual([]);
    expect(h.reveal.presentingRowPromotions).toEqual([]);
    expect(h.reveal.revealBoundary).toBeNull();
    expect(h.reveal.smootherCount()).toBe(0);
  });

  it('holds a block whose trimmed tail is a prefix of its text', async () => {
    // A reasoning row publishes the tail of its text. Repetitive text makes
    // that tail a prefix of the whole, which is the shape the rewind
    // tripwire reads as a rewind on a prose row.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const text = 'aaaa '.repeat(1_200);
    const h = harness([thinkingRow(), toolRow()]);
    h.upsert([thinkingRow()]);
    h.delta(text);
    for (let frame = 0; frame < 40_000 && h.reveal.liveThinkingTailFor('think') !== text; frame++) clock.tick(16);
    expect(h.reveal.liveThinkingTailFor('think')).toBe(text);
    expect(h.row().summary.length).toBeLessThan(text.length);
    expect(text.startsWith(h.row().summary)).toBe(true);
    expect(() => h.upsert([proseRow({ summary: text })])).not.toThrow();
    expect(h.row().kind).toBe('thinking');
  });

  it('drops the hold of a row that left the window without disposal', async () => {
    const { clock, h } = startHeldNarration();
    h.replaceWindow([toolRow()]);
    await settle();
    clock.tick(2_000);
    await settle();
    expect(h.installed).toEqual([]);
    expect(h.reveal.smootherCount()).toBe(0);
    expect(h.reveal.revealBoundary).toBeNull();
  });

  it('installs a settled read directly when no reveal is running', async () => {
    const h = harness([thinkingRow({ status: 'completed', summary: 'earlier words' }), toolRow()]);
    h.upsert([proseRow()]);
    expect(h.row()).toEqual(proseRow());
    await settle();
    expect(h.installed).toEqual([]);
    expect(h.reveal.revealBoundary).toBeNull();
  });

  it('does not hold a read for a row with no reveal, even one owed a re-read', async () => {
    // No upsert opened a stream here, so the row has no smoother. The patch
    // names text past what the pane holds, which marks the row stale and
    // sends its next read through the chokepoint.
    const h = harness([thinkingRow({ summary: 'hello', streamEnd: 5 }), toolRow()]);
    h.reveal.applyPatch('think', { rev: 2, streamEnd: 50 });
    expect(h.reveal.staleRowIds().has('think')).toBe(true);
    expect(h.reveal.isSmoothing('think')).toBe(false);

    h.upsert([proseRow()]);
    expect(h.row()).toEqual(proseRow());
    await settle();
    expect(h.installed).toEqual([]);
  });

  it('does not hold an interrupted read: the snap path owns it', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const h = harness([thinkingRow(), toolRow()]);
    h.upsert([thinkingRow()]);
    h.delta(NARRATION);
    h.upsert([proseRow({ status: 'killed' })]);
    expect(h.row()).toEqual(proseRow({ status: 'killed' }));
    expect(h.reveal.isSmoothing('think')).toBe(false);
    await settle();
    expect(h.installed).toEqual([]);
  });

  it('installs without waiting when the reveal is immediate', async () => {
    getSettings().lowPowerMode = true;
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const h = harness([thinkingRow(), toolRow()]);
    h.upsert([thinkingRow()]);
    h.delta(NARRATION);
    clock.tick(16);
    h.upsert([proseRow()]);
    expect(h.row().kind).toBe('thinking');
    await settle();
    expect(h.row()).toEqual(proseRow());
  });

  it('holds a later narration block behind the frontier until its turn', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const first = makeItem({ id: 'text', kind: 'assistant_text', status: 'streaming', summary: '', itemIndex: 0, rev: -1 });
    const h = harness([first, thinkingRow({ itemIndex: 1 })]);
    h.upsert([first, thinkingRow({ itemIndex: 1 })]);
    h.reveal.appendStreamingDelta(h.row('text'), NARRATION, undefined, 2);
    h.delta(NARRATION);
    h.upsert([proseRow({ itemIndex: 1 })]);
    await settle();

    // The frontier is the earlier row; the held block has not started.
    expect(h.reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    expect(h.row('think').kind).toBe('thinking');
    expect(h.installed).toEqual([]);

    h.upsert([{ ...h.row('text'), status: 'completed', summary: NARRATION, rev: 3 }]);
    for (let frame = 0; frame < 4_000 && h.row('think').kind === 'thinking'; frame++) {
      clock.tick(16);
      await settle();
    }
    expect(h.row('think')).toEqual(proseRow({ itemIndex: 1 }));
    expect(h.reveal.revealBoundary).toBeNull();
  });
});
