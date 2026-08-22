import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadSubagentMemory } from './threadSubagentMemory';
import { createThreadPane } from './thread.svelte';
import type { Item, Thread } from '../types/models';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { installPaneMocks, makeItem, makeThread } from '../../test/helpers/chat';

// Live-stream admission: history windows load top-level rows only, so a
// streamed subagent child whose launch anchor is not loadable in the pane
// has nothing that can render it. Admitting it anyway left orphan leaves
// in pane memory forever — no eviction policy can reach a row whose
// parent is gone. These cover the boundary itself (the memory module) and
// the chokepoint that enforces it (the pane's streaming upsert path).

const THREAD_ID = 'subagent-admit';

function anchorItem(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'anchor',
    threadId: THREAD_ID,
    turnIndex: 1,
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Task',
    status: 'running',
    summary: 'Task: investigate',
    ...overrides,
  });
}

function childItem(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'child-1',
    threadId: THREAD_ID,
    turnIndex: 1,
    itemIndex: 1,
    parentId: 'anchor',
    status: 'streaming',
    summary: 'working',
    ...overrides,
  });
}

/**
 * Drives the real `createThreadSubagentMemory` over a plain array so the
 * admission rules can be asserted without a pane: the module only ever
 * reaches the window through these getters and the two replace/drop
 * chokepoints, which is exactly what the pane hands it.
 */
function makeMemoryHarness(initial: readonly Item[] = []) {
  let items: Item[] = [...initial];
  let index = new Map<string, number>();
  let switchGeneration = 0;
  let thread: Thread | null = makeThread({ id: THREAD_ID });
  const expandedGroups = new Set<string>();

  function install(next: Item[]): void {
    items = next;
    index = new Map(next.map((item, i) => [item.id, i]));
  }
  install(items);

  const memory = createThreadSubagentMemory({
    getItems: () => items,
    getItemIndex: (itemId) => index.get(itemId),
    replaceTimelineItems: (next) => {
      install(next);
      return true;
    },
    dropTimelineItems: (shouldDrop) => {
      const dropped = items.filter(shouldDrop);
      if (dropped.length > 0) install(items.filter((it) => !shouldDrop(it)));
      return dropped;
    },
    getThread: () => thread,
    getSwitchGeneration: () => switchGeneration,
    recomputeReveal: () => {},
    isSubagentGroupExpanded: (groupKey) => expandedGroups.has(groupKey),
  });

  return {
    memory,
    get items() {
      return items;
    },
    install,
    expandedGroups,
    setThread(next: Thread | null) {
      thread = next;
      switchGeneration += 1;
    },
  };
}

// The admission DECISION lives in the window merges — see
// `applyItemUpsertsToWindow parented admission` and `applySyncPage
// subagent admission` in threadItems.test.ts. This block covers the
// ledger that silences what those merges reject.
describe('threadSubagentMemory swallow ledger', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('records rejected rows and silences their ids', () => {
    const harness = makeMemoryHarness();

    harness.memory.recordAdmission([], [childItem()]);

    expect(harness.memory.isSwallowedChild('child-1')).toBe(true);
    expect(harness.memory.isSwallowedChild('anchor')).toBe(false);
  });

  it('unswallows rows that land', () => {
    const harness = makeMemoryHarness();
    harness.memory.recordAdmission([], [childItem()]);

    harness.memory.recordAdmission([childItem({ summary: 'landed' })], []);

    expect(harness.memory.isSwallowedChild('child-1')).toBe(false);
  });

  it('unswallows ids that hydration loads for real', async () => {
    const harness = makeMemoryHarness();
    harness.memory.recordAdmission([], [childItem()]);
    expect(harness.memory.isSwallowedChild('child-1')).toBe(true);

    harness.install([anchorItem()]);
    setBindingMock('ListSubagentDescendants', async () => [
      childItem({ status: 'completed', summary: 'ran the build' }),
    ]);

    await expect(harness.memory.hydrateChildren('anchor')).resolves.toBe(true);
    expect(harness.items.some((it) => it.id === 'child-1')).toBe(true);
    expect(harness.memory.isSwallowedChild('child-1')).toBe(false);
  });

  it('clears the swallow set on a fresh-thread reset', () => {
    const harness = makeMemoryHarness();
    harness.memory.recordAdmission([], [childItem()]);
    expect(harness.memory.isSwallowedChild('child-1')).toBe(true);

    harness.memory.resetForFreshThread();

    expect(harness.memory.isSwallowedChild('child-1')).toBe(false);
  });

  it('clears the swallow set with the window-derived state (cache restore path)', () => {
    const harness = makeMemoryHarness();
    harness.memory.recordAdmission([], [childItem()]);
    expect(harness.memory.isSwallowedChild('child-1')).toBe(true);

    // Warm cache restore and replica paint call this instead of
    // resetForFreshThread (folds survive via snapshot; swallows must not).
    harness.memory.clearWindowDerivedState();

    expect(harness.memory.isSwallowedChild('child-1')).toBe(false);
  });

  it('clears wholesale at the cap instead of growing without bound', () => {
    const harness = makeMemoryHarness();
    // 4096 mirrors MAX_SWALLOWED_CHILD_IDS; a drift in the constant
    // should be a deliberate edit here too.
    const flood = Array.from({ length: 4096 }, (_, i) =>
      childItem({ id: `flood-${i}` }),
    );
    harness.memory.recordAdmission([], flood);
    expect(harness.memory.isSwallowedChild('flood-0')).toBe(true);

    harness.memory.recordAdmission([], [childItem({ id: 'overflow' })]);

    expect(harness.memory.isSwallowedChild('flood-0')).toBe(false);
    expect(harness.memory.isSwallowedChild('overflow')).toBe(true);
  });
});

