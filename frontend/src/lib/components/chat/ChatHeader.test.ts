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
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import {
  resetKeybindingsStore,
  setKeybindingsForTest,
} from '../../stores/keybindings.svelte';
import { projectTurnStarted, setThreadStatus } from '../../stores/threadStatuses.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import {
  addProjectLocal,
  resetProjectsForTest,
} from '../../stores/projects.svelte';
import { resetSidebarForTest } from '../../stores/sidebar.svelte';
import { resetEditorsForTest } from '../../stores/editors.svelte';
import { openTerminalThread } from '../../stores/threadCreation.svelte';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import {
  applyThreadTitleGeneration,
  resetThreadTitleGenerationForTest,
} from '../../stores/threadTitleGeneration.svelte';
import type { Project, Thread } from '../../types/models';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';

// The terminal button's ctrl/cmd-click opens a fresh terminal pane via
// openTerminalThread; stub it so the gesture can be asserted without standing
// up the real StartTerminal binding + pane registry. ChatHeader is the only
// threadCreation consumer in this test's module graph, so a minimal factory is
// safe (the draft-helper exports stay unused here).
vi.mock('../../stores/threadCreation.svelte', () => ({ openTerminalThread: vi.fn() }));

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
  return makeBaseThread({
    title: 'My awesome thread',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/proj',
    projectId: 'project-1',
    ...overrides,
  });
}

async function buildPane(thread: Thread = makeThread()) {
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
  return buildRegisteredPane(thread);
}

