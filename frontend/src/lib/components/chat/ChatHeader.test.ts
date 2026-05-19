// ChatHeader tests cover the rewrite's contract: title (view + inline
// rename), project badge, git actions, and the Diffs toggle. The old
// InteractionModeBadge + ModelPicker / RuntimeModePicker / BranchToolbar
// have been deleted; those covers are carried in the composer/below-bar
// tests now.

import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatHeader from './ChatHeader.svelte';
import {
  focusPane,
  getAllPanes,
  registerPaneForTest,
  resetPanesForTest,
} from '../../stores/panes.svelte';
import {
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../stores/paneLayout.svelte';
import { setThreadStatus } from '../../stores/threadStatuses.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import {
  addProjectLocal,
  resetProjectsForTest,
} from '../../stores/projects.svelte';
import { resetSidebarForTest } from '../../stores/sidebar.svelte';
import type { Thread } from '../../types/models';

beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
          addEventListener() {}, removeEventListener() {},
          onfinish: null, oncancel: null,
        };
      };
  }
});

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'My awesome thread',
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/proj',
    projectId: 'project-1',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread = makeThread()) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  // GitActionsControl calls GetGitStatus on mount; keep it a no-repo
  // so that component renders nothing and doesn't distract the tests.
  setBindingMock('GetGitStatus', async () => ({
    isRepo: false,
    branch: '',
    hasChanges: false,
    hasUpstream: false,
    isDefaultBranch: false,
    aheadCount: 0,
    behindCount: 0,
    openPrUrl: '',
    dirty: false,
    files: [],
  }));
  const pane = createThreadPane();
  await pane.switchThread(thread);
  getAllPanes().set('main', pane);
  return pane;
}

