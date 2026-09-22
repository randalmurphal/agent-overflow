// The two tail facts the projection feeds the run pass, through a real pane:
//
// - `windowReachesTail` (from `pane.hasMoreNewer`): a window loaded short of
//   the thread's newest item has no tail run.
// - `windowVerified` (from `pane.loading`): a warm re-entry paints its cached
//   window before the sync answers. The cached tail run renders open on that
//   guess, but the open hold is recorded only once the sync has verified the
//   window — so a run the sync displaces takes the defaults, and a run the
//   sync confirms as the tail keeps its hold when prose later displaces it.

import { beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, installPaneMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { makeSettings } from '../../../test/helpers/settings';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import type { ActivityRunNode, TimelineNode } from '../../utils/subagentGrouping';
import { createTimelineRowProjection } from './timelineRowProjection.svelte';

const THREAD = 't-run';

function tool(id: string, itemIndex: number, turnIndex = 1): Item {
  return makeItem({
    id, threadId: THREAD, turnIndex, itemIndex,
    kind: 'tool_call', toolName: 'Bash', summary: id,
  });
}

function prose(id: string, itemIndex: number, turnIndex = 1): Item {
  return makeItem({ id, threadId: THREAD, turnIndex, itemIndex, kind: 'assistant_text', summary: id });
}

function user(): Item {
  return makeItem({ id: 'u0', threadId: THREAD, turnIndex: 1, itemIndex: 0, kind: 'user_text', role: 'user', summary: 'go' });
}

interface MountedProjection {
  readonly evaluations: number;
  readonly nodes: TimelineNode[];
  dispose(): void;
}

function mountProjection(pane: ThreadPane): MountedProjection {
  let evaluations = 0;
  let nodes: TimelineNode[] = [];
  const dispose = $effect.root(() => {
    const projection = createTimelineRowProjection({ getPane: () => pane });
    $effect(() => {
      nodes = projection.revealedNodes;
      evaluations += 1;
    });
  });
  flushSync();
  return {
    get evaluations() {
      return evaluations;
    },
    get nodes() {
      return nodes;
    },
    dispose,
  };
}

function runs(nodes: readonly TimelineNode[]): ActivityRunNode[] {
  return nodes.filter((node): node is ActivityRunNode => node.kind === 'activity_run');
}

function page(items: Item[]) {
  return { items, oldestTurnIndex: 1, newestTurnIndex: 1, hasMore: false, hasMoreOlder: false, hasMoreNewer: false,
    oldestCursor: { turnIndex: 1, itemIndex: items[0].itemIndex, itemId: items[0].id },
    newestCursor: { turnIndex: 1, itemIndex: items.at(-1)!.itemIndex, itemId: items.at(-1)!.id } };
}

/**
 * First visit ends inside a run and is verified by the switch; leaving
 * caches that window; the return paints it and holds the sync open until
 * the test answers.
 */
async function reenter(cachedItems: Item[]): Promise<{
  pane: ThreadPane;
  projection: MountedProjection;
  switching: Promise<void>;
  answer: (response: unknown) => void;
  syncCalls: () => number;
}> {
  const pane = await buildPane(makeThread({ id: THREAD }), cachedItems);
  const projection = mountProjection(pane);
  setBindingMock('ListThreadSliceAround', async () => page([]));
  await pane.switchThread(makeThread({ id: 't-other' }));
  flushSync();
  expect(projection.nodes).toEqual([]);

  let answer!: (response: unknown) => void;
  let calls = 0;
  setBindingMock('SyncThreadWindow', () => {
    calls += 1;
    return new Promise((resolve) => {
      answer = resolve;
    });
  });
  const switching = pane.switchThread(makeThread({ id: THREAD }));
  flushSync();
  return { pane, projection, switching, answer: (r) => answer(r), syncCalls: () => calls };
}

describe('tail facts through the pane', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => makeSettings({ activityRunDefault: 'collapsed' }));
    await loadSettings();
  });

  it('a window short of the thread tail has no tail run', async () => {
    const items = [user(), prose('p0', 1), tool('t1', 2)];
    installPaneMocks(items);
    setBindingMock('ListThreadSliceAround', async () => ({ ...page(items), hasMoreNewer: true }));
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: THREAD }));
    const projection = mountProjection(pane);
    try {
      expect(pane.hasMoreNewer).toBe(true);
      const [run] = runs(projection.nodes);
      expect(run.atTail).toBe(false);
      expect(run.live).toBe(false);
      expect(run.collapsed).toBe(true);
      expect(pane.activityRuns.openedLiveRunIds()).toEqual([]);
    } finally {
      projection.dispose();
    }
  });

  it('paints the cached tail run open without a hold, then collapses it when the sync displaces it', async () => {
    const { pane, projection, switching, answer } = await reenter([user(), tool('t1', 1)]);
    try {
      expect(pane.loading).toBe(true);
      const [painted] = runs(projection.nodes);
      expect(painted.atTail).toBe(true);
      expect(painted.collapsed).toBe(false);
      expect(pane.activityRuns.openedLiveRunIds()).toEqual([]);

      // The turn finished while the thread was closed: prose displaced the
      // cached run and a later run sits at the tail, already followed by
      // the final response.
      answer({
        status: 'stale', epoch: 2, rev: 2, generation: 'test-generation',
        page: page([user(), tool('t1', 1), prose('p1', 2), tool('t2', 3), prose('p2', 4)]),
      });
      await switching;
      flushSync();

      expect(pane.loading).toBe(false);
      const [first, second] = runs(projection.nodes);
      expect(first.collapsed).toBe(true);
      expect(second.collapsed).toBe(true);
      expect(pane.activityRuns.openedLiveRunIds()).toEqual([]);
    } finally {
      projection.dispose();
    }
  });

  it('records the hold once the sync confirms the cached run is still the tail', async () => {
    const { pane, projection, switching, answer } = await reenter([user(), tool('t1', 1)]);
    try {
      expect(pane.activityRuns.openedLiveRunIds()).toEqual([]);

      // Still running: the run grew while the thread was closed.
      answer({
        status: 'stale', epoch: 2, rev: 2, generation: 'test-generation',
        page: page([user(), tool('t1', 1), tool('t2', 2)]),
      });
      await switching;
      flushSync();

      const [live] = runs(projection.nodes);
      expect(live.atTail).toBe(true);
      expect(live.collapsed).toBe(false);
      expect(pane.activityRuns.openedLiveRunIds()).toEqual([live.runId]);

      // Prose displaces it in front of the reader: the hold keeps it open.
      pane.upsertItem(prose('p1', 3));
      flushSync();
      const [held] = runs(projection.nodes);
      expect(held.runId).toBe(live.runId);
      expect(held.atTail).toBe(false);
      expect(held.collapsed).toBe(false);
    } finally {
      projection.dispose();
    }
  });

  it('re-resolves on verification alone when the sync installs nothing', async () => {
    // `fresh` keeps the painted rows: no structural change, so the pass
    // that records the confirmed tail's hold has only `pane.loading` to
    // wake on.
    const { pane, projection, switching, answer, syncCalls } = await reenter([user(), tool('t1', 1)]);
    try {
      const before = projection.nodes;
      const evaluationsBefore = projection.evaluations;
      answer({ status: 'fresh', epoch: 1, rev: 1, generation: 'test-generation' });
      await switching;
      flushSync();

      expect(syncCalls()).toBe(1);
      expect(pane.items.map((item) => item.id)).toEqual(['u0', 't1']);
      expect(projection.evaluations).toBeGreaterThan(evaluationsBefore);
      const [confirmed] = runs(projection.nodes);
      expect(confirmed.runId).toBe(runs(before)[0].runId);
      expect(pane.activityRuns.openedLiveRunIds()).toEqual([confirmed.runId]);
    } finally {
      projection.dispose();
    }
  });
});
