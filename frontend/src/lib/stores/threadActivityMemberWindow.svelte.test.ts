import { beforeEach, expect, it } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem, makeThread } from '../../test/helpers/chat';
import { flushMicrotasks, installThreadPaneTestEnv } from '../../test/helpers/threadPane';

beforeEach(installThreadPaneTestEnv);

it('keeps a held agent scope and newer live rows while replacing a run window', async () => {
  const { holdAgentScope } = await import('./agentPane.svelte');
  const pane = createThreadPane();
  const launch = makeItem({ id: 'agent', threadId: 't', itemIndex: 1, kind: 'tool_call', toolName: 'Agent', status: 'running' });
  const member = (id: string, itemIndex: number, rev = 1) => makeItem({ id, threadId: 't', itemIndex, kind: 'tool_call', toolName: 'Bash', rev });
  const run = {
    firstItemId: 'agent', lastItemId: 'e', memberCount: 5,
    firstTurnIndex: 0, firstItemIndex: 1, lastTurnIndex: 0, lastItemIndex: 5,
    loadedFirstItemId: 'agent', loadedLastItemId: 'd',
    unshippedBefore: 0, unshippedAfter: 1, unshippedDigest: '0000000000000000',
    unshippedGroups: [], unshippedPairedLaunchIds: [], shippedSupersededLaunchIds: [],
    unshippedFailed: false, runningBefore: null, runningAfter: null,
  };
  setBindingMock('ListThreadSliceAround', async () => ({ items: [launch, member('b', 2), member('c', 3), member('d', 4)], runs: [run],
    hasMoreOlder: false, hasMoreNewer: false, oldestTurnIndex: 0, newestTurnIndex: 0 }));
  await pane.switchThread(makeThread({ id: 't' }));
  const release = holdAgentScope(pane.paneId, 't', 'agent');
  try {
    setBindingMock('ListSubagentDescendants', async () => [makeItem({ id: 'child', threadId: 't', parentId: 'agent', itemIndex: 8, status: 'running' })]);
    await pane.ensureSubagentChildren('agent');
    setBindingMock('GetThreadItem', async () => member('e', 5));
    let respond!: (value: unknown) => void;
    setBindingMock('ListActivityRunMembers', () => new Promise(resolve => { respond = resolve; }));
    const pending = pane.loadUntilItem('e');
    await flushMicrotasks();
    pane.upsertItem({ ...member('c', 3, 5), summary: 'newer live result', status: 'completed' });
    respond({ items: [member('c', 3), member('d', 4), member('e', 5)],
      stub: { ...run, loadedFirstItemId: 'c', loadedLastItemId: 'e', unshippedBefore: 2, unshippedAfter: 0 } });
    expect(await pending).toBe('loaded');
    expect(pane.getItemById('c')?.summary).toBe('newer live result');
    expect(pane.getItemById('agent')).toBeDefined();
    expect(pane.getItemById('child')).toBeDefined();
    expect(pane.activityRuns.loadedItems(pane.items).filter(item => !item.parentId).map(item => item.id)).toEqual(['c', 'd', 'e']);
    expect(pane.activityRuns.snapshotStubs()?.[0]).toMatchObject({ loadedFirstItemId: 'c', unshippedBefore: 2 });
    release();
    pane.sweepUnheldAgentScopes();
    expect(pane.getItemById('agent')).toBeUndefined();
    expect(pane.getItemById('child')).toBeUndefined();
    expect(pane.items.map(item => item.id)).toEqual(['c', 'd', 'e']);
    const releaseAgain = holdAgentScope(pane.paneId, 't', 'agent');
    try {
      setBindingMock('GetThreadItem', async () => launch);
      expect(await pane.loadAgentScope('agent')).toBe('loaded');
      expect(pane.getItemById('child')).toBeDefined();
      expect(pane.activityRuns.loadedItems(pane.items).filter(item => !item.parentId).map(item => item.id)).toEqual(['c', 'd', 'e']);
    } finally { releaseAgain(); }
    pane.sweepUnheldAgentScopes();
    expect(pane.getItemById('child')).toBeUndefined();
  } finally {
    release();
    pane.clear();
  }
});

