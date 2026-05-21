import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import UnifiedThreadPicker from './UnifiedThreadPicker.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { refreshThreads } from '../../stores/threads.svelte';
import {
  projectTurnStarted,
  resetForTest as resetThreadStatuses,
  setThreadStatus,
} from '../../stores/threadStatuses.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';
import type { Thread } from '../../types/models';

beforeAll(installAnimateShim);

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test Thread',
    provider: 'claude',
    workspacePath: '/home/me/work/alpha',
    projectPath: '/home/me/work/alpha',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function seedThreads(threads: Thread[]): Promise<void> {
  setBindingMock('ListThreads', async () => threads);
  await refreshThreads();
}

beforeEach(async () => {
  resetThreadStatuses();
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  await seedThreads([]);
});

function makePane() {
  return createThreadPane();
}

function markThreadRunning(threadId: string): void {
  projectTurnStarted(threadId, `turn:${threadId}`, 0, 0);
}

// ---- Visibility ----

describe('<UnifiedThreadPicker> — visibility', () => {
  it('renders nothing when closed', async () => {
    await seedThreads([makeThread({ id: 'a', title: 'Alpha' })]);
    const pane = makePane();
    const { queryByTestId } = render(UnifiedThreadPicker, {
      open: false,
      pane,
      onClose: vi.fn(),
    });
    expect(queryByTestId('thread-picker')).toBeNull();
  });

  it('renders the dialog when open and shows every non-archived thread', async () => {
    await seedThreads([
      makeThread({ id: 'a', title: 'Alpha' }),
      makeThread({ id: 'b', title: 'Bravo' }),
    ]);
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    expect(getByTestId('thread-picker')).toBeInTheDocument();
    expect(getByTestId('thread-picker-hit-a')).toBeInTheDocument();
    expect(getByTestId('thread-picker-hit-b')).toBeInTheDocument();
  });

  it('excludes archived threads from the list', async () => {
    await seedThreads([
      makeThread({ id: 'live', title: 'Live' }),
      makeThread({ id: 'gone', title: 'Archived One', archived: true }),
    ]);
    const pane = makePane();
    const { getByTestId, queryByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    expect(getByTestId('thread-picker-hit-live')).toBeInTheDocument();
    expect(queryByTestId('thread-picker-hit-gone')).toBeNull();
  });

  it('shows an empty-state message when the store has no visible threads', async () => {
    await seedThreads([]);
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    expect(getByTestId('thread-picker-empty').textContent).toContain('No threads yet');
  });

  it('shows the "no match" empty state when a filter excludes every thread', async () => {
    await seedThreads([makeThread({ id: 'a', title: 'Alpha' })]);
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    await fireEvent.input(getByTestId('thread-picker-input'), { target: { value: 'zzz' } });
    expect(getByTestId('thread-picker-empty').textContent).toContain('zzz');
  });
});

// ---- Filtering ----

describe('<UnifiedThreadPicker> — filtering', () => {
  it('filters by title substring (case-insensitive)', async () => {
    await seedThreads([
      makeThread({ id: 'a', title: 'Refactor storage layer' }),
      makeThread({ id: 'b', title: 'Design system spike' }),
    ]);
    const pane = makePane();
    const { getByTestId, queryByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    await fireEvent.input(getByTestId('thread-picker-input'), { target: { value: 'DESIGN' } });
    expect(getByTestId('thread-picker-hit-b')).toBeInTheDocument();
    expect(queryByTestId('thread-picker-hit-a')).toBeNull();
  });

  it('filters by project path', async () => {
    await seedThreads([
      makeThread({ id: 'a', title: 'Alpha', projectPath: '/home/me/work/alpha' }),
      makeThread({ id: 'b', title: 'Bravo', projectPath: '/home/me/work/bravo' }),
    ]);
    const pane = makePane();
    const { getByTestId, queryByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    await fireEvent.input(getByTestId('thread-picker-input'), { target: { value: 'bravo' } });
    expect(getByTestId('thread-picker-hit-b')).toBeInTheDocument();
    expect(queryByTestId('thread-picker-hit-a')).toBeNull();
  });

  it('ranks prefix matches above plain substring matches', async () => {
    await seedThreads([
      makeThread({ id: 'sub', title: 'My api thing' }),
      makeThread({ id: 'pre', title: 'api client' }),
    ]);
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    await fireEvent.input(getByTestId('thread-picker-input'), { target: { value: 'api' } });
    const results = getByTestId('thread-picker-results');
    const ordered = Array.from(results.querySelectorAll('[data-testid^="thread-picker-hit-"]')).map(
      (el) => el.getAttribute('data-testid'),
    );
    expect(ordered[0]).toBe('thread-picker-hit-pre');
    expect(ordered[1]).toBe('thread-picker-hit-sub');
  });

  it('highlights matching substrings with <mark>', async () => {
    await seedThreads([makeThread({ id: 'a', title: 'Release pipeline' })]);
    const pane = makePane();
    const { getByTestId, findByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    await fireEvent.input(getByTestId('thread-picker-input'), { target: { value: 'release' } });
    const row = await findByTestId('thread-picker-hit-a');
    const marks = row.querySelectorAll('mark');
    expect(marks.length).toBeGreaterThan(0);
    expect(marks[0].textContent).toBe('Release');
  });

  it('shows the "N more — refine your query" footer when results exceed the cap', async () => {
    const many: Thread[] = [];
    for (let i = 0; i < 55; i += 1) {
      many.push(makeThread({ id: `t${i}`, title: `Thread ${i}` }));
    }
    await seedThreads(many);
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    const overflow = getByTestId('thread-picker-overflow');
    expect(overflow.textContent).toContain('5 more');
    expect(overflow.textContent).toContain('refine your query');
  });

  it('does not render the overflow footer when results fit under the cap', async () => {
    await seedThreads([makeThread({ id: 'a', title: 'Alpha' })]);
    const pane = makePane();
    const { queryByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    expect(queryByTestId('thread-picker-overflow')).toBeNull();
  });

  it('resets the query on reopen', async () => {
    await seedThreads([makeThread({ id: 'a', title: 'Alpha' })]);
    const pane = makePane();
    const { getByTestId, rerender } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    await fireEvent.input(getByTestId('thread-picker-input'), { target: { value: 'stale' } });
    await rerender({ open: false, pane, onClose: vi.fn() });
    await rerender({ open: true, pane, onClose: vi.fn() });
    expect((getByTestId('thread-picker-input') as HTMLInputElement).value).toBe('');
  });
});

// ---- Keyboard navigation ----

describe('<UnifiedThreadPicker> — keyboard', () => {
  it('ArrowDown / ArrowUp wrap the active index', async () => {
    await seedThreads([
      makeThread({ id: 'a', title: 'Alpha' }),
      makeThread({ id: 'b', title: 'Bravo' }),
      makeThread({ id: 'c', title: 'Charlie' }),
    ]);
    const pane = makePane();
    const { getByTestId, container } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    // Arrow / Enter are now caught at the dialog body level (the
    // `thread-picker` div) so the search <input> keeps receiving
    // alphanumeric keystrokes it cares about. The backdrop still owns
    // Escape via Modal.
    const body = getByTestId('thread-picker');
    expect(getByTestId('thread-picker-hit-a').getAttribute('aria-current')).toBe('true');
    await fireEvent.keyDown(body, { key: 'ArrowDown' });
    expect(getByTestId('thread-picker-hit-b').getAttribute('aria-current')).toBe('true');
    await fireEvent.keyDown(body, { key: 'ArrowDown' });
    expect(getByTestId('thread-picker-hit-c').getAttribute('aria-current')).toBe('true');
    // Wrap forward.
    await fireEvent.keyDown(body, { key: 'ArrowDown' });
    expect(getByTestId('thread-picker-hit-a').getAttribute('aria-current')).toBe('true');
    // Wrap backward.
    await fireEvent.keyDown(body, { key: 'ArrowUp' });
    expect(getByTestId('thread-picker-hit-c').getAttribute('aria-current')).toBe('true');
    // Backdrop is still the place Escape lands.
    expect(container.querySelector('[data-modal-backdrop]')).not.toBeNull();
  });

  it('Enter switches to the highlighted thread and calls onClose', async () => {
    await seedThreads([
      makeThread({ id: 'a', title: 'Alpha' }),
      makeThread({ id: 'b', title: 'Bravo' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, { open: true, pane, onClose });
    const body = getByTestId('thread-picker');
    await fireEvent.keyDown(body, { key: 'ArrowDown' });
    await fireEvent.keyDown(body, { key: 'Enter' });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(pane.threadId).toBe('b');
  });

  it('Escape calls onClose', async () => {
    await seedThreads([makeThread({ id: 'a', title: 'Alpha' })]);
    const onClose = vi.fn();
    const pane = makePane();
    const { container } = render(UnifiedThreadPicker, { open: true, pane, onClose });
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    await fireEvent.keyDown(backdrop, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// ---- Click interactions ----

describe('<UnifiedThreadPicker> — interactions', () => {
  it('clicking a hit switches the pane and closes the dialog', async () => {
    await seedThreads([makeThread({ id: 'a', title: 'Alpha' })]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, { open: true, pane, onClose });
    await fireEvent.click(getByTestId('thread-picker-hit-a'));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(pane.threadId).toBe('a');
  });

  it('clicking the backdrop closes but clicking the dialog does not', async () => {
    await seedThreads([makeThread({ id: 'a', title: 'Alpha' })]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId, container } = render(UnifiedThreadPicker, { open: true, pane, onClose });
    // Clicking the dialog body should not dismiss — Modal only closes
    // when the click originates on the backdrop itself.
    await fireEvent.click(getByTestId('thread-picker'));
    expect(onClose).not.toHaveBeenCalled();
    const backdrop = container.querySelector('[data-modal-backdrop]') as HTMLElement;
    await fireEvent.click(backdrop);
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// ---- Status dot rendering ----

describe('<UnifiedThreadPicker> — status dots', () => {
  it('renders a status dot for non-idle threads and omits it when idle', async () => {
    await seedThreads([
      makeThread({ id: 'idle', title: 'Quiet' }),
      makeThread({ id: 'busy', title: 'Busy' }),
    ]);
    markThreadRunning('busy');
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    const busyRow = getByTestId('thread-picker-hit-busy');
    const idleRow = getByTestId('thread-picker-hit-idle');
    // The busy row carries the explicit status dot.
    expect(busyRow.querySelector('[data-testid="thread-picker-status-dot"]')).not.toBeNull();
    expect(
      busyRow.querySelector('[data-testid="thread-picker-status-dot"]')?.getAttribute('data-status'),
    ).toBe('running');
    // The idle row doesn't.
    expect(idleRow.querySelector('[data-testid="thread-picker-status-dot"]')).toBeNull();
  });

  it('uses the shared sidebar pill presentation for running discussion threads', async () => {
    await seedThreads([
      makeThread({ id: 'discussion', title: 'Roundtable', mode: 'discussion' }),
    ]);
    markThreadRunning('discussion');
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });

    const dot = getByTestId('thread-picker-status-dot');
    expect(dot.getAttribute('aria-label')).toBe('Discussing');
    expect(dot.classList.contains('border-info')).toBe(true);
    expect(dot.classList.contains('bg-transparent')).toBe(true);
    expect(dot.classList.contains('animate-pulse')).toBe(false);
  });

  it('renders durable Interrupted from the thread row without a live event', async () => {
    await seedThreads([
      makeThread({ id: 'interrupted', title: 'Stopped', hasIncompleteTurn: true }),
    ]);
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });

    const dot = getByTestId('thread-picker-status-dot');
    expect(dot.getAttribute('data-status')).toBe('interrupted');
    expect(dot.getAttribute('aria-label')).toBe('Interrupted');
    expect(dot.classList.contains('bg-warning')).toBe(true);
  });

  it('lets live status override durable Interrupted', async () => {
    await seedThreads([
      makeThread({ id: 'interrupted', title: 'Stopped', hasIncompleteTurn: true }),
    ]);
    markThreadRunning('interrupted');
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });

    const dot = getByTestId('thread-picker-status-dot');
    expect(dot.getAttribute('data-status')).toBe('running');
    expect(dot.getAttribute('aria-label')).toBe('Working');
  });
});

// ---- Project basename + worktree badge ----

describe('<UnifiedThreadPicker> — row metadata', () => {
  it('renders the project basename on the right of each row', async () => {
    await seedThreads([
      makeThread({ id: 'a', title: 'Alpha', projectPath: '/home/me/work/alpha' }),
    ]);
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    expect(getByTestId('thread-picker-hit-a').textContent).toContain('alpha');
  });

  it('renders a worktree badge when the thread was created in a worktree', async () => {
    await seedThreads([
      makeThread({
        id: 'wt',
        title: 'Worktree thread',
        projectPath: '/home/me/work/alpha',
        worktreePath: '/home/me/work/alpha/.worktrees/foo',
      }),
    ]);
    const pane = makePane();
    const { getByTestId } = render(UnifiedThreadPicker, {
      open: true,
      pane,
      onClose: vi.fn(),
    });
    expect(getByTestId('thread-picker-hit-wt').textContent).toContain('worktree');
  });
});
