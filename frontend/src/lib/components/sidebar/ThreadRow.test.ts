import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadRow from './ThreadRow.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { refreshThreads } from '../../stores/threads.svelte';
import {
  beginThreadLiveStateHydration,
  finishThreadLiveStateHydration,
  projectTurnStarted,
  resetForTest as resetThreadStatuses,
  setThreadStatus,
} from '../../stores/threadStatuses.svelte';
import {
  resetKeybindingsStore,
  setKeybindingsForTest,
} from '../../stores/keybindings.svelte';
import { relativeTime } from '../../utils/format';
import {
  resetKeyboardModifiersForTest,
  subscribeJumpHints,
} from '../../stores/keyboardModifiers.svelte';
import type { Thread } from '../../types/models';
import type { Settings } from '../../types/settings';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { emitItemEventUpsert } from '../../../test/helpers/chat';
import { THREAD_ROW_DRAG_MIME } from '../../utils/threadDragPayload';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test Thread',
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function primeSettings(overrides: Partial<Settings> | null = null) {
  setBindingMock('GetSettings', async () => overrides);
  await loadSettings();
}

function nextFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

function markThreadRunning(threadId: string): void {
  projectTurnStarted(threadId, `turn:${threadId}`, 0, 0);
}

function makeDataTransfer(): DataTransfer {
  const values = new Map<string, string>();
  return {
    effectAllowed: 'none',
    dropEffect: 'none',
    setData: (type: string, value: string) => { values.set(type, value); },
    getData: (type: string) => values.get(type) ?? '',
    setDragImage: () => {},
    get types() { return Array.from(values.keys()); },
  } as unknown as DataTransfer;
}

describe('<ThreadRow> archive action', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetPaneLayoutForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
    resetKeybindingsStore();
    resetKeyboardModifiersForTest();
  });

  it('shows the archive action (and no delete X) for a chat thread', async () => {
    const thread = makeThread({ archived: false });
    const pane = createThreadPane();
    const { getByTestId, queryByTestId } = render(ThreadRow, { props: { thread, pane } });
    expect(getByTestId('thread-row-archive')).toBeInTheDocument();
    expect(queryByTestId('thread-row-delete')).toBeNull();
  });
});

describe('<ThreadRow> terminal delete action', () => {
  // Terminals aren't archivable — the row offers an X that deletes the
  // terminal thread. The new behavior under test is the confirmDelete
  // gate: off → delete immediately, on → confirm first. deleteThreadAction
  // itself (StopSession → DeleteThread → store/pane cleanup) is shared with
  // the right-click Delete path and already covered there.
  beforeEach(async () => {
    resetPanesForTest();
    resetPaneLayoutForTest();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
    resetKeybindingsStore();
    resetKeyboardModifiersForTest();
    // Best-effort stop the (sentinel-provider) session before delete.
    setBindingMock('StopSession', async () => {});
  });

  it('shows a delete X (not archive) for a terminal thread', async () => {
    await primeSettings();
    const thread = makeThread({ mode: 'terminal' });
    const pane = createThreadPane();
    const { getByTestId, queryByTestId } = render(ThreadRow, { props: { thread, pane } });
    expect(getByTestId('thread-row-delete')).toBeInTheDocument();
    expect(queryByTestId('thread-row-archive')).toBeNull();
  });

  it('deletes immediately, without a confirm dialog, when confirmDelete is off', async () => {
    await primeSettings({ confirmDelete: false });
    let deletedId: string | null = null;
    setBindingMock('DeleteThread', async (id: string) => { deletedId = id; });

    const thread = makeThread({ id: 'term-off', mode: 'terminal' });
    const pane = createThreadPane();
    const { getByTestId, queryByRole } = render(ThreadRow, { props: { thread, pane } });

    await fireEvent.click(getByTestId('thread-row-delete'));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(deletedId).toBe('term-off');
    // No confirm dialog opened — the X's accessible name is "Delete
    // Terminal", so a button named exactly "Delete" only exists if the
    // dialog rendered its confirm button.
    expect(queryByRole('button', { name: 'Delete' })).toBeNull();
  });

  it('confirms first when confirmDelete is on, deleting only after confirm', async () => {
    await primeSettings({ confirmDelete: true });
    let deletedId: string | null = null;
    setBindingMock('DeleteThread', async (id: string) => { deletedId = id; });

    const thread = makeThread({ id: 'term-on', mode: 'terminal' });
    const pane = createThreadPane();
    const { getByTestId, getByRole } = render(ThreadRow, { props: { thread, pane } });

    await fireEvent.click(getByTestId('thread-row-delete'));
    await tick();

    // The X opened the confirm dialog; nothing deleted yet.
    const confirmBtn = getByRole('button', { name: 'Delete' });
    expect(confirmBtn).toBeInTheDocument();
    expect(deletedId).toBeNull();

    await fireEvent.click(confirmBtn);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(deletedId).toBe('term-on');
  });
});

