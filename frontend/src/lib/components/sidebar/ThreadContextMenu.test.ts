// Right-click menu visibility gating. Items + order match
// /Users/randy/repos/forge/apps/web/src/components/sidebar/useSidebarInteractions.ts
// (handleThreadContextMenu): Rename, Fork (when fork-able), Mark Unread,
// Copy Path, Copy Thread ID, Delete (when not a child thread).

import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadContextMenu from './ThreadContextMenu.svelte';
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
  setBindingMock('ListPayloadMetas', async () => []);
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

  it('renders the forge item set in order when fork-able and not a child', () => {
    const { baseElement } = renderMenu(makeThread());
    expect(visibleLabels(baseElement)).toEqual([
      'Open in New Pane',
      'Rename Thread',
      'Fork Thread',
      'Mark Unread',
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

  it('hides Delete for child (discussion) threads — the parent owns the lifecycle', () => {
    const { baseElement } = renderMenu(makeThread({ parentThreadId: 'parent-1' }));
    const labels = visibleLabels(baseElement);
    expect(labels).not.toContain('Delete');
    // Delete-divider is paired with Delete in the template, so it must
    // also be absent — visually a child-thread menu has no trailing rule.
    expect(baseElement.querySelectorAll('[role="separator"]').length).toBe(0);
  });

  it('does NOT include Pin/Unpin or Open Workspace in Editor (forge parity)', () => {
    const { baseElement } = renderMenu(makeThread({ pinnedAt: 1 }));
    const labels = visibleLabels(baseElement);
    expect(labels).not.toContain('Pin');
    expect(labels).not.toContain('Unpin');
    expect(labels).not.toContain('Open Workspace in Editor');
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
