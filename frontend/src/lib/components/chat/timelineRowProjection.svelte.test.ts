// Reactive-granularity contract for the timeline node pipeline.
//
// `groupedNodes` → `revealedNodes` is the expensive part of rendering a
// thread: it walks every item in the window, groups subagents and reads,
// wraps activity runs, and its output array IS the virtualizer's data. It
// must therefore be invalidated by STRUCTURE ONLY. Content that moves
// inside a turn — a streaming child's summary, a status flip, a tool
// result landing — is resolved by the row components against the store
// (`TimelineLeaf`, `ReadGroupRow`, `SubagentGroup`, `WaitGroup`), so a
// streaming tick must leave this array reference-identical.
//
// These tests pin both directions. Only asserting "streaming doesn't
// rebuild" would pass on a derivation that never re-runs at all, so every
// no-rebuild case is paired with the structural change that must rebuild.

import { ActivityRunStub } from '../../../../bindings/agent-overflow/internal/store/models';
import { beforeEach, describe, expect, it } from 'vitest';
import { flushSync, tick } from 'svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { createTimelineRowProjection } from './timelineRowProjection.svelte';
import { createAgentScopeView } from '../../stores/agentScopeView.svelte';

interface MountedProjection {
  /** Re-evaluations of the tracked read; the initial run is 1. */
  readonly evaluations: number;
  readonly nodes: TimelineNode[];
  dispose(): void;
}

/**
 * Runs a real projection over a real pane inside an effect root, with a
 * live subscriber so `revealedNodes` behaves as it does under a mounted
 * timeline (an unsubscribed `$derived` recomputes lazily on read and
 * would report nothing about invalidation).
 */
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

/**
 * Depth-first search for the subagent group. Runs wrap consecutive tool
 * rows, so the group is normally nested inside an `activity_run` rather
 * than sitting at the top level.
 */
function findGroup(nodes: readonly TimelineNode[]): Extract<TimelineNode, { kind: 'group' }> | null {
  for (const node of nodes) {
    if (node.kind === 'group') return node;
    if (node.kind === 'activity_run') {
      const nested = findGroup(node.children);
      if (nested) return nested;
    }
  }
  return null;
}

function agentLaunch(id: string, itemIndex: number) {
  return makeItem({
    id,
    itemIndex,
    kind: 'tool_call',
    toolName: 'Agent',
    role: 'assistant',
    status: 'running',
    summary: 'Agent: exploring',
    payloadMeta: JSON.stringify({
      toolName: 'Agent',
      input: { description: 'Find the bell icon', subagent_type: 'Explore' },
    }),
  });
}

describe('timeline row projection window edges', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('renders only rows whose root lies inside the loaded window', async () => {
    // A held scope loaded above the window (loadAgentScope for a tray
    // digest or the companion) is an island: in pane memory for the
    // scoped surface, absent from the transcript so the reader and the
    // paging edges stay put.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'w5', turnIndex: 5, itemIndex: 0, summary: 'five' }),
      makeItem({ id: 'w6', turnIndex: 6, itemIndex: 0, summary: 'six' }),
    ]);
    const launch = { ...agentLaunch('launch', 0), turnIndex: 0 };
    setBindingMock('GetThreadItem', async () => launch);
    setBindingMock('ListSubagentDescendants', async () => [
      makeItem({ id: 'child', turnIndex: 6, itemIndex: 3, parentId: 'launch', summary: 'late child' }),
    ]);
    const projection = mountProjection(pane);
    try {
      await pane.loadAgentScope('launch');
      flushSync();
      expect(pane.items.map((item) => item.id)).toEqual(['launch', 'w5', 'w6', 'child']);
      const ids = projection.nodes.map((node) => (node.kind === 'leaf' ? node.item.id : node.kind));
      expect(ids).toEqual(['w5', 'w6']);
    } finally {
      projection.dispose();
    }
  });

  it('renders a scoped view whole, whatever the source window edges are', async () => {
    // A companion or digest scope has no edges of its own: its children
    // keep the coordinates they arrived with, which can lie past the
    // source pane's newest loaded row.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'w5', turnIndex: 5, itemIndex: 0, summary: 'five' }),
      { ...agentLaunch('launch', 1), turnIndex: 5 },
    ]);
    // Hydration never moves the source window's edges, so the child
    // lands past its newest row.
    setBindingMock('ListSubagentDescendants', async () => [
      makeItem({ id: 'late', turnIndex: 9, itemIndex: 0, parentId: 'launch', summary: 'late child' }),
    ]);
    await pane.ensureSubagentChildren('launch');
    expect(pane.newestLoadedCursor?.turnIndex).toBe(5);
    const view = createAgentScopeView(pane, 'launch', { viewKey: 'agent', openAgentPane: () => {} });
    const scoped = mountProjection(view.pane);
    const main = mountProjection(pane);
    try {
      expect(view.pane.oldestLoadedCursor).toBeNull();
      expect(view.pane.newestLoadedCursor).toBeNull();
      expect(scoped.nodes.map((node) => (node.kind === 'leaf' ? node.item.id : node.kind))).toEqual(['late']);
      // The transcript keeps the child too: it keys on the launch, which is inside.
      expect(findGroup(main.nodes)?.children.some((child) => child.kind === 'leaf' && child.item.id === 'late')).toBe(true);
    } finally {
      scoped.dispose();
      main.dispose();
      view.dispose();
    }
  });
});

