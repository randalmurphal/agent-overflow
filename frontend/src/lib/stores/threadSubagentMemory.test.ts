import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { createThreadSubagentMemory } from './threadSubagentMemory';
import { createThreadPane } from './thread.svelte';
import type { Item } from '../types/models';
import type { ItemPatchEvent } from '../types/events';
import { MAX_ACTIVE_PER_ANCHOR, MAX_TRACKED_CHILDREN } from '../utils/subagentFold';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { installPaneMocks, makeItem, makeThread } from '../../test/helpers/chat';

// Streamed subagent rows never enter a pane window. The memory module
// records each one against its launch anchors' aggregates, which is all a
// collapsed card reads. These cover the module over plain getters and the
// pane chokepoint that routes children to it.

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
    kind: 'tool_call',
    toolName: 'Bash',
    status: 'completed',
    summary: 'ran the build',
    ...overrides,
  });
}

function patch(itemId: string, fields: Omit<ItemPatchEvent['patch'], 'rev'>, kind = 'tool_call'): ItemPatchEvent {
  return { threadId: THREAD_ID, itemId, kind, patch: { rev: fields.updatedAt ?? 0, ...fields } };
}

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

/** The real module over a plain map of loaded window rows. */
function makeMemoryHarness(loaded: readonly Item[] = []) {
  const rows = new Map(loaded.map((item) => [item.id, item]));
  const structureChanged = vi.fn();
  const memory = createThreadSubagentMemory({
    getThreadId: () => THREAD_ID,
    getLoadedItem: (itemId) => rows.get(itemId),
    noteStructureChanged: structureChanged,
  });
  return { memory, rows, structureChanged };
}

