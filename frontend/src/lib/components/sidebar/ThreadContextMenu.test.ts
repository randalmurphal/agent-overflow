// Right-click menu visibility gating. Items + order are pinned:
// Rename, Fork (when fork-able), Mark Unread, pin controls, Copy Path,
// Copy Thread ID, Delete (when not a child thread).

import { afterEach, describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadContextMenu from './ThreadContextMenu.svelte';
import { setViewOnlySessionFromBootstrap } from '../../transport/runMode';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { clearThreadSelection, setThreadSelection } from '../../stores/threadFilter.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { Thread } from '../../types/models';
import type { Settings } from '../../types/settings';

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
      'Fork Thread',
      'Mark Unread',
      'Pin Thread',
      'Copy Path',
      'Copy Thread ID',
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

  it('hides Delete for child (discussion) threads — the parent owns the lifecycle', () => {
    const { baseElement } = renderMenu(makeThread({ parentThreadId: 'parent-1' }));
    const labels = visibleLabels(baseElement);
    expect(labels).not.toContain('Delete');
    // Delete-divider is paired with Delete in the template, so it must
    // also be absent — visually a child-thread menu has no trailing rule.
    expect(baseElement.querySelectorAll('[role="separator"]').length).toBe(0);
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
});

describe('<ThreadContextMenu> Check for Provider Updates', () => {
  // Gated on the write-once import stamp, never on sessionRef: every thread
  // that has run a turn has a sessionRef, so that gate would offer the item
  // on threads with no source file to check.
  beforeEach(() => {
    resetBindingMocks();
    clearThreadSelection();
    setViewOnlySessionFromBootstrap(false);
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
  });

  afterEach(() => {
    setViewOnlySessionFromBootstrap(false);
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

  it('is disabled with a Local only tooltip in a view-only session', async () => {
    setViewOnlySessionFromBootstrap(true);
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
    expect(item.getAttribute('title')).toBe('Local only');

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