describe('<ThreadRow> leading mode icon', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetPaneLayoutForTest();
    await primeSettings();
  });

  it('renders the terminal icon (not the draft icon) for a terminal thread the backend marks as a draft', () => {
    const pane = createThreadPane();
    // Real backend shape: a terminal is an item-less thread, so the store
    // computes isDraft=true for it (store.go: IsDraft is "no items exist").
    // ThreadRow must still show the terminal glyph, not the draft document
    // icon — mode is the stronger signal and is checked first. This is the
    // regression guard for the sidebar showing a document icon on terminals;
    // it fails if the isDraft branch is ever allowed to win again.
    const thread = makeThread({ mode: 'terminal', isDraft: true });
    const { getByTestId, queryByTestId } = render(ThreadRow, { props: { thread, pane } });
    // The green terminal glyph (boxless `>_`, matching the chat-history bash
    // tool-call icon) that distinguishes terminal rows from chat rows (mirrors
    // the draft icon's leading slot).
    expect(getByTestId('thread-row-terminal-icon')).toBeInTheDocument();
    expect(queryByTestId('thread-row-draft-icon')).toBeNull();
  });

  it('renders no terminal icon for a chat thread', () => {
    const pane = createThreadPane();
    const thread = makeThread({ mode: 'chat' });
    const { queryByTestId } = render(ThreadRow, { props: { thread, pane } });
    expect(queryByTestId('thread-row-terminal-icon')).toBeNull();
  });
});

describe('<ThreadRow> drag source', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetPaneLayoutForTest();
    await primeSettings();
  });

  it('publishes a sidebar-thread drag payload', async () => {
    const thread = makeThread({ id: 'drag-source', title: 'Drag Source' });
    const pane = createThreadPane();
    const rendered = render(ThreadRow, { props: { thread, pane } });
    const row = rendered.getByTestId('thread-row');
    const dataTransfer = makeDataTransfer();

    await fireEvent.dragStart(row, { dataTransfer });

    expect(dataTransfer.types).toContain(THREAD_ROW_DRAG_MIME);
    expect(JSON.parse(dataTransfer.getData(THREAD_ROW_DRAG_MIME))).toEqual({
      threadId: 'drag-source',
      title: 'Drag Source',
    });
  });
});

describe('<ThreadRow> title tooltip', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
  });

  it('uses the full thread title as hover text', () => {
    const pane = createThreadPane();
    const thread = makeThread({ title: 'A long thread title that the sidebar may truncate' });

    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-title').getAttribute('title')).toBe(
      'A long thread title that the sidebar may truncate',
    );
  });

  it('uses Untitled as hover text when the thread title is empty', () => {
    const pane = createThreadPane();
    const thread = makeThread({ title: '' });

    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-title').getAttribute('title')).toBe('Untitled');
  });
});