describe('threadSubagentMemory aggregates', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it.each([
    ['an awaited Claude agent', anchorItem()],
    ['a backgrounded Claude agent', anchorItem({ isBackground: true })],
    ['a forked skill', forkedSkill()],
    ['a Codex spawn', codexSpawn()],
    ['a SendMessage resume carrier', resumeCarrier()],
  ])('records a child under %s', (_label, anchor) => {
    const { memory } = makeMemoryHarness([anchor]);
    memory.admitChildren([childItem({ parentId: anchor.id })]);
    expect(memory.aggregate(anchor.id)).toEqual({
      count: 1,
      activePreview: '',
      activeTurnIndex: -1,
      activeItemIndex: -1,
      terminalPreview: 'ran the build',
      terminalTurnIndex: 1,
      terminalItemIndex: 1,
    });
  });

  it('ignores top-level rows, other threads and children of unloaded roots', () => {
    const { memory } = makeMemoryHarness([anchorItem()]);
    memory.admitChildren([
      childItem({ id: 'top', parentId: undefined }),
      childItem({ id: 'foreign', threadId: 'other' }),
      childItem({ id: 'orphan', parentId: 'gone' }),
    ]);
    expect(memory.aggregate('anchor')).toBeUndefined();
    expect(memory.isKnownChild('top')).toBe(false);
    expect(memory.isKnownChild('foreign')).toBe(false);
    // Tracked for delta swallowing, counted nowhere.
    expect(memory.isKnownChild('orphan')).toBe(true);
    expect(memory.aggregate('gone')).toBeUndefined();
  });

  it('counts a nested chain of mixed launch kinds on every enclosing card', () => {
    // Claude agent > forked skill > nested agent > rows.
    const { memory } = makeMemoryHarness([anchorItem()]);
    memory.admitChildren([
      forkedSkill({ itemIndex: 1, parentId: 'anchor' }),
      makeItem({
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
      }),
      childItem({ id: 'skill-row', itemIndex: 3, parentId: 'skill-1', summary: 'read the diff' }),
      childItem({ id: 'deep-row', itemIndex: 4, parentId: 'nested-agent', summary: 'deep work' }),
      childItem({ id: 'live-row', itemIndex: 5, parentId: 'nested-agent', status: 'streaming', summary: 'still going' }),
    ]);

    expect(memory.aggregate('anchor')).toMatchObject({ count: 5, activePreview: 'still going' });
    expect(memory.aggregate('skill-1')).toMatchObject({ count: 4, activePreview: 'still going' });
    expect(memory.aggregate('nested-agent')).toMatchObject({
      count: 2,
      activePreview: 'still going',
      terminalPreview: 'deep work',
    });
  });

  it('counts a row parented on an ordinary tool under the enclosing launch', () => {
    const { memory } = makeMemoryHarness([anchorItem()]);
    memory.admitChildren([
      childItem({ id: 'mid-bash', itemIndex: 1, summary: 'Bash: build' }),
      childItem({ id: 'grandchild', itemIndex: 2, parentId: 'mid-bash', summary: 'build output' }),
    ]);
    expect(memory.aggregate('anchor')).toMatchObject({ count: 2, terminalPreview: 'build output' });
    expect(memory.aggregate('mid-bash')).toBeUndefined();
  });

  it('prose and thinking never become the preview', () => {
    const { memory } = makeMemoryHarness([anchorItem()]);
    memory.admitChildren([
      childItem({ id: 'tool', itemIndex: 1, summary: 'ran the build' }),
      childItem({ id: 'prose', itemIndex: 2, kind: 'assistant_text', summary: 'I will now' }),
    ]);
    expect(memory.aggregate('anchor')).toMatchObject({ count: 2, terminalPreview: 'ran the build' });

    // A prose row's patch clears nothing it never contributed.
    memory.applyChildPatch(patch('prose', { summary: 'more prose', updatedAt: 5 }, 'assistant_text'));
    expect(memory.aggregate('anchor')?.terminalPreview).toBe('ran the build');
  });

  it('applies child patches: status settles the active preview into the terminal one', () => {
    const { memory } = makeMemoryHarness([anchorItem()]);
    memory.admitChildren([childItem({ status: 'running', summary: 'building', updatedAt: 1 })]);
    expect(memory.aggregate('anchor')).toMatchObject({ activePreview: 'building', terminalPreview: '' });

    expect(memory.applyChildPatch(patch('child-1', { summary: 'building step 2', updatedAt: 2 }))).toBe(true);
    expect(memory.aggregate('anchor')?.activePreview).toBe('building step 2');

    expect(memory.applyChildPatch(patch('child-1', { status: 'completed', updatedAt: 3 }))).toBe(true);
    expect(memory.aggregate('anchor')).toMatchObject({ activePreview: '', terminalPreview: 'building step 2' });

    expect(memory.applyChildPatch(patch('unknown', { status: 'completed', updatedAt: 3 }))).toBe(false);
  });

  it('bumps structure once when a loaded Skill row admits its first child', () => {
    const plainSkill = forkedSkill({ meta: JSON.stringify({ toolName: 'Skill', input: { skill: 'x' } }) });
    const { memory, structureChanged } = makeMemoryHarness([plainSkill, anchorItem({ itemIndex: 5 })]);

    memory.admitChildren([childItem({ id: 's1', parentId: 'skill-1' })]);
    expect(structureChanged).toHaveBeenCalledTimes(1);
    memory.admitChildren([childItem({ id: 's2', itemIndex: 2, parentId: 'skill-1' })]);
    expect(structureChanged).toHaveBeenCalledTimes(1);

    // A non-Skill anchor's first child changes no grouping.
    memory.admitChildren([childItem({ id: 'a1', itemIndex: 6 })]);
    expect(structureChanged).toHaveBeenCalledTimes(1);
  });

  it('wakes a reader only for its own anchor', () => {
    const { memory } = makeMemoryHarness([anchorItem(), codexSpawn({ itemIndex: 10 })]);
    memory.admitChildren([childItem({ id: 'mine', status: 'running', summary: 'mine', updatedAt: 1 })]);
    memory.admitChildren([childItem({ id: 'theirs', parentId: 'spawn-1', itemIndex: 11 })]);
    const probe = probeReactivity(() => memory.aggregate('anchor'));
    try {
      const baseline = probe.evaluations;
      for (let i = 0; i < 50; i += 1) {
        memory.admitChildren([childItem({ id: `other-${i}`, parentId: 'spawn-1', itemIndex: 12 + i, status: 'running', summary: `step ${i}`, updatedAt: 1 })]);
        memory.applyChildPatch(patch(`other-${i}`, { status: 'completed', updatedAt: 2 }));
        flushSync();
      }
      expect(probe.evaluations).toBe(baseline);

      // Restating a known child with nothing new wakes nobody.
      memory.admitChildren([childItem({ id: 'mine', status: 'running', summary: 'mine', updatedAt: 1 })]);
      flushSync();
      expect(probe.evaluations).toBe(baseline);

      memory.applyChildPatch(patch('mine', { summary: 'mine, later', updatedAt: 2 }));
      flushSync();
      expect(probe.evaluations).toBe(baseline + 1);
      expect(probe.latest?.activePreview).toBe('mine, later');
    } finally {
      probe.dispose();
    }
  });

  it('drops aggregates whose root left the window, and wakes their readers', () => {
    const { memory, rows } = makeMemoryHarness([anchorItem(), codexSpawn({ itemIndex: 10 })]);
    memory.admitChildren([childItem(), childItem({ id: 'c2', parentId: 'spawn-1', itemIndex: 11 })]);
    const probe = probeReactivity(() => memory.aggregate('anchor'));
    try {
      rows.delete('anchor');
      memory.retainFoldAnchors();
      flushSync();
      expect(probe.latest).toBeUndefined();
      expect(memory.isKnownChild('child-1')).toBe(false);
      expect(memory.aggregate('spawn-1')?.count).toBe(1);
    } finally {
      probe.dispose();
    }
  });

  it('carries counts and terminal previews through snapshot and restore, and clears', () => {
    const { memory } = makeMemoryHarness([anchorItem()]);
    memory.admitChildren([childItem(), childItem({ id: 'c2', itemIndex: 2, status: 'running', summary: 'live', updatedAt: 1 })]);
    const snapshot = memory.snapshotFolds();

    const other = makeMemoryHarness([anchorItem()]);
    other.memory.restoreFolds(snapshot);
    expect(other.memory.aggregate('anchor')).toMatchObject({ count: 2, activePreview: '', terminalPreview: 'ran the build' });

    // A replay of a carried child is not counted again.
    other.memory.admitChildren([childItem({ id: 'c2', itemIndex: 2, summary: 'done' })]);
    expect(other.memory.aggregate('anchor')).toMatchObject({ count: 2, terminalPreview: 'done' });

    other.memory.clearFolds();
    expect(other.memory.aggregate('anchor')).toBeUndefined();
    expect(other.memory.snapshotFolds()).toBeNull();
  });

  it('bounds retained memory across 100 agents with 200 rows each', () => {
    const anchors = Array.from({ length: 100 }, (_, a) =>
      anchorItem({ id: `agent-${a}`, turnIndex: a, itemIndex: 0 }));
    const { memory } = makeMemoryHarness(anchors);
    for (let r = 0; r < 200; r += 1) {
      const batch: Item[] = [];
      for (let a = 0; a < 100; a += 1) {
        batch.push(childItem({
          id: `agent-${a}-row-${r}`,
          parentId: `agent-${a}`,
          turnIndex: a,
          itemIndex: r + 1,
          status: 'running',
          summary: `row ${r}`,
          updatedAt: r,
        }));
      }
      memory.admitChildren(batch);
    }
    const stats = memory.foldStats();
    expect(stats.children).toBe(MAX_TRACKED_CHILDREN);
    expect(stats.anchors).toBe(100);
    expect(stats.activeEntries).toBeLessThanOrEqual(100 * MAX_ACTIVE_PER_ANCHOR);
    for (let a = 0; a < 100; a += 1) {
      expect(memory.aggregate(`agent-${a}`)).toMatchObject({ count: 200, activePreview: 'row 199' });
    }
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

  it('records a streamed child on its loaded anchor without entering pane.items', async () => {
    const pane = await paneWithSlice([
      makeItem({ id: 'pre', threadId: THREAD_ID, turnIndex: 0, itemIndex: 0 }),
      anchorItem(),
    ]);
    const before = pane.items;

    expect(pane.upsertItem(childItem({ status: 'running', summary: 'building', updatedAt: 1 }))).toBe(false);

    expect(pane.items).toBe(before);
    expect(pane.subagentLiveAggregate('anchor')).toMatchObject({ count: 1, activePreview: 'building' });

    pane.applyItemPatch(patch('child-1', { status: 'completed', summary: 'built', updatedAt: 2 }));
    expect(pane.items).toBe(before);
    expect(pane.subagentLiveAggregate('anchor')).toMatchObject({ count: 1, activePreview: '', terminalPreview: 'built' });
  });

  it('keeps an anchorless streamed child out of pane memory', async () => {
    const pane = await paneWithSlice([
      makeItem({ id: 'pre', threadId: THREAD_ID, turnIndex: 0, itemIndex: 0 }),
    ]);
    expect(pane.upsertItem(childItem())).toBe(false);
    expect(pane.items.some((it) => it.id === 'child-1')).toBe(false);
    expect(pane.subagentLiveAggregate('anchor')).toBeUndefined();
  });

  it('counts a child once when its anchor and the child land in one batch', async () => {
    const pane = await paneWithSlice([
      makeItem({ id: 'pre', threadId: THREAD_ID, turnIndex: 0, itemIndex: 0 }),
    ]);
    expect(pane.upsertItems([anchorItem(), childItem()])).toBe(true);
    expect(pane.items.map((it) => it.id)).toEqual(['pre', 'anchor']);
    expect(pane.subagentLiveAggregate('anchor')?.count).toBe(1);

    pane.upsertItems([childItem()]);
    expect(pane.subagentLiveAggregate('anchor')?.count).toBe(1);
  });

  it('silences deltas for a tracked child and warns once for a real gap', async () => {
    const pane = await paneWithSlice([
      makeItem({ id: 'pre', threadId: THREAD_ID, turnIndex: 0, itemIndex: 0 }),
    ]);
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      pane.upsertItem(childItem({ kind: 'assistant_text', status: 'streaming', summary: '' }));

      pane.applyItemDelta({ threadId: THREAD_ID, itemId: 'child-1', kind: 'assistant_text', delta: 'more', updatedAt: 2 });
      pane.applyItemDelta({ threadId: THREAD_ID, itemId: 'child-1', kind: 'assistant_text', delta: ' and more', updatedAt: 3 });
      expect(warn).not.toHaveBeenCalled();

      // A delta for a row that was never streamed at all is the genuine
      // transport-gap case: still reported, but only once per id.
      for (let i = 0; i < 3; i += 1) {
        pane.applyItemDelta({ threadId: THREAD_ID, itemId: 'ghost', kind: 'assistant_text', delta: 'x', updatedAt: 4 + i });
      }
      expect(warn).toHaveBeenCalledTimes(1);
      expect(warn.mock.calls[0][1]).toBe('ghost');
    } finally {
      warn.mockRestore();
    }
  });
});