describe('timeline row projection reactivity', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('holds revealedNodes identity across a streaming subagent child tick', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'user:0', itemIndex: 0, kind: 'user_text', role: 'user', summary: 'go' }),
      agentLaunch('agent:1', 1),
      makeItem({
        id: 'child:1', itemIndex: 2, parentId: 'agent:1',
        status: 'streaming', summary: 'reading',
      }),
    ]);
    const projection = mountProjection(pane);
    try {
      const before = projection.nodes;
      const evaluationsBefore = projection.evaluations;
      expect(findGroup(before)).not.toBeNull();

      // A content-only replacement of a group descendant: exactly what a
      // streaming delta produces once the smoother commits it.
      pane.upsertItem(makeItem({
        id: 'child:1', itemIndex: 2, parentId: 'agent:1',
        status: 'streaming', summary: 'reading src/lib/stores/thread.svelte.ts',
        updatedAt: 1,
      }));
      flushSync();

      expect(pane.getItemById('child:1')?.summary).toContain('thread.svelte.ts');
      expect(projection.evaluations).toBe(evaluationsBefore);
      expect(projection.nodes).toBe(before);
    } finally {
      projection.dispose();
    }
  });

  it('holds revealedNodes identity when the group anchor settles', async () => {
    const pane = await buildPane(undefined, [
      agentLaunch('agent:1', 0),
      // Left running on purpose: the pane evicts SETTLED descendants of a
      // collapsed card, which is a real structural change. The anchor
      // itself is never evicted, so its status flip is the clean case.
      makeItem({
        id: 'child:1', itemIndex: 1, parentId: 'agent:1',
        kind: 'tool_call', toolName: 'Read', status: 'running', summary: 'Read a.ts',
      }),
    ]);
    const projection = mountProjection(pane);
    try {
      const before = projection.nodes;
      const evaluationsBefore = projection.evaluations;

      pane.applyItemPatch({
        threadId: 'thread-1', itemId: 'agent:1', kind: 'tool_call',
        patch: { rev: 0, status: 'completed', updatedAt: 2 },
      });
      flushSync();

      expect(pane.getItemById('agent:1')?.status).toBe('completed');
      expect(projection.evaluations).toBe(evaluationsBefore);
      expect(projection.nodes).toBe(before);
      // The snapshot is deliberately stale here — `SubagentGroup` resolves
      // the anchor against the store, which is what this identity buys.
      expect(findGroup(before)?.parent.status).toBe('running');
    } finally {
      projection.dispose();
    }
  });

  it('rebuilds revealedNodes when a new child appends under the group', async () => {
    const pane = await buildPane(undefined, [
      agentLaunch('agent:1', 0),
      // Both children stay active so the collapsed card's live eviction
      // does not drop the first one and mask the append.
      makeItem({
        id: 'child:1', itemIndex: 1, parentId: 'agent:1',
        status: 'running', summary: 'one',
      }),
    ]);
    const projection = mountProjection(pane);
    try {
      const before = projection.nodes;
      const evaluationsBefore = projection.evaluations;

      pane.upsertItem(makeItem({
        id: 'child:2', itemIndex: 2, parentId: 'agent:1',
        status: 'streaming', summary: 'two', updatedAt: 3,
      }));
      flushSync();

      expect(projection.evaluations).toBeGreaterThan(evaluationsBefore);
      expect(projection.nodes).not.toBe(before);
      expect(findGroup(projection.nodes)?.children).toHaveLength(2);
    } finally {
      projection.dispose();
    }
  });

  it('rebuilds revealedNodes when a top-level row appends', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', itemIndex: 0, summary: 'first' }),
    ]);
    const projection = mountProjection(pane);
    try {
      const before = projection.nodes;

      pane.upsertItem(makeItem({ id: 'b', itemIndex: 1, summary: 'second', updatedAt: 4 }));
      flushSync();

      expect(projection.nodes).not.toBe(before);
      expect(projection.nodes).toHaveLength(2);
    } finally {
      projection.dispose();
    }
  });
});

