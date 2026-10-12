// A drop on a standing agent's row, on a thread it spawned, or on its done
// footer moves nothing and never lights the list's ungroup outline
// (docs/specs/standing-agents.md, "Sidebar": the drop is a no-op with the
// affordance of a drop on another project).
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ProjectThreadList from './ProjectThreadList.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { isAgentDoneExpanded, isAgentExpanded, resetSidebarForTest, setAgentExpanded, toggleAgentDone } from '../../stores/sidebar.svelte';
import { resetArchivedChildrenForTest } from '../../stores/agentArchivedChildren.svelte';
import { resetAgentWakesForTest } from '../../stores/agentWakes.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { endThreadRowDrag, THREAD_ROW_DRAG_MIME, type ThreadDragPayload } from '../../utils/threadDragPayload';
import { replaceAllThreads } from '../../stores/threads.svelte';
import type { Thread } from '../../types/models';

function thread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id, title: id, provider: 'claude', mode: 'chat', model: 'test', projectId: 'project',
    projectPath: '/tmp/ws', workspacePath: '/tmp/ws', createdAt: 0, updatedAt: 0, archived: false, ...overrides,
  };
}

function dragEventInit(payload: ThreadDragPayload) {
  const raw = JSON.stringify(payload);
  const dataTransfer = {
    types: [THREAD_ROW_DRAG_MIME],
    dropEffect: 'copy',
    effectAllowed: 'copyMove',
    getData: (type: string) => (type === THREAD_ROW_DRAG_MIME ? raw : ''),
    setData: () => {},
  } as unknown as DataTransfer;
  return { dataTransfer };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
  await tick();
}