describe('<ThreadRow> timestamp label', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-05-17T18:00:00.000Z'));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders compact locale-relative time in the sidebar', () => {
    const pane = createThreadPane();
    const thread = makeThread({
      updatedAt: new Date('2026-05-17T17:39:00.000Z').getTime(),
    });

    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-time').textContent?.trim()).toBe('21m');
  });

  it('renders now instead of just now for current timestamps', () => {
    const pane = createThreadPane();
    const thread = makeThread({
      updatedAt: new Date('2026-05-17T18:00:00.000Z').getTime(),
    });

    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-time').textContent?.trim()).toBe('now');
  });

  it('leaves absolute timestamp formats unchanged', async () => {
    await primeSettings({ timestampFormat: '24-hour' });
    const pane = createThreadPane();
    const thread = makeThread({
      updatedAt: new Date('2026-05-17T17:39:00.000Z').getTime(),
    });

    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-time').textContent?.trim()).toBe(
      relativeTime(thread.updatedAt, '24-hour'),
    );
  });
});

describe('<ThreadRow> fork lineage affordance', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
  });

  it('is absent on a top-level (non-forked) thread', async () => {
    const thread = makeThread();
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, { props: { thread, pane } });
    expect(queryByTestId('thread-row-fork-lineage')).toBeNull();
  });

  it('is visible when forkedFromThreadId is set', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', title: 'Derived', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const forkIndicator = getByTestId('thread-row-fork-lineage');
    expect(forkIndicator).toBeInTheDocument();
    expect(forkIndicator.querySelector('.lucide-icon')).not.toBeNull();
    expect(forkIndicator.textContent).not.toContain('F');
  });

  it('renders the fork indicator before the title like the left-side row icons', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', title: 'Derived', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const forkIndicator = getByTestId('thread-row-fork-lineage');
    const title = getByTestId('thread-row-title');

    expect(forkIndicator.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);
  });

  it('surfaces the parent title in the tooltip when the parent is loaded', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const forkIndicator = getByTestId('thread-row-fork-lineage') as HTMLButtonElement;
    expect(forkIndicator.title).toMatch(/"Original"/);
    expect(forkIndicator.disabled).toBe(false);
  });

  it('is disabled (with explanatory tooltip) when the parent is not in the sidebar view', async () => {
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent-not-loaded' });
    // Only the forked thread is in the list; the parent is absent.
    setBindingMock('ListThreads', async () => [forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const forkIndicator = getByTestId('thread-row-fork-lineage') as HTMLButtonElement;
    expect(forkIndicator.disabled).toBe(true);
    expect(forkIndicator.title).toMatch(/not loaded/i);
  });

  it('clicking the affordance switches the pane to the parent thread', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    await fireEvent.click(getByTestId('thread-row-fork-lineage'));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(pane.threadId).toBe('parent');
  });

  it('affordance click does not trigger the row-level thread switch', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);

    let rowSelectCalled = 0;
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: forked,
        pane,
        onSelectClick: () => { rowSelectCalled += 1; return false; },
      },
    });
    await fireEvent.click(getByTestId('thread-row-fork-lineage'));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(rowSelectCalled).toBe(0);
    // Pane should be on the PARENT (not the forked thread we rendered).
    expect(pane.threadId).toBe('parent');
  });
});

