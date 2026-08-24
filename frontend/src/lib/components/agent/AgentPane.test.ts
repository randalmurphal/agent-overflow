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
    expect(getByTestId('agent-pane-composer-shell')).toBeTruthy();
    // Model chip: this launch names no override, so it inherits the caller.
    expect(getByTestId('agent-pane-model').textContent?.trim()).toBe('Sonnet 4.6');
    // The header carries the launch's own one-line task.
    expect(getByTestId('agent-pane-description').textContent?.trim()).toBe('Explore the parser');
  });

  it('hides the root breadcrumb entry at depth one (closing the pane is "back to main")', async () => {
    const { ctx } = await setup([
      launchItem(),
      makeItem({ id: 'child-1', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', summary: 'work' }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    const { getByTestId, queryAllByTestId } = render(AgentPane, { props: { ctx } });

    expect(getByTestId('agent-pane-breadcrumb-current').textContent?.trim()).toBe('Explore');
    expect(queryAllByTestId('agent-pane-breadcrumb-entry')).toHaveLength(0);
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

    const { getByTestId, getAllByTestId, getAllByText, queryAllByTestId } = render(AgentPane, {
      props: { ctx: patched },
    });
    expect(getByTestId('agent-pane-breadcrumb-current').textContent?.trim()).toBe('nested');

    // Pop one hop: back to the launch scope — and the outer scope's OWN
    // leaf rows come back with it (e2e regression: after a pop the pane
    // rendered only the nested card).
    const entries = getAllByTestId('agent-pane-breadcrumb-entry');
    await fireEvent.click(entries[entries.length - 1]);
    expect(getByTestId('agent-pane-breadcrumb-current').textContent?.trim()).toBe('Explore');
    expect(getAllByText('outer note').length).toBeGreaterThanOrEqual(1);
    // Back at depth one the root entry is hidden — the pane's close
    // button is the way back to main (user ruling 2026-08-22).
    expect(queryAllByTestId('agent-pane-breadcrumb-entry')).toHaveLength(0);

    // Popping to the root (programmatic — layout restore, scope resets)
    // still empties the scope and the body closes the companion.
    state?.popTo(0);
    await tick();
    expect(closeAgentPane).toHaveBeenCalled();
  });

  it('pages the window to a restored scope whose launch sits above the tail', async () => {
    // Restore-shaped (bug 2026-08-22): layout restore re-seeds the scope,
    // but the restored window is the thread's TAIL — the launch row sits
    // above it, so the pane came back as a husk (bare label, dead body).
    // The pane must page the window to its scope row itself.
    const { pane, ctx } = await setup([
      makeItem({ id: 'tail-row', itemIndex: 9, threadId: THREAD_ID, summary: 'tail prose' }),
    ]);
    const loadUntilItem = vi.fn(async (itemId: string) => {
      expect(itemId).toBe('launch-1');
      pane.upsertItem(launchItem());
      return true;
    });
    (pane as { loadUntilItem: ThreadPane['loadUntilItem'] }).loadUntilItem = loadUntilItem;
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'General Purpose');

    const { getByTestId } = render(AgentPane, { props: { ctx } });
    await waitFor(() => expect(loadUntilItem).toHaveBeenCalledTimes(1));
    // Once the row pages in, the header fills out and the not-loaded body goes away.
    await waitFor(() =>
      expect(getByTestId('agent-pane-description').textContent?.trim()).toBe('Explore the parser'),
    );
    // One attempt per scope: the effect re-runs (launch now present) without re-paging.
    expect(loadUntilItem).toHaveBeenCalledTimes(1);
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

    // The stop control is the real SendButton in its stop variant.
    await fireEvent.click(getByTestId('composer-interrupt'));
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

  it('keeps a manually backgrounded launch live instead of showing the old paused marker', async () => {
    const { ctx } = await setup([
      launchItem({
        isBackground: true,
        meta: JSON.stringify({ task_id: 'task-9', subagentBackgroundedAt: 123 }),
      }),
      makeItem({ id: 'child-1', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', summary: 'work' }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    const { queryByTestId } = render(AgentPane, { props: { ctx } });

    expect(queryByTestId('agent-pane-streaming-paused')).toBeNull();
  });

  it('renders a nested launch as a navigation row without embedding its transcript', async () => {
    // Regression (found by the F6 harness): a direct child whose parentId
    // is absent from the grouping input ranks as an orphan, a nested
    // launch never becomes a card, and grandchild rows are dropped. The
    // facade lifts direct children to the scope's top level instead.
    const { ctx } = await setup([
      launchItem(),
      makeItem({ id: 'nested-launch', itemIndex: 1, threadId: THREAD_ID, parentId: 'launch-1', kind: 'tool_call', toolName: 'Agent', status: 'running', summary: 'Agent: nested', meta: JSON.stringify({ subagentDescendantCount: 8 }), payloadMeta: JSON.stringify({ toolName: 'Agent', input: { description: 'nested', subagent_type: 'Explore' } }) }),
      makeItem({ id: 'grandchild', itemIndex: 2, threadId: THREAD_ID, parentId: 'nested-launch', summary: 'grandchild work' }),
    ]);
    const state = openAgentCompanion('main', THREAD_ID, 'launch-1', 'Explore');
    const { getByTestId, queryByText, queryByTestId, getAllByTestId } = render(AgentPane, { props: { ctx } });

    expect(queryByText(/Orphan subagent entry/)).toBeNull();
    const card = getByTestId('subagent-group');
    expect(card).toBeTruthy();
    // Expanding a child row does not recursively embed the child's pane.
    expect(getByTestId('subagent-group-toggle')).toHaveAttribute('aria-disabled', 'true');
    await fireEvent.click(getByTestId('subagent-group-toggle'));
    expect(queryByText('grandchild work')).toBeNull();
    expect(queryByTestId('subagent-group-loading')).toBeNull();

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

  // A Codex child's final answer is a NORMAL message in the transcript —
  // its assistant text streams to the parent thread parented to the
  // launch, exactly like Claude's. The completion sibling's `preview` is
  // a 240-char truncation of that same text; rendering it as a second
  // body block showed the answer twice, unformatted and cut mid-word
  // (user ruling 2026-08-23).
  it("shows a finished Codex child's answer once, as its own transcript message", async () => {
    const { ctx } = await setup([
      launchItem({
        toolName: 'collab_agent',
        status: 'completed',
        summary: 'Spawn reviewer',
        payloadMeta: JSON.stringify({ toolName: 'collab_agent', input: { tool: 'spawn_agent', newAgentNickname: 'reviewer' } }),
      }),
      makeItem({
        id: 'child-text',
        itemIndex: 1,
        threadId: THREAD_ID,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'completed',
        parentId: 'launch-1',
        summary: 'Final verdict: LGTM, with one caveat about the parser drift.',
      }),
      makeItem({
        id: 'complete-1',
        itemIndex: 2,
        threadId: THREAD_ID,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        status: 'completed',
        completionOf: 'launch-1',
        payloadMeta: JSON.stringify({ preview: 'Final verdict: LGTM, with one caveat' }),
      }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'reviewer');
    const { getByText, queryByTestId } = render(AgentPane, { props: { ctx } });

    expect(queryByTestId('agent-pane-empty')).toBeNull();
    expect(getByText(/Final verdict: LGTM, with one caveat about the parser drift/)).toBeTruthy();
    expect(queryByTestId('agent-pane-final-answer')).toBeNull();
  });

  // MultiAgentV2 encrypts the spawn message, so the model-chosen task
  // name is the only plaintext statement of what the agent was asked to
  // do. Before this the header showed nothing at all for a V2 spawn.
  // Real V2 wire shape (codex 0.149.0): `{task_name, fork_turns,
  // message}` with the message encrypted and NO nickname anywhere. The
  // crumb already carries the model-chosen task name, so the header must
  // not append it a second time as a description.
  it('says a V2 Codex task name once, in the crumb, not twice', async () => {
    const { ctx } = await setup([
      launchItem({
        toolName: 'collab_agent',
        status: 'completed',
        summary: 'Spawn audit_internal_tail',
        payloadMeta: JSON.stringify({
          toolName: 'collab_agent',
          input: {
            tool: 'spawn_agent',
            activityKind: 'started',
            taskName: '/root/audit_internal_tail',
          },
        }),
      }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'audit_internal_tail');
    const { getByTestId, queryByTestId } = render(AgentPane, { props: { ctx } });

    expect(getByTestId('agent-pane-breadcrumb-current').textContent).toContain('audit_internal_tail');
    expect(queryByTestId('agent-pane-description')).toBeNull();
  });

  // V1 (`collabAgentToolCall`) DOES carry a plaintext prompt, and it is
  // read off the Codex input rather than through the Claude reader,
  // whose `description` branch returns unclamped text.
  it('describes a V1 Codex scope with its plaintext prompt, clamped', async () => {
    const prompt = 'Audit '.repeat(30);
    const { ctx } = await setup([
      launchItem({
        toolName: 'collab_agent',
        status: 'completed',
        summary: 'Spawn reviewer',
        payloadMeta: JSON.stringify({
          toolName: 'collab_agent',
          input: { tool: 'spawn_agent', newAgentNickname: 'reviewer', prompt },
        }),
      }),
    ]);
    openAgentCompanion('main', THREAD_ID, 'launch-1', 'reviewer');
    const { getByTestId } = render(AgentPane, { props: { ctx } });

    const shown = getByTestId('agent-pane-description').textContent ?? '';
    expect(shown).toContain('Audit Audit');
    expect(shown.length).toBeLessThanOrEqual(81);
  });
});