describe('<ProjectThreadList> drops on a standing agent', () => {
  const grouped: ThreadDragPayload = { threadId: 'loose', title: 'Loose', projectId: 'project', groupId: 'ops' };

  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetSidebarForTest();
    resetArchivedChildrenForTest();
    resetAgentWakesForTest();
    endThreadRowDrag();
    setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: false }));
    setBindingMock('ListArchivedSpawnedThreads', async () => [thread('done', { spawnedByThreadId: 'agent', archived: true })]);
  });

  function renderList() {
    const threads = [
      thread('agent', { agentPromotedAt: 1, archivedChildCount: 1 }),
      thread('child', { spawnedByThreadId: 'agent' }),
      thread('loose', { groupId: 'ops' }),
    ];
    replaceAllThreads(threads);
    const view = render(ProjectThreadList, {
      projectId: 'project', threads,
      groups: [{ id: 'ops', projectId: 'project', name: 'Ops', createdAt: 0, updatedAt: 0 }],
      pane: createThreadPane(),
    });
    return view;
  }

  it.each([
    ['the agent row', (el: HTMLElement) => el.querySelector<HTMLElement>('[data-sidebar-thread-id="agent"]')!],
    ['a spawned thread', (el: HTMLElement) => el.querySelector<HTMLElement>('[data-sidebar-thread-id="child"]')!],
    ['the done footer', (el: HTMLElement) => el.querySelector<HTMLElement>('[data-testid="thread-row-agent-done"]')!],
  ])('a grouped thread dragged over %s puts the ungroup outline down and its drop moves nothing', async (_, pick) => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { container } = renderList();
    await flush();
    const target = pick(container);
    expect(target).not.toBeNull();
    const list = container.querySelector('[data-testid="project-thread-list"]')!;

    // Over the background the ungroup outline is lit; over the agent's
    // rows it goes down, which is the swallow: the container never sees
    // the event.
    await fireEvent.dragOver(list, dragEventInit(grouped));
    await tick();
    expect(list.hasAttribute('data-ungroup-target')).toBe(true);
    await fireEvent.dragOver(target, dragEventInit(grouped));
    await tick();
    expect(list.hasAttribute('data-ungroup-target')).toBe(false);

    // A drop that reached the container would ungroup; this one does not.
    await fireEvent.drop(target, dragEventInit(grouped));
    await flush();
    expect(setGroup).not.toHaveBeenCalled();
  });

  it('still ungroups on the list background beside the agent', async () => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { container } = renderList();
    await flush();
    const list = container.querySelector('[data-testid="project-thread-list"]')!;
    await fireEvent.dragOver(list, dragEventInit(grouped));
    await tick();
    expect(list.hasAttribute('data-ungroup-target')).toBe(true);
    await fireEvent.drop(list, dragEventInit(grouped));
    await flush();
    expect(setGroup).toHaveBeenCalledWith(['loose'], '');
  });

  it('opens the done footer by reading the list fresh', async () => {
    const list = setBindingMock('ListArchivedSpawnedThreads', vi.fn(async () => [thread('done', { spawnedByThreadId: 'agent', archived: true })]));
    const { container } = renderList();
    await flush();
    toggleAgentDone('agent');
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(container.querySelector('[data-sidebar-thread-id="done"]')).not.toBeNull();
    // Shut and reopened from the footer's own toggle: a second read.
    await fireEvent.click(container.querySelector('[data-testid="thread-row-agent-done-toggle"]')!);
    await flush();
    await fireEvent.click(container.querySelector('[data-testid="thread-row-agent-done-toggle"]')!);
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('comes back closed after its last archived child leaves', async () => {
    const view = renderList();
    await flush();
    toggleAgentDone('agent');
    await flush();
    expect(view.container.querySelector('[data-sidebar-thread-id="done"]')).not.toBeNull();
    // The last archived child is unarchived: no footer, and the open state
    // goes with it.
    const rerender = (count: number) => view.rerender({
      projectId: 'project',
      threads: [
        thread('agent', { agentPromotedAt: 1, archivedChildCount: count }),
        thread('child', { spawnedByThreadId: 'agent' }),
        thread('loose', { groupId: 'ops' }),
      ],
      groups: [{ id: 'ops', projectId: 'project', name: 'Ops', createdAt: 0, updatedAt: 0 }],
      pane: createThreadPane(),
    });
    await rerender(0);
    await flush();
    expect(view.container.querySelector('[data-testid="thread-row-agent-done-toggle"]')).toBeNull();
    expect(isAgentDoneExpanded('agent')).toBe(false);
    // Three children archive: the footer returns closed with its count.
    await rerender(3);
    await flush();
    const toggle = view.container.querySelector('[data-testid="thread-row-agent-done-toggle"]');
    expect(toggle?.getAttribute('aria-expanded')).toBe('false');
    expect(view.container.querySelector('[data-testid="thread-row-agent-done-label"]')?.textContent?.trim()).toBe('3 done');
    expect(view.container.querySelector('[data-archived-child="true"]')).toBeNull();
  });

  it('forgets the fold, the open footer and its list when the agent is demoted', async () => {
    const list = setBindingMock('ListArchivedSpawnedThreads', vi.fn(async () => [thread('done', { spawnedByThreadId: 'agent', archived: true })]));
    const view = renderList();
    await flush();
    setAgentExpanded('agent', false);
    toggleAgentDone('agent');
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    const rows = (promoted: boolean) => [
      thread('agent', promoted ? { agentPromotedAt: 1, archivedChildCount: 1 } : {}),
      thread('child', { spawnedByThreadId: 'agent' }),
      thread('loose', { groupId: 'ops' }),
    ];
    // Demoted: the row is a plain thread and every agent-only state goes.
    replaceAllThreads(rows(false));
    await view.rerender({ projectId: 'project', threads: rows(false), groups: [], pane: createThreadPane() });
    await flush();
    expect(view.container.querySelector('[data-sidebar-agent]')).toBeNull();
    expect(isAgentExpanded('agent')).toBe(true);
    expect(isAgentDoneExpanded('agent')).toBe(false);
    // Promoted again at the same count: unfolded, footer shut, and opening
    // it reads the list again instead of showing the one from before.
    replaceAllThreads(rows(true));
    await view.rerender({ projectId: 'project', threads: rows(true), groups: [], pane: createThreadPane() });
    await flush();
    expect(view.container.querySelector('[data-sidebar-agent]')?.getAttribute('data-expanded')).toBe('true');
    expect(view.container.querySelector('[data-testid="thread-row-agent-done-toggle"]')?.getAttribute('aria-expanded')).toBe('false');
    expect(view.container.querySelector('[data-sidebar-thread-id="done"]')).toBeNull();
    await fireEvent.click(view.container.querySelector('[data-testid="thread-row-agent-done-toggle"]')!);
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
    expect(view.container.querySelector('[data-sidebar-thread-id="done"]')).not.toBeNull();
  });
});
