// Right-click menu visibility gating. Items + order match
// /Users/randy/repos/forge/apps/web/src/components/sidebar/useSidebarInteractions.ts
// (handleThreadContextMenu): Rename, Fork (when fork-able), Mark Unread,
// Copy Path, Copy Thread ID, Delete (when not a child thread).

import { describe, expect, it, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import ThreadContextMenu from './ThreadContextMenu.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { clearThreadSelection } from '../../stores/threadFilter.svelte';
import type { Thread } from '../../types/models';

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
