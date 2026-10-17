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
import { resetThreadWakesForTest } from '../../stores/threadWakes.svelte';
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
    resetThreadWakesForTest();
    endThreadRowDrag();
    setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: false }));
    setBindingMock('ListArchivedSpawnedThreads', async () => [thread('done', { spawnedByThreadId: 'agent', archived: true })]);
  });

  // The child is pinned, so it stays a live row rather than folding into
  // the done footer as a settled idle child would.
  function renderList() {
    const threads = [
      thread('agent', { agentId: 'agent', archivedChildCount: 1 }),
      thread('child', { spawnedByThreadId: 'agent', pinnedAt: 1 }),
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

  it('a spawned thread dragged onto a group joins it', async () => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { container } = renderList();
    await flush();
    const group = container.querySelector<HTMLElement>('[data-testid="thread-group-row-shell"]')!;
    const child: ThreadDragPayload = { threadId: 'child', title: 'Child', projectId: 'project' };
    expect(await fireEvent.dragOver(group, dragEventInit(child))).toBe(false);
    await tick();
    expect(group.hasAttribute('data-drop-active')).toBe(true);
    await fireEvent.drop(group, dragEventInit(child));
    await flush();
    expect(setGroup).toHaveBeenCalledWith(['child'], expect.any(String));
  });

  it.each([
    ['the agent row', { threadId: 'agent', title: 'Agent', projectId: 'project', agentBound: true }],
  ])('%s dragged onto a group is refused: no move, the cursor says no', async (_, payload: ThreadDragPayload) => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { container } = renderList();
    await flush();
    const group = container.querySelector<HTMLElement>('[data-testid="thread-group-row-shell"]')!;
    expect(group).not.toBeNull();
    // Control: a loose thread lights the group and the dragover is taken
    // (preventDefault, so fireEvent reports false).
    const loose: ThreadDragPayload = { threadId: 'plain', title: 'Plain', projectId: 'project' };
    expect(await fireEvent.dragOver(group, dragEventInit(loose))).toBe(false);
    await tick();
    expect(group.hasAttribute('data-drop-active')).toBe(true);
    // The agent-bound row: not taken, so the cursor says no, and the group
    // does not light.
    expect(await fireEvent.dragOver(group, dragEventInit(payload))).toBe(true);
    await tick();
    expect(group.hasAttribute('data-drop-active')).toBe(false);
    await fireEvent.drop(group, dragEventInit(payload));
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

  it('marks the agent and every child it nests that has something to read from the agent row\'s menu', async () => {
    const unread = { latestTurnCompletedAt: 20, lastReadAt: 10 };
    const threads = [
      thread('agent', { agentId: 'agent', ...unread }),
      thread('live-child', { spawnedByThreadId: 'agent', ...unread }),
      thread('pinned-child', { spawnedByThreadId: 'agent', pinnedAt: 1, ...unread }),
      thread('loose', unread),
    ];
    replaceAllThreads(threads);
    const calls: string[][] = [];
    setBindingMock('MarkThreadsRead', vi.fn(async (ids: string[]) => {
      calls.push(ids);
      return [];
    }));
    const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [], pane: createThreadPane() });
    await flush();
    await fireEvent.contextMenu(view.container.querySelector<HTMLElement>('[data-sidebar-thread-id="agent"]')!);
    await flush();
    await fireEvent.click(view.getByRole('menuitem', { name: 'Mark All Read' }));
    await flush();
    expect(calls.map((ids) => [...ids].sort())).toEqual([['agent', 'live-child', 'pinned-child']]);
  });

  it('counts only the live children on a collapsed agent row', async () => {
    const threads = [
      thread('agent', { agentId: 'agent' }),
      thread('live-child', { spawnedByThreadId: 'agent', latestTurnCompletedAt: 20, lastReadAt: 10 }),
      thread('pinned-child', { spawnedByThreadId: 'agent', pinnedAt: 1 }),
      thread('settled-child', { spawnedByThreadId: 'agent' }),
    ];
    replaceAllThreads(threads);
    setAgentExpanded('agent', false);
    const view = render(ProjectThreadList, { projectId: 'project', threads, groups: [], pane: createThreadPane() });
    await flush();
    expect(view.container.querySelector('[data-testid="thread-row-agent-count"]')?.textContent?.trim()).toBe('2');
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
        thread('agent', { agentId: 'agent', archivedChildCount: count }),
        thread('child', { spawnedByThreadId: 'agent', pinnedAt: 1 }),
        thread('loose', { groupId: 'ops' }),
      ],
      groups: [{ id: 'ops', projectId: 'project', name: 'Ops', createdAt: 0, updatedAt: 0 }],
      pane: createThreadPane(),
    });
    // A zero that passes (the agent row's count catching up with a child
    // that just left) keeps the footer open, however long after it the
    // settle window ends.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      await rerender(0);
      await tick();
      expect(view.container.querySelector('[data-testid="thread-row-agent-done-toggle"]')).toBeNull();
      vi.advanceTimersByTime(700);
      await rerender(1);
      await tick();
      vi.advanceTimersByTime(2000);
      expect(isAgentDoneExpanded('agent')).toBe(true);
      // A zero that stays closes it once the 750 ms window has passed, not
      // before.
      await rerender(0);
      await tick();
      vi.advanceTimersByTime(749);
      expect(isAgentDoneExpanded('agent')).toBe(true);
      vi.advanceTimersByTime(1);
      expect(isAgentDoneExpanded('agent')).toBe(false);
    } finally {
      vi.useRealTimers();
    }
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
      thread('agent', promoted ? { agentId: 'agent', archivedChildCount: 1 } : {}),
      thread('child', { spawnedByThreadId: 'agent', pinnedAt: 1 }),
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
