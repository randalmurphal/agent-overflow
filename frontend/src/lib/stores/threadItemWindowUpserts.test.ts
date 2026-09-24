// The upsert commit through `threadItemWindow` directly: a batch that only
// rewrites held rows keeps the window's identity, admitted rows are placed
// by position, and the id index and windowed-row count stay exact through
// every chokepoint without a rebuild per batch.
import { flushSync } from 'svelte';
import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import type { Item } from '../types/models';
import type { TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';
import { createThreadItemWindow } from './threadItemWindow.svelte';
import { applyItemUpsertsToWindow } from './threadItemUpserts';
import { compareItemsByTimelinePosition } from './threadItems';
import { isWindowedTimelineRow } from './threadWindowDigest';

const threadId = 't';

function harness(selection?: TimelineSelection) {
  const optimisticItemIds = new Set<string>();
  const window = createThreadItemWindow({
    optimisticItemIds,
    selection: selection ? () => selection : undefined,
    streamingReveal: () => ({
      reconcileItemWrite() {},
      withReconciledItems: <T>(items: Item[], apply: (items: Item[]) => T) => apply(items),
      disposeSmoothersForItems() {},
    }) as never,
    rowUiState: () => ({ disposeItems() {} }) as never,
    activityRuns: () => ({ noteMemberContentChanged() {}, noteWholesaleReplace() {} }) as never,
    switchLoad: () => ({ noteItemMutation() {}, noteItemMutations() {}, noteItemWindowReplacement() {} }),
  });
  function upsert(incoming: Item[]) {
    const next = applyItemUpsertsToWindow({
      current: window.getItems(),
      incoming,
      itemIndexById: window.itemIndexById,
      optimisticItemIds,
      currentThreadId: threadId,
      scopeRootId: selection?.scopeRootId,
      hasMoreNewer: false,
    });
    if (next && (next.structureChanged || next.changedItems.length > 0)) {
      window.commitUpsertResult(next, () => {});
    }
    return next;
  }
  function expectConsistent() {
    const items = window.getItems();
    for (let index = 1; index < items.length; index += 1) {
      expect(compareItemsByTimelinePosition(items[index - 1], items[index])).toBeLessThanOrEqual(0);
    }
    expect(window.itemIndexById.size).toBe(items.length);
    items.forEach((item, index) => {
      expect(window.itemIndexById.get(item.id)).toBe(index);
      expect(window.getItemById(item.id)).toBe(item);
    });
    expect(window.windowedRowCount()).toBe(items.filter((item) => isWindowedTimelineRow(item, selection)).length);
  }
  return { window, upsert, expectConsistent, optimisticItemIds };
}

function row(id: string, itemIndex: number, overrides: Partial<Item> = {}): Item {
  return makeItem({ id, threadId, turnIndex: 1, itemIndex, kind: 'tool_call', toolName: 'Bash', summary: id, ...overrides });
}

/** Deterministic PRNG so a failing sequence replays. */
function prng(seed: number) {
  let state = seed >>> 0;
  return (below: number) => {
    state = (state * 1664525 + 1013904223) >>> 0;
    return state % below;
  };
}

describe('threadItemWindow upsert commit', () => {
  it('rewrites held rows in place when a batch admits nothing', () => {
    const h = harness();
    h.upsert([row('a', 0), row('b', 1), row('c', 2)]);
    const items = h.window.getItems();
    const revision = h.window.timelineRevision;
    const arrayReads = probeReactivity(() => h.window.getItems());
    const rowReads = probeReactivity(() => h.window.getItemById('b'));
    const otherRowReads = probeReactivity(() => h.window.getItemById('a'));

    const settled = row('b', 1, { status: 'completed', summary: 'done', updatedAt: 5 });
    const next = h.upsert([settled]);

    expect(next?.items).toBe(items);
    expect(h.window.getItems()).toBe(items);
    expect(items[1]).toBe(settled);
    expect(h.window.timelineRevision).toBe(revision);
    flushSync();
    expect(arrayReads.evaluations).toBe(1);
    expect(rowReads.evaluations).toBe(2);
    expect(rowReads.latest).toBe(settled);
    expect(otherRowReads.evaluations).toBe(1);
    h.expectConsistent();
    for (const probe of [arrayReads, rowReads, otherRowReads]) probe.dispose();
  });

  it('bumps the structural revision for a structural rewrite in place', () => {
    const h = harness();
    h.upsert([row('a', 0), row('b', 1)]);
    const items = h.window.getItems();
    const revision = h.window.timelineRevision;
    h.upsert([row('b', 1, { toolName: 'Read', updatedAt: 5 })]);
    expect(h.window.getItems()).toBe(items);
    expect(h.window.timelineRevision).toBe(revision + 1);
    h.expectConsistent();
  });

  it('places out-of-order rows between held rows and reindexes from the first insertion', () => {
    const h = harness();
    h.upsert([0, 10, 20, 30, 40].map((i) => row(`h${i}`, i)));
    const next = h.upsert([row('x35', 35), row('x5', 5), row('x50', 50), row('x15', 15)]);
    expect(next?.reindexFrom).toBe(1);
    expect(h.window.getItems().map((item) => item.id))
      .toEqual(['h0', 'x5', 'h10', 'x15', 'h20', 'h30', 'x35', 'h40', 'x50']);
    h.expectConsistent();
  });

  it('keeps held rows first, then arrival order, among rows sharing a position', () => {
    const h = harness();
    h.upsert([row('h0', 0), row('h1', 1), row('h2', 2)]);
    h.upsert([row('late', 1), row('later', 1), row('earlier', 0)]);
    expect(h.window.getItems().map((item) => item.id))
      .toEqual(['h0', 'earlier', 'h1', 'late', 'later', 'h2']);
    h.expectConsistent();
  });

  it('places a row admitted and re-upserted in one batch at its final position', () => {
    const h = harness();
    h.upsert([row('a', 0), row('b', 10)]);
    const next = h.upsert([row('n', 20), row('n', 5, { summary: 'moved' })]);
    expect(h.window.getItems().map((item) => item.id)).toEqual(['a', 'n', 'b']);
    expect(next?.appendedItems.map((item) => item.summary)).toEqual(['moved']);
    h.expectConsistent();
  });

  it('confirms a provisional send in place of its row and retires its id', () => {
    const h = harness();
    const meta = JSON.stringify({ sendId: 'send-1' });
    const provisional = row('local-1', 5, { kind: 'user_text', role: 'user', toolName: '', meta });
    h.upsert([row('a', 0), provisional, row('b', 10)]);
    h.optimisticItemIds.add('local-1');
    const confirmed = row('server-1', 5, { kind: 'user_text', role: 'user', toolName: '', meta, updatedAt: 3 });
    const next = h.upsert([confirmed]);
    expect(next?.replacedItems).toEqual([provisional]);
    expect(h.window.getItems().map((item) => item.id)).toEqual(['a', 'server-1', 'b']);
    expect(h.window.itemIndexById.has('local-1')).toBe(false);
    expect(h.optimisticItemIds.has('local-1')).toBe(false);
    h.expectConsistent();
  });

  it('keeps the index, boxes and windowed count exact across random batches', () => {
    for (const selection of [undefined, { scopeRootId: 'agent', tools: true }] as const) {
      const scope = selection?.scopeRootId;
      const h = harness(selection);
      const next = prng(selection ? 7 : 3);
      let serial = 0;
      const planUpdate = { kind: 'notification', toolName: 'plan_update' } as const;
      for (let step = 0; step < 300; step += 1) {
        const held = h.window.getItems();
        const op = next(10);
        if (op === 0 && held.length > 0) {
          const victims = new Set(held.filter(() => next(4) === 0).map((item) => item.id));
          h.window.dropTimelineItems((item) => victims.has(item.id));
        } else if (op === 1 && held.length > 0) {
          const index = next(held.length);
          const previous = held[index];
          h.window.writeItemAt(index, { ...previous, ...(next(2) === 0 ? planUpdate : { kind: 'tool_call', toolName: 'Bash' }) });
        } else {
          const batch: Item[] = [];
          const size = 1 + next(8);
          for (let i = 0; i < size; i += 1) {
            const choice = next(5);
            if (choice <= 1 || held.length === 0) {
              batch.push(row(`n${serial++}`, next(200), {
                parentId: scope, ...(next(4) === 0 ? planUpdate : {}), ...(next(5) === 0 ? { kind: 'assistant_text', toolName: '' } : {}),
              }));
            } else {
              const target = held[next(held.length)];
              const moved = choice === 4 && next(3) === 0;
              batch.push({
                ...target,
                summary: `${target.summary}+`,
                itemIndex: moved ? next(200) : target.itemIndex,
                ...(choice === 3 ? planUpdate : {}),
                updatedAt: target.updatedAt + 1,
              });
            }
          }
          const before = held.slice();
          const result = h.upsert(batch);
          // Placement matches a stable sort of the held rows (with their
          // final writes) followed by the admitted rows in arrival order.
          if (result && result.items !== held) {
            const written = new Map(result.rowWrites.map((write) => [write.index, write.item]));
            const expected = [...before.map((item, index) => written.get(index) ?? item), ...result.appendedItems]
              .sort(compareItemsByTimelinePosition);
            expect(h.window.getItems().map((item) => item.id)).toEqual(expected.map((item) => item.id));
          }
        }
        h.expectConsistent();
      }
      expect(h.window.getItems().length).toBeGreaterThan(20);
    }
  });
});