describe('<ChatHeader>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProjectsForTest();
    resetSidebarForTest();
    resetPanesForTest();
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetEditorsForTest();
    resetThreadTitleGenerationForTest();
    // The header's Open-in-editor control loads this catalog on mount.
    // An empty catalog keeps the primary button working (the backend
    // still resolves the default) while rendering no dropdown, which is
    // all these header tests care about.
    setBindingMock('ListAvailableEditors', async () => []);
    setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
    vi.mocked(openTerminalThread).mockClear();
  });

  it('keeps its bottom border above the timeline fade overdraw', async () => {
    const pane = await buildPane(makeThread({ title: 'Layered header' }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    const header = getByTestId('chat-header');
    expect(header.classList).toContain('relative');
    expect(header.classList).toContain('z-10');
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

  it('toggles the review pane via the Diffs button', async () => {
    const pane = await buildPane();
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 1 }]);
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.showReviewPane).toBe(false);
    await fireEvent.click(getByTestId('review-toggle'));
    expect(pane.showReviewPane).toBe(true);
  });

  it('toggles the terminal drawer and latches focus-on-open via the terminal button', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.showTerminal).toBe(false);
    await fireEvent.click(getByTestId('terminal-toggle'));
    expect(pane.showTerminal).toBe(true);
    // The button must open AND focus, identical to the ⌘`/⌘J chord: it routes
    // through runTerminalToggle, which latches the focus intent the drawer
    // consumes on mount. consumeTerminalFocusRequest is read-and-clear, so the
    // open click must have set it exactly once.
    expect(pane.consumeTerminalFocusRequest()).toBe(true);
    await fireEvent.click(getByTestId('terminal-toggle'));
    expect(pane.showTerminal).toBe(false);
    // Plain clicks must never spawn a new pane — that path is modifier-gated.
    expect(vi.mocked(openTerminalThread)).not.toHaveBeenCalled();
  });

  it('ctrl-click opens a fresh terminal pane rooted at the thread workspace and skips the drawer toggle', async () => {
    const pane = await buildPane(makeThread({ projectId: 'proj-7', workspacePath: '/tmp/ws7' }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.showTerminal).toBe(false);

    await fireEvent.click(getByTestId('terminal-toggle'), { ctrlKey: true });

    expect(vi.mocked(openTerminalThread)).toHaveBeenCalledWith({
      projectId: 'proj-7',
      cwd: '/tmp/ws7',
    });
    // The drawer toggle is bypassed — ctrl-click is "new pane", not "show here".
    expect(pane.showTerminal).toBe(false);
  });

  it('cmd-click (macOS) also opens a fresh terminal pane', async () => {
    const pane = await buildPane(makeThread({ projectId: 'proj-7', workspacePath: '/tmp/ws7' }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    await fireEvent.click(getByTestId('terminal-toggle'), { metaKey: true });

    expect(vi.mocked(openTerminalThread)).toHaveBeenCalledWith({
      projectId: 'proj-7',
      cwd: '/tmp/ws7',
    });
    expect(pane.showTerminal).toBe(false);
  });

  it('uses platform-aware shortcut labels in action tooltips', async () => {
    // Chord hints come from the loaded keybindings store — there is no
    // hardcoded fallback (one would resurrect a chord the user rebound
    // or cleared), so the test seeds the store like the runtime load
    // does.
    setKeybindingsForTest([
      { key: 'ctrl+`', command: 'terminal.toggle' },
      { key: 'ctrl+shift+g', command: 'diff.panel.toggle' },
    ]);
    try {
      const pane = await buildPane();
      const { getByTestId } = render(ChatHeader, { props: { pane } });
      await tick();
      expect(getByTestId('terminal-toggle').getAttribute('title')).toBe('Toggle Terminal (Ctrl+`)');
      expect(getByTestId('review-toggle').getAttribute('title')).toBe('Toggle Review Pane (Ctrl+Shift+G)');
    } finally {
      resetKeybindingsStore();
    }
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
    expect(open.mock.calls[0]).toEqual(['/tmp/proj', 0, 0, '', '']);
  });

  it('hides the Open button when the thread has no project', async () => {
    const pane = await buildPane(makeThread({ projectId: undefined }));
    const { queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('chat-header-open-editor')).toBeNull();
  });

  it('renders the attention dot when the thread reports live status', async () => {
    const pane = await buildPane(makeThread({ id: 'attention-thread', title: 'Running' }));
    projectTurnStarted('attention-thread', 'turn:attention-thread', 0, 0);
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
      { id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 },
      { id: 'close-item', paneId: 'to-close', kind: 'thread', widthPx: 1 },
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
      { id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 },
      { id: 'other-item', paneId: 'other', kind: 'thread', widthPx: 1 },
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
      { id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 },
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
      { id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 },
      { id: 'other-item', paneId: 'other', kind: 'thread', widthPx: 1 },
    ]);
    focusPane('main');

    const { getByTestId } = render(ChatHeader, { props: { pane: other } });
    await tick();
    const title = getByTestId('chat-header-title');
    expect(title).toHaveAttribute('data-focused', 'false');
    expect(title.className).not.toContain('bg-accent/15');
  });

  // --- placeholder (draft) thread coverage ---
  //
  // A pane in "draft placeholder" state has `pane.thread` (synthetic) but
  // `pane.threadId === null`. The header used to gate the entire right
  // cluster on `pane.threadId`, so users saw a bare title + close button
  // with no Open/Terminal/Diff buttons — and the cluster only popped in
  // after a metadata change materialized the row. The outer `pane.thread`
  // gate is still present, but the right cluster must render on
  // placeholders too; only panels that truly need a persisted row refuse
  // until content materializes the placeholder.

  function placeholderPane(paneId = 'placeholder-header') {
    const project: Project = {
      id: 'project-placeholder-header',
      path: '/tmp/placeholder-header',
      name: 'Placeholder Project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    const pane = createThreadPane({ paneId });
    pane.startDraftPlaceholder(project, 'chat');
    return { pane, project };
  }

  it('renders the right cluster (Open + Terminal + Diff) on a placeholder thread', async () => {
    addProjectLocal({
      id: 'project-placeholder-header',
      path: '/tmp/placeholder-header',
      name: 'Placeholder Project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    });
    const { pane } = placeholderPane();
    expect(pane.threadId).toBeNull();
    expect(pane.thread?.isDraft).toBe(true);

    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    // Outer gate still passes (pane.thread is set), title renders.
    expect(getByTestId('chat-header-title').textContent?.trim()).toBe('New Thread');
    // Right cluster: each button must be present even though the row
    // hasn't materialized yet. Thread-bound actions refuse to open
    // until real content creates the row.
    expect(getByTestId('chat-header-open-editor')).toBeTruthy();
    expect(getByTestId('terminal-toggle')).toBeTruthy();
    expect(getByTestId('review-toggle')).toBeTruthy();
  });

  it('terminal-toggle click on a placeholder opens without creating a thread', async () => {
    const { pane } = placeholderPane('placeholder-term-click');
    const create = setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not be called for terminal-toggle on a placeholder');
    });

    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.threadId).toBeNull();
    expect(pane.showTerminal).toBe(false);

    await fireEvent.click(getByTestId('terminal-toggle'));

    await tick();

    expect(create).not.toHaveBeenCalled();
    expect(pane.threadId).toBeNull();
    expect(pane.showTerminal).toBe(true);
  });

  it('acks the regeneration RPC and spins until the completion event clears it', async () => {
    const pane = await buildPane(makeThread({ title: 'New Thread' }));
    const regenerate = setBindingMock('RegenerateThreadTitle', async () => undefined);

    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    const button = getByTestId('thread-title-regenerate') as HTMLButtonElement;
    await fireEvent.click(button);

    // Pending is set before the ack resolves and held after it: the run keeps
    // going server-side, and only the completion event may clear the flag.
    expect(button.disabled).toBe(true);
    expect(button.dataset.pending).toBe('true');
    await vi.waitFor(() => expect(regenerate).toHaveBeenCalledWith('thread-1'));
    expect(button.dataset.pending).toBe('true');

    applyThreadTitleGeneration({ threadId: 'thread-1', error: '' });
    await vi.waitFor(() => expect(button.disabled).toBe(false));
    expect(button.dataset.pending).toBe('false');
    expect(pane.generalError).toBeNull();
  });

  it('ignores a second click while a regeneration is pending', async () => {
    const pane = await buildPane(makeThread({ title: 'New Thread' }));
    const regenerate = setBindingMock('RegenerateThreadTitle', async () => undefined);

    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    const button = getByTestId('thread-title-regenerate') as HTMLButtonElement;
    await fireEvent.click(button);
    // The button disables on pending, but the store's own guard must hold
    // even if a stale handler fires (disabled rendering races the click).
    await fireEvent.click(button);
    await vi.waitFor(() => expect(regenerate).toHaveBeenCalledTimes(1));
  });

  it('surfaces the error on the pane when the regeneration ack rejects', async () => {
    const pane = await buildPane();
    setBindingMock('RegenerateThreadTitle', async () => {
      throw new Error('provider exploded');
    });

    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    const button = getByTestId('thread-title-regenerate') as HTMLButtonElement;
    await fireEvent.click(button);

    await vi.waitFor(() => {
      expect(pane.generalError).toContain('Failed to regenerate title');
    });
    // A rejected ack means no run started, so pending must release.
    expect(button.disabled).toBe(false);
  });

  it('surfaces a failed run on the pane when its completion event carries an error', async () => {
    const pane = await buildPane();
    setBindingMock('RegenerateThreadTitle', async () => undefined);

    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    await fireEvent.click(getByTestId('thread-title-regenerate'));
    await vi.waitFor(() => {
      expect((getByTestId('thread-title-regenerate') as HTMLButtonElement).disabled).toBe(true);
    });

    applyThreadTitleGeneration({ threadId: 'thread-1', error: 'provider CLI failed' });
    await vi.waitFor(() => {
      expect(pane.generalError).toBe('Failed to regenerate title: provider CLI failed');
    });
    expect((getByTestId('thread-title-regenerate') as HTMLButtonElement).disabled).toBe(false);
  });

  it('disables the regenerate button in a view-only session (ungranted RPC)', async () => {
    setPageGrantsFromBootstrap(true);
    try {
      const pane = await buildPane();
      const { getByTestId } = render(ChatHeader, { props: { pane } });
      await tick();

      const button = getByTestId('thread-title-regenerate') as HTMLButtonElement;
      expect(button.disabled).toBe(true);
      expect(button.title).toBe('Local only');
    } finally {
      setPageGrantsFromBootstrap(false);
    }
  });
});