describe('quiet queue reservations', () => {
  it('keeps a restored pending row in the preview until its confirmation, including unchanged coordinates', async () => {
    const { markItemsFlushed, getFlushedForThread, clearForThread, applyFlushedLifecycle } = await import('../../stores/sendQueue.svelte');
    const row = makeItem({ id: 'user:0:flush:quiet', kind: 'user_text', role: 'user', status: 'completed', summary: 'Still queued', meta: '{"pendingFlush":true}', itemIndex: 1 });
    const pane = await buildPane(undefined, [row]);
    markItemsFlushed(pane.threadId!, [{ queueItemId: 'q-quiet', userItemId: row.id, message: row.summary }]);
    pane.syncRenderedFlushRows();
    const view = mountProjection(pane);
    try {
      expect(view.nodes).toHaveLength(0);
      expect(getFlushedForThread(pane.threadId!)).toHaveLength(1);
      const evaluations = view.evaluations;
      applyFlushedLifecycle(pane.threadId!, row.id, { state: 'queued' });
      flushSync();
      expect(view.evaluations).toBe(evaluations);
      // Confirmation REMOVES the marker rather than storing false, so a
      // settled row is byte-identical to an imported one.
      pane.applyProviderItemUpserts([{ ...row, threadId: pane.threadId!, meta: '{"provider_item_id":"echo-1"}' }]);
      flushSync();
      expect(view.nodes).toHaveLength(1);
      expect(getFlushedForThread(pane.threadId!)).toHaveLength(0);
    } finally {
      view.dispose();
      clearForThread(pane.threadId!);
    }
  });
});

it('does not hide retained history after the live pending registry is cleared', async () => {
  const { markItemsFlushed, clearForThread } = await import('../../stores/sendQueue.svelte');
  const row = makeItem({ id: 'user:0:flush:lost-session', kind: 'user_text', role: 'user', meta: '{"pendingFlush":true}' });
  const pane = await buildPane(undefined, [row]);
  markItemsFlushed(pane.threadId!, [{ queueItemId: 'q-lost', userItemId: row.id, message: row.summary }]);
  const view = mountProjection(pane);
  try {
    expect(view.nodes).toHaveLength(0);
    clearForThread(pane.threadId!);
    flushSync();
    expect(view.nodes).toHaveLength(1);
  } finally {
    view.dispose();
    clearForThread(pane.threadId!);
  }
});

it('forms a detached completion card before expanding or loading the old launch', async () => {
  const launch = { ...agentLaunch('old-launch', 0), isBackground: true, status: 'completed' as const };
  const done = makeItem({ id: 'done', itemIndex: 100, kind: 'tool_completion', toolName: 'Agent',
    completionOf: launch.id, completionLaunch: launch, summary: 'Final report' });
  const pane = await buildPane(undefined, [done]);
  const projection = mountProjection(pane);
  try {
    expect(pane.getItemById(launch.id)).toBeUndefined();
    expect(findGroup(projection.nodes)?.parent.id).toBe(launch.id);
    expect(findGroup(projection.nodes)?.anchor?.id).toBe(done.id);
    const before = projection.nodes;
    pane.upsertItem({ ...done, rev: 2, completionLaunch: { ...launch, rev: 2, summary: 'Updated launch summary' } });
    flushSync();
    expect(projection.nodes).toBe(before);
    setBindingMock('GetThreadItem', async () => launch);
    setBindingMock('ListSubagentDescendants', async () => [
      makeItem({ id: 'child', itemIndex: 80, parentId: launch.id, summary: 'Agent work' }),
    ]);
    await pane.loadAgentScope(launch.id);
    flushSync();
    expect(findGroup(projection.nodes)?.anchor?.id).toBe(done.id);
    expect(findGroup(projection.nodes)?.children.some(child => child.kind === 'leaf' && child.item.id === 'child')).toBe(true);
    expect(projection.nodes).toHaveLength(1);
  } finally {
    projection.dispose();
  }
});


it('reclassifies a leading notification from the authoritative run window without an item change', async () => {
  const pane = await buildPane(undefined, [
    makeItem({ id: 'report', kind: 'notification', itemIndex: 50, summary: 'Agent report' }),
    makeItem({ id: 'bash', kind: 'tool_call', toolName: 'Bash', itemIndex: 51 }),
    makeItem({ id: 'prose', itemIndex: 52 }),
  ]);
  const projection = mountProjection(pane);
  try {
    pane.activityRuns.syncRunSpans(pane.items, [new ActivityRunStub({
      firstItemId: 'earlier', firstTurnIndex: 0, firstItemIndex: 0,
      lastItemId: 'bash', lastTurnIndex: 0, lastItemIndex: 51,
      loadedFirstItemId: 'report', loadedLastItemId: 'bash', memberCount: 52,
      unshippedBefore: 50, unshippedAfter: 0, unshippedDigest: '0000000000000000',
    })]);
    await tick();
    expect(projection.nodes.map(node => node.kind)).toEqual(['activity_run', 'leaf']);
    const run = projection.nodes[0];
    expect(run.kind === 'activity_run' && run.children.some(node => node.kind === 'leaf' && node.item.id === 'report')).toBe(true);
  } finally {
    projection.dispose();
    pane.clear();
  }
});