describe('<ChatHeader>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProjectsForTest();
    resetSidebarForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
  });

  it('shows the thread title as a button in view mode', async () => {
    const pane = await buildPane(makeThread({ title: 'Shipping design' }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const title = getByTestId('chat-header-title');
    expect(title.textContent?.trim()).toBe('Shipping design');
    expect(title.tagName.toLowerCase()).toBe('button');
  });

  it('switches to an input on title right-click and commits the rename on Enter', async () => {
    const pane = await buildPane(makeThread({ title: 'Old name' }));
    // RenameThread resolves to void; ChatHeader then re-reads via GetThread.
    const rename = setBindingMock('RenameThread', async () => undefined);
    setBindingMock('GetThread', async () => ({
      ...(pane.thread as Thread),
      title: 'New name',
    }));
    const { getByTestId, queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    await fireEvent.contextMenu(getByTestId('chat-header-title'));
    await tick();
    const input = getByTestId('chat-header-title-input') as HTMLInputElement;
    expect(input.value).toBe('Old name');

    // Clear and type a new name.
    await fireEvent.input(input, { target: { value: 'New name' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    for (let i = 0; i < 4; i += 1) await Promise.resolve();

    expect(rename.mock.calls[0]).toEqual(['thread-1', 'New name']);
    expect(queryByTestId('chat-header-title-input')).toBeNull();
    expect(pane.thread?.title).toBe('New name');
  });

  it('left-click on the title does not enter rename mode (drag handle only)', async () => {
    const pane = await buildPane(makeThread({ title: 'Stays' }));
    const { getByTestId, queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    await fireEvent.click(getByTestId('chat-header-title'));
    await tick();
    expect(queryByTestId('chat-header-title-input')).toBeNull();
  });

  it('cancels the rename when Escape is pressed without calling the binding', async () => {
    const pane = await buildPane(makeThread({ title: 'Keep me' }));
    const rename = setBindingMock('RenameThread', async () => pane.thread as Thread);
    const { getByTestId, queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    await fireEvent.contextMenu(getByTestId('chat-header-title'));
    await tick();
    const input = getByTestId('chat-header-title-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Something else' } });
    await fireEvent.keyDown(input, { key: 'Escape' });
    await tick();

    expect(queryByTestId('chat-header-title-input')).toBeNull();
    expect(rename.mock.calls).toHaveLength(0);
    // Pane still shows the original title.
    expect(pane.thread?.title).toBe('Keep me');
  });

  it('skips the RenameThread call when the draft is empty or unchanged', async () => {
    const pane = await buildPane(makeThread({ title: 'Same' }));
    const rename = setBindingMock('RenameThread', async () => pane.thread as Thread);
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    // Empty submit.
    await fireEvent.contextMenu(getByTestId('chat-header-title'));
    await tick();
    let input = getByTestId('chat-header-title-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await tick();

    // Unchanged submit.
    await fireEvent.contextMenu(getByTestId('chat-header-title'));
    await tick();
    input = getByTestId('chat-header-title-input') as HTMLInputElement;
    await fireEvent.keyDown(input, { key: 'Enter' });
    await tick();

    expect(rename.mock.calls).toHaveLength(0);
  });

  it('surfaces the error on the pane when RenameThread rejects', async () => {
    const pane = await buildPane(makeThread({ title: 'Old' }));
    setBindingMock('RenameThread', async () => {
      throw new Error('backend said no');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    await fireEvent.contextMenu(getByTestId('chat-header-title'));
    await tick();
    const input = getByTestId('chat-header-title-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Tries to rename' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(pane.generalError).toMatch(/Failed to rename thread/);
    consoleErr.mockRestore();
  });

  it('toggles the diff panel via the Diffs button', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.diffPanel.open).toBe(false);
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    expect(pane.diffPanel.open).toBe(true);
  });

  it('shows the design preview toggle and hides the diff toggle on design threads', async () => {
    const pane = await buildPane(makeThread({ mode: 'design' }));
    const { getByTestId, queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    expect(queryByTestId('diff-panel-toggle')).toBeNull();
    expect(pane.showDesignPreviewPanel).toBe(false);
    await fireEvent.click(getByTestId('design-preview-toggle'));
    expect(pane.showDesignPreviewPanel).toBe(true);
    await fireEvent.click(getByTestId('design-preview-toggle'));
    expect(pane.showDesignPreviewPanel).toBe(false);
  });

  it('toggles the terminal drawer via the terminal button', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.showTerminal).toBe(false);
    await fireEvent.click(getByTestId('terminal-toggle'));
    expect(pane.showTerminal).toBe(true);
    await fireEvent.click(getByTestId('terminal-toggle'));
    expect(pane.showTerminal).toBe(false);
  });

  it('uses platform-aware shortcut labels in action tooltips', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(getByTestId('terminal-toggle').getAttribute('title')).toBe('Toggle Terminal (Ctrl+J)');
    expect(getByTestId('diff-panel-toggle').getAttribute('title')).toBe('Toggle Diff Panel (Ctrl+Shift+G)');
  });

  it('opens the project root in the editor via the Open button', async () => {
    const now = Date.now();
    addProjectLocal({
      id: 'project-1',
      path: '/tmp/proj',
      name: 'Alpha',
      sortPosition: 0,
      createdAt: now,
      updatedAt: now,
      archived: false,
    });
    const open = setBindingMock('OpenInEditor', async () => undefined);
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    await fireEvent.click(getByTestId('chat-header-open-editor'));
    await tick();
    expect(open.mock.calls[0]).toEqual(['/tmp/proj', 0, 0, '']);
  });

  it('hides the Open button when the thread has no project', async () => {
    const pane = await buildPane(makeThread({ projectId: undefined }));
    const { queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('chat-header-open-editor')).toBeNull();
  });

  it('renders the attention dot when the thread reports live status', async () => {
    const pane = await buildPane(makeThread({ id: 'attention-thread', title: 'Running' }));
    setThreadStatus('attention-thread', 'running');
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const dot = getByTestId('pane-attention-dot');
    expect(dot).toHaveAttribute('data-status', 'running');
    expect(dot.getAttribute('class')).toContain('bg-success');
  });

  it('omits the dot when the thread has no attention status', async () => {
    // makeThread leaves lastReadAt / latestTurnCompletedAt undefined →
    // hasUnread is false → no pill → no dot.
    const pane = await buildPane(makeThread({ id: 'idle-thread' }));
    const { queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('pane-attention-dot')).toBeNull();
  });

  it('applies the attention glow class to the title', async () => {
    const pane = await buildPane(makeThread({ id: 'pending-thread', title: 'Pending' }));
    setThreadStatus('pending-thread', 'pending-approval');
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(getByTestId('chat-header-title').className).toContain('status-glow-warning');
  });

  it('renders a draggable title that fires onPaneDragStart on dragstart', async () => {
    const pane = await buildPane();
    const onPaneDragStart = vi.fn();
    const { getByTestId } = render(ChatHeader, { props: { pane, onPaneDragStart } });
    await tick();
    const title = getByTestId('chat-header-title');
    expect(title.getAttribute('draggable')).toBe('true');
    await fireEvent.dragStart(title);
    expect(onPaneDragStart).toHaveBeenCalledTimes(1);
  });

  it('leaves the title non-draggable when no drag handler is provided', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const title = getByTestId('chat-header-title');
    expect(title.getAttribute('draggable')).toBe('false');
  });

  it('renders a close button that destroys the pane', async () => {
    // Explicit paneId so destroyPane(pane.paneId) matches the registry key.
    const pane = createThreadPane({ paneId: 'to-close' });
    await pane.switchThread(makeThread({ id: 'close-thread', title: 'Closes' }));
    registerPaneForTest('to-close', pane);
    setPaneLayoutItemsForTest([
      { id: 'main-item', paneId: 'main', kind: 'thread', ratio: 1 },
      { id: 'close-item', paneId: 'to-close', kind: 'thread', ratio: 1 },
    ]);
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(getAllPanes().has('to-close')).toBe(true);
    await fireEvent.click(getByTestId('pane-close'));
    expect(getAllPanes().has('to-close')).toBe(false);
  });

  it('accent-tints and rings the title when the pane is focused', async () => {
    const focused = createThreadPane({ paneId: 'main' });
    await focused.switchThread(makeThread({ id: 'focused-thread', title: 'Focused' }));
    const other = createThreadPane({ paneId: 'other' });
    other.replaceThread(makeThread({ id: 'other-thread', title: 'Other' }));
    registerPaneForTest('main', focused);
    registerPaneForTest('other', other);
    setPaneLayoutItemsForTest([
      { id: 'main-item', paneId: 'main', kind: 'thread', ratio: 1 },
      { id: 'other-item', paneId: 'other', kind: 'thread', ratio: 1 },
    ]);
    focusPane('main');

    const { getByTestId } = render(ChatHeader, { props: { pane: focused } });
    await tick();
    const title = getByTestId('chat-header-title');
    expect(title).toHaveAttribute('data-focused', 'true');
    expect(title.className).toContain('bg-accent/15');
    expect(title.className).toContain('ring-accent/40');
  });

  it('shows the focus highlight even in single-pane mode so the input target is unambiguous', async () => {
    const pane = createThreadPane({ paneId: 'main' });
    await pane.switchThread(makeThread({ id: 'solo-thread', title: 'Solo' }));
    registerPaneForTest('main', pane);
    setPaneLayoutItemsForTest([
      { id: 'main-item', paneId: 'main', kind: 'thread', ratio: 1 },
    ]);
    focusPane('main');
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const title = getByTestId('chat-header-title');
    expect(title).toHaveAttribute('data-focused', 'true');
    expect(title.className).toContain('bg-accent/15');
  });

  it('drops the focus highlight from an unfocused pane', async () => {
    const focused = createThreadPane({ paneId: 'main' });
    await focused.switchThread(makeThread({ id: 'main-thread', title: 'Main' }));
    const other = createThreadPane({ paneId: 'other' });
    other.replaceThread(makeThread({ id: 'other-thread', title: 'Other' }));
    registerPaneForTest('main', focused);
    registerPaneForTest('other', other);
    setPaneLayoutItemsForTest([
      { id: 'main-item', paneId: 'main', kind: 'thread', ratio: 1 },
      { id: 'other-item', paneId: 'other', kind: 'thread', ratio: 1 },
    ]);
    focusPane('main');

    const { getByTestId } = render(ChatHeader, { props: { pane: other } });
    await tick();
    const title = getByTestId('chat-header-title');
    expect(title).toHaveAttribute('data-focused', 'false');
    expect(title.className).not.toContain('bg-accent/15');
  });
});
