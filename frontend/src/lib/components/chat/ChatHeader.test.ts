// ChatHeader tests cover the header's own contract: title (view + inline
// rename), the project crumb, the pane status line, the cluster on a draft
// placeholder, and the spinner a title regeneration shows. The Thread
// actions menu's rows are covered in ChatHeaderMenu.test.ts.

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
import {
  applyThreadTitleGeneration,
  resetThreadTitleGenerationForTest,
} from '../../stores/threadTitleGeneration.svelte';
import type { Project, Thread } from '../../types/models';
import { setCompactLayoutForTest, getCompactScreen, showCompactThread } from '../../stores/layoutMode.svelte';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';
import { idleWorkspaceActivity } from '../../../test/helpers/workspaceLock';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { noteThread } from '../../transport/entityIndex';

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
    // The header's actions menu loads the editor catalog on mount.
    setBindingMock('ListAvailableEditors', async () => []);
    setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
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

  it('uses platform-aware shortcut labels in action tooltips', async () => {
    // Chord hints come from the loaded keybindings store — there is no
    // hardcoded fallback (one would resurrect a chord the user rebound
    // or cleared), so the test seeds the store like the runtime load
    // does.
    setKeybindingsForTest([{ key: 'ctrl+shift+g', command: 'diff.panel.toggle' }]);
    try {
      const pane = await buildPane();
      const { getByTestId } = render(ChatHeader, { props: { pane } });
      await tick();
      expect(getByTestId('review-toggle').getAttribute('title')).toBe('Toggle Review Pane (Ctrl+Shift+G)');
    } finally {
      resetKeybindingsStore();
    }
  });

  it('leads with the project crumb before the title, a label once the thread has messages', async () => {
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
    const pane = await buildPane(makeThread({ isDraft: false }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const crumb = getByTestId('chat-header-project') as HTMLButtonElement;
    expect(crumb).toHaveTextContent('Alpha');
    expect(crumb.disabled).toBe(true);
    expect(crumb.dataset.locked).toBe('true');
    // Crumb, then the title: `project / title`.
    const title = getByTestId('chat-header-title');
    expect(crumb.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('lets a draft change project from the crumb', async () => {
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
    const pane = await buildPane(makeThread({ isDraft: true }));
    const { getByTestId, findByRole } = render(ChatHeader, { props: { pane } });
    await tick();
    const crumb = getByTestId('chat-header-project') as HTMLButtonElement;
    expect(crumb.disabled).toBe(false);
    await fireEvent.click(crumb);
    expect(await findByRole('menu', { name: 'Project' })).toBeInTheDocument();
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

  // The header's status line (PaneHeaderLine): accent on the focused pane,
  // the thread's attention color on an unfocused one, shown only when there
  // is more than one pane to tell apart and never in compact layout.
  async function renderTwoPaneHeader(): Promise<{ focused: ReturnType<typeof createThreadPane>; other: ReturnType<typeof createThreadPane> }> {
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
    return { focused, other };
  }

  it('lays the accent line over the separator of the focused pane', async () => {
    const { focused } = await renderTwoPaneHeader();
    const { getByTestId } = render(ChatHeader, { props: { pane: focused } });
    await tick();
    const header = getByTestId('chat-header');
    const line = getByTestId('pane-header-line');
    expect(header).toContainElement(line);
    expect(line).toHaveAttribute('data-line', 'accent');
    expect(line.className).toContain('accent-hairline');
    expect(line.className).toContain('-bottom-px');
    expect(getByTestId('pane-header-line-label')).toHaveTextContent('Focused pane');
    // The title itself carries no focus styling any more.
    const title = getByTestId('chat-header-title');
    expect(title).not.toHaveAttribute('data-focused');
    expect(title.className).not.toContain('bg-accent/15');
  });

  it('omits the line from an unfocused idle pane', async () => {
    const { other } = await renderTwoPaneHeader();
    const { queryByTestId } = render(ChatHeader, { props: { pane: other } });
    await tick();
    expect(queryByTestId('pane-header-line')).toBeNull();
  });

  it('follows focus as it moves between panes', async () => {
    const { other } = await renderTwoPaneHeader();
    const { queryByTestId } = render(ChatHeader, { props: { pane: other } });
    await tick();
    expect(queryByTestId('pane-header-line')).toBeNull();
    focusPane('other');
    await tick();
    expect(queryByTestId('pane-header-line')).not.toBeNull();
    focusPane('main');
    await tick();
    expect(queryByTestId('pane-header-line')).toBeNull();
  });

  it('omits the line when the strip holds a single pane', async () => {
    const pane = createThreadPane({ paneId: 'main' });
    await pane.switchThread(makeThread({ id: 'solo-thread', title: 'Solo' }));
    registerPaneForTest('main', pane);
    setPaneLayoutItemsForTest([
      { id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 },
    ]);
    focusPane('main');
    const { queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('pane-header-line')).toBeNull();
    // A second pane appearing makes the mark meaningful again.
    setPaneLayoutItemsForTest([
      { id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 },
      { id: 'other-item', paneId: 'other', kind: 'thread', widthPx: 1 },
    ]);
    await tick();
    expect(queryByTestId('pane-header-line')).not.toBeNull();
  });

  it('colors an unfocused pane by its attention state, and never by a running turn', async () => {
    const { other } = await renderTwoPaneHeader();
    const { queryByTestId, getByTestId } = render(ChatHeader, { props: { pane: other } });
    await tick();
    expect(queryByTestId('pane-header-line')).toBeNull();

    setThreadStatus('other-thread', 'pending-approval');
    await tick();
    expect(getByTestId('pane-header-line')).toHaveAttribute('data-line', 'warning');
    expect(getByTestId('pane-header-line-label')).toHaveTextContent('Pending Approval');

    setThreadStatus('other-thread', 'awaiting-input');
    await tick();
    expect(getByTestId('pane-header-line')).toHaveAttribute('data-line', 'info');

    setThreadStatus('other-thread', 'error');
    await tick();
    expect(getByTestId('pane-header-line')).toHaveAttribute('data-line', 'error');

    setThreadStatus('other-thread', 'idle');
    other.replaceThread({ ...other.thread!, lastReadAt: 1, latestTurnCompletedAt: 2 });
    await tick();
    expect(getByTestId('pane-header-line')).toHaveAttribute('data-line', 'success');
    expect(getByTestId('pane-header-line-label')).toHaveTextContent('Completed');

    // A background turn is the composer rail's job, not the header's: a
    // running turn outranks the unread completion and draws nothing.
    projectTurnStarted('other-thread', 'turn:other-thread', 0, 0);
    await tick();
    expect(queryByTestId('pane-header-line')).toBeNull();
  });

  it('keeps the focused pane accent whatever its thread is doing', async () => {
    const { focused } = await renderTwoPaneHeader();
    setThreadStatus('focused-thread', 'pending-approval');
    const { getByTestId } = render(ChatHeader, { props: { pane: focused } });
    await tick();
    expect(getByTestId('pane-header-line')).toHaveAttribute('data-line', 'accent');
  });

  it('shows no attention line on a lone pane', async () => {
    const pane = createThreadPane({ paneId: 'main' });
    await pane.switchThread(makeThread({ id: 'lone-thread', title: 'Lone' }));
    registerPaneForTest('main', pane);
    setPaneLayoutItemsForTest([{ id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 }]);
    setThreadStatus('lone-thread', 'pending-approval');
    const { queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('pane-header-line')).toBeNull();
  });

  it('omits the line in compact layout even with several panes', async () => {
    const { focused } = await renderTwoPaneHeader();
    setCompactLayoutForTest(true);
    try {
      const { queryByTestId } = render(ChatHeader, { props: { pane: focused } });
      await tick();
      expect(queryByTestId('pane-header-line')).toBeNull();
    } finally {
      setCompactLayoutForTest(false);
    }
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

  it('renders the right cluster (diff badge + actions menu) on a placeholder thread', async () => {
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

    const { getByTestId, findByRole } = render(ChatHeader, { props: { pane } });
    await tick();

    // Outer gate still passes (pane.thread is set), title renders.
    expect(getByTestId('chat-header-title').textContent?.trim()).toBe('New Thread');
    // Right cluster: each control must be present even though the row
    // hasn't materialized yet. Thread-bound actions refuse to open
    // until real content creates the row.
    expect(getByTestId('review-toggle')).toBeTruthy();
    await fireEvent.click(getByTestId('chat-header-more'));
    expect(await findByRole('menuitemcheckbox', { name: 'Terminal' })).toBeInTheDocument();
    expect(await findByRole('menuitem', { name: 'Open in editor' })).toBeInTheDocument();
  });

  it('the Terminal row on a placeholder opens the drawer without creating a thread', async () => {
    const { pane } = placeholderPane('placeholder-term-click');
    // The shared teardown closes the placeholder, which closes the terminal it opened.
    setBindingMock('CloseThreadTerminals', async () => undefined);
    const create = setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not be called for the Terminal row on a placeholder');
    });

    const { getByTestId, findByRole } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.threadId).toBeNull();
    expect(pane.showTerminal).toBe(false);

    await fireEvent.click(getByTestId('chat-header-more'));
    await fireEvent.click(await findByRole('menuitemcheckbox', { name: 'Terminal' }));

    await tick();

    expect(create).not.toHaveBeenCalled();
    expect(pane.threadId).toBeNull();
    expect(pane.showTerminal).toBe(true);
  });

  // Regeneration is a row of the actions menu, which closes on the pick:
  // the header itself has to say a run is in flight.
  async function regenerateFromMenu(view: {
    getByTestId: (id: string) => HTMLElement;
    findByRole: (role: string, options: { name: string }) => Promise<HTMLElement>;
  }): Promise<void> {
    await fireEvent.click(view.getByTestId('chat-header-more'));
    await fireEvent.click(await view.findByRole('menuitem', { name: 'Regenerate title' }));
  }

  it('shows a spinner beside the title from the pick until the completion event', async () => {
    const pane = await buildPane(makeThread({ title: 'New Thread' }));
    const regenerate = setBindingMock('RegenerateThreadTitle', async () => undefined);

    const view = render(ChatHeader, { props: { pane } });
    await tick();
    expect(view.queryByTestId('thread-title-regenerating')).toBeNull();

    await regenerateFromMenu(view);

    // Pending is set before the ack resolves and held after it: the run keeps
    // going server-side, and only the completion event may clear the flag.
    const spinner = view.getByTestId('thread-title-regenerating');
    expect(spinner).toHaveAttribute('role', 'status');
    expect(spinner).toHaveAccessibleName('Regenerating title');
    const title = view.getByTestId('chat-header-title');
    expect(title.compareDocumentPosition(spinner) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await vi.waitFor(() => expect(regenerate).toHaveBeenCalledWith('thread-1'));
    expect(view.getByTestId('thread-title-regenerating')).toBeInTheDocument();

    applyThreadTitleGeneration({ threadId: 'thread-1', error: '' });
    await vi.waitFor(() => expect(view.queryByTestId('thread-title-regenerating')).toBeNull());
    expect(pane.generalError).toBeNull();
  });

  it('surfaces the error on the pane and drops the spinner when the regeneration ack rejects', async () => {
    const pane = await buildPane();
    setBindingMock('RegenerateThreadTitle', async () => {
      throw new Error('provider exploded');
    });

    const view = render(ChatHeader, { props: { pane } });
    await tick();
    await regenerateFromMenu(view);

    await vi.waitFor(() => {
      expect(pane.generalError).toContain('Failed to regenerate title');
    });
    // A rejected ack means no run started, so pending must release.
    await vi.waitFor(() => expect(view.queryByTestId('thread-title-regenerating')).toBeNull());
  });

  it('surfaces a failed run on the pane when its completion event carries an error', async () => {
    const pane = await buildPane();
    setBindingMock('RegenerateThreadTitle', async () => undefined);

    const view = render(ChatHeader, { props: { pane } });
    await tick();
    await regenerateFromMenu(view);
    await vi.waitFor(() => expect(view.getByTestId('thread-title-regenerating')).toBeInTheDocument());

    applyThreadTitleGeneration({ threadId: 'thread-1', error: 'provider CLI failed' });
    await vi.waitFor(() => {
      expect(pane.generalError).toBe('Failed to regenerate title: provider CLI failed');
    });
    expect(view.queryByTestId('thread-title-regenerating')).toBeNull();
  });
});

// Compact layout: the header leads with the way back to the list and
// drops the pane close control (closing the only pane would leave the
// thread screen empty with no list under it).
describe('<ChatHeader> under compact', () => {
  beforeEach(() => {
    setCompactLayoutForTest(true);
  });

  it('shows a back button that returns to the list, and no close button', async () => {
    const pane = createThreadPane({ paneId: 'main' });
    await pane.switchThread(makeThread({ id: 'compact-thread', title: 'Compact' }));
    registerPaneForTest('main', pane);
    setPaneLayoutItemsForTest([{ id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 }]);
    showCompactThread();
    const { getByTestId, queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('pane-close')).toBeNull();
    await fireEvent.click(getByTestId('compact-back'));
    expect(getCompactScreen()).toBe('list');
    setCompactLayoutForTest(false);
  });

  it('regenerates the title from the actions menu and shows the spinner on the title row', async () => {
    const pane = await buildPane(makeThread({ title: 'New Thread' }));
    const regenerate = setBindingMock('RegenerateThreadTitle', async () => undefined);
    const { getByTestId, findByRole } = render(ChatHeader, { props: { pane } });
    await tick();
    await fireEvent.click(getByTestId('chat-header-more'));
    const row = await findByRole('menuitem', { name: /Regenerate title/ });
    await fireEvent.click(row);
    await vi.waitFor(() => expect(regenerate).toHaveBeenCalledWith('thread-1'));
    expect(getByTestId('chat-header-title-row')).toContainElement(getByTestId('thread-title-regenerating'));
    setCompactLayoutForTest(false);
  });

  it('shows the facts line: project, branch and a pinned worktree icon, each opening its picker', async () => {
    setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
    setBindingMock('GitListBranches', async () => []);
    setBindingMock('GitListWorktrees', async () => []);
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
    const pane = await buildPane(makeThread({ branch: 'feature/a-very-long-branch-name', isDraft: false, workspacePath: '/tmp/proj' }));
    const { getByTestId, queryByTestId, findByRole } = render(ChatHeader, { props: { pane } });
    await tick();
    const facts = getByTestId('chat-header-facts');
    // Its own full-width row under the title row, not squeezed beside it.
    expect(getByTestId('chat-header-title-row').contains(facts)).toBe(false);
    expect(getByTestId('chat-header-title-row').compareDocumentPosition(facts) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // Single backend: no machine segment.
    expect(queryByTestId('chat-header-machine')).toBeNull();
    expect(getByTestId('chat-header-project')).toHaveTextContent('Alpha');
    const branch = getByTestId('chat-header-branch');
    expect(branch).toHaveTextContent('feature/a-very-long-branch-name');
    // The branch is the one segment that ellipsizes; the worktree icon never shrinks.
    expect(branch.classList.contains('min-w-0')).toBe(true);
    expect(branch.querySelector('.truncate')).not.toBeNull();
    const worktree = getByTestId('chat-header-worktree');
    expect(worktree.classList.contains('shrink-0')).toBe(true);
    expect(worktree.getAttribute('aria-label')).toBe('Worktree: Base');
    expect(worktree.dataset.atBase).toBe('true');
    // No strip controls anywhere in the header.
    expect(queryByTestId('branch-picker-trigger')).toBeNull();
    expect(queryByTestId('env-picker-trigger')).toBeNull();

    await fireEvent.click(branch);
    expect(await findByRole('menu', { name: 'Branches' })).toBeInTheDocument();
    await fireEvent.click(getByTestId('chat-header-title'));
    await fireEvent.click(worktree);
    expect(await findByRole('menu', { name: 'Workspace' })).toBeInTheDocument();
    setCompactLayoutForTest(false);
  });

  it('shows no branch or worktree for a thread outside every project, and no empty facts row', async () => {
    const pane = await buildPane(makeThread({ projectId: '', projectPath: '', workspacePath: '/config/agents/ops', agentId: 'ops' }));
    const { queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('chat-header-facts')).toBeNull();
    expect(queryByTestId('chat-header-branch')).toBeNull();
    expect(queryByTestId('chat-header-worktree')).toBeNull();
    setCompactLayoutForTest(false);
  });

  it('marks a linked worktree with the accent git folder', async () => {
    setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
    setBindingMock('GitListBranches', async () => []);
    const pane = await buildPane(makeThread({ workspacePath: '/tmp/proj/.worktrees/wt-1', projectPath: '/tmp/proj' }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const worktree = getByTestId('chat-header-worktree');
    expect(worktree.dataset.atBase).toBeUndefined();
    expect(worktree.getAttribute('aria-label')).toBe('Worktree: wt-1');
    expect(worktree.classList.contains('text-accent')).toBe(true);
    // The name shows beside the icon and is the segment that gives way
    // first: it may shrink, while the base-workspace icon never does.
    expect(getByTestId('chat-header-worktree-label')).toHaveTextContent('wt-1');
    expect(worktree.classList.contains('min-w-0')).toBe(true);
    expect(worktree.classList.contains('shrink-0')).toBe(false);
    setCompactLayoutForTest(false);
  });

  it('leads the facts line with the machine once a second backend is attached', async () => {
    setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
    setBindingMock('GitListBranches', async () => []);
    stageBackend();
    noteThread('thread-1', '');
    try {
      const pane = await buildPane(makeThread());
      const { getByTestId } = render(ChatHeader, { props: { pane } });
      await tick();
      const segments = Array.from(
        getByTestId('chat-header-facts').querySelectorAll<HTMLElement>(':scope > [data-testid^="chat-header-"]'),
      ).map((el) => el.dataset.testid);
      expect(segments).toEqual([
        'chat-header-machine',
        'chat-header-project',
        'chat-header-branch',
        'chat-header-worktree',
      ]);
    } finally {
      resetStagedBackends();
      setCompactLayoutForTest(false);
    }
  });

  it('full layout has no back button', async () => {
    setCompactLayoutForTest(false);
    const pane = createThreadPane({ paneId: 'main' });
    await pane.switchThread(makeThread({ id: 'full-thread', title: 'Full' }));
    registerPaneForTest('main', pane);
    setPaneLayoutItemsForTest([{ id: 'main-item', paneId: 'main', kind: 'thread', widthPx: 1 }]);
    const { queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('compact-back')).toBeNull();
    expect(queryByTestId('pane-close')).not.toBeNull();
  });
});
