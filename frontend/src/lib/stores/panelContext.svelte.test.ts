import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makePanelContext } from './panelContext.svelte';
import { createThreadPane } from './thread.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import {
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  getPaneLayoutItems,
} from './paneLayout.svelte';
import {
  installCompanionPanes,
  isCompanionOpen,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
import {
  __resetAgentPaneStateForTest,
  agentScopeForPane,
  agentStateForPane,
} from './agentPane.svelte';
import { installPaneMocks, makeItem, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks } from '../../test/mocks/bindings-app';

async function mountedPane(items = [makeItem({ id: 'item-1' })]) {
  installPaneMocks(items);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: 'thread-1' }));
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 560 }]);
  return pane;
}

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  __resetAgentPaneStateForTest();
  installCompanionPanes();
});

afterEach(() => {
  __resetAgentPaneStateForTest();
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
});

describe('makePanelContext', () => {
  it('projects the timeline accessors the agent body renders from', async () => {
    const pane = await mountedPane([makeItem({ id: 'item-1' }), makeItem({ id: 'item-2', itemIndex: 1 })]);
    const ctx = makePanelContext(pane, () => {});

    expect(ctx.items.map((item) => item.id)).toEqual(['item-1', 'item-2']);
    expect(ctx.getItemById('item-2')?.id).toBe('item-2');
    expect(ctx.getItemById('missing')).toBeUndefined();
    expect(ctx.timelineRevision).toBe(pane.timelineRevision);
    expect(ctx.pendingApprovals).toEqual([]);
    expect(ctx.activityRuns).toBe(pane.activityRuns);
    expect(ctx.latestSettledTurn).toBe(pane.latestSettledTurn);
    expect(ctx.canCompose).toBe(true);
    expect(await ctx.ensureSubagentChildren('item-1')).toBe(false);
  });

  it('keeps object identity stable while the pane reassigns its thread', async () => {
    const pane = await mountedPane();
    const ctx = makePanelContext(pane, () => {});
    const before = ctx.thread;

    pane.replaceThread(makeThread({ id: 'thread-1', title: 'Renamed' }));

    // The ctx object is captured once by the panel shell; the getters are
    // what must see the new thread, not a rebuilt object.
    expect(ctx.thread).not.toBe(before);
    expect(ctx.thread?.title).toBe('Renamed');
    expect(ctx.threadId).toBe('thread-1');
  });

  it('publishes a scroll-to-item intent on the source pane', async () => {
    const pane = await mountedPane();
    const ctx = makePanelContext(pane, () => {});
    const before = pane.scrollToItemRequest.nonce;

    ctx.requestScrollToItem('item-1');

    expect(pane.scrollToItemRequest).toEqual({ itemId: 'item-1', nonce: before + 1 });
  });

  it('descends the agent scope and closes the agent pane', async () => {
    const pane = await mountedPane();
    const ctx = makePanelContext(pane, () => {});
    pane.openAgentPane('launch-1', 'code-review');

    ctx.openAgentScope('launch-2', 'Angle B');

    expect(agentStateForPane('main', 'thread-1').scopeItemId).toBe('launch-2');
    expect(agentStateForPane('main', 'thread-1').breadcrumb.map((entry) => entry.label))
      .toEqual(['main', 'code-review', 'Angle B']);
    expect(pane.showAgentPane).toBe(true);

    ctx.closeAgentPane();

    expect(pane.showAgentPane).toBe(false);
    expect(isCompanionOpen('main', 'agent')).toBe(false);
    expect(agentScopeForPane('main', 'thread-1')).toBeNull();
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['main']);
  });

  it('ignores an empty scope id', async () => {
    const pane = await mountedPane();
    const ctx = makePanelContext(pane, () => {});
    pane.openAgentPane('launch-1', 'code-review');

    ctx.openAgentScope('', 'nothing');

    expect(agentStateForPane('main', 'thread-1').scopeItemId).toBe('launch-1');
  });
});