describe('<ThreadRow> worktree metadata', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
  });

  it('renders the worktree basename under the row instead of the WT badge', () => {
    const thread = makeThread({
      id: 'worktree-thread',
      title: 'Worktree Thread',
      worktreePath: '/tmp/agent-overflow-worktrees/feature-demo',
    });
    const pane = createThreadPane();
    const { container, getByTestId, queryByText } = render(ThreadRow, {
      props: { thread, pane },
    });

    expect(queryByText('WT')).toBeNull();
    expect(container.textContent).not.toContain('WT');
    expect(getByTestId('thread-row-worktree').style.paddingLeft).toBe('38px');
    expect(getByTestId('thread-row-worktree-name').textContent?.trim()).toBe('feature-demo');
  });

  it('uses the same color token as the updated-at timestamp', () => {
    const thread = makeThread({
      id: 'worktree-thread',
      worktreePath: '/tmp/worktrees/feature-demo',
    });
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-worktree-label').classList.contains('text-fg-hint')).toBe(true);
    expect(getByTestId('thread-row-worktree-label').classList.contains('text-fg-muted')).toBe(false);
  });

  it('uses an icon instead of a connector line', () => {
    const thread = makeThread({
      id: 'worktree-thread',
      worktreePath: '/tmp/worktrees/feature-demo',
    });
    const pane = createThreadPane();
    const { container, getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-worktree-label').querySelector('.lucide-icon')).not.toBeNull();
    expect(container.querySelector('[data-testid="thread-row-worktree"] .border-l')).toBeNull();
  });

  it('uses the final path segment with trailing slashes ignored', () => {
    const thread = makeThread({
      id: 'worktree-thread',
      worktreePath: '/tmp/worktrees/feature-demo/',
    });
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-worktree-name').textContent?.trim()).toBe('feature-demo');
  });

  it('handles Windows-style path separators', () => {
    const thread = makeThread({
      id: 'worktree-thread',
      worktreePath: 'C:\\worktrees\\feature-demo',
    });
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });

    expect(getByTestId('thread-row-worktree-name').textContent?.trim()).toBe('feature-demo');
  });

  it('does not render metadata when the thread has no worktree path', () => {
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ worktreePath: undefined }), pane },
    });

    expect(queryByTestId('thread-row-worktree')).toBeNull();
  });

  it('exposes the full worktree path without making the metadata row interactive', async () => {
    const thread = makeThread({
      id: 'worktree-thread',
      worktreePath: '/tmp/agent-overflow-worktrees/feature-demo',
    });
    let rowSelectCalled = 0;
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread,
        pane,
        onSelectClick: () => {
          rowSelectCalled += 1;
          return true;
        },
      },
    });

    const worktreeMeta = getByTestId('thread-row-worktree');
    expect(worktreeMeta.getAttribute('title')).toBe('Worktree: /tmp/agent-overflow-worktrees/feature-demo');
    expect(worktreeMeta.getAttribute('aria-label')).toBe('Worktree feature-demo');

    await fireEvent.click(worktreeMeta);
    await Promise.resolve();

    expect(rowSelectCalled).toBe(0);
    expect(pane.threadId).toBeNull();
  });
});

describe('<ThreadRow> pin affordance placement', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
  });

  it('renders the pin in the leading gutter before the title', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ pinnedAt: 1 }), pane },
    });
    const title = getByTestId('thread-row-title');
    const pin = getByTestId('thread-row-pin');

    // Pin lives left of the title now — the leading-gutter slot.
    expect(pin.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);
  });

  it('keeps pinned state visible at rest and to the left of the timestamp', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ pinnedAt: 1 }), pane },
    });
    const pin = getByTestId('thread-row-pin');
    const time = getByTestId('thread-row-time');

    expect(pin.getAttribute('aria-label')).toBe('Unpin Thread');
    expect(pin.compareDocumentPosition(time) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);
  });

  it('uses accent for front-burner pins and the muted token for back-burner pins', () => {
    const pane = createThreadPane();
    const frontView = render(ThreadRow, {
      props: { thread: makeThread({ id: 'front', pinnedAt: 1, pinGroup: 0 }), pane },
    });
    const backView = render(ThreadRow, {
      props: { thread: makeThread({ id: 'back', pinnedAt: 1, pinGroup: 1 }), pane },
    });
    const front = frontView.container.querySelector('[data-testid="thread-row-pin"]') as HTMLElement;
    const back = backView.container.querySelector('[data-testid="thread-row-pin"]') as HTMLElement;

    expect(front.className).toContain('text-accent');
    expect(front.getAttribute('data-pin-group')).toBe('front');
    expect(back.className).toContain('text-fg-muted');
    expect(back.className).not.toContain('text-accent');
    expect(back.getAttribute('data-pin-group')).toBe('back');
  });

  it('right-clicking a pinned icon toggles its group without opening the row menu', async () => {
    const pane = createThreadPane();
    const move = setBindingMock('SetThreadPinGroup', vi.fn(async () => makeThread({
      pinnedAt: 1,
      pinGroup: 1,
    })));
    const { getByTestId, queryByRole } = render(ThreadRow, {
      props: { thread: makeThread({ pinnedAt: 1, pinGroup: 0 }), pane },
    });

    await fireEvent.contextMenu(getByTestId('thread-row-pin'));
    await Promise.resolve();

    expect(move).toHaveBeenCalledWith('thread-1', 1);
    expect(queryByRole('menu', { name: 'Thread Actions' })).toBeNull();
  });

  it('renders an unpinned pin button so row-hover can reveal it', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread(), pane },
    });
    const pin = getByTestId('thread-row-pin');

    expect(pin.getAttribute('aria-label')).toBe('Pin Thread');
    // Unpinned button is rendered but kept hidden until row hover/focus.
    expect(pin.className).toContain('opacity-0');
    expect(pin.className).toContain('group-hover/thread-item:opacity-100');
  });

  it('keeps the pin action out of nested discussion participant rows', () => {
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ parentThreadId: 'parent' }),
        pane,
        indent: 2,
      },
    });

    expect(queryByTestId('thread-row-pin')).toBeNull();
  });
});

