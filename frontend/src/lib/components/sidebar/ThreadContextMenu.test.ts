// Right-click menu visibility gating. Items + order are pinned:
// Rename, Fork (when fork-able), Mark Unread, pin controls, Copy Path,
// Copy Thread ID, Delete (when not a child thread).

import { afterEach, describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadContextMenu from './ThreadContextMenu.svelte';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import { createThreadPane } from '../../stores/thread.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { answerAsV0019 } from '../../../test/helpers/olderComputer';
import { clearThreadSelection, getSelectedThreadIds, setThreadSelection } from '../../stores/threadFilter.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { getThreadById, replaceAllThreads } from '../../stores/threads.svelte';
import { closeAssistantProjectsDialog, getAssistantProjectsAgentId } from '../../stores/newAssistant.svelte';
import { getPreviousThreads, resetPreviousThreadsForTest } from '../../stores/assistantPastThreads.svelte';
import { getPane } from '../../stores/panes.svelte';
import { getToasts, resetToastsForTest } from '../../stores/toast.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { noteThread } from '../../transport/entityIndex';
import { HOME_BACKEND } from '../../transport/backendKey';
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

/** The confirmation's Delete button, beside its Cancel. */
function confirmButton(root: HTMLElement): HTMLButtonElement | undefined {
  return Array.from(root.querySelectorAll<HTMLButtonElement>('button:not([role="menuitem"])')).find((b) => b.textContent?.trim() === 'Delete');
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
    const archive = setBindingMock('ArchiveThread', vi.fn(async () => ({})));
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
    const archive = setBindingMock('ArchiveThread', vi.fn(async () => ({})));
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

  // Each archive reports its own failure into the walk, which shows one
  // aggregate toast and keeps the selection. The pane the menu belongs to
  // shows the first thread, which the walk archives, so the aggregate must
  // not go to that pane.
  it('toasts a failed archive in a bulk archive that closed its own pane', async () => {
    await primeSettings({ confirmArchive: false });
    resetToastsForTest();
    const first = makeThread({ id: 'thread-1' });
    replaceAllThreads([first, makeThread({ id: 'thread-2' })]);
    setBindingMock('ArchiveThread', async (id: string) => {
      if (id === 'thread-2') throw new Error('archive refused');
    });
    setThreadSelection(['thread-1', 'thread-2']);
    const pane = await buildPane(first, [], 'pane-first');
    const anchor = document.createElement('div');
    document.body.appendChild(anchor);
    const { getByRole } = render(ThreadContextMenu, {
      props: { thread: first, pane, anchor, open: true, onClose: () => {}, onRename: () => {}, isActive: true },
    });
    await fireEvent.click(getByRole('menuitem', { name: 'Archive (2)' }));

    await vi.waitFor(() => expect(getToasts().filter((t) => t.type === 'error').map((t) => t.message))
      .toEqual(['1/2 succeeded — Archive refused.']));
    expect(getPane('pane-first')).toBeUndefined();
    expect(getSelectedThreadIds()).toEqual(new Set(['thread-1', 'thread-2']));
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
    const archive = setBindingMock('ArchiveThread', vi.fn(async () => ({})));
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
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    setBindingMock('DeleteThreads', async (ids: string[]) => { deletedId = ids[0]; });

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
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    setBindingMock('DeleteThreads', async (ids: string[]) => { deletedId = ids[0]; });

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

  // A computer that refuses deletes nothing there and keeps the selection;
  // the menu's pane shows a thread the delete removed elsewhere, so the
  // refusal is a toast, not that pane's error.
  it('toasts a refused bulk delete and keeps the selection', async () => {
    await primeSettings({ confirmDelete: false });
    resetToastsForTest();
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    setBindingMock('DeleteThreads', async () => { throw new Error('delete refused'); });
    const first = makeThread({ id: 'thread-1' });
    replaceAllThreads([first, makeThread({ id: 'thread-2' })]);
    setThreadSelection(['thread-1', 'thread-2']);
    const pane = await buildPane(first, [], 'pane-bulk-delete');
    const anchor = document.createElement('div');
    document.body.appendChild(anchor);
    const { getByRole } = render(ThreadContextMenu, {
      props: { thread: first, pane, anchor, open: true, onClose: () => {}, onRename: () => {}, isActive: true },
    });
    await fireEvent.click(getByRole('menuitem', { name: 'Delete (2)' }));
    await waitFor(() => expect(getByRole('button', { name: 'Delete' })).toBeEnabled());
    await fireEvent.click(getByRole('button', { name: 'Delete' }));

    await vi.waitFor(() => expect(getToasts().filter((t) => t.type === 'error').map((t) => t.message))
      .toEqual(['Delete refused.']));
    expect(getPane('pane-bulk-delete')?.generalError ?? null).toBeNull();
    expect(getSelectedThreadIds()).toEqual(new Set(['thread-1', 'thread-2']));
  });

  it('keeps bulk delete always-confirming even when confirmDelete is off', async () => {
    await primeSettings({ confirmDelete: false });
    let deleted = false;
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    setBindingMock('DeleteThreads', async () => { deleted = true; });

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

  // The confirmation says how many workers go with the thread
  // (docs/specs/standing-agents.md, "Owned workers").
  it('counts the workers a delete takes with the thread', async () => {
    await primeSettings({ confirmDelete: true });
    for (const [workers, sentence] of [
      [3, 'Its 3 workers are deleted with it.'],
      [1, 'Its worker is deleted with it.'],
    ] as const) {
      setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers }));
      const view = renderMenu(makeThread({ id: `owner-${workers}` }));
      await fireEvent.click(view.getByRole('menuitem', { name: 'Delete' }));
      const dialog = () => view.baseElement.querySelector('[role="alertdialog"], [role="dialog"]');
      await waitFor(() => expect(dialog()?.textContent).toContain(
        `This will permanently delete this thread and all its messages. This action cannot be undone. ${sentence}`,
      ));
      view.unmount();
    }
  });

  // Workers on other computers are not counted: only that computer knows
  // which still belong to the thread.
  it('says workers spawned on other computers are deleted there too, only when it spawned any', async () => {
    await primeSettings({ confirmDelete: true });
    for (const [dependents, sentence] of [
      [{ workers: 0, remoteWorkers: true }, 'Workers it spawned on other computers are deleted there too.'],
      [{ workers: 2, remoteWorkers: true }, 'Its 2 workers are deleted with it. Workers it spawned on other computers are deleted there too.'],
    ] as const) {
      setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, ...dependents }));
      const view = renderMenu(makeThread({ id: `remote-owner-${dependents.workers}` }));
      await fireEvent.click(view.getByRole('menuitem', { name: 'Delete' }));
      const dialog = () => view.baseElement.querySelector('[role="alertdialog"], [role="dialog"]');
      await waitFor(() => expect(dialog()?.textContent).toContain(
        `This will permanently delete this thread and all its messages. This action cannot be undone. ${sentence}`,
      ));
      view.unmount();
    }
    const counted = setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 1, remoteWorkers: false }));
    const view = renderMenu(makeThread({ id: 'local-owner' }));
    await fireEvent.click(view.getByRole('menuitem', { name: 'Delete' }));
    await waitFor(() => expect(view.baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain('Its worker is deleted with it.'));
    expect(counted).toHaveBeenCalledWith(['local-owner']);
    expect(view.baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).not.toContain('other computers');
    view.unmount();
  });

  it('says workers the selection spawned on other computers are deleted there too in the bulk delete confirm', async () => {
    await primeSettings({ confirmDelete: false });
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0, remoteWorkers: true }));
    replaceAllThreads([makeThread({ id: 'thread-1' }), makeThread({ id: 'thread-2' })]);
    setThreadSelection(['thread-1', 'thread-2']);
    const { getByRole, baseElement } = renderMenu(makeThread({ id: 'thread-1' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete (2)' }));
    await waitFor(() => expect(baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain(
      'This will permanently delete 2 threads and all their messages. This action cannot be undone. Workers they spawned on other computers are deleted there too.',
    ));
  });

  it('says nothing of workers when the thread has none, or is on a computer older than the read', async () => {
    await primeSettings({ confirmDelete: true });
    for (const read of [
      async () => ({ pastThreads: 0, helpers: 0, workers: 0 }),
      // v0.0.19 has no GetDeleteDependents: its delete takes no workers.
      async () => { throw Object.assign(new Error('method not found'), { code: 'method_not_found' }); },
    ]) {
      const counted = setBindingMock('GetDeleteDependents', read);
      const remove = setBindingMock('DeleteThreads', vi.fn(async () => ({})));
      const view = renderMenu(makeThread({ id: 'plain' }));
      await fireEvent.click(view.getByRole('menuitem', { name: 'Delete' }));
      await waitFor(() => expect(counted).toHaveBeenCalledWith(['plain']));
      await waitFor(() => expect(confirmButton(view.baseElement)?.disabled).toBe(false));
      const text = view.baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent;
      expect(text).not.toContain('worker');
      expect(text).not.toContain('could not be checked');
      await fireEvent.click(confirmButton(view.baseElement)!);
      await waitFor(() => expect(remove).toHaveBeenCalledWith(['plain']));
      view.unmount();
    }
  });

  // Unknown is not none: until the read answers, the delete cannot be
  // confirmed, whatever it would take on another computer.
  it('cannot be confirmed before it knows what the delete takes', async () => {
    await primeSettings({ confirmDelete: true });
    let answer: (counts: object) => void = () => {};
    setBindingMock('GetDeleteDependents', () => new Promise((resolve) => { answer = resolve; }));
    const remove = setBindingMock('DeleteThreads', vi.fn(async () => ({})));
    const view = renderMenu(makeThread({ id: 'early' }));
    await fireEvent.click(view.getByRole('menuitem', { name: 'Delete' }));
    await tick();
    const confirm = confirmButton(view.baseElement)!;
    expect(confirm.disabled).toBe(true);
    await fireEvent.click(confirm);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(remove).not.toHaveBeenCalled();
    answer({ pastThreads: 0, helpers: 0, workers: 0, remoteWorkers: true });
    await waitFor(() => expect(confirmButton(view.baseElement)?.disabled).toBe(false));
    expect(view.baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent)
      .toContain('Workers it spawned on other computers are deleted there too.');
    await fireEvent.click(confirmButton(view.baseElement)!);
    await waitFor(() => expect(remove).toHaveBeenCalledWith(['early']));
  });

  it('cannot confirm a bulk delete before it knows what the delete takes, and says when the read failed', async () => {
    await primeSettings({ confirmDelete: false });
    let fail: (err: Error) => void = () => {};
    setBindingMock('GetDeleteDependents', () => new Promise((_resolve, reject) => { fail = reject; }));
    const remove = setBindingMock('DeleteThreads', vi.fn(async () => ({})));
    replaceAllThreads([makeThread({ id: 'thread-1' }), makeThread({ id: 'thread-2' })]);
    setThreadSelection(['thread-1', 'thread-2']);
    const { getByRole, baseElement } = renderMenu(makeThread({ id: 'thread-1' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete (2)' }));
    await tick();
    expect(confirmButton(baseElement)?.disabled).toBe(true);
    await fireEvent.click(confirmButton(baseElement)!);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(remove).not.toHaveBeenCalled();
    fail(new Error('database is locked'));
    await waitFor(() => expect(confirmButton(baseElement)?.disabled).toBe(false));
    expect(baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain(
      'This will permanently delete 2 threads and all their messages. This action cannot be undone. Their sub-threads could not be checked; any they have are deleted too, here and on other computers.',
    );
    await fireEvent.click(confirmButton(baseElement)!);
    await waitFor(() => expect(remove).toHaveBeenCalledWith(['thread-1', 'thread-2']));
  });

  it('says the sub-threads could not be checked when the read fails, and can still be confirmed', async () => {
    await primeSettings({ confirmDelete: true });
    setBindingMock('GetDeleteDependents', async () => { throw new Error('database is locked'); });
    const remove = setBindingMock('DeleteThreads', vi.fn(async () => ({})));
    const view = renderMenu(makeThread({ id: 'unchecked' }));
    await fireEvent.click(view.getByRole('menuitem', { name: 'Delete' }));
    await waitFor(() => expect(view.baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain(
      'This will permanently delete this thread and all its messages. This action cannot be undone. Its sub-threads could not be checked; any it has are deleted too, here and on other computers.',
    ));
    expect(confirmButton(view.baseElement)?.disabled).toBe(false);
    await fireEvent.click(confirmButton(view.baseElement)!);
    await waitFor(() => expect(remove).toHaveBeenCalledWith(['unchecked']));
  });

  // v0.0.19 has neither GetDeleteDependents nor DeleteThreads: the bulk
  // confirmation knows at once the delete takes nothing more, and the
  // threads go one at a time through DeleteThread.
  it('deletes a selection on a computer on v0.0.19 one thread at a time', async () => {
    await primeSettings({ confirmDelete: false });
    answerAsV0019();
    replaceAllThreads([makeThread({ id: 'thread-1' }), makeThread({ id: 'thread-2' })]);
    setThreadSelection(['thread-1', 'thread-2']);
    const { getByRole, baseElement } = renderMenu(makeThread({ id: 'thread-1' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete (2)' }));
    await waitFor(() => expect(confirmButton(baseElement)?.disabled).toBe(false));
    expect(baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).not.toContain('could not be checked');
    await fireEvent.click(confirmButton(baseElement)!);
    await waitFor(() => expect(getBindingMock('DeleteThread')!.mock.calls).toEqual([['thread-1'], ['thread-2']]));
  });

  // The selection is one delete on its computer: a worker under two of the
  // threads counts once, as the backend reads the set.
  it('counts the workers of the selected threads as one set in the bulk delete confirm', async () => {
    await primeSettings({ confirmDelete: false });
    const counted = setBindingMock('GetDeleteDependents', async (ids: string[]) => ({
      pastThreads: 0, helpers: 0, workers: ids.length === 2 ? 3 : 0,
    }));
    replaceAllThreads([makeThread({ id: 'thread-1' }), makeThread({ id: 'thread-2' })]);
    setThreadSelection(['thread-1', 'thread-2']);
    const { getByRole, baseElement } = renderMenu(makeThread({ id: 'thread-1' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete (2)' }));
    await waitFor(() => expect(baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain(
      'Their 3 workers are deleted with them.',
    ));
    expect(counted).toHaveBeenCalledWith(['thread-1', 'thread-2']);
  });

  it('deletes the selection as one delete on its computer', async () => {
    await primeSettings({ confirmDelete: false });
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    const remove = setBindingMock('DeleteThreads', vi.fn(async () => ({})));
    replaceAllThreads([makeThread({ id: 'thread-1' }), makeThread({ id: 'thread-2' })]);
    setThreadSelection(['thread-1', 'thread-2']);
    const { getByRole, baseElement } = renderMenu(makeThread({ id: 'thread-1' }));
    await fireEvent.click(getByRole('menuitem', { name: 'Delete (2)' }));
    await waitFor(() => expect(confirmButton(baseElement)?.disabled).toBe(false));
    await fireEvent.click(confirmButton(baseElement)!);
    await waitFor(() => expect(remove).toHaveBeenCalledTimes(1));
    expect(remove).toHaveBeenCalledWith(['thread-1', 'thread-2']);
  });

  // The delete keeps the worktrees, so neither confirmation says anything
  // of them or reads anything about them.
  it('says nothing of the worktrees of the threads it deletes', async () => {
    await primeSettings({ confirmDelete: true });
    const first = makeThread({ id: 'thread-1', projectId: 'project-1', workspacePath: '/tmp/wt-a', worktreePath: '/tmp/wt-a' });
    const second = makeThread({ id: 'thread-2', projectId: 'project-1', workspacePath: '/tmp/wt-b', worktreePath: '/tmp/wt-b' });
    replaceAllThreads([first, second]);
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    const status = setBindingMock('GitWorktreeStatus', vi.fn(async () => ({ terminals: 3 })));

    const single = renderMenu(first);
    await fireEvent.click(single.getByRole('menuitem', { name: 'Delete' }));
    expect((await single.findByText(/This action cannot be undone\./)).textContent).toBe(
      'This will permanently delete this thread and all its messages. This action cannot be undone.',
    );
    single.unmount();

    setThreadSelection(['thread-1', 'thread-2']);
    const bulk = renderMenu(first);
    await fireEvent.click(bulk.getByRole('menuitem', { name: 'Delete (2)' }));
    expect((await bulk.findByText(/This action cannot be undone\./)).textContent).toBe(
      'This will permanently delete 2 threads and all their messages. This action cannot be undone.',
    );
    expect(status).not.toHaveBeenCalled();
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

// An assistant's row (docs/specs/standing-agents.md, "Sidebar") adds New
// Thread, Same Assistant, Projects, Previous Threads and Loops and Watches,
// and drops the pin and group items. A thread another local thread spawned
// into a project offers Remove from Sidebar. No thread offers promotion.

describe('<ThreadContextMenu> standing agent', () => {
  beforeEach(() => {
    resetBindingMocks();
    clearThreadSelection();
    resetSidebarForTest();
    resetThreadGroupsForTest();
    resetPreviousThreadsForTest();
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

  function renderAgentMenu(thread: Thread, extra: { agentBound?: boolean } = {}) {
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

  it('replaces the pin and group items on an agent row with the agent items, in order', () => {
    const { baseElement } = renderAgentMenu(
      makeThread({ projectId: 'project-1', agentId: 'agent-1', pinnedAt: 1, pinGroup: 0 }),
      { agentBound: true },
    );
    expect(labelsOf(baseElement)).toEqual([
      'Open in New Pane',
      'Rename Thread',
      'New Thread, Same Assistant',
      'Projects…',
      'Previous Threads…',
      'Loops and Watches',
      'Fork Thread',
      'Mark Unread',
      'Copy Path',
      'Copy Thread ID',
      'Archive Thread',
      'Delete',
    ]);
  });

  // docs/specs/standing-agents.md ruling 36: a placed worker's row takes it
  // back out of the sidebar; its group items stay as they are.
  it.each([
    ['standalone', { placement: 'standalone' as const }, ['Move to Group', 'Remove from Sidebar'], ['Remove from Group']],
    ['grouped', { placement: 'grouped' as const, groupId: 'g1' }, ['Move to Group', 'Remove from Group', 'Remove from Sidebar'], []],
  ])('offers a %s thread a local thread spawned Remove from Sidebar and keeps its pin items', (_, overrides, present, absent) => {
    const { baseElement } = renderAgentMenu(makeThread({ projectId: 'project-1', spawnedByThreadId: 'spawner', ...overrides }));
    const labels = labelsOf(baseElement);
    expect(labels).toContain('Pin Thread');
    expect(labels).not.toContain('New Thread, Same Assistant');
    for (const label of present) expect(labels).toContain(label);
    for (const label of absent) expect(labels).not.toContain(label);
    for (const label of ['Make Standalone', 'Previous Threads…']) expect(labels).not.toContain(label);
    expect(labels.some((label) => label.startsWith('Nest Under'))).toBe(false);
  });

  // A thread a paired computer's thread spawned here is that thread's worker
  // too; Show in Sidebar gave it its row here.
  it('offers a thread a thread on another computer spawned Remove from Sidebar', () => {
    const { baseElement } = renderAgentMenu(makeThread({ projectId: 'project-1', spawnedByComputerId: 'computer-laptop', placement: 'standalone' }));
    expect(labelsOf(baseElement)).toContain('Remove from Sidebar');
  });

  it.each([
    ['a thread nobody spawned', makeThread({ projectId: 'project-1', placement: 'standalone' })],
    ['a thread outside every project', makeThread({ projectId: '', spawnedByThreadId: 'spawner', placement: 'standalone' })],
    ['an assistant', makeThread({ projectId: 'project-1', agentId: 'agent-1', spawnedByThreadId: 'creator' })],
  ])('offers %s no Remove from Sidebar', (_, thread) => {
    const view = renderAgentMenu(thread, { agentBound: Boolean(thread.agentId) });
    expect(labelsOf(view.baseElement)).not.toContain('Remove from Sidebar');
  });

  it('offers no Remove from Sidebar in a bulk menu', () => {
    replaceAllThreads([
      makeThread({ id: 'a', projectId: 'project-1', spawnedByThreadId: 'spawner' }),
      makeThread({ id: 'b', projectId: 'project-1', spawnedByThreadId: 'spawner' }),
    ]);
    setThreadSelection(['a', 'b']);
    const { baseElement } = renderAgentMenu(makeThread({ id: 'a', projectId: 'project-1', spawnedByThreadId: 'spawner' }));
    expect(labelsOf(baseElement)).not.toContain('Remove from Sidebar');
  });

  it('Remove from Sidebar makes the thread owned through the binding, and the row leaves the sidebar unpinned', async () => {
    replaceAllThreads([
      makeThread({ id: 'child', projectId: 'project-1', spawnedByThreadId: 'spawner', placement: 'grouped', groupId: 'g-alpha', pinnedAt: 5 }),
    ]);
    const place = setBindingMock('SetThreadPlacement', vi.fn(async (ids: string[], placement: string) => [
      makeThread({ id: ids[0], projectId: 'project-1', spawnedByThreadId: 'spawner', placement: placement as 'owned' }),
    ]));
    const { baseElement } = renderAgentMenu(getThreadById('child')!);
    await fireEvent.click(item(baseElement, 'Remove from Sidebar'));
    await waitFor(() => expect(place).toHaveBeenCalledWith(['child'], 'owned'));
    await waitFor(() => {
      const row = getThreadById('child');
      expect(row?.placement).toBe('owned');
      expect(row?.groupId ?? '').toBe('');
      expect(row?.pinnedAt ?? null).toBeNull();
    });
  });

  it('shows a refused Remove from Sidebar to the user and leaves the row', async () => {
    replaceAllThreads([makeThread({ id: 'child', projectId: 'project-1', spawnedByThreadId: 'spawner', placement: 'standalone' })]);
    setBindingMock('SetThreadPlacement', async () => {
      throw new Error('store: the thread was not spawned by a thread on this computer');
    });
    const { baseElement } = renderAgentMenu(getThreadById('child')!);
    await fireEvent.click(item(baseElement, 'Remove from Sidebar'));
    await waitFor(() => expect(getToasts().some((toast) => toast.type === 'error')).toBe(true));
    expect(getThreadById('child')?.placement).toBe('standalone');
  });

  it('Previous Threads… opens the assistant\'s dialog from its computer, held by the pane that asked', async () => {
    noteThread('thread-1', HOME_BACKEND);
    setBindingMock('ListAssistantPastThreads', async () => ({ threads: [] }));
    const { baseElement, pane } = renderAgentMenu(makeThread({ projectId: '', agentId: 'agent-1' }), { agentBound: true });
    await fireEvent.click(item(baseElement, 'Previous Threads…'));
    expect(getPreviousThreads()?.opening).toMatchObject({ agentId: 'agent-1', backend: HOME_BACKEND, targetPane: pane });
  });

  it('New Thread, Same Assistant starts the next thread through the binding', async () => {
    const next = setBindingMock('StartNextAssistantThread', async () => makeThread({ id: 'thread-2', agentId: 'agent-1' }));
    const { baseElement } = renderAgentMenu(makeThread({ agentId: 'agent-1' }));
    await fireEvent.click(item(baseElement, 'New Thread, Same Assistant'));
    await waitFor(() => expect(next).toHaveBeenCalledWith('thread-1'));
  });

  it('Projects… opens the assistant\'s Projects dialog', async () => {
    closeAssistantProjectsDialog();
    const thread = makeThread({ projectId: '', agentId: 'agent-1' });
    const { baseElement } = renderAgentMenu(thread);
    await fireEvent.click(item(baseElement, 'Projects…'));
    expect(getAssistantProjectsAgentId()).toBe('agent-1');
    closeAssistantProjectsDialog();
  });

  it('disables New Thread, Same Assistant and Projects… on a device without threads:autonomy', async () => {
    await pairWithScopes([...OBSERVE_SCOPES, 'threads:operate']);
    try {
      const { baseElement } = renderAgentMenu(makeThread({ agentId: 'agent-1' }));
      for (const label of ['New Thread, Same Assistant', 'Projects…']) {
        expect(item(baseElement, label).getAttribute('aria-disabled')).toBe('true');
        expect(item(baseElement, label).getAttribute('title')).toBe('Not granted to this device');
      }
      expect(item(baseElement, 'Rename Thread').getAttribute('aria-disabled')).not.toBe('true');
    } finally {
      resetToLocalPage();
    }
  });

  it("confirms an assistant's delete with confirmDelete off, naming its home, and deletes only after it", async () => {
    setBindingMock('GetSettings', async () => ({ confirmDelete: false }));
    await loadSettings();
    setBindingMock('StopSession', async () => {});
    setBindingMock('GetStandingAgent', async () => ({ id: 'agent-1', threadId: 'thread-1', home: '/config/agents/agent-1' }));
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 2, helpers: 1, workers: 0 }));
    const remove = setBindingMock('DeleteThreads', async () => {});
    const { baseElement } = renderAgentMenu(makeThread({ title: 'Ops', agentId: 'agent-1' }));
    await fireEvent.click(item(baseElement, 'Delete'));
    await tick();
    const dialog = () => baseElement.querySelector('[role="alertdialog"], [role="dialog"]');
    expect(dialog()?.textContent).toContain('Delete Assistant');
    await waitFor(() => expect(dialog()?.textContent).toContain(
      'The assistant "Ops" is deleted too, with its 2 past threads and 1 helper, and its home folder /config/agents/agent-1 is removed with everything in it.',
    ));
    expect(remove).not.toHaveBeenCalled();
    // The menu's own Delete item is a button too: the dialog's is the one
    // that is no menu item.
    const confirm = Array.from(baseElement.querySelectorAll('button:not([role="menuitem"])')).find((b) => b.textContent?.trim() === 'Delete');
    if (!confirm) throw new Error('confirm button not rendered');
    await fireEvent.click(confirm);
    await waitFor(() => expect(remove).toHaveBeenCalledWith(['thread-1']));
  });

  it("still says an assistant's threads go with it when they could not be counted", async () => {
    setBindingMock('GetStandingAgent', async () => ({ id: 'agent-1', threadId: 'thread-1', home: '/config/agents/agent-1' }));
    setBindingMock('GetDeleteDependents', async () => {
      throw new Error('database is locked');
    });
    const { baseElement } = renderAgentMenu(makeThread({ title: 'Ops', agentId: 'agent-1' }));
    await fireEvent.click(item(baseElement, 'Delete'));
    await waitFor(() => expect(baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain(
      'The assistant "Ops" is deleted too, with every thread that lives under it, and its home folder /config/agents/agent-1 is removed with everything in it.',
    ));
  });

  it("counts an assistant's workers beside its past threads and helpers", async () => {
    setBindingMock('GetStandingAgent', async () => ({ id: 'agent-1', threadId: 'thread-1', home: '/config/agents/agent-1' }));
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 2, helpers: 1, workers: 3 }));
    const { baseElement } = renderAgentMenu(makeThread({ title: 'Ops', agentId: 'agent-1' }));
    await fireEvent.click(item(baseElement, 'Delete'));
    await waitFor(() => expect(baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain(
      'The assistant "Ops" is deleted too, with its 2 past threads, 1 helper and 3 workers, and its home folder /config/agents/agent-1 is removed with everything in it.',
    ));
  });

  it("says an assistant's workers on other computers are deleted there too", async () => {
    setBindingMock('GetStandingAgent', async () => ({ id: 'agent-1', threadId: 'thread-1', home: '/config/agents/agent-1' }));
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 1, helpers: 0, workers: 0, remoteWorkers: true }));
    const { baseElement } = renderAgentMenu(makeThread({ title: 'Ops', agentId: 'agent-1' }));
    await fireEvent.click(item(baseElement, 'Delete'));
    await waitFor(() => expect(baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain(
      'The assistant "Ops" is deleted too, with its 1 past thread, and its home folder /config/agents/agent-1 is removed with everything in it. Workers it spawned on other computers are deleted there too.',
    ));
  });

  it("names every selected assistant's home in the bulk delete confirm", async () => {
    replaceAllThreads([
      makeThread({ id: 'a', projectId: 'project-1', title: 'Ops', agentId: 'agent-a' }),
      makeThread({ id: 'b', projectId: 'project-1', title: 'Plain' }),
    ]);
    setThreadSelection(['a', 'b']);
    setBindingMock('GetStandingAgent', async () => ({ id: 'agent-a', threadId: 'a', home: '/config/agents/agent-a' }));
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    const { baseElement } = renderAgentMenu(makeThread({ id: 'a', projectId: 'project-1' }));
    await fireEvent.click(item(baseElement, 'Delete (2)'));
    await waitFor(() => expect(baseElement.querySelector('[role="alertdialog"], [role="dialog"]')?.textContent).toContain(
      'The assistant "Ops" is deleted too, and its home folder /config/agents/agent-a is removed with everything in it.',
    ));
  });

  it('hides the group items in bulk when the selection holds an agent, and keeps them for a spawned thread', () => {
    replaceAllThreads([
      makeThread({ id: 'a', projectId: 'project-1' }),
      makeThread({ id: 'b', projectId: 'project-1', spawnedByThreadId: 'agent-1' }),
      makeThread({ id: 'c', projectId: 'project-1', agentId: 'agent-c' }),
    ]);
    setThreadSelection(['a', 'b']);
    const spawned = renderAgentMenu(makeThread({ id: 'a', projectId: 'project-1' }));
    expect(labelsOf(spawned.baseElement)).toContain('Move to Group');
    spawned.unmount();
    setThreadSelection(['a', 'c']);
    const { baseElement } = renderAgentMenu(makeThread({ id: 'a', projectId: 'project-1' }));
    const labels = labelsOf(baseElement);
    expect(labels).toContain('Archive (2)');
    expect(labels).not.toContain('Move to Group');
    expect(labels).not.toContain('Remove from Group');
  });

  it('Loops and Watches opens the thread with its background rail open', async () => {
    setBindingMock('GetThread', async () => makeThread({ agentId: 'agent-1' }));
    setBindingMock('GetThreadDisplayState', async () => null);
    const { baseElement, pane } = renderAgentMenu(makeThread({ agentId: 'agent-1' }));
    await fireEvent.click(item(baseElement, 'Loops and Watches'));
    // The rail opens once the thread is mounted, after the pane's switch
    // has read the thread's own rail preferences.
    await waitFor(() => expect(pane.activityRailBackgroundOpen).toBe(true));
    expect(pane.threadId).toBe('thread-1');
  });
});
