// The agent companion pane (docs/specs/agent-visibility.md Q4/Q5): the
// REAL MessageTimeline over the scoped facade (agentScopeView.svelte.ts).
// These tests drive it with a real ThreadPane so the scoped projection,
// the breadcrumb, the self-close rule, and the composer shell's Stop gate
// run against the same state the chat surface uses.
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import AgentPane from './AgentPane.svelte';
import { installPaneMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import {
  __resetAgentPaneStateForTest,
  openAgentCompanion,
} from '../../stores/agentPane.svelte';
import { makePanelContext, type PanelContext } from '../../stores/panelContext.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { loadSettings } from '../../stores/settings.svelte';
import type { Item } from '../../types/models';

const THREAD_ID = 'thread-agent';

function launchItem(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'launch-1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Agent',
    role: 'assistant',
    status: 'running',
    threadId: THREAD_ID,
    summary: 'Agent: exploring',
    payloadMeta: JSON.stringify({
      toolName: 'Agent',
      input: { description: 'Explore the parser', subagent_type: 'Explore' },
    }),
    ...overrides,
  });
}

async function setup(items: Item[]): Promise<{ pane: ThreadPane; ctx: PanelContext }> {
  const thread = makeThread({ id: THREAD_ID });
  installPaneMocks(items);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(thread);
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 400 }]);
  const ctx = makePanelContext(pane, () => {});
  return { pane, ctx };
}