describe('<ThreadRow> hover-action reveal trigger', () => {
  // Regression: the pin (left) and the archive/delete action (right) reveal
  // on row HOVER or KEYBOARD focus only. They previously keyed off
  // `group-focus-within/thread-row`, but the row is a tabindex=0 button, so a
  // plain mouse click focuses it and `:focus-within` left both affordances
  // stuck visible on the active row after the pointer left (reported on
  // terminal rows — which, unlike chat, don't hand focus off to a composer on
  // open, so the row keeps focus). The correct trigger is
  // `group-has-[:focus-visible]/thread-row`: a focus-VISIBLE descendant
  // (keyboard Tab into an action), which a mouse click on the row never
  // produces. happy-dom doesn't compute `:has()`/`:focus-visible` against
  // Tailwind CSS (the stylesheet isn't loaded in unit tests), so this asserts
  // the class contract — the same proxy the pin-placement tests above use.
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
  });

  it('reveals the unpinned pin on hover and keyboard focus-visible, never on focus-within', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: makeThread(), pane } });
    const pin = getByTestId('thread-row-pin');

    expect(pin.className).toContain('opacity-0');
    expect(pin.className).toContain('group-hover/thread-item:opacity-100');
    expect(pin.className).toContain('group-has-[:focus-visible]/thread-row:opacity-100');
    // The mouse-click-sticks bug: focus-within must never be a reveal trigger.
    expect(pin.className).not.toContain('focus-within');
  });

  it('reveals the terminal delete action on hover and keyboard focus-visible, never on focus-within', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ mode: 'terminal' }), pane },
    });
    // The delete X lives inside the absolute right-side action wrapper, which
    // owns the reveal opacity (a child's opacity can't override an opacity-0
    // parent), so assert the wrapper's trigger classes.
    const wrapper = getByTestId('thread-row-delete').parentElement as HTMLElement;

    expect(wrapper.className).toContain('opacity-0');
    expect(wrapper.className).toContain('group-hover/thread-item:opacity-100');
    expect(wrapper.className).toContain('group-has-[:focus-visible]/thread-row:opacity-100');
    expect(wrapper.className).not.toContain('focus-within');
  });

  it('cross-fades the timestamp out on the same hover / keyboard-focus trigger', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: makeThread(), pane } });
    const time = getByTestId('thread-row-time');

    expect(time.className).toContain('group-hover/thread-item:opacity-0');
    expect(time.className).toContain('group-has-[:focus-visible]/thread-row:opacity-0');
    expect(time.className).not.toContain('focus-within');
    // Visible at rest: only the hover/focus-scoped fades carry opacity-0;
    // no standalone opacity-0 token that would hide the stamp by default.
    expect(time.className.split(/\s+/)).not.toContain('opacity-0');
  });
});

