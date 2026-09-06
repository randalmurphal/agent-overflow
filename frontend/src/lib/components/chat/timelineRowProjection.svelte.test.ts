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

import { beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { createTimelineRowProjection } from './timelineRowProjection.svelte';

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
        patch: { status: 'completed', updatedAt: 2 },
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
