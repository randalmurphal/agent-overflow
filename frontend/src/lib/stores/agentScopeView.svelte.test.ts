// The scoped ThreadPane facade the agent pane mounts MessageTimeline on.
// These tests pin the override table against a REAL ThreadPane: what the
// scope window contains, which identities diverge, and that everything
// else forwards to the source pane.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { createAgentScopeView } from './agentScopeView.svelte';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import {
  closeCompanionsForSource,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
import {
  __resetAgentPaneStateForTest,
  openAgentCompanion,
  type AgentPaneState,
} from './agentPane.svelte';
import { installPaneMocks, makeItem, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks } from '../../test/mocks/bindings-app';
import type { Item } from '../types/models';

const THREAD_ID = 'thread-scope';

function fixtureItems(): Item[] {
  return [
    makeItem({ id: 'launch-1', itemIndex: 0, threadId: THREAD_ID, kind: 'tool_call', toolName: 'Agent', status: 'running', summary: 'Agent: outer' }),
    makeItem({ id: 'main-text', itemIndex: 1, threadId: THREAD_ID, summary: 'main thread prose' }),
    makeItem({ id: 'child-a', itemIndex: 2, threadId: THREAD_ID, parentId: 'launch-1', summary: 'child a' }),
    makeItem({ id: 'nested-launch', itemIndex: 3, threadId: THREAD_ID, parentId: 'launch-1', kind: 'tool_call', toolName: 'Agent', status: 'completed', summary: 'Agent: nested' }),
    makeItem({ id: 'grandchild', itemIndex: 4, threadId: THREAD_ID, parentId: 'nested-launch', summary: 'grandchild work' }),
    // The NESTED launch's completion sibling: no parentId, only completionOf.
    makeItem({ id: 'nested-completion', itemIndex: 5, threadId: THREAD_ID, kind: 'tool_completion', status: 'completed', completionOf: 'nested-launch', summary: 'nested done' }),
    // The SCOPE's own completion sibling: stays out (feeds the status line).
    makeItem({ id: 'scope-completion', itemIndex: 6, threadId: THREAD_ID, kind: 'tool_completion', status: 'completed', completionOf: 'launch-1', summary: 'outer done' }),
  ];
}

async function setup(): Promise<{ pane: ThreadPane; agent: AgentPaneState }> {
  installPaneMocks(fixtureItems());
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: THREAD_ID }));
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 400 }]);
  const agent = openAgentCompanion('main', THREAD_ID, 'launch-1', 'outer')!;
  return { pane, agent };
}

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetCompanionPanesForTest();
  __resetAgentPaneStateForTest();
});

afterEach(() => {
  __resetAgentPaneStateForTest();
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
});