describe('<ThreadRow> live status dot', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
    resetThreadStatuses();
  });

  it('renders no dot at all when the thread is idle and read', () => {
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-idle' }), pane },
    });
    // Compact layout: idle + read = just the title and time, no dot.
    expect(queryByTestId('thread-row-status-dot')).toBeNull();
  });

  it('renders a success pulsing dot labelled Working when running in chat mode', () => {
    markThreadRunning('t-run');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-run', mode: 'chat' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('running');
    expect(dot.getAttribute('aria-label')).toBe('Working');
    expect(dot.getAttribute('title')).toBe('Working');
    expect(dot.classList.contains('bg-success')).toBe(true);
    expect(dot.classList.contains('animate-pulse')).toBe(true);
  });

  // Regression: row must react LIVE to status changes pushed into the
  // projection store AFTER it mounted. This mirrors what happens in
  // production when a running row first receives a turn-start signal while
  // the sidebar is already rendered. Before the fix, the pill never
  // appeared because the $derived wasn't recomputing on statuses-store
  // reassignment.
  it('reactively shows the pill when status flips AFTER mount', async () => {
    const pane = createThreadPane();
    const { queryByTestId, getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-post', mode: 'chat' }), pane },
    });
    expect(queryByTestId('thread-row-status-dot')).toBeNull();

    markThreadRunning('t-post');
    // Drain microtasks so the $derived recomputes and the DOM
    // reconciles.
    for (let i = 0; i < 3; i += 1) await Promise.resolve();

    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('running');
    expect(dot.getAttribute('aria-label')).toBe('Working');
  });

  // Regression: durable item status is not thread liveness. A stale
  // foreground running item from history must not keep the sidebar
  // stuck on Working after the backend turn has already completed.
  it('does not flip the pill to Working from a running provider:item_event row alone', async () => {
    const { setupEventListeners } = await import('../../stores/events');
    const cleanup = setupEventListeners();
    try {
      const pane = createThreadPane();
      const { queryByTestId } = render(ThreadRow, {
        props: { thread: makeThread({ id: 't-stream', mode: 'chat' }), pane },
      });
      expect(queryByTestId('thread-row-status-dot')).toBeNull();

      emitItemEventUpsert({
        id: 'item-1',
        threadId: 't-stream',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'tool_call',
        role: 'assistant',
        status: 'running',
        summary: 'advisor',
        toolName: 'advisor',
        isBackground: false,
        createdAt: 1,
        updatedAt: 1,
      });
      await nextFrame();

      expect(queryByTestId('thread-row-status-dot')).toBeNull();
    } finally {
      cleanup();
    }
  });

  // Regression: the mode-aware label must flip when the thread prop
  // updates. In production this comes from the sidebar's {#each} loop
  // re-rendering the row with a new thread object after replaceThread;
  // here we drive it via rerender({ thread: ... }).
  it('flips the pill label when thread.mode changes mid-turn', async () => {
    markThreadRunning('t-mode');
    const pane = createThreadPane();
    const chat = makeThread({ id: 't-mode', mode: 'chat' });
    const { getByTestId, rerender } = render(ThreadRow, {
      props: { thread: chat, pane },
    });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Working');

    await rerender({ thread: { ...chat, mode: 'plan' }, pane });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Planning');

    await rerender({ thread: { ...chat, mode: 'design' }, pane });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Designing');

    await rerender({ thread: { ...chat, mode: 'discussion' }, pane });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Discussing');
  });

  it('flips the pill to Working when provider:turn_started arrives', async () => {
    const { setupEventListeners } = await import('../../stores/events');
    const cleanup = setupEventListeners();
    try {
      const pane = createThreadPane();
      const { queryByTestId, getByTestId } = render(ThreadRow, {
        props: { thread: makeThread({ id: 't-turn', mode: 'chat' }), pane },
      });
      expect(queryByTestId('thread-row-status-dot')).toBeNull();

      emitWailsEvent('provider:turn_started', {
        threadId: 't-turn',
        turnId: 'turn-1',
        turnIndex: 0,
        startedAt: 1,
      });
      await nextFrame();

      const dot = getByTestId('thread-row-status-dot');
      expect(dot.getAttribute('data-status')).toBe('running');
    } finally {
      cleanup();
    }
  });

  it('labels the pill "Planning" when running in plan mode', () => {
    markThreadRunning('t-plan');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-plan', mode: 'plan' }), pane },
    });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Planning');
  });

  it('renders a warning dot labelled Pending approval when a blocking approval is pending', () => {
    setThreadStatus('t-approval', 'pending-approval');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-approval' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('pending-approval');
    expect(dot.getAttribute('aria-label')).toBe('Pending Approval');
    expect(dot.classList.contains('bg-warning')).toBe(true);
  });

  it('renders an info dot labelled Awaiting input for user-input requests', () => {
    setThreadStatus('t-input', 'awaiting-input');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-input' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('awaiting-input');
    expect(dot.getAttribute('aria-label')).toBe('Awaiting Input');
    expect(dot.classList.contains('bg-info')).toBe(true);
  });

  it('applies the pulsing warning glow class to the row when pending approval', () => {
    setThreadStatus('t-glow-approval', 'pending-approval');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-glow-approval' }), pane },
    });
    const shell = getByTestId('thread-row-shell');
    expect(shell.classList.contains('status-glow-warning')).toBe(true);
    expect(shell.classList.contains('status-glow-info')).toBe(false);
  });

  it('applies the pulsing info glow class to the row when awaiting input', () => {
    setThreadStatus('t-glow-input', 'awaiting-input');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-glow-input' }), pane },
    });
    const shell = getByTestId('thread-row-shell');
    expect(shell.classList.contains('status-glow-info')).toBe(true);
    expect(shell.classList.contains('status-glow-warning')).toBe(false);
  });

  it('does not apply a glow class when the row is merely running', () => {
    markThreadRunning('t-glow-run');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-glow-run' }), pane },
    });
    const shell = getByTestId('thread-row-shell');
    expect(shell.classList.contains('status-glow-warning')).toBe(false);
    expect(shell.classList.contains('status-glow-info')).toBe(false);
  });

  it('renders a non-pulsing accent dot labelled Plan ready when a plan is waiting', () => {
    setThreadStatus('t-plan-ready', 'plan-ready');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-plan-ready' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('plan-ready');
    expect(dot.getAttribute('aria-label')).toBe('Plan Ready');
    expect(dot.classList.contains('bg-accent')).toBe(true);
    expect(dot.classList.contains('animate-pulse')).toBe(false);
  });

  it('renders durable Plan ready from the thread row without a live event', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ id: 't-durable-plan', hasActionableProposedPlan: true }),
        pane,
      },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('plan-ready');
    expect(dot.getAttribute('aria-label')).toBe('Plan Ready');
  });

  it('renders durable Interrupted from the thread row without a live event', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ id: 't-interrupted', hasIncompleteTurn: true }),
        pane,
      },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('interrupted');
    expect(dot.getAttribute('aria-label')).toBe('Interrupted');
    expect(dot.classList.contains('bg-warning')).toBe(true);
    expect(dot.classList.contains('animate-pulse')).toBe(false);
  });

  it('does not render durable Interrupted while server live state is hydrating', () => {
    const token = beginThreadLiveStateHydration('t-hydrating');
    try {
      const pane = createThreadPane();
      const { queryByTestId } = render(ThreadRow, {
        props: {
          thread: makeThread({ id: 't-hydrating', hasIncompleteTurn: true }),
          pane,
        },
      });
      expect(queryByTestId('thread-row-status-dot')).toBeNull();
    } finally {
      finishThreadLiveStateHydration('t-hydrating', token);
    }
  });

  it('live running overrides durable Interrupted', () => {
    markThreadRunning('t-live-over-durable');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ id: 't-live-over-durable', hasIncompleteTurn: true }),
        pane,
      },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('running');
    expect(dot.getAttribute('aria-label')).toBe('Working');
  });

  it('renders an error dot labelled Failed when the thread has errored', () => {
    setThreadStatus('t-err', 'error');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-err' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('error');
    expect(dot.getAttribute('aria-label')).toBe('Failed');
    expect(dot.classList.contains('bg-error')).toBe(true);
  });

  it('uses the thread id (not some shared row instance) for the lookup', () => {
    markThreadRunning('t-a');
    setThreadStatus('t-b', 'error');
    const pane = createThreadPane();
    // Scope each query to its own render's baseElement so duplicate
    // data-testid attributes in the shared document body don't collide.
    const rowA = render(ThreadRow, { props: { thread: makeThread({ id: 't-a' }), pane } });
    const dotA = rowA.container.querySelector<HTMLElement>('[data-testid="thread-row-status-dot"]');
    expect(dotA?.getAttribute('data-status')).toBe('running');
    rowA.unmount();

    const rowB = render(ThreadRow, { props: { thread: makeThread({ id: 't-b' }), pane } });
    const dotB = rowB.container.querySelector<HTMLElement>('[data-testid="thread-row-status-dot"]');
    expect(dotB?.getAttribute('data-status')).toBe('error');
    rowB.unmount();
  });

  it('renders the active thread-jump keybinding in the jump hint', async () => {
    vi.useFakeTimers();
    try {
      setKeybindingsForTest([{ key: 'ctrl+alt+2', command: 'thread.jump.1' }]);
      const release = subscribeJumpHints();
      const pane = createThreadPane();
      const { getByTestId } = render(ThreadRow, {
        props: { thread: makeThread({ id: 'jump-target' }), pane },
      });

      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Control', bubbles: true }));
      vi.advanceTimersByTime(101);
      await tick();

      expect(getByTestId('thread-row-jump-hint').textContent?.trim()).toBe('Ctrl+Alt+2');
      release();
    } finally {
      resetKeyboardModifiersForTest();
      resetKeybindingsStore();
      vi.useRealTimers();
    }
  });
});

