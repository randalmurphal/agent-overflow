import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BackgroundTaskTrayDigest from './BackgroundTaskTrayDigest.svelte';
import { installPaneMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { __resetAgentPaneStateForTest, agentScopeHeld } from '../../stores/agentPane.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import type { Item } from '../../types/models';
import type { TrayTask } from '../../utils/backgroundTray';

const THREAD_ID = 'thread-tray';

function claudeLaunch(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'launch-1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Agent',
    role: 'assistant',
    status: 'running',
    isBackground: true,
    threadId: THREAD_ID,
    summary: 'Agent: exploring',
    payloadMeta: JSON.stringify({
      toolName: 'Agent',
      input: { description: 'Explore the parser', subagent_type: 'Explore' },
    }),
    ...overrides,
  });
}

function codexLaunch(): Item {
  return makeItem({
    id: 'spawn-1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'collab_agent',
    role: 'assistant',
    status: 'running',
    threadId: THREAD_ID,
    summary: 'spawn agent',
    meta: JSON.stringify({
      input: {
        tool: 'spawn_agent',
        receiverThreadIds: ['child-thread'],
        newAgentNickname: 'Scanner',
        prompt: 'Inspect renderer coverage',
      },
    }),
  });
}

function children(parentId: string): Item[] {
  return [
    makeItem({ id: 'prompt', itemIndex: 1, threadId: THREAD_ID, parentId, kind: 'user_text', summary: 'Explore the parser' }),
    makeItem({ id: 'thinking', itemIndex: 2, threadId: THREAD_ID, parentId, kind: 'thinking', summary: 'private reasoning' }),
    makeItem({ id: 'read-1', itemIndex: 3, threadId: THREAD_ID, parentId, kind: 'tool_call', toolName: 'Read', status: 'completed', summary: 'parser.ts' }),
    makeItem({ id: 'grep-1', itemIndex: 4, threadId: THREAD_ID, parentId, kind: 'tool_call', toolName: 'Grep', status: 'running', summary: 'tokenize' }),
    makeItem({ id: 'prose', itemIndex: 5, threadId: THREAD_ID, parentId, kind: 'assistant_text', summary: 'so far so good' }),
    makeItem({ id: 'main-after', itemIndex: 6, threadId: THREAD_ID, summary: 'main thread prose' }),
  ];
}

function taskFor(launch: Item): TrayTask {
  return { rowId: launch.id, anchor: launch, launch, completion: null, status: 'running', elapsedMs: 1_000, depth: 0 };
}

async function setup(items: Item[]): Promise<ThreadPane> {
  installPaneMocks(items);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: THREAD_ID }));
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 400 }]);
  return pane;
}

describe('<BackgroundTaskTrayDigest>', () => {
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

  for (const [provider, launch] of [['claude', claudeLaunch()], ['codex', codexLaunch()]] as const) {
    it(`renders a ${provider} agent's tools, prompt and latest text under the tray row, nothing from the main thread`, async () => {
      const pane = await setup([launch, ...children(launch.id)]);
      const { getByTestId, queryByText } = render(BackgroundTaskTrayDigest, {
        props: { pane, task: taskFor(launch), id: 'digest-1' },
      });

      const digest = getByTestId('background-task-tray-row-digest');
      await waitFor(() => expect(digest.querySelectorAll('[data-item-id]').length).toBeGreaterThanOrEqual(4));
      const ids = [...digest.querySelectorAll('[data-item-id]')].map((el) => el.getAttribute('data-item-id'));
      expect(ids).toEqual(['prompt', 'read-1', 'grep-1', 'prose']);
      expect(queryByText('private reasoning')).toBeNull();
      expect(queryByText('main thread prose')).toBeNull();
      expect(digest.querySelector('[data-testid="subagent-group-body"]')?.id).toBe('digest-1');
      expect(digest.getAttribute('data-scope-id')).toBe(launch.id);
    });
  }

  it('holds the scope against eviction while mounted and releases it on unmount', async () => {
    const launch = claudeLaunch();
    const pane = await setup([launch, ...children(launch.id)]);
    expect(agentScopeHeld('main', THREAD_ID, 'launch-1')).toBe(false);
    const view = render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await tick();
    expect(agentScopeHeld('main', THREAD_ID, 'launch-1')).toBe(true);
    view.unmount();
    expect(agentScopeHeld('main', THREAD_ID, 'launch-1')).toBe(false);
  });

  it('scopes a Claude resume carrier to its transcript root', async () => {
    const launch = claudeLaunch({ meta: JSON.stringify({ subagentDescendantCount: 4 }) });
    const carrier = makeItem({
      id: 'carrier-1',
      itemIndex: 7,
      threadId: THREAD_ID,
      kind: 'tool_call',
      toolName: 'SendMessage',
      status: 'running',
      isBackground: true,
      summary: 'resume',
      meta: JSON.stringify({ task_id: 'task-resume', transcript_root_id: 'launch-1' }),
    });
    const pane = await setup([launch, ...children(launch.id), carrier]);
    const { getByTestId } = render(BackgroundTaskTrayDigest, {
      props: { pane, task: taskFor(carrier), id: 'digest-carrier' },
    });
    const digest = getByTestId('background-task-tray-row-digest');
    expect(digest.getAttribute('data-scope-id')).toBe('launch-1');
    await waitFor(() => expect(digest.querySelectorAll('[data-item-id]').length).toBeGreaterThanOrEqual(4));
    await tick();
    expect(agentScopeHeld('main', THREAD_ID, 'launch-1')).toBe(true);
  });

  it('hydrates evicted children through ListSubagentDescendants when the count says rows are missing', async () => {
    const listDescendants = vi.fn(async () => []);
    setBindingMock('ListSubagentDescendants', listDescendants);
    const launch = claudeLaunch({ meta: JSON.stringify({ task_id: 'task-1', subagentDescendantCount: 9 }) });
    const pane = await setup([launch, ...children(launch.id)]);
    render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await waitFor(() => expect(listDescendants).toHaveBeenCalledWith(THREAD_ID, 'launch-1', false));
  });

  it('loads the scope once when its launch is outside the loaded window, and sweeps it on close', async () => {
    const launch = claudeLaunch();
    const pane = await setup([makeItem({ id: 'only-main', itemIndex: 20, threadId: THREAD_ID, summary: 'tail' })]);
    const load = vi.spyOn(pane, 'loadAgentScope').mockResolvedValue('missing');
    const sweep = vi.spyOn(pane, 'sweepUnheldAgentScopes');
    const { getByTestId, unmount } = render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await waitFor(() => expect(load).toHaveBeenCalledWith('launch-1'));
    await tick();
    expect(load).toHaveBeenCalledTimes(1);
    expect(getByTestId('subagent-group-loading').textContent).toContain('not in the loaded history');
    expect(sweep).not.toHaveBeenCalled();
    unmount();
    expect(agentScopeHeld('main', THREAD_ID, 'launch-1')).toBe(false);
    expect(sweep).toHaveBeenCalledTimes(1);
  });
});