describe('createAgentScopeView', () => {
  it('scopes items to the launch subtree, lifting direct children to top level', async () => {
    const { pane, agent } = await setup();
    const view = createAgentScopeView(pane, agent, 'launch-1');

    const ids = view.items.map((item) => item.id);
    expect(ids).toContain('child-a');
    expect(ids).toContain('nested-launch');
    expect(ids).toContain('grandchild');
    expect(ids).not.toContain('main-text');
    expect(ids).not.toContain('launch-1');

    // Direct children read as this surface's top level; deeper rows keep
    // their real parent chain so nested cards still group.
    const childA = view.items.find((item) => item.id === 'child-a')!;
    expect(childA.parentId).toBeUndefined();
    const grandchild = view.items.find((item) => item.id === 'grandchild')!;
    expect(grandchild.parentId).toBe('nested-launch');

    view.dispose();
  });

  it('carries a nested launch’s completion sibling but never the scope’s own', async () => {
    const { pane, agent } = await setup();
    const view = createAgentScopeView(pane, agent, 'launch-1');

    const ids = view.items.map((item) => item.id);
    expect(ids).toContain('nested-completion');
    expect(ids).not.toContain('scope-completion');

    view.dispose();
  });

  it('answers scoped identities and inert paging, forwards the rest', async () => {
    const { pane, agent } = await setup();
    const view = createAgentScopeView(pane, agent, 'launch-1');
    const facade = view.pane;

    // Diverging identities.
    expect(facade.paneId).toBe('main~agent');
    expect(facade.scrollStateKey).toBe(`${THREAD_ID}~agent:launch-1`);
    expect(facade.scrollStateKey).not.toBe(pane.scrollStateKey);
    expect(facade.revealBoundary).toBeNull();
    expect(facade.activityRuns).not.toBe(pane.activityRuns);

    // A scope window has no edges to page.
    expect(facade.hasMoreHistory).toBe(false);
    expect(facade.hasMoreNewer).toBe(false);
    expect(facade.loading).toBe(false);
    expect(facade.showLoadingSpinner).toBe(false);
    await expect(facade.loadOlder()).resolves.toMatchObject({ status: 'noop' });
    await expect(facade.loadUntilItem('grandchild')).resolves.toBe(true);
    await expect(facade.loadUntilItem('main-text')).resolves.toBe(false);

    // Everything else is the source pane.
    expect(facade.threadId).toBe(pane.threadId);
    expect(facade.getItemById('main-text')?.id).toBe('main-text');
    expect(facade.timelineRevision).toBe(pane.timelineRevision);

    view.dispose();
  });

  it('keeps its scroll-to-item slot separate from the source pane’s', async () => {
    const { pane, agent } = await setup();
    const view = createAgentScopeView(pane, agent, 'launch-1');
    const before = pane.scrollToItemRequest;

    view.pane.requestScrollToItem('grandchild');

    expect(view.pane.scrollToItemRequest.itemId).toBe('grandchild');
    expect(pane.scrollToItemRequest).toBe(before);

    view.dispose();
  });

  it('routes openAgentPane to a breadcrumb hop instead of re-seeding the companion', async () => {
    const { pane, agent } = await setup();
    const view = createAgentScopeView(pane, agent, 'launch-1');

    view.pane.openAgentPane('nested-launch', 'nested');

    expect(agent.scopeItemId).toBe('nested-launch');
    expect(agent.breadcrumb.map((entry) => entry.itemId)).toEqual(['', 'launch-1', 'nested-launch']);

    view.dispose();
  });

  it('never lets the scoped instance’s prune reach the shared row-UI store', async () => {
    // Regression for the 2026-08-22 dead-screenshots incident: the agent
    // pane's MessageTimeline ran the row-UI prune with scope-only
    // retention against the SHARED store, revoking the main timeline's
    // attachment blobs (and the main prune disposed agent-pane rows).
    const { pane, agent } = await setup();
    const view = createAgentScopeView(pane, agent, 'launch-1');
    pane.setUserMessageExpanded('main-text', true);

    view.pane.pruneRowUiState({ itemIds: new Set(), payloads: [], groupKeys: new Set() });

    expect(pane.isUserMessageExpanded('main-text')).toBe(true);
    view.dispose();
  });

  it('host prune spares the open scope subtree, and stops sparing once the pane closes', async () => {
    // The other half of the shared-store contract: the source pane's own
    // prune widens its retention with the open scope's subtree, so state
    // under rows only the agent pane has mounted survives the chat
    // timeline's bounded-memory pass — exactly while the pane is open.
    const { pane } = await setup();
    pane.setUserMessageExpanded('grandchild', true);
    expect(pane.toggleSubagentGroupExpanded('nested-launch')).toBe(true);
    const emptyRetention = () => ({
      itemIds: new Set<string>(),
      payloads: [],
      groupKeys: new Set<string>(),
    });

    pane.pruneRowUiState(emptyRetention());
    expect(pane.isUserMessageExpanded('grandchild')).toBe(true);
    expect(pane.isSubagentGroupExpanded('nested-launch')).toBe(true);

    closeCompanionsForSource('main');
    pane.pruneRowUiState(emptyRetention());
    expect(pane.isUserMessageExpanded('grandchild')).toBe(false);
    expect(pane.isSubagentGroupExpanded('nested-launch')).toBe(false);
  });

  it('keys the scoped window as one turn that follows the launch lifecycle', async () => {
    // The live regression: the main turn settling stamped "Response 1m 58s"
    // on a still-running subagent, because the decorations keyed on
    // `item.turnIndex` + the THREAD's active/settled turn. The facade
    // answers its own facet: one key for every scoped row, active while
    // the scoped launch runs, settled on the launch's own completion with
    // the agent's own duration.
    const { pane, agent } = await setup();
    // Fixture: launch-1 is still running and has no completion yet.
    pane.removeItemById('scope-completion', THREAD_ID);
    const view = createAgentScopeView(pane, agent, 'launch-1');
    const turns = view.pane.timelineTurns;

    const first = view.items[0];
    const last = view.items[view.items.length - 1];
    expect(turns.keyOf(first)).toBe(turns.keyOf(last));
    // The source pane keys on the provider turn; the facade must not.
    expect(pane.timelineTurns.keyOf(makeItem({ turnIndex: 7 }))).toBe(7);

    expect(turns.activeKey).toBe(turns.keyOf(first));
    expect(turns.settled).toBeNull();

    // The completion sibling lands: the scope's turn settles on IT, not on
    // anything the main thread did.
    pane.upsertItem(
      makeItem({
        id: 'scope-completion',
        itemIndex: 6,
        threadId: THREAD_ID,
        kind: 'tool_completion',
        status: 'completed',
        completionOf: 'launch-1',
        createdAt: 5_000,
        updatedAt: 9_000,
        summary: 'outer done',
      }),
    );

    expect(turns.activeKey).toBeNull();
    expect(turns.settled).toEqual({
      key: turns.keyOf(first),
      startedAt: pane.getItemById('launch-1')!.createdAt,
      completedAt: 9_000,
    });

    view.dispose();
  });

  it('recomputes the window when the source timeline changes', async () => {
    const { pane, agent } = await setup();
    const view = createAgentScopeView(pane, agent, 'launch-1');
    expect(view.items.some((item) => item.id === 'child-a')).toBe(true);

    pane.removeItemById('child-a', THREAD_ID);

    expect(view.items.some((item) => item.id === 'child-a')).toBe(false);

    view.dispose();
  });
});
