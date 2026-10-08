// The explicit jump in timelineRestore.svelte.ts (`scrollToItem`): the
// lifetimes that may end it, the outcomes it reports, and the window lookup
// it retries. Rendering and resolution through real rows are covered by
// scroll.test.ts; this suite drives the session against fakes so each
// interleaving is exact.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { LoadUntilItemResult } from '../../stores/threadPaneShared';
import type { TimelineNode } from '../../utils/subagentGrouping';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import { createTimelineRestore } from './timelineRestore.svelte';
import { getToasts } from '../../stores/toast.svelte';
import { installDiagnosticsCapture } from '../../../test/helpers/diagnostics';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import {
  clearUiRenderTrace,
  getUiRenderTraceRecords,
  setUiRenderTraceEnabled,
} from '../../utils/uiRenderTrace';

function leaf(id: string): TimelineNode {
  return { kind: 'leaf', item: { id } } as unknown as TimelineNode;
}

function nodeId(node: TimelineNode): string | undefined {
  return (node as { item?: { id?: string } }).item?.id;
}

interface Deferred {
  promise: Promise<LoadUntilItemResult>;
  resolve(result: LoadUntilItemResult): void;
}

function deferred(): Deferred {
  let resolve!: (result: LoadUntilItemResult) => void;
  const promise = new Promise<LoadUntilItemResult>((done) => { resolve = done; });
  return { promise, resolve };
}

function harness(opts: { revealed?: string[]; grouped?: string[]; nodes?: TimelineNode[]; resolveAs?: Record<string, number> } = {}) {
  const revealed = opts.nodes ?? (opts.revealed ?? ['a', 'b', 'c']).map(leaf);
  const grouped = opts.nodes ?? (opts.grouped ?? opts.revealed ?? ['a', 'b', 'c']).map(leaf);
  const lookups: Deferred[] = [];
  const pane = {
    paneId: 'pane',
    threadId: 't',
    scrollStateKey: 't',
    switchGeneration: 1,
    items: [],
    loading: false,
    hasMoreNewer: false,
    activityRuns: {},
    loadUntilItem: vi.fn((): Promise<LoadUntilItemResult> => {
      const lookup = deferred();
      lookups.push(lookup);
      return lookup.promise;
    }),
  };
  const stick = {
    markEscaped: vi.fn(),
    clearRestoreConsent: vi.fn(),
  };
  const scrollToIndex = vi.fn();
  let listRef: TimelineVirtualizerHandle | undefined = { scrollToIndex } as unknown as TimelineVirtualizerHandle;
  const restore = createTimelineRestore({
    getPane: () => pane as unknown as ThreadPane,
    stick: stick as unknown as UseStickToBottomController,
    getListRef: () => listRef,
    getScrollEl: () => undefined,
    getRevealedNodes: () => revealed,
    getGroupedNodes: () => grouped,
    windowVerified: () => true,
    resolveTimelineNode: (itemId, nodes) => {
      const index = opts.resolveAs?.[itemId] ?? nodes.findIndex((node) => nodeId(node) === itemId);
      return index < 0 ? null : { index, itemId };
    },
    persistSizePriors: () => {},
    persistSizePriorsExact: () => {},
    armWarmupWithReset: () => {},
    resetAutoLoadGates: () => {},
  });
  /** Answer the newest pending lookup and let the jump run to its end. */
  async function answer(result: LoadUntilItemResult): Promise<void> {
    lookups.at(-1)!.resolve(result);
    await tick();
    await tick();
  }
  return {
    restore,
    pane,
    stick,
    scrollToIndex,
    lookups,
    answer,
    dropListRef() { listRef = undefined; },
  };
}

function toastsSince(count: number) {
  return getToasts().slice(count);
}