describe('<ThreadRow> nested row chrome', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
  });

  it('renders no chevron when hasChildren is false', () => {
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, {
      props: { thread: makeThread(), pane, hasChildren: false },
    });
    expect(queryByTestId('thread-row-expand')).toBeNull();
  });

  it('renders the chevron (not rotated) when hasChildren is true and expanded is false', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ discussionId: 'def-1' }),
        pane,
        hasChildren: true,
        expanded: false,
      },
    });
    const btn = getByTestId('thread-row-expand');
    expect(btn.getAttribute('aria-expanded')).toBe('false');
    expect(btn.querySelector('.lucide-icon')?.classList.contains('rotate-90')).toBe(false);
  });

  it('rotates the chevron when expanded is true', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ discussionId: 'def-1' }),
        pane,
        hasChildren: true,
        expanded: true,
      },
    });
    const btn = getByTestId('thread-row-expand');
    expect(btn.getAttribute('aria-expanded')).toBe('true');
    expect(btn.querySelector('.lucide-icon')?.classList.contains('rotate-90')).toBe(true);
  });

  it('chevron click calls onToggleExpand and not the row-click path', async () => {
    const onToggleExpand = vi.fn();
    const onSelectClick = vi.fn(() => false);
    const pane = createThreadPane();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);

    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ discussionId: 'def-1' }),
        pane,
        hasChildren: true,
        expanded: false,
        onToggleExpand,
        onSelectClick,
      },
    });
    await fireEvent.click(getByTestId('thread-row-expand'));
    expect(onToggleExpand).toHaveBeenCalledTimes(1);
    expect(onSelectClick).not.toHaveBeenCalled();
    expect(pane.threadId).toBeNull();
  });

  it('applies indent via padding-left on the outer container', () => {
    const pane = createThreadPane();
    const { container } = render(ThreadRow, {
      props: { thread: makeThread(), pane, indent: 2 },
    });
    const outer = container.querySelector('[role="button"]') as HTMLElement;
    // Compact layout: every row reserves a 24px leading pin gutter, then
    // depth 2+ steps 8px per nesting level. indent=2 -> 24 + 8 = 32px.
    expect(outer.style.paddingLeft).toBe('32px');
  });
});
