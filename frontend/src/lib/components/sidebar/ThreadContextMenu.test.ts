// Right-click menu visibility gating. Items + order are pinned:
// Rename, Fork (when fork-able), Mark Unread, pin controls, Copy Path,
// Copy Thread ID, Delete (when not a child thread).

import { afterEach, describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadContextMenu from './ThreadContextMenu.svelte';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { clearThreadSelection, setThreadSelection } from '../../stores/threadFilter.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { setThreadStatus } from '../../stores/threadStatuses.svelte';
import {
  consumePendingGroupRename,
  resetThreadGroupsForTest,
  upsertThreadGroup,
} from '../../stores/threadGroups.svelte';
import {
  collapseProject,
  isProjectExpanded,
  resetSidebarForTest,
} from '../../stores/sidebar.svelte';
import type { Thread } from '../../types/models';
import type { Settings } from '../../types/settings';
import { OBSERVE_SCOPES, pairViewOnly, pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    sessionRef: 'session-1',
    ...overrides,
  };
}

function renderMenu(thread: Thread) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  const pane = createThreadPane();
  const anchor = document.createElement('div');
  document.body.appendChild(anchor);
  return render(ThreadContextMenu, {
    props: {
      thread,
      pane,
      anchor,
      open: true,
      onClose: () => {},
      onRename: () => {},
      isActive: false,
    },
  });
}

function visibleLabels(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll('[role="menuitem"]'))
    .map((el) => el.textContent?.trim() ?? '')
    .filter((text) => text.length > 0);
}

describe('<ThreadContextMenu> single-row menu', () => {
  beforeEach(() => {
    resetBindingMocks();
    clearThreadSelection();
  });

  it('renders the full item set in order when fork-able and not a child', () => {
    const { baseElement } = renderMenu(makeThread());
    expect(visibleLabels(baseElement)).toEqual([
      'Open in New Pane',
      'Rename Thread',
      'Promote to Standing Agent',
      'Fork Thread',
      'Mark Unread',
      'Pin Thread',
      'Copy Path',
      'Copy Thread ID',
      'Archive Thread',
      'Delete',
    ]);
  });

  it('hides Fork Thread when the source has no session reference yet', () => {
    const { baseElement } = renderMenu(makeThread({ sessionRef: undefined }));
    const labels = visibleLabels(baseElement);
    expect(labels).not.toContain('Fork Thread');
    // Other items remain so the gating is targeted, not blanket-disabling.
    expect(labels).toContain('Rename Thread');
    expect(labels).toContain('Delete');
  });

  it('hides Fork Thread for claude-tui even with a session reference', () => {
    // sessionRef is present, so the only thing hiding Fork is the provider
    // capability gate: claude-tui forks inside the real TUI (via take-control),
    // not through AO's fork-thread path.
    const { baseElement } = renderMenu(makeThread({ provider: 'claude-tui' }));
    const labels = visibleLabels(baseElement);
    expect(labels).not.toContain('Fork Thread');
    expect(labels).toContain('Rename Thread');
    expect(labels).toContain('Delete');
  });

  it('omits Archive Thread but keeps Delete for a terminal thread', () => {
    const { baseElement } = renderMenu(makeThread({ mode: 'terminal' }));
    const labels = visibleLabels(baseElement);
    expect(labels).not.toContain('Archive Thread');
    expect(labels).toContain('Delete');
  });

  it('shows move-to-back and unpin controls for a front-burner thread', () => {
    const { baseElement } = renderMenu(makeThread({ pinnedAt: 1, pinGroup: 0 }));
    expect(visibleLabels(baseElement)).toEqual([
      'Open in New Pane',
      'Rename Thread',
      'Promote to Standing Agent',
      'Fork Thread',
      'Mark Unread',
      'Move to Back Burner',
      'Unpin Thread',
      'Copy Path',
      'Copy Thread ID',
      'Archive Thread',
      'Delete',
    ]);
  });

  it('shows move-to-front and unpin controls for a back-burner thread', () => {
    const { baseElement } = renderMenu(makeThread({ pinnedAt: 1, pinGroup: 1 }));
    const labels = visibleLabels(baseElement);
    expect(labels).toContain('Move to Front Burner');
    expect(labels).toContain('Unpin Thread');
    expect(labels).not.toContain('Move to Back Burner');
  });

  it('does not expose single-thread pin controls in a bulk menu', () => {
    setThreadSelection(['thread-1', 'thread-2']);
    const { baseElement } = renderMenu(makeThread());
    const labels = visibleLabels(baseElement);
    expect(labels).not.toContain('Pin Thread');
    expect(labels).not.toContain('Unpin Thread');
    expect(labels.some((label) => label.startsWith('Move to '))).toBe(false);
  });
});