describe('threadSubagentMemory admission through the pane', () => {
  beforeEach(() => {
    resetBindingMocks();
    installPaneMocks();
  });

  async function paneWithSlice(items: Item[]) {
    const pane = createThreadPane();
    setBindingMock('ListThreadSliceAround', async () => ({
      items,
      oldestTurnIndex: 0,
      newestTurnIndex: 1,
      hasMore: false,
      hasMoreOlder: false,
      hasMoreNewer: false,
    }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    return pane;
  }

  it('keeps an anchorless streamed child out of pane memory', async () => {
    const pane = await paneWithSlice([
      makeItem({ id: 'pre', threadId: THREAD_ID, turnIndex: 0, itemIndex: 0 }),
    ]);

    expect(pane.upsertItem(childItem())).toBe(false);

    expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
    // The row is not folded either — nothing to fold it under. SQLite
    // holds it, and hydration renders it if the anchor comes back.
    expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
  });

  it('streams a child normally while its anchor is loaded', async () => {
    const pane = await paneWithSlice([
      makeItem({ id: 'pre', threadId: THREAD_ID, turnIndex: 0, itemIndex: 0 }),
      anchorItem(),
    ]);

    expect(pane.upsertItem(childItem())).toBe(true);
    expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
  });

  it('re-admits a swallowed child once its anchor lands, and its deltas apply again', async () => {
    const pane = await paneWithSlice([
      makeItem({ id: 'pre', threadId: THREAD_ID, turnIndex: 0, itemIndex: 0 }),
    ]);
    expect(pane.upsertItem(childItem())).toBe(false);

    // The anchor arrives (transport replay, backfill) leading its child
    // through the same batch — the pair lands and the ledger entry goes.
    expect(pane.upsertItems([anchorItem(), childItem()])).toBe(true);
    expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      pane.applyItemDelta({
        threadId: THREAD_ID,
        itemId: 'child-1',
        kind: 'assistant_text',
        delta: 'resumed',
        updatedAt: 5,
      });
      expect(warn).not.toHaveBeenCalled();
    } finally {
      warn.mockRestore();
    }
  });

  it('silences deltas for a swallowed child and warns once for a real gap', async () => {
    const pane = await paneWithSlice([
      makeItem({ id: 'pre', threadId: THREAD_ID, turnIndex: 0, itemIndex: 0 }),
    ]);
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      pane.upsertItem(childItem());

      pane.applyItemDelta({
        threadId: THREAD_ID,
        itemId: 'child-1',
        kind: 'assistant_text',
        delta: 'more',
        updatedAt: 2,
      });
      pane.applyItemDelta({
        threadId: THREAD_ID,
        itemId: 'child-1',
        kind: 'assistant_text',
        delta: ' and more',
        updatedAt: 3,
      });
      expect(warn).not.toHaveBeenCalled();

      // A delta for a row that was never streamed at all is the genuine
      // transport-gap case: still reported, but only once per id.
      for (let i = 0; i < 3; i += 1) {
        pane.applyItemDelta({
          threadId: THREAD_ID,
          itemId: 'ghost',
          kind: 'assistant_text',
          delta: 'x',
          updatedAt: 4 + i,
        });
      }
      expect(warn).toHaveBeenCalledTimes(1);
      expect(warn.mock.calls[0][1]).toBe('ghost');
    } finally {
      warn.mockRestore();
    }
  });
});