describe('<AgentPane>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    __resetAgentPaneStateForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('ListSubagentDescendants', async () => []);
    await loadSettings();
  });

  afterEach(() => {
    cleanup();
    __resetAgentPaneStateForTest();
  });

  it('renders only the scoped subtree, thinking and intermediate text included', async () => {
    const { ctx } = await setup([
      launchItem(),
      makeItem({ id: 'top-text', itemIndex: 1, threadId: THREAD_ID, summary: 'main thread prose' }),
      makeItem({ id: 'child-think', itemIndex: 2, threadId: THREAD_ID, parentId: 'launch-1', kind: 'thinking', summary: 'pondering' }),
      makeItem({ id: 'child-tool', itemIndex: 3, threadId: THREAD_ID, parentId: 'launch-1', kind: 'tool_call', toolName: 'Bash', status: 'completed', summary: 'ls' }),
      makeItem({ id: 'child-text', itemIndex: 4, threadId: THREAD_ID, parentId: 'launch-1', summary: 'intermediate note' }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');

    const { getByTestId, queryByText } = render(AgentPane, { props: { ctx } });

    const timeline = getByTestId('agent-pane-timeline');
    expect(timeline.textContent).toContain('intermediate note');
    expect(timeline.textContent).toContain('ls');
    // The main thread's own rows never leak into the scope.
    expect(queryByText('main thread prose')).toBeNull();
    expect(getByTestId('agent-pane-breadcrumb-current').textContent?.trim()).toBe('Explore');
    expect(getByTestId('agent-pane-status-line')).toBeTruthy();
    expect(getByTestId('agent-pane-composer-shell')).toBeTruthy();
  });

  it('breadcrumb pop returns to the outer scope; popping to the root closes the pane', async () => {
    const closeAgentPane = vi.fn();
    const { ctx } = await setup([
      launchItem(),
      makeItem({ id: 'outer-note', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', summary: 'outer note' }),
      makeItem({ id: 'nested-launch', itemIndex: 2, threadId: THREAD_ID, parentId: 'launch-1', kind: 'tool_call', toolName: 'Agent', status: 'running', summary: 'Agent: nested', payloadMeta: JSON.stringify({ toolName: 'Agent', input: { description: 'nested', subagent_type: 'Explore' } }) }),
      makeItem({ id: 'nested-child', itemIndex: 3, threadId: THREAD_ID, parentId: 'nested-launch', summary: 'deep work' }),
    ]);
    const state = openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    state?.pushScope('nested-launch', 'nested');
    const patched = { ...ctx, closeAgentPane } as PanelContext;

    const { getByTestId, getAllByTestId, getAllByText } = render(AgentPane, { props: { ctx: patched } });
    expect(getByTestId('agent-pane-breadcrumb-current').textContent?.trim()).toBe('nested');

    // Pop one hop: back to the launch scope — and the outer scope's OWN
    // leaf rows come back with it (e2e regression: after a pop the pane
    // rendered only the nested card).
    const entries = getAllByTestId('agent-pane-breadcrumb-entry');
    await fireEvent.click(entries[entries.length - 1]);
    expect(getByTestId('agent-pane-breadcrumb-current').textContent?.trim()).toBe('Explore');
    expect(getAllByText('outer note').length).toBeGreaterThanOrEqual(1);

    // Pop to root: empty scope — the body closes the companion.
    await fireEvent.click(getAllByTestId('agent-pane-breadcrumb-entry')[0]);
    await tick();
    expect(closeAgentPane).toHaveBeenCalled();
  });

  it('self-closes when a row it has seen vanishes, not when the row was never loaded', async () => {
    const closeAgentPane = vi.fn();
    const { pane, ctx } = await setup([launchItem(), makeItem({ id: 'other', itemIndex: 1, threadId: THREAD_ID })]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    const patched = { ...ctx, closeAgentPane } as PanelContext;
    render(AgentPane, { props: { ctx: patched } });
    expect(closeAgentPane).not.toHaveBeenCalled();

    // The revert cut the launch row out of a still-loaded timeline.
    pane.removeItemById('launch-1', THREAD_ID);
    await waitFor(() => expect(closeAgentPane).toHaveBeenCalled());
  });

  it('shows Stop only for a running Claude launch that carries a task_id', async () => {
    const stop = vi.fn(async () => {});
    setBindingMock('StopClaudeTask', stop);
    const { ctx } = await setup([
      launchItem({
        isBackground: true,
        meta: JSON.stringify({ task_id: 'task-9' }),
      }),
      makeItem({ id: 'child-1', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', summary: 'work' }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    const { getByTestId } = render(AgentPane, { props: { ctx } });

    await fireEvent.click(getByTestId('agent-pane-stop'));
    await waitFor(() => expect(stop).toHaveBeenCalledWith(THREAD_ID, 'task-9'));
  });

  it('offers no Stop for a forked skill (no task lifecycle on the wire)', async () => {
    const { ctx } = await setup([
      launchItem({
        toolName: 'Skill',
        summary: 'Skill: code-review',
        payloadMeta: JSON.stringify({ toolName: 'Skill', input: { command: 'code-review' } }),
        meta: JSON.stringify({ skillFork: { agentId: 'a1', commandName: 'code-review' } }),
      }),
      makeItem({ id: 'child-1', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', summary: 'fork work' }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'code-review');
    const { queryByTestId, getByTestId } = render(AgentPane, { props: { ctx } });

    expect(getByTestId('agent-pane-composer-shell')).toBeTruthy();
    expect(queryByTestId('agent-pane-stop')).toBeNull();
  });

  it('marks a backgrounded running launch as streaming-paused', async () => {
    const { ctx } = await setup([
      launchItem({
        isBackground: true,
        meta: JSON.stringify({ task_id: 'task-9', subagentBackgroundedAt: 123 }),
      }),
      makeItem({ id: 'child-1', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', summary: 'work' }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    const { getByTestId } = render(AgentPane, { props: { ctx } });

    expect(getByTestId('agent-pane-streaming-paused')).toBeTruthy();
    expect(getByTestId('agent-pane-background-pill')).toBeTruthy();
  });

  it('renders a nested launch as a real card (no orphan warnings) with grandchildren intact', async () => {
    // Regression (found by the F6 harness): a direct child whose parentId
    // is absent from the grouping input ranks as an orphan, a nested
    // launch never becomes a card, and grandchild rows are dropped. The
    // facade lifts direct children to the scope's top level instead.
    const { ctx } = await setup([
      launchItem(),
      makeItem({ id: 'nested-launch', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', kind: 'tool_call', toolName: 'Agent', status: 'running', summary: 'Agent: nested', payloadMeta: JSON.stringify({ toolName: 'Agent', input: { description: 'nested', subagent_type: 'Explore' } }) }),
      makeItem({ id: 'grandchild', itemIndex: 2, threadId: THREAD_ID, parentId: 'nested-launch', summary: 'grandchild work' }),
    ]);
    const state = openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    const { getByTestId, queryByText, getAllByTestId, getAllByText } = render(AgentPane, { props: { ctx } });

    expect(queryByText(/Orphan subagent entry/)).toBeNull();
    const card = getByTestId('subagent-group');
    expect(card).toBeTruthy();
    // Expand the nested card: the grandchild ROW renders inside it (the
    // card's collapsed preview also echoes the text, hence getAll).
    await fireEvent.click(getByTestId('subagent-group-toggle'));
    expect(getAllByText('grandchild work').length).toBeGreaterThanOrEqual(2);

    // Descending from inside the pane goes through the facade's
    // openAgentPane override — a breadcrumb hop, not a companion re-seed.
    await fireEvent.click(getAllByTestId('subagent-group-open-pane')[0]);
    expect(state?.scopeItemId).toBe('nested-launch');
    expect(state?.breadcrumb.map((entry) => entry.itemId)).toEqual(['', 'launch-1', 'nested-launch']);
  });

  it('renders a NESTED scope’s children after descending (scope row with a parentId)', async () => {
    // Regression (found by the F6 harness, second round): a descended-into
    // scope row carries a parentId pointing OUTSIDE the pane's grouping
    // input, which orphan-leafed the scope itself — no group node to
    // unwrap, so the pane went empty ("No output yet.") at exactly the
    // scope the breadcrumb said it was showing. The scope row's parentId
    // is cleared before grouping: within this pane the scope is the root.
    const { ctx } = await setup([
      launchItem(),
      makeItem({ id: 'nested-launch', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', kind: 'tool_call', toolName: 'Agent', status: 'running', summary: 'Agent: nested', payloadMeta: JSON.stringify({ toolName: 'Agent', input: { description: 'nested', subagent_type: 'Explore' } }) }),
      makeItem({ id: 'grandchild', itemIndex: 2, threadId: THREAD_ID, parentId: 'nested-launch', summary: 'grandchild work' }),
    ]);
    const state = openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    state?.pushScope('nested-launch', 'nested');

    const { getByText, queryByTestId } = render(AgentPane, { props: { ctx } });
    expect(queryByTestId('agent-pane-empty')).toBeNull();
    expect(getByText('grandchild work')).toBeTruthy();
  });

  it("renders a finished Codex child's final answer from the completion sibling", async () => {
    const { ctx } = await setup([
      launchItem({
        toolName: 'collab_agent',
        status: 'completed',
        summary: 'Spawn reviewer',
        payloadMeta: JSON.stringify({ toolName: 'collab_agent', input: { tool: 'spawn_agent', newAgentNickname: 'reviewer' } }),
      }),
      makeItem({
        id: 'complete-1',
        itemIndex: 1,
        threadId: THREAD_ID,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        status: 'completed',
        completionOf: 'launch-1',
        payloadMeta: JSON.stringify({ preview: 'Final verdict: LGTM' }),
      }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'reviewer');
    const { getByTestId, queryByTestId } = render(AgentPane, { props: { ctx } });

    expect(queryByTestId('agent-pane-empty')).toBeNull();
    expect(getByTestId('agent-pane-final-answer').textContent).toContain('Final verdict: LGTM');
  });
});
