// Group-row menu gating. Items + order are pinned (Rename, the pin controls,
// Archive Threads (N), Ungroup All, Delete Group), the two count-gated items
// disable rather than vanish on an empty group, and Delete Group ungroups —
// which is what its dialog has to say.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadGroupContextMenu from './ThreadGroupContextMenu.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { resetThreadGroupsForTest } from '../../stores/threadGroups.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import type { Thread, ThreadGroup } from '../../types/models';

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

function mkThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: id,
    provider: 'claude',
    projectId: 'project-1',
    groupId: 'group-1',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function renderMenu(group: ThreadGroup = mkGroup()) {
  const anchor = document.createElement('div');
  document.body.appendChild(anchor);
  return render(ThreadGroupContextMenu, {
    props: {
      group,
      pane: createThreadPane(),
      anchor,
      open: true,
      onClose: () => {},
      onRename: () => {},
    },
  });
}

function visibleLabels(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll('[role="menuitem"]'))
    .map((el) => el.textContent?.trim() ?? '')
    .filter((text) => text.length > 0);
}

async function primeSettings(overrides: Partial<Settings> | null = null) {
  setBindingMock('GetSettings', async () => overrides);
  await loadSettings();
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
  await tick();
}

describe('<ThreadGroupContextMenu>', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetThreadGroupsForTest();
    resetBindingMocks();
    replaceAllThreads([mkThread('t1'), mkThread('t2')]);
    await primeSettings();
  });

  it('renders the unpinned item set in order', () => {
    const { baseElement } = renderMenu();
    expect(visibleLabels(baseElement)).toEqual([
      'Rename Group',
      'Pin Group',
      'Archive Threads (2)',
      'Ungroup All',
      'Delete Group',
    ]);
  });

  it('swaps in the burner move and unpin once the group is pinned', () => {
    const { baseElement } = renderMenu(mkGroup({ pinnedAt: 1, pinGroup: 0 }));
    expect(visibleLabels(baseElement)).toEqual([
      'Rename Group',
      'Move to Back Burner',
      'Unpin Group',
      'Archive Threads (2)',
      'Ungroup All',
      'Delete Group',
    ]);
  });

  it('offers the front burner from the back one', () => {
    const { baseElement } = renderMenu(mkGroup({ pinnedAt: 1, pinGroup: 1 }));
    expect(visibleLabels(baseElement)).toContain('Move to Front Burner');
  });

  it('disables the member actions on an empty group rather than hiding them', () => {
    replaceAllThreads([]);
    const { getByRole } = renderMenu();
    expect(getByRole('menuitem', { name: 'Archive Threads (0)' }))
      .toHaveAttribute('aria-disabled', 'true');
    expect(getByRole('menuitem', { name: 'Ungroup All' }))
      .toHaveAttribute('aria-disabled', 'true');
  });

  it('counts the whole membership, not the members a search left on screen', async () => {
    // The rows the group renders are search-filtered; "Archive Threads (n)"
    // acting on that subset would silently do a fraction of what it says.
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    replaceAllThreads([
      mkThread('t1'),
      mkThread('t2'),
      mkThread('t3'),
      // Not members: another group, no group, and one that is only a child.
      mkThread('other', { groupId: 'group-2' }),
      mkThread('loose', { groupId: undefined }),
      mkThread('kid', { parentThreadId: 't1' }),
      mkThread('gone', { archived: true }),
    ]);
    const { getByRole } = renderMenu();

    expect(getByRole('menuitem', { name: 'Archive Threads (3)' })).toBeInTheDocument();
    await fireEvent.click(getByRole('menuitem', { name: 'Ungroup All' }));
    await flush();

    expect(setGroup).toHaveBeenCalledWith(['t1', 't2', 't3'], '');
  });

  it('ungroups every member with one call', async () => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { getByRole } = renderMenu();

    await fireEvent.click(getByRole('menuitem', { name: 'Ungroup All' }));
    await flush();

    expect(setGroup).toHaveBeenCalledWith(['t1', 't2'], '');
  });

  it('deletes immediately when confirmDelete is off', async () => {
    await primeSettings({ confirmDelete: false });
    const del = setBindingMock('DeleteThreadGroup', vi.fn(async () => {}));
    const { getByRole, queryByRole } = renderMenu();

    await fireEvent.click(getByRole('menuitem', { name: 'Delete Group' }));
    await flush();

    expect(del).toHaveBeenCalledWith('group-1');
    expect(queryByRole('button', { name: 'Delete' })).toBeNull();
  });

  it('confirms first when confirmDelete is on, and says the threads survive', async () => {
    await primeSettings({ confirmDelete: true });
    const del = setBindingMock('DeleteThreadGroup', vi.fn(async () => {}));
    const { getByRole, getByText } = renderMenu();

    await fireEvent.click(getByRole('menuitem', { name: 'Delete Group' }));
    await tick();

    expect(del).not.toHaveBeenCalled();
    expect(getByText('Remove this group. Its 2 threads stay in the project.'))
      .toBeInTheDocument();

    await fireEvent.click(getByRole('button', { name: 'Delete' }));
    await flush();
    expect(del).toHaveBeenCalledWith('group-1');
  });

  it('archives every member sequentially when confirmArchive is off', async () => {
    await primeSettings({ confirmArchive: false });
    setBindingMock('StopSession', async () => {});
    const archived: string[] = [];
    setBindingMock('ArchiveThread', async (id: string) => { archived.push(id); });
    const { getByRole } = renderMenu();

    await fireEvent.click(getByRole('menuitem', { name: 'Archive Threads (2)' }));
    await flush();
    await flush();

    expect(archived).toEqual(['t1', 't2']);
  });

  it('confirms the whole batch once when confirmArchive is on', async () => {
    await primeSettings({ confirmArchive: true });
    setBindingMock('StopSession', async () => {});
    const archived: string[] = [];
    setBindingMock('ArchiveThread', async (id: string) => { archived.push(id); });
    const { getByRole } = renderMenu();

    await fireEvent.click(getByRole('menuitem', { name: 'Archive Threads (2)' }));
    await tick();
    expect(archived).toEqual([]);

    await fireEvent.click(getByRole('button', { name: 'Archive' }));
    await flush();
    await flush();
    expect(archived).toEqual(['t1', 't2']);
  });
});
