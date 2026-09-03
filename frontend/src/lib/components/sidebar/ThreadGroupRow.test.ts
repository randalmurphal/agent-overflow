// The group row is a sibling of ThreadRow, so what is pinned here is what
// makes it a GROUP row: the count/time swap on the collapse state, the
// rename flow over the "New Group" placeholder a fresh group is born with,
// the chevron writing the sidebar's collapsed set, and the drop target that
// only accepts threads from the group's own project.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadGroupRow from './ThreadGroupRow.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { isGroupExpanded, resetSidebarForTest, toggleGroup } from '../../stores/sidebar.svelte';
import {
  requestGroupRename,
  resetThreadGroupsForTest,
} from '../../stores/threadGroups.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  beginThreadRowDrag,
  endThreadRowDrag,
  THREAD_ROW_DRAG_MIME,
  threadDragPayloadForEvent,
} from '../../utils/threadDragPayload';
import type { ThreadDragPayload } from '../../utils/threadDragPayload';
import {
  replaceAllThreads,
  touchThreadActivity,
} from '../../stores/threads.svelte';
import type { Thread, ThreadGroup } from '../../types/models';

function mkThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: id,
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    projectId: 'project-1',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function mkGroup(overrides: Partial<ThreadGroup> = {}): ThreadGroup {
  return {
    id: 'group-1',
    projectId: 'project-1',
    name: 'Refactors',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

function renderRow(props: Record<string, unknown> = {}) {
  return render(ThreadGroupRow, {
    props: {
      group: mkGroup(),
      pane: createThreadPane(),
      indent: 1,
      expanded: true,
      memberThreadIds: ['t1', 't2'],
      ...props,
    },
  });
}

function dragEventInit(payload: ThreadDragPayload) {
  const raw = JSON.stringify(payload);
  return {
    dataTransfer: {
      types: [THREAD_ROW_DRAG_MIME],
      dropEffect: 'none',
      effectAllowed: 'copyMove',
      getData: (type: string) => (type === THREAD_ROW_DRAG_MIME ? raw : ''),
      setData: () => {},
    } as unknown as DataTransfer,
  };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
  await tick();
}

describe('<ThreadGroupRow>', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetSidebarForTest();
    resetThreadGroupsForTest();
    resetBindingMocks();
    endThreadRowDrag();
    replaceAllThreads([mkThread('t1'), mkThread('t2')]);
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  /** What a dragover target sees: DataTransfer says nothing, so the record answers. */
  function inFlightPayload(): ThreadDragPayload | null {
    return threadDragPayloadForEvent({
      dataTransfer: {
        types: [THREAD_ROW_DRAG_MIME],
        getData: () => '',
      } as unknown as DataTransfer,
    } as DragEvent);
  }

  it('shows the member count when collapsed', () => {
    const { getByTestId, queryByTestId } = renderRow({ expanded: false });
    expect(getByTestId('thread-group-row-count')).toHaveTextContent('2');
    expect(queryByTestId('thread-group-row-time')).toBeNull();
  });

  it('shows the activity time when expanded', () => {
    const { getByTestId, queryByTestId } = renderRow({ expanded: true });
    expect(queryByTestId('thread-group-row-count')).toBeNull();
    expect(getByTestId('thread-group-row-time')).toBeInTheDocument();
  });

  it('follows a member\'s live activity, which no prop would carry', async () => {
    // The tree's latestActivityAt is deliberately not compared by
    // sameSidebarVisibleNodes, so a prop would freeze this label at the last
    // render-changing beat while a member streams. The row reads the member's
    // own activity box instead.
    replaceAllThreads([mkThread('t1', { updatedAt: Date.now() - 7_200_000 })]);
    const { getByTestId } = renderRow({ memberThreadIds: ['t1'] });
    expect(getByTestId('thread-group-row-time')).toHaveTextContent('2h');

    touchThreadActivity('t1', Date.now());
    await tick();

    expect(getByTestId('thread-group-row-time')).toHaveTextContent('now');
  });

  it('falls back to the group\'s own last write when no member is in the store', () => {
    replaceAllThreads([]);
    const { getByTestId } = renderRow({
      memberThreadIds: ['t1'],
      group: mkGroup({ updatedAt: Date.now() - 7_200_000 }),
    });
    expect(getByTestId('thread-group-row-time')).toHaveTextContent('2h');
  });

  it('shows nothing on the right for an empty expanded group', () => {
    const { queryByTestId } = renderRow({ expanded: true, memberThreadIds: [] });
    expect(queryByTestId('thread-group-row-count')).toBeNull();
    expect(queryByTestId('thread-group-row-time')).toBeNull();
  });

  it('renders the chevron even when the group is empty', () => {
    const { getByTestId } = renderRow({ memberThreadIds: [] });
    expect(getByTestId('thread-group-row-expand')).toBeInTheDocument();
  });

  it('saves an inline rename on Enter', async () => {
    const rename = setBindingMock('RenameThreadGroup', vi.fn(async () => mkGroup({ name: 'Spikes' })));
    const { getByTestId, getByLabelText } = renderRow();

    await fireEvent.dblClick(getByTestId('thread-group-row'));
    const input = getByLabelText('Rename Group') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Spikes' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await flush();

    expect(rename).toHaveBeenCalledWith('group-1', 'Spikes');
  });

  it('cancels on Escape without writing', async () => {
    const rename = setBindingMock('RenameThreadGroup', vi.fn(async () => mkGroup()));
    const { getByTestId, getByLabelText, queryByLabelText } = renderRow();

    await fireEvent.dblClick(getByTestId('thread-group-row'));
    const input = getByLabelText('Rename Group') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Spikes' } });
    await fireEvent.keyDown(input, { key: 'Escape' });
    await flush();

    expect(rename).not.toHaveBeenCalled();
    expect(queryByLabelText('Rename Group')).toBeNull();
  });

  it('treats a blank name as a cancel rather than a round-trip', async () => {
    const rename = setBindingMock('RenameThreadGroup', vi.fn(async () => mkGroup()));
    const { getByTestId, getByLabelText, queryByLabelText } = renderRow();

    await fireEvent.dblClick(getByTestId('thread-group-row'));
    const input = getByLabelText('Rename Group') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '   ' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await flush();

    expect(rename).not.toHaveBeenCalled();
    expect(queryByLabelText('Rename Group')).toBeNull();
  });

  it('opens the rename editor on mount for a freshly created group', async () => {
    requestGroupRename('group-1');
    const { getByLabelText } = renderRow();
    await tick();
    expect(getByLabelText('Rename Group')).toBeInTheDocument();
  });

  it('opens the rename editor when the request lands after the row mounted', async () => {
    // Create-and-move asks once the move has settled the row's position, by
    // which time the row has been on screen since the create.
    const { queryByLabelText, getByLabelText } = renderRow();
    await tick();
    expect(queryByLabelText('Rename Group')).toBeNull();

    requestGroupRename('group-1');
    await tick();
    expect(getByLabelText('Rename Group')).toBeInTheDocument();
  });

  it('leaves a group alone when the pending rename names another one', async () => {
    requestGroupRename('group-other');
    const { queryByLabelText } = renderRow();
    await tick();
    expect(queryByLabelText('Rename Group')).toBeNull();
  });

  it('toggles the sidebar store from the chevron', async () => {
    const { getByTestId } = renderRow();
    expect(isGroupExpanded('group-1')).toBe(true);

    await fireEvent.click(getByTestId('thread-group-row-expand'));
    expect(isGroupExpanded('group-1')).toBe(false);

    await fireEvent.click(getByTestId('thread-group-row-expand'));
    expect(isGroupExpanded('group-1')).toBe(true);
  });

  it('toggles from the row body and from Enter, like a project header', async () => {
    const { getByTestId } = renderRow();
    await fireEvent.click(getByTestId('thread-group-row'));
    expect(isGroupExpanded('group-1')).toBe(false);

    toggleGroup('group-1');
    await fireEvent.keyDown(getByTestId('thread-group-row'), { key: 'Enter' });
    expect(isGroupExpanded('group-1')).toBe(false);
  });

  it('moves a dropped thread of the same project into this group', async () => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { getByTestId } = renderRow();

    await fireEvent.drop(
      getByTestId('thread-group-row-shell'),
      dragEventInit({ threadId: 'dragged', title: 'Dragged', projectId: 'project-1' }),
    );
    await flush();

    expect(setGroup).toHaveBeenCalledWith(['dragged'], 'group-1');
  });

  it('refuses a thread dragged from another project', async () => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { getByTestId } = renderRow();

    await fireEvent.drop(
      getByTestId('thread-group-row-shell'),
      dragEventInit({ threadId: 'dragged', title: 'Dragged', projectId: 'other-project' }),
    );
    await flush();

    expect(setGroup).not.toHaveBeenCalled();
  });

  it('refuses a thread that is already in this group', async () => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { getByTestId } = renderRow();

    await fireEvent.drop(
      getByTestId('thread-group-row-shell'),
      dragEventInit({
        threadId: 'member',
        title: 'Member',
        projectId: 'project-1',
        groupId: 'group-1',
      }),
    );
    await flush();

    expect(setGroup).not.toHaveBeenCalled();
  });

  it('reports the hover state to its owner while a droppable thread is over it', async () => {
    const seen: Array<[boolean, boolean]> = [];
    const { getByTestId } = renderRow({
      onDropTargetChange: (active: boolean, accepts: boolean) => seen.push([active, accepts]),
    });
    const shell = getByTestId('thread-group-row-shell');
    const init = dragEventInit({ threadId: 'dragged', title: 'Dragged', projectId: 'project-1' });

    await fireEvent.dragEnter(shell, init);
    await fireEvent.dragOver(shell, init);
    expect(seen.at(-1)).toEqual([true, true]);

    await fireEvent.dragLeave(shell, init);
    expect(seen.at(-1)).toEqual([false, false]);
  });

  it('reports a REFUSED payload as hovering too, so the ungroup outline goes down', async () => {
    // A member dragged back over its own group is refused here and swallowed,
    // so the container behind never revises its own state — it has to be told
    // the pointer is over a group, or its dashed ungroup outline stays lit
    // over a drop that ungroups nothing.
    const seen: Array<[boolean, boolean]> = [];
    const { getByTestId } = renderRow({
      onDropTargetChange: (active: boolean, accepts: boolean) => seen.push([active, accepts]),
    });
    const shell = getByTestId('thread-group-row-shell');
    const init = dragEventInit({
      threadId: 'member',
      title: 'Member',
      projectId: 'project-1',
      groupId: 'group-1',
    });

    await fireEvent.dragEnter(shell, init);
    await fireEvent.dragOver(shell, init);

    expect(seen.at(-1)).toEqual([true, false]);
    // Reported, but not lit: the row highlight is the owner's call on `accepts`.
    expect(shell.getAttribute('data-drop-active')).toBeNull();
  });

  it('clears the in-flight drag record on drop, with no dragend to rely on', async () => {
    // The source row fires dragend only if it is still mounted; collapsing its
    // project or typing a search mid-drag takes it away, and the record would
    // outlive the drag.
    setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const payload = { threadId: 'dragged', title: 'Dragged', projectId: 'project-1' };
    beginThreadRowDrag(payload);
    const { getByTestId } = renderRow();

    await fireEvent.drop(getByTestId('thread-group-row-shell'), dragEventInit(payload));
    await flush();

    expect(inFlightPayload()).toBeNull();
  });

  it('names the pin affordance for a group, not a thread', () => {
    const { container } = renderRow();
    const pin = container.querySelector('[data-testid="thread-row-pin"]') as HTMLElement;
    expect(pin.getAttribute('aria-label')).toBe('Pin Group');
  });

  it('names the pinned affordance Unpin Group', () => {
    const { container } = renderRow({ group: mkGroup({ pinnedAt: 1 }) });
    const pin = container.querySelector('[data-testid="thread-row-pin"]') as HTMLElement;
    expect(pin.getAttribute('aria-label')).toBe('Unpin Group');
  });

  it('pins and unpins the group from the gutter affordance', async () => {
    const pin = setBindingMock('PinThreadGroup', vi.fn(async () => mkGroup({ pinnedAt: 1 })));
    const { getByTestId } = renderRow();
    await fireEvent.click(getByTestId('thread-row-pin'));
    await flush();
    expect(pin).toHaveBeenCalledWith('group-1');
  });

  it('right-clicking a pinned gutter affordance cycles the burner', async () => {
    const move = setBindingMock(
      'SetThreadGroupPinGroup',
      vi.fn(async () => mkGroup({ pinnedAt: 1, pinGroup: 1 })),
    );
    const { getByTestId } = renderRow({ group: mkGroup({ pinnedAt: 1, pinGroup: 0 }) });

    await fireEvent.contextMenu(getByTestId('thread-row-pin'));
    await flush();

    expect(move).toHaveBeenCalledWith('group-1', 1);
  });

  it('a left click on the row itself closes its open context menu', async () => {
    const { getByTestId, queryByRole } = renderRow();

    await fireEvent.contextMenu(getByTestId('thread-group-row'));
    await tick();
    expect(queryByRole('menu', { name: 'Group Actions' })).not.toBeNull();

    // The row is the menu's anchor, not a toggle: the chevron click must
    // collapse the group with the menu out of the way.
    await fireEvent.mouseDown(getByTestId('thread-group-row-expand'));
    await tick();
    expect(queryByRole('menu', { name: 'Group Actions' })).toBeNull();
  });
});