// The fold registry and the eviction sweep key on the SAME provider-neutral
// launch predicate the timeline tree does. What follows pins that a launch
// AO renders as a card is also a launch the memory policy treats as an
// anchor — for every kind, and through a nested chain of mixed kinds.
describe('threadSubagentMemory anchors every launch kind', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  function forkedSkill(overrides: Partial<Item> = {}): Item {
    return makeItem({
      id: 'skill-1',
      threadId: THREAD_ID,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'tool_call',
      toolName: 'Skill',
      status: 'running',
      summary: 'Skill: code-review',
      meta: JSON.stringify({
        toolName: 'Skill',
        input: { skill: 'code-review' },
        skillFork: { agentId: 'a1', commandName: 'code-review' },
      }),
      ...overrides,
    });
  }

  function codexSpawn(overrides: Partial<Item> = {}): Item {
    return makeItem({
      id: 'spawn-1',
      threadId: THREAD_ID,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'tool_call',
      toolName: 'collab_agent',
      status: 'running',
      summary: 'collab_agent: review',
      meta: JSON.stringify({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
      ...overrides,
    });
  }

  function resumeCarrier(overrides: Partial<Item> = {}): Item {
    return makeItem({
      id: 'toolu_resume',
      threadId: THREAD_ID,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'tool_call',
      toolName: 'SendMessage',
      isBackground: true,
      status: 'running',
      summary: 'Agent: resumed work',
      meta: JSON.stringify({ task_id: 'a464e54e96a45cd0c', description: 'resumed work' }),
      ...overrides,
    });
  }

  it.each([
    ['a forked skill', forkedSkill()],
    ['a Codex spawn', codexSpawn()],
    ['a SendMessage resume carrier', resumeCarrier()],
    ['a backgrounded Claude agent', anchorItem({ isBackground: true })],
  ])('folds a settled child under %s', (_label, anchor) => {
    const child = childItem({
      parentId: anchor.id,
      status: 'completed',
      summary: 'ran the build',
    });
    const harness = makeMemoryHarness([anchor, child]);

    harness.memory.evictSettledChildren([child]);

    expect(harness.items.map((it) => it.id)).toEqual([anchor.id]);
    expect(harness.memory.aggregate(anchor.id)).toEqual({
      evictedCount: 1,
      terminalPreview: 'ran the build',
      terminalTurnIndex: 1,
      terminalItemIndex: 1,
    });
  });

  it('keeps a child loaded while its card is expanded, whichever kind the card is', () => {
    const anchor = forkedSkill();
    const child = childItem({ parentId: 'skill-1', status: 'completed' });
    const harness = makeMemoryHarness([anchor, child]);
    harness.expandedGroups.add('skill-1');

    harness.memory.evictSettledChildren([child]);

    expect(harness.items.map((it) => it.id)).toEqual(['skill-1', 'child-1']);
    expect(harness.memory.aggregate('skill-1')).toBeUndefined();
  });

  it('resolves a settled subtree through a chain of nested launches of mixed kinds', () => {
    // Claude agent > forked skill > nested agent > its settled row. Every
    // launch in the chain stays loaded (each is a fold key and a card); only
    // the non-launch rows fold, each under its OWN nearest launch.
    const anchor = anchorItem();
    const skill = forkedSkill({ itemIndex: 1, parentId: 'anchor' });
    const nestedAgent = makeItem({
      id: 'nested-agent',
      threadId: THREAD_ID,
      turnIndex: 1,
      itemIndex: 2,
      kind: 'tool_call',
      toolName: 'Agent',
      parentId: 'skill-1',
      status: 'running',
      summary: 'Agent: angle B',
      meta: JSON.stringify({ toolName: 'Agent' }),
    });
    const skillRow = childItem({
      id: 'skill-row',
      itemIndex: 3,
      parentId: 'skill-1',
      status: 'completed',
      summary: 'read the diff',
    });
    const deepRow = childItem({
      id: 'deep-row',
      itemIndex: 4,
      parentId: 'nested-agent',
      status: 'completed',
      summary: 'deep work',
    });
    const stillStreaming = childItem({
      id: 'live-row',
      itemIndex: 5,
      parentId: 'nested-agent',
      status: 'streaming',
      summary: 'still going',
    });
    const harness = makeMemoryHarness([
      anchor, skill, nestedAgent, skillRow, deepRow, stillStreaming,
    ]);

    harness.memory.evictCollapsedSubtree('anchor');

    expect(harness.items.map((it) => it.id)).toEqual([
      'anchor',
      'skill-1',
      'nested-agent',
      'live-row',
    ]);
    expect(harness.memory.aggregate('skill-1')?.evictedCount).toBe(1);
    expect(harness.memory.aggregate('nested-agent')?.evictedCount).toBe(1);
    expect(harness.memory.aggregate('anchor')).toBeUndefined();
  });

  it('folds a row parented on an ordinary tool under its nearest launch ancestor', () => {
    // The grouping pass buckets such a row inside the launch's card, so the
    // fold has to agree — evicting it under the same anchor, not refusing.
    const anchor = anchorItem();
    const midTool = childItem({
      id: 'mid-bash',
      itemIndex: 1,
      parentId: 'anchor',
      kind: 'tool_call',
      toolName: 'Bash',
      status: 'completed',
      summary: 'Bash: build',
    });
    const grandchild = childItem({
      id: 'grandchild',
      itemIndex: 2,
      parentId: 'mid-bash',
      status: 'completed',
      summary: 'build output',
    });
    const harness = makeMemoryHarness([anchor, midTool, grandchild]);

    harness.memory.evictSettledChildren([midTool, grandchild]);

    expect(harness.items.map((it) => it.id)).toEqual(['anchor']);
    expect(harness.memory.aggregate('anchor')?.evictedCount).toBe(2);
  });

  it('never folds a launch row itself, whichever kind it is', () => {
    const anchor = anchorItem();
    const nested = codexSpawn({ itemIndex: 1, parentId: 'anchor', status: 'completed' });
    const harness = makeMemoryHarness([anchor, nested]);

    harness.memory.evictSettledChildren([nested]);

    expect(harness.items.map((it) => it.id)).toEqual(['anchor', 'spawn-1']);
    expect(harness.memory.aggregate('anchor')).toBeUndefined();
  });

  it.each([
    ['a forked skill', forkedSkill()],
    ['a Codex spawn', codexSpawn()],
    ['a SendMessage resume carrier', resumeCarrier()],
  ])('hydrates the child transcript of %s', async (_label, anchor) => {
    // The store query (ListSubagentDescendants) walks parent_id and knows no
    // tool names, so hydration is provider-neutral by construction — this
    // pins that the FRONTEND does not gate it on kind either.
    const harness = makeMemoryHarness([anchor]);
    const requested: string[] = [];
    setBindingMock('ListSubagentDescendants', async (_threadId, rootItemID) => {
      requested.push(rootItemID as string);
      return [childItem({ parentId: anchor.id, status: 'completed', summary: 'done' })];
    });

    await expect(harness.memory.hydrateChildren(anchor.id)).resolves.toBe(true);

    expect(requested).toEqual([anchor.id]);
    expect(harness.items.map((it) => it.id)).toEqual([anchor.id, 'child-1']);
  });

  it('reclaims a folded child on hydration so it is never counted twice', async () => {
    const anchor = forkedSkill();
    const child = childItem({ parentId: 'skill-1', status: 'completed', summary: 'done' });
    const harness = makeMemoryHarness([anchor, child]);
    harness.memory.evictSettledChildren([child]);
    expect(harness.memory.aggregate('skill-1')?.evictedCount).toBe(1);

    setBindingMock('ListSubagentDescendants', async () => [child]);
    await expect(harness.memory.hydrateChildren('skill-1')).resolves.toBe(true);

    expect(harness.items.map((it) => it.id)).toEqual(['skill-1', 'child-1']);
    expect(harness.memory.aggregate('skill-1')).toBeUndefined();
  });
});