describe('scrollToItem', () => {
  // Before the capture's own beforeEach, which installs its binding mock.
  beforeEach(() => {
    resetBindingMocks();
  });
  const diagnostics = installDiagnosticsCapture();

  it('centers the row once its window holds it, escaping bottom follow', async () => {
    const h = harness();
    const jump = h.restore.scrollToItem('b');
    expect(h.stick.clearRestoreConsent).toHaveBeenCalled();
    expect(h.scrollToIndex).not.toHaveBeenCalled();
    await h.answer('loaded');
    expect(await jump).toBe(true);
    expect(h.stick.markEscaped).toHaveBeenCalled();
    expect(h.scrollToIndex).toHaveBeenCalledWith(1, { align: 'center' });
  });

  it('is not cancelled by a viewport hold that starts during its lookup', async () => {
    const h = harness();
    const jump = h.restore.scrollToItem('c');
    h.restore.beginHold();
    await h.answer('loaded');
    expect(await jump).toBe(true);
    expect(h.scrollToIndex).toHaveBeenCalledWith(2, { align: 'center' });
  });

  it('ends every hold when it writes, so none restores over the landing', async () => {
    const h = harness();
    const hold = h.restore.beginHold();
    const jump = h.restore.scrollToItem('b');
    // Starting the jump leaves the hold alone: a jump that fails must not
    // strand a hold's restore.
    expect(h.restore.isHoldCurrent(hold)).toBe(true);
    const during = h.restore.beginHold();
    await h.answer('loaded');
    expect(await jump).toBe(true);
    expect(h.restore.isHoldCurrent(hold)).toBe(false);
    expect(h.restore.isHoldCurrent(during)).toBe(false);
  });

  it('leaves holds current when it ends without writing', async () => {
    const h = harness();
    const jump = h.restore.scrollToItem('gone');
    const hold = h.restore.beginHold();
    await h.answer('missing');
    expect(await jump).toBe(false);
    expect(h.restore.isHoldCurrent(hold)).toBe(true);
  });

  it('yields to a newer navigation without a word', async () => {
    const h = harness();
    const toasts = getToasts().length;
    const first = h.restore.scrollToItem('a');
    const second = h.restore.scrollToItem('c');
    h.lookups[0].resolve('loaded');
    expect(await first).toBe(false);
    await h.answer('loaded');
    expect(await second).toBe(true);
    expect(h.scrollToIndex).toHaveBeenCalledTimes(1);
    expect(h.scrollToIndex).toHaveBeenCalledWith(2, { align: 'center' });
    expect(toastsSince(toasts)).toEqual([]);
  });

  it('stops without a word when the pane switches during its lookup', async () => {
    const h = harness();
    const toasts = getToasts().length;
    const jump = h.restore.scrollToItem('b');
    h.pane.switchGeneration += 1;
    await h.answer('loaded');
    expect(await jump).toBe(false);
    expect(h.scrollToIndex).not.toHaveBeenCalled();
    expect(toastsSince(toasts)).toEqual([]);
  });

  it('yields to reader input on the scroller during its lookup', async () => {
    const h = harness();
    const toasts = getToasts().length;
    const jump = h.restore.scrollToItem('b');
    h.restore.noteReaderGesture();
    await h.answer('missing');
    expect(await jump).toBe(false);
    expect(h.scrollToIndex).not.toHaveBeenCalled();
    expect(toastsSince(toasts)).toEqual([]);
  });

  it('a gesture before the jump started does not cancel it', async () => {
    const h = harness();
    h.restore.noteReaderGesture();
    const jump = h.restore.scrollToItem('a');
    await h.answer('loaded');
    expect(await jump).toBe(true);
  });

  it('repeats a lookup a window cut superseded while it still owns the viewport', async () => {
    const h = harness();
    const jump = h.restore.scrollToItem('b');
    h.lookups[0].resolve('superseded');
    await vi.waitFor(() => expect(h.lookups).toHaveLength(2));
    await h.answer('loaded');
    expect(await jump).toBe(true);
    expect(h.scrollToIndex).toHaveBeenCalledWith(1, { align: 'center' });
  });

  it('reports a lookup that never stops being superseded, after a bounded number of tries', async () => {
    const h = harness();
    const toasts = getToasts().length;
    const jump = h.restore.scrollToItem('b');
    for (let attempt = 0; attempt < 3; attempt++) {
      await vi.waitFor(() => expect(h.lookups).toHaveLength(attempt + 1));
      h.lookups[attempt].resolve('superseded');
    }
    expect(await jump).toBe(false);
    expect(h.lookups).toHaveLength(3);
    expect(h.scrollToIndex).not.toHaveBeenCalled();
    expect(toastsSince(toasts).map((toast) => toast.message)).toEqual(['Could not show that message']);
    expect(await diagnostics.messages()).toEqual(['Timeline jump could not resolve a loaded row']);
  });

  it('tells the reader when the row is gone from the thread', async () => {
    const h = harness();
    const toasts = getToasts().length;
    const jump = h.restore.scrollToItem('gone');
    await h.answer('missing');
    expect(await jump).toBe(false);
    expect(toastsSince(toasts).map((toast) => [toast.type, toast.message])).toEqual([
      ['warning', 'Message is no longer in this thread'],
    ]);
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('adds nothing to a failed lookup the window already reported', async () => {
    const h = harness();
    const toasts = getToasts().length;
    const jump = h.restore.scrollToItem('b');
    await h.answer('failed');
    expect(await jump).toBe(false);
    expect(toastsSince(toasts)).toEqual([]);
  });

  it('reports a loaded row no node renders as a defect', async () => {
    const h = harness();
    const toasts = getToasts().length;
    const jump = h.restore.scrollToItem('unrendered');
    await h.answer('loaded');
    expect(await jump).toBe(false);
    expect(h.scrollToIndex).not.toHaveBeenCalled();
    expect(toastsSince(toasts).map((toast) => [toast.type, toast.message])).toEqual([
      ['warning', 'Could not show that message'],
    ]);
    const records = await diagnostics.all();
    expect(records.map((record) => [record.message, record.detail])).toEqual([
      ['Timeline jump could not resolve a loaded row', 'stage=no-node'],
    ]);
  });

  it('reports a loaded row when no list is mounted to scroll', async () => {
    const h = harness();
    const jump = h.restore.scrollToItem('b');
    h.dropListRef();
    await h.answer('loaded');
    expect(await jump).toBe(false);
    const records = await diagnostics.all();
    expect(records.map((record) => record.detail)).toEqual(['stage=no-list']);
  });

  it('lands on the reveal frontier for a row the reveal gate still withholds', async () => {
    const h = harness({ revealed: ['a', 'b'], grouped: ['a', 'b', 'c'] });
    const toasts = getToasts().length;
    const jump = h.restore.scrollToItem('c');
    await h.answer('loaded');
    expect(await jump).toBe(true);
    expect(h.scrollToIndex).toHaveBeenCalledWith(1, { align: 'center' });
    expect(toastsSince(toasts)).toEqual([]);
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('lands on a run that cannot reveal the row, and reports it', async () => {
    // The resolver placed the row in a run whose projected members do not
    // carry it: the run is still the best landing, and the gap is a defect.
    const run = { kind: 'activity_run', runId: 'run', children: [], mountedRows: 1, mountedFrom: 0 } as unknown as TimelineNode;
    const h = harness({ nodes: [leaf('a'), run], resolveAs: { member: 1 } });
    const toasts = getToasts().length;
    const jump = h.restore.scrollToItem('member');
    await h.answer('loaded');
    await tick();
    expect(await jump).toBe(true);
    expect(h.scrollToIndex).toHaveBeenCalledWith(1, { align: 'center' });
    expect(toastsSince(toasts)).toEqual([]);
    expect(await diagnostics.messages()).toEqual(['Timeline jump could not reveal an activity run member']);
  });

  it('traces each outcome', async () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    try {
      const h = harness();
      const landed = h.restore.scrollToItem('b');
      await h.answer('loaded');
      await landed;
      const cancelled = h.restore.scrollToItem('c');
      h.restore.noteReaderGesture();
      await h.answer('loaded');
      await cancelled;
      const records = getUiRenderTraceRecords().filter((record) => record.label === 'timeline.jump');
      expect(records.map((record) => record.data)).toEqual([
        expect.objectContaining({ itemId: 'b', outcome: 'issued', index: 1, landedItemId: 'b' }),
        expect.objectContaining({ itemId: 'c', outcome: 'cancelled', index: null }),
      ]);
    } finally {
      setUiRenderTraceEnabled(false);
      clearUiRenderTrace();
    }
  });
});