describe('<ThreadContextMenu> Archive Thread', () => {
  async function primeSettings(overrides: Partial<Settings>) {
    setBindingMock('GetSettings', async () => overrides);
    await loadSettings();
  }

  beforeEach(() => {
    resetBindingMocks();
    clearThreadSelection();
  });

  it('archives immediately and closes the menu when confirmArchive is off', async () => {
    await primeSettings({ confirmArchive: false });
    const archive = setBindingMock('ArchiveThread', vi.fn(async () => {}));
    const onClose = vi.fn();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
    const anchor = document.createElement('div');
    document.body.appendChild(anchor);
    const { getByRole, queryByRole } = render(ThreadContextMenu, {
      props: {
        thread: makeThread({ id: 'arch-off' }),
        pane: createThreadPane(),
        anchor,
        open: true,
        onClose,
        onRename: () => {},
        isActive: false,
      },
    });
    await fireEvent.click(getByRole('menuitem', { name: 'Archive Thread' }));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(onClose).toHaveBeenCalledTimes(1);
    expect(archive).toHaveBeenCalledWith('arch-off');
    expect(queryByRole('button', { name: 'Archive' })).toBeNull();
  });

  it('confirms first when confirmArchive is on, archiving only after confirm', async () => {
    await primeSettings({ confirmArchive: true });
    const archive = setBindingMock('ArchiveThread', vi.fn(async () => {}));
    const { getByRole } = renderMenu(makeThread({ id: 'arch-on' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Archive Thread' }));
    await tick();

    const confirmBtn = getByRole('button', { name: 'Archive' });
    expect(archive).not.toHaveBeenCalled();
    await fireEvent.click(confirmBtn);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(archive).toHaveBeenCalledWith('arch-on');
  });

  it('is not offered for a terminal thread', () => {
    const { baseElement } = renderMenu(makeThread({ mode: 'terminal' }));
    expect(visibleLabels(baseElement)).not.toContain('Archive Thread');
  });
});

describe('<ThreadContextMenu> without threads:operate', () => {
  // Every write in the menu rides `threads:operate`; a view-only device sees
  // the rows but they are inert and say why. The Copy rows stay live.
  const INERT = 'Not granted to this device';

  beforeEach(async () => {
    resetBindingMocks();
    clearThreadSelection();
    resetThreadGroupsForTest();
    upsertThreadGroup({ id: 'g-1', projectId: 'project-1', name: 'One', createdAt: 0, updatedAt: 0 });
    await pairViewOnly();
  });

  afterEach(() => {
    resetToLocalPage();
    resetThreadGroupsForTest();
  });

  function item(el: HTMLElement, label: string): HTMLElement {
    const node = Array.from(el.querySelectorAll('[role="menuitem"]'))
      .find((node) => node.textContent?.trim().replace(/[▸\s]+$/u, '') === label);
    if (!node) throw new Error(`${label} not rendered`);
    return node as HTMLElement;
  }

  it('disables every mutating single-thread row with the reason, leaving Copy live', () => {
    const { baseElement } = renderMenu(makeThread({ projectId: 'project-1', pinnedAt: 1, pinGroup: 0 }));
    for (const label of [
      'Rename Thread',
      'Fork Thread',
      'Mark Unread',
      'Move to Back Burner',
      'Unpin Thread',
      'Archive Thread',
      'Delete',
    ]) {
      const row = item(baseElement, label);
      expect(row.getAttribute('aria-disabled'), label).toBe('true');
      expect(row.getAttribute('title'), label).toBe(INERT);
    }
    // The submenu trigger carries no title of its own; its targets do.
    expect(item(baseElement, 'Move to Group').getAttribute('aria-disabled')).toBe('true');
    for (const label of ['Copy Path', 'Copy Thread ID']) {
      const row = item(baseElement, label);
      expect(row.getAttribute('aria-disabled'), label).not.toBe('true');
    }
  });

  it('does not run the archive action from an inert row', async () => {
    const archive = setBindingMock('ArchiveThread', vi.fn(async () => {}));
    const { baseElement } = render(ThreadContextMenu, {
      props: {
        thread: makeThread(),
        pane: createThreadPane(),
        anchor: document.body,
        open: true,
        onClose: () => {},
        onRename: () => {},
        isActive: false,
      },
    });
    await fireEvent.click(item(baseElement, 'Archive Thread'));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(archive).not.toHaveBeenCalled();
  });

  it('disables the bulk rows when any selected thread lacks the grant', () => {
    replaceAllThreads([makeThread({ id: 'a' }), makeThread({ id: 'b' })]);
    setThreadSelection(['a', 'b']);
    const { baseElement } = renderMenu(makeThread({ id: 'a' }));
    for (const label of ['Mark unread (2)', 'Archive (2)', 'Delete (2)']) {
      const row = item(baseElement, label);
      expect(row.getAttribute('aria-disabled'), label).toBe('true');
      expect(row.getAttribute('title'), label).toBe(INERT);
    }
  });
});

describe('<ThreadContextMenu> Delete honors the confirm-delete setting', () => {
  // The right-click Delete now mirrors the terminal row-X: it consults the
  // global confirmDelete setting instead of always confirming. off → delete
  // immediately; on → confirm first. (Bulk delete keeps its own always-on
  // confirm — a multi-thread delete is a higher-stakes action.) The Delete
  // menu item is role="menuitem"; the confirm dialog's button is role="button"
  // (both named "Delete"), so the two are queryable without collision.
  async function primeSettings(overrides: Partial<Settings>) {
    setBindingMock('GetSettings', async () => overrides);
    await loadSettings();
  }

  beforeEach(() => {
    resetBindingMocks();
    clearThreadSelection();
    // deleteThreadAction best-effort stops the session before deleting.
    setBindingMock('StopSession', async () => {});
  });

  it('deletes immediately, without a confirm dialog, when confirmDelete is off', async () => {
    await primeSettings({ confirmDelete: false });
    let deletedId: string | null = null;
    setBindingMock('DeleteThread', async (id: string) => { deletedId = id; });

    const { getByRole, queryByRole } = renderMenu(makeThread({ id: 'ctx-off' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete' }));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(deletedId).toBe('ctx-off');
    // The dialog's confirm button (role=button) must be absent — only the
    // menu item (role=menuitem) named "Delete" exists.
    expect(queryByRole('button', { name: 'Delete' })).toBeNull();
  });

  it('confirms first when confirmDelete is on, deleting only after confirm', async () => {
    await primeSettings({ confirmDelete: true });
    let deletedId: string | null = null;
    setBindingMock('DeleteThread', async (id: string) => { deletedId = id; });

    const { getByRole } = renderMenu(makeThread({ id: 'ctx-on' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete' }));
    await tick();

    // The menu item opened the confirm dialog; nothing deleted yet.
    const confirmBtn = getByRole('button', { name: 'Delete' });
    expect(confirmBtn).toBeInTheDocument();
    expect(deletedId).toBeNull();

    await fireEvent.click(confirmBtn);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(deletedId).toBe('ctx-on');
  });

  it('keeps bulk delete always-confirming even when confirmDelete is off', async () => {
    await primeSettings({ confirmDelete: false });
    let deleted = false;
    setBindingMock('DeleteThread', async () => { deleted = true; });

    // Two selected threads (one is the right-clicked row) flips the menu
    // into bulk mode. Bulk delete deliberately ignores confirmDelete — a
    // multi-thread delete always confirms — so the dialog must appear and
    // nothing is deleted until the user confirms.
    setThreadSelection(['thread-1', 'thread-2']);
    const { getByRole } = renderMenu(makeThread({ id: 'thread-1' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete (2)' }));
    await tick();

    expect(getByRole('button', { name: 'Delete' })).toBeInTheDocument();
    expect(deleted).toBe(false);
  });

  it('says how many terminals deleting a thread on a worktree closes', async () => {
    await primeSettings({ confirmDelete: true });
    setBindingMock('GitWorktreeStatus', async () => ({ path: '/tmp/wt', terminals: 3 }));

    const { getByRole, findByText } = renderMenu(makeThread({
      id: 'ctx-worktree',
      projectId: 'project-1',
      workspacePath: '/tmp/wt',
      worktreePath: '/tmp/wt',
    }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete' }));

    await findByText(/This action cannot be undone\. 3 terminals will close\./);
  });

  it('counts the terminals of each selected worktree in a bulk delete', async () => {
    await primeSettings({ confirmDelete: false });
    const first = makeThread({ id: 'thread-1', projectId: 'project-1', workspacePath: '/tmp/wt-a', worktreePath: '/tmp/wt-a' });
    const second = makeThread({ id: 'thread-2', projectId: 'project-1', workspacePath: '/tmp/wt-b', worktreePath: '/tmp/wt-b' });
    replaceAllThreads([first, second]);
    setBindingMock('GitWorktreeStatus', async (_ws: unknown, path: string) => ({ path, terminals: 1 }));
    setThreadSelection(['thread-1', 'thread-2']);

    const { getByRole, findByText } = renderMenu(first);
    await fireEvent.click(getByRole('menuitem', { name: 'Delete (2)' }));

    await findByText(/This action cannot be undone\. 2 terminals will close\./);
  });
});

describe('<ThreadContextMenu> Check for Provider Updates', () => {
  // Gated on the write-once import stamp, never on sessionRef: every thread
  // that has run a turn has a sessionRef, so that gate would offer the item
  // on threads with no source file to check.
  beforeEach(() => {
    resetBindingMocks();
    clearThreadSelection();
    setPageGrantsFromBootstrap(false);
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
  });

  afterEach(() => {
    setPageGrantsFromBootstrap(false);
  });

  it('is absent on a thread Agent Overflow created itself', () => {
    const { baseElement } = renderMenu(makeThread());
    expect(visibleLabels(baseElement)).not.toContain('Check for Provider Updates');
  });

  it('is absent when the stamp is the empty string, not just missing', () => {
    const { baseElement } = renderMenu(makeThread({ importSource: '' }));
    expect(visibleLabels(baseElement)).not.toContain('Check for Provider Updates');
  });

  it('is present on an imported thread', () => {
    const { baseElement } = renderMenu(makeThread({ importSource: 'codex' }));
    expect(visibleLabels(baseElement)).toContain('Check for Provider Updates');
  });

  it('confirms with the backend prose, then applies', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'updates-available',
      newItems: 12,
      newTurns: 2,
      detail: '12 new messages are waiting in the Claude session file.',
    }));
    let appliedId: string | null = null;
    setBindingMock('ImportThreadUpdates', async (id: string) => {
      appliedId = id;
      return { appliedItems: 12, appliedTurns: 2 };
    });

    const { getByRole, queryByRole } = renderMenu(makeThread({ importSource: 'claude' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Check for Provider Updates' }));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    await tick();

    expect(queryByRole('dialog')).toBeInTheDocument();
    // The backend ships prose for the verdict it returned; re-deriving it
    // here would drop the turn count and drift from the toast path.
    expect(queryByRole('dialog')?.textContent).toContain(
      '12 new messages are waiting in the Claude session file.',
    );
    expect(appliedId).toBeNull();

    await fireEvent.click(getByRole('button', { name: 'Import' }));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(appliedId).toBe('thread-1');
  });

  it('falls back to both counts when the backend sends no prose', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'updates-available',
      newItems: 12,
      newTurns: 2,
      detail: '',
    }));

    const { getByRole, queryByRole } = renderMenu(makeThread({ importSource: 'claude' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Check for Provider Updates' }));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    await tick();

    expect(queryByRole('dialog')?.textContent).toContain(
      '12 new items across 2 turns since this thread was imported.',
    );
  });

  it('presents a profile-only repair as a restore, not an empty import', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'updates-available',
      newItems: 0,
      newTurns: 0,
      restoresModelProfile: true,
      detail: 'The model settings recorded in the session file can be restored.',
    }));

    const { getByRole, queryByRole } = renderMenu(makeThread({ importSource: 'codex' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Check for Provider Updates' }));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    await tick();

    expect(queryByRole('dialog')?.textContent).toContain('Restore Model Settings');
    expect(queryByRole('dialog')?.textContent).toContain(
      'The model settings recorded in the session file can be restored.',
    );
    expect(getByRole('button', { name: 'Restore' })).toBeInTheDocument();
  });

  it('is disabled with an ungranted tooltip in a view-only session', async () => {
    setPageGrantsFromBootstrap(true);
    const check = setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'updates-available',
      newItems: 1,
      newTurns: 1,
    }));

    const { getByRole } = renderMenu(makeThread({ importSource: 'claude' }));
    const item = getByRole('menuitem', { name: 'Check for Provider Updates' });

    // Visible-but-disabled, not hidden: hiding it would read as "this thread
    // wasn't imported", which is a different fact.
    expect(item.getAttribute('aria-disabled')).toBe('true');
    expect(item.getAttribute('title')).toBe('Not granted to this device');

    await fireEvent.click(item);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(check).not.toHaveBeenCalled();
  });

  it('opens no dialog when there is nothing to import', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'up-to-date',
      newItems: 0,
      newTurns: 0,
    }));

    const { getByRole, queryByRole } = renderMenu(makeThread({ importSource: 'claude' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Check for Provider Updates' }));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    await tick();

    expect(queryByRole('dialog')).toBeNull();
  });

  it('shows the check is running instead of a menu that looks unresponsive', async () => {
    let release: (value: unknown) => void = () => {};
    setBindingMock('CheckThreadImportUpdates', () => new Promise((resolve) => (release = resolve)));

    const { getByRole } = renderMenu(makeThread({ importSource: 'claude' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Check for Provider Updates' }));
    await tick();

    // MenuItem disables via aria-disabled (Menu owns roving tabindex, so the
    // rows are never natively disabled buttons).
    const item = getByRole('menuitem', { name: 'Checking for Provider Updates…' });
    expect(item.getAttribute('aria-disabled')).toBe('true');

    release({ threadId: 'thread-1', status: 'up-to-date', newItems: 0, newTurns: 0 });
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
  });
});

// ── Thread groups ────────────────────────────────────────────────────────
//
// "Move to Group" in bulk needs one shared project, because a group belongs
// to one. And a grouped row shows no pin items at all — the group carries the
// one pin the row is allowed.

describe('<ThreadContextMenu> group items', () => {
  // The submenu trigger renders a trailing ▸ inside the same menuitem, so
  // labels here are normalized rather than read raw.
  function groupMenuLabels(el: HTMLElement): string[] {
    return visibleLabels(el).map((text) => text.replace(/[\u25B8\s]+$/u, ''));
  }

  function clickItem(el: HTMLElement, label: string): Promise<boolean> {
    const item = Array.from(el.querySelectorAll('[role="menuitem"]'))
      .find((node) => node.textContent?.trim() === label);
    if (!item) throw new Error(`${label} not rendered`);
    return fireEvent.click(item);
  }

  beforeEach(() => {
    resetBindingMocks();
    clearThreadSelection();
    resetSidebarForTest();
    resetThreadGroupsForTest();
    upsertThreadGroup({
      id: 'g-zebra',
      projectId: 'project-1',
      name: 'Zebra',
      createdAt: 0,
      updatedAt: 0,
    });
    upsertThreadGroup({
      id: 'g-alpha',
      projectId: 'project-1',
      name: 'Alpha',
      createdAt: 0,
      updatedAt: 0,
    });
  });

  async function openSubmenu(baseElement: HTMLElement) {
    const trigger = Array.from(baseElement.querySelectorAll('[data-submenu-trigger]'))
      .find((el) => el.textContent?.includes('Move to Group')) as HTMLElement;
    await fireEvent.click(trigger);
    await tick();
    return trigger;
  }

  it('offers Move to Group on a top-level row, and no Remove from Group', () => {
    const { baseElement } = renderMenu(makeThread({ projectId: 'project-1' }));
    const labels = groupMenuLabels(baseElement);
    expect(labels).toContain('Move to Group');
    expect(labels).not.toContain('Remove from Group');
    // Placed after Mark Unread and before the pin items.
    expect(labels.indexOf('Move to Group')).toBe(labels.indexOf('Mark Unread') + 1);
    expect(labels.indexOf('Move to Group')).toBeLessThan(labels.indexOf('Pin Thread'));
  });

  it('hides both group items on a thread with no project', () => {
    const { baseElement } = renderMenu(makeThread());
    expect(groupMenuLabels(baseElement)).not.toContain('Move to Group');
  });

  it('adds Remove from Group and keeps the pin items for a grouped row', () => {
    const { baseElement } = renderMenu(makeThread({
      projectId: 'project-1',
      groupId: 'g-alpha',
    }));
    const labels = groupMenuLabels(baseElement);
    expect(labels).toContain('Remove from Group');
    expect(labels).toContain('Pin Thread');
  });

  it('offers the burner move and unpin on a pinned grouped row', () => {
    const { baseElement } = renderMenu(makeThread({
      projectId: 'project-1',
      groupId: 'g-alpha',
      pinnedAt: 1,
      pinGroup: 1,
    }));
    const labels = groupMenuLabels(baseElement);
    expect(labels).toContain('Move to Front Burner');
    expect(labels).toContain('Unpin Thread');
  });

  it('lists the project groups by name, then New Group…', async () => {
    const { baseElement } = renderMenu(makeThread({ projectId: 'project-1' }));
    await openSubmenu(baseElement);

    const submenu = baseElement.querySelector('[role="menu"][aria-label="Move to Group"]') as HTMLElement;
    expect(visibleLabels(submenu).map((t) => t.replace(/\s+/gu, ' ')))
      .toEqual(['Alpha', 'Zebra', 'New Group…']);
  });

  it('marks the row’s current group as the answer, not an action', async () => {
    const { baseElement } = renderMenu(makeThread({
      projectId: 'project-1',
      groupId: 'g-alpha',
    }));
    await openSubmenu(baseElement);

    const submenu = baseElement.querySelector('[role="menu"][aria-label="Move to Group"]') as HTMLElement;
    const alpha = Array.from(submenu.querySelectorAll('[role="menuitem"]'))
      .find((el) => el.textContent?.includes('Alpha')) as HTMLElement;
    expect(alpha.getAttribute('aria-disabled')).toBe('true');
  });

  it('moves the row into the picked group', async () => {
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { baseElement } = renderMenu(makeThread({ id: 'row', projectId: 'project-1' }));
    await openSubmenu(baseElement);

    const submenu = baseElement.querySelector('[role="menu"][aria-label="Move to Group"]') as HTMLElement;
    const zebra = Array.from(submenu.querySelectorAll('[role="menuitem"]'))
      .find((el) => el.textContent?.includes('Zebra')) as HTMLElement;
    await fireEvent.click(zebra);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(setGroup).toHaveBeenCalledWith(['row'], 'g-zebra');
  });

  it('New Group… creates, moves, and asks the new row to open its rename', async () => {
    setBindingMock('CreateThreadGroup', async (projectId: string, name: string) => ({
      id: 'g-new',
      projectId,
      name,
      createdAt: 0,
      updatedAt: 0,
    }));
    let pendingDuringMove: boolean | null = null;
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => {
      pendingDuringMove = consumePendingGroupRename('g-new');
      return [];
    }));
    const { baseElement } = renderMenu(makeThread({ id: 'row', projectId: 'project-1' }));
    await openSubmenu(baseElement);

    const submenu = baseElement.querySelector('[role="menu"][aria-label="Move to Group"]') as HTMLElement;
    const create = Array.from(submenu.querySelectorAll('[role="menuitem"]'))
      .find((el) => el.textContent?.includes('New Group…')) as HTMLElement;
    await fireEvent.click(create);
    for (let i = 0; i < 10; i += 1) await Promise.resolve();

    expect(setGroup).toHaveBeenCalledWith(['row'], 'g-new');
    // Asked AFTER the move: the move re-sorts the group and a moved row
    // blurs an editor that is already open in it.
    expect(pendingDuringMove).toBe(false);
    expect(consumePendingGroupRename('g-new')).toBe(true);
  });

  it('offers the group items in bulk when every selection shares one project', () => {
    replaceAllThreads([
      makeThread({ id: 'a', projectId: 'project-1', groupId: 'g-alpha' }),
      makeThread({ id: 'b', projectId: 'project-1' }),
    ]);
    setThreadSelection(['a', 'b']);
    const { baseElement } = renderMenu(makeThread({ id: 'a', projectId: 'project-1' }));
    const labels = groupMenuLabels(baseElement);
    expect(labels).toContain('Move to Group');
    expect(labels).toContain('Remove from Group');
  });

  it('hides the group items in bulk when the selection spans projects', () => {
    replaceAllThreads([
      makeThread({ id: 'a', projectId: 'project-1' }),
      makeThread({ id: 'b', projectId: 'project-2' }),
    ]);
    setThreadSelection(['a', 'b']);
    const { baseElement } = renderMenu(makeThread({ id: 'a', projectId: 'project-1' }));
    const labels = groupMenuLabels(baseElement);
    expect(labels).not.toContain('Move to Group');
    expect(labels).not.toContain('Remove from Group');
  });

  it('ungroups only the selected rows that are IN a group', async () => {
    // canRemoveFromGroup is satisfied by ONE grouped row, so the write must
    // name that row and not the whole selection — the rest would spend a
    // backend round trip to change nothing.
    replaceAllThreads([
      makeThread({ id: 'a', projectId: 'project-1', groupId: 'g-alpha' }),
      makeThread({ id: 'b', projectId: 'project-1' }),
    ]);
    setThreadSelection(['a', 'b']);
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { baseElement } = renderMenu(
      makeThread({ id: 'a', projectId: 'project-1', groupId: 'g-alpha' }),
    );

    await clickItem(baseElement, 'Remove from Group');
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(setGroup).toHaveBeenCalledWith(['a'], '');
  });

  it('expands the project before New Group…, so the new row can open its rename', async () => {
    setBindingMock('CreateThreadGroup', async (projectId: string, name: string) => ({
      id: 'g-new',
      projectId,
      name,
      createdAt: 0,
      updatedAt: 0,
    }));
    setBindingMock('SetThreadGroup', vi.fn(async () => []));
    collapseProject('project-1');
    const { baseElement } = renderMenu(makeThread({ id: 'row', projectId: 'project-1' }));
    await openSubmenu(baseElement);

    const submenu = baseElement.querySelector('[role="menu"][aria-label="Move to Group"]') as HTMLElement;
    const create = Array.from(submenu.querySelectorAll('[role="menuitem"]'))
      .find((el) => el.textContent?.includes('New Group…')) as HTMLElement;
    await fireEvent.click(create);
    for (let i = 0; i < 10; i += 1) await Promise.resolve();

    expect(isProjectExpanded('project-1')).toBe(true);
  });

  it('moves the whole bulk selection in one call', async () => {
    replaceAllThreads([
      makeThread({ id: 'a', projectId: 'project-1' }),
      makeThread({ id: 'b', projectId: 'project-1' }),
    ]);
    setThreadSelection(['a', 'b']);
    const setGroup = setBindingMock('SetThreadGroup', vi.fn(async () => []));
    const { baseElement } = renderMenu(makeThread({ id: 'a', projectId: 'project-1' }));
    await openSubmenu(baseElement);

    const submenu = baseElement.querySelector('[role="menu"][aria-label="Move to Group"]') as HTMLElement;
    const zebra = Array.from(submenu.querySelectorAll('[role="menuitem"]'))
      .find((el) => el.textContent?.includes('Zebra')) as HTMLElement;
    await fireEvent.click(zebra);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(setGroup).toHaveBeenCalledWith(['a', 'b'], 'g-zebra');
  });
});

// A standing agent's row (docs/specs/standing-agents.md, "Sidebar") swaps
// Promote for Demote, Loops and Watches and Archive Done Children, and
// drops the pin and group items; a thread it spawned drops the group items.

describe('<ThreadContextMenu> standing agent', () => {
  beforeEach(() => {
    resetBindingMocks();
    clearThreadSelection();
    resetSidebarForTest();
    resetThreadGroupsForTest();
    upsertThreadGroup({ id: 'g-alpha', projectId: 'project-1', name: 'Alpha', createdAt: 0, updatedAt: 0 });
  });

  function labelsOf(el: HTMLElement): string[] {
    return visibleLabels(el).map((text) => text.replace(/[▸\s]+$/u, ''));
  }

  function item(el: HTMLElement, label: string): HTMLElement {
    const node = Array.from(el.querySelectorAll('[role="menuitem"]'))
      .find((node) => node.textContent?.trim().replace(/[▸\s]+$/u, '') === label);
    if (!node) throw new Error(`${label} not rendered`);
    return node as HTMLElement;
  }

  function renderAgentMenu(thread: Thread, extra: { agentBound?: boolean; agentChildren?: Thread[]; archivedChild?: boolean } = {}) {
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
    const pane = createThreadPane();
    const anchor = document.createElement('div');
    document.body.appendChild(anchor);
    const view = render(ThreadContextMenu, {
      props: {
        thread, pane, anchor, open: true, onClose: () => {}, onRename: () => {}, isActive: false, ...extra,
      },
    });
    return { ...view, pane };
  }

  it.each([
    [{ projectId: 'project-1' }, true],
    [{ mode: 'plan' as const }, true],
    [{ mode: 'terminal' as const }, false],
    [{ archived: true }, false],
    [{ isDraft: true }, false],
    [{ agentPromotedAt: 1 }, false],
  ])('offers Promote only on a chat or plan thread: %j → %s', (overrides, offered) => {
    const labels = labelsOf(renderMenu(makeThread(overrides)).baseElement);
    expect(labels.includes('Promote to Standing Agent')).toBe(offered);
  });

  it('promotes through the binding', async () => {
    const promote = setBindingMock('PromoteStandingAgent', async () => ({ threadId: 'thread-1' }));
    const { baseElement } = renderMenu(makeThread());
    await fireEvent.click(item(baseElement, 'Promote to Standing Agent'));
    await tick();
    expect(promote).toHaveBeenCalledWith('thread-1');
  });

  it('replaces the pin and group items on an agent row with the agent items, in order', () => {
    const { baseElement } = renderAgentMenu(
      makeThread({ projectId: 'project-1', agentPromotedAt: 1, pinnedAt: 1, pinGroup: 0 }),
      { agentBound: true },
    );
    expect(labelsOf(baseElement)).toEqual([
      'Open in New Pane',
      'Rename Thread',
      'Loops and Watches',
      'Archive Done Children',
      'Demote Standing Agent',
      'Fork Thread',
      'Mark Unread',
      'Copy Path',
      'Copy Thread ID',
      'Archive Thread',
      'Delete',
    ]);
    expect(item(baseElement, 'Archive Done Children').getAttribute('aria-disabled')).toBe('true');
    expect(item(baseElement, 'Archive Done Children').getAttribute('title')).toBe('No spawned thread is done');
  });

  it('keeps the pin items but drops the group items on a thread an agent spawned', () => {
    const { baseElement } = renderAgentMenu(
      makeThread({ projectId: 'project-1', spawnedByThreadId: 'agent-1' }),
      { agentBound: true },
    );
    const labels = labelsOf(baseElement);
    expect(labels).toContain('Pin Thread');
    expect(labels).toContain('Promote to Standing Agent');
    expect(labels).not.toContain('Move to Group');
    expect(labels).not.toContain('Remove from Group');
    expect(labels).not.toContain('Demote Standing Agent');
  });

  it('demotes behind a confirm that says the home stays, and only after it', async () => {
    const demote = setBindingMock('DemoteStandingAgent', async () => {});
    const { baseElement } = renderAgentMenu(makeThread({ agentPromotedAt: 1 }));
    await fireEvent.click(item(baseElement, 'Demote Standing Agent'));
    await tick();
    expect(demote).not.toHaveBeenCalled();
    const dialog = baseElement.querySelector('[role="alertdialog"], [role="dialog"]');
    expect(dialog?.textContent).toContain('home directory stay');
    await fireEvent.click(Array.from(baseElement.querySelectorAll('button')).find((b) => b.textContent?.trim() === 'Demote')!);
    await tick();
    expect(demote).toHaveBeenCalledWith('thread-1');
  });

  it('deletes the home only through the confirm\'s own destructive choice', async () => {
    const demote = setBindingMock('DemoteStandingAgent', async () => {});
    const demoteAndDelete = setBindingMock('DemoteStandingAgentAndDeleteHome', async () => {});
    const { baseElement } = renderAgentMenu(makeThread({ agentPromotedAt: 1 }));
    await fireEvent.click(item(baseElement, 'Demote Standing Agent'));
    await tick();
    const choice = Array.from(baseElement.querySelectorAll('button')).find(
      (b) => b.textContent?.trim() === 'Demote and Delete Home',
    );
    expect(choice).toBeTruthy();
    await fireEvent.click(choice!);
    await tick();
    expect(demoteAndDelete).toHaveBeenCalledWith('thread-1');
    expect(demote).not.toHaveBeenCalled();
  });

  it('leaves Demote live but Demote and Delete Home inert without threads:autonomy', async () => {
    await pairWithScopes([...OBSERVE_SCOPES, 'threads:operate']);
    try {
      const demoteAndDelete = setBindingMock('DemoteStandingAgentAndDeleteHome', async () => {});
      const { baseElement } = renderAgentMenu(makeThread({ agentPromotedAt: 1 }));
      await fireEvent.click(item(baseElement, 'Demote Standing Agent'));
      await tick();
      const buttons = Array.from(baseElement.querySelectorAll('button'));
      const choice = buttons.find((b) => b.textContent?.trim() === 'Demote and Delete Home') as HTMLButtonElement;
      expect(choice.disabled).toBe(true);
      expect(choice.getAttribute('title')).toBe('Not granted to this device');
      expect((buttons.find((b) => b.textContent?.trim() === 'Demote') as HTMLButtonElement).disabled).toBe(false);
      await fireEvent.click(choice);
      expect(demoteAndDelete).not.toHaveBeenCalled();
    } finally {
      resetToLocalPage();
    }
  });

  it('hides the group items in bulk when the selection holds an agent or a spawned thread', () => {
    replaceAllThreads([
      makeThread({ id: 'a', projectId: 'project-1' }),
      makeThread({ id: 'b', projectId: 'project-1', spawnedByThreadId: 'agent-1', spawnedByAgent: true }),
    ]);
    setThreadSelection(['a', 'b']);
    const { baseElement } = renderAgentMenu(makeThread({ id: 'a', projectId: 'project-1' }));
    const labels = labelsOf(baseElement);
    expect(labels).toContain('Archive (2)');
    expect(labels).not.toContain('Move to Group');
    expect(labels).not.toContain('Remove from Group');
  });

  it('offers an archived child Unarchive, Copy and Delete, and no pin, group or archive', () => {
    const { baseElement } = renderAgentMenu(
      makeThread({ projectId: 'project-1', spawnedByThreadId: 'agent-1', archived: true }),
      { agentBound: true, archivedChild: true },
    );
    expect(labelsOf(baseElement)).toEqual([
      'Open in New Pane',
      'Rename Thread',
      'Unarchive Thread',
      'Copy Path',
      'Copy Thread ID',
      'Delete',
    ]);
  });

  it('unarchives an archived child through the binding', async () => {
    const unarchive = setBindingMock('UnarchiveThread', async () => makeThread());
    const { baseElement } = renderAgentMenu(
      makeThread({ projectId: 'project-1', spawnedByThreadId: 'agent-1', archived: true }),
      { agentBound: true, archivedChild: true },
    );
    await fireEvent.click(item(baseElement, 'Unarchive Thread'));
    await tick();
    expect(unarchive).toHaveBeenCalledWith('thread-1');
  });

  it('Loops and Watches opens the thread with its background rail open', async () => {
    setBindingMock('GetThread', async () => makeThread({ agentPromotedAt: 1 }));
    setBindingMock('GetThreadDisplayState', async () => null);
    const { baseElement, pane } = renderAgentMenu(makeThread({ agentPromotedAt: 1 }));
    await fireEvent.click(item(baseElement, 'Loops and Watches'));
    // The rail opens once the thread is mounted, after the pane's switch
    // has read the thread's own rail preferences.
    await waitFor(() => expect(pane.activityRailBackgroundOpen).toBe(true));
    expect(pane.threadId).toBe('thread-1');
  });

  it('Archive Done Children counts the idle children, confirms, then archives only those', async () => {
    const archive = setBindingMock('ArchiveThread', async () => {});
    const idle = makeThread({ id: 'child-idle', spawnedByThreadId: 'thread-1' });
    const idle2 = makeThread({ id: 'child-idle-2', spawnedByThreadId: 'thread-1' });
    const busy = makeThread({ id: 'child-busy', spawnedByThreadId: 'thread-1' });
    setThreadStatus('child-busy', 'awaiting-input');
    const { baseElement } = renderAgentMenu(
      makeThread({ agentPromotedAt: 1 }),
      { agentChildren: [idle, busy, idle2] },
    );
    const row = item(baseElement, 'Archive Done Children (2)');
    expect(row.getAttribute('aria-disabled')).not.toBe('true');
    await fireEvent.click(row);
    await tick();
    const dialog = baseElement.querySelector('[role="dialog"], [role="alertdialog"]');
    expect(dialog?.textContent).toContain('2 spawned threads');
    expect(archive).not.toHaveBeenCalled();
    const confirm = Array.from(baseElement.querySelectorAll('button')).find((b) => b.textContent?.trim() === 'Archive');
    if (!confirm) throw new Error('confirm button not rendered');
    await fireEvent.click(confirm);
    await waitFor(() => expect(archive).toHaveBeenCalledTimes(2));
    expect(archive.mock.calls.map((call) => call[0])).toEqual(['child-idle', 'child-idle-2']);
  });
});
