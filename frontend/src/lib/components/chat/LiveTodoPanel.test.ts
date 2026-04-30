import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import LiveTodoPanel from './LiveTodoPanel.svelte';
import { LIVE_TODO_AUTOHIDE_MS, __resetLiveTodoUiPrefsForTest } from '../../stores/thread.svelte';
import type { TodoStep } from '../../types/events';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';

describe('<LiveTodoPanel>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetBindingMocks();
    __resetLiveTodoUiPrefsForTest();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it('renders nothing when there is no live todo list', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    expect(queryByTestId('live-todo-panel')).toBeNull();
  });

  it('shows count summary collapsed by default and expands the list on click', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'Refactor parser', status: 'inProgress' },
      { step: 'Update tests', status: 'pending' },
      { step: 'Wire UI', status: 'completed' },
      { step: 'Ship it', status: 'pending' },
    ]);

    const { getByTestId, queryByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    expect(getByTestId('live-todo-panel')).toBeInTheDocument();
    expect(getByTestId('live-todo-counts').textContent).toBe(
      '1 in progress, 2 pending, 1 completed',
    );
    expect(queryByTestId('live-todo-list')).toBeNull();

    await fireEvent.click(getByTestId('live-todo-toggle'));
    await tick();

    const list = getByTestId('live-todo-list');
    expect(list).toBeInTheDocument();
    const items = list.querySelectorAll('li');
    // 4 plan rows + no "Show more…" since count <= 5.
    expect(items).toHaveLength(4);
    // First row should be the in-progress step (sorted to top).
    expect(items[0].textContent).toContain('Refactor parser');
    // Last row should be the completed step (sorted to bottom).
    expect(items[items.length - 1].textContent).toContain('Wire UI');
  });

  it('sorts steps in-progress -> pending -> completed and preserves original order within each bucket', async () => {
    const pane = await buildPane();
    // 5-item todo list stays under the truncation limit so the sort
    // ordering is the only thing under test here.
    const steps: TodoStep[] = [
      { step: 'one', status: 'completed' },
      { step: 'two', status: 'pending' },
      { step: 'three', status: 'inProgress' },
      { step: 'four', status: 'completed' },
      { step: 'five', status: 'pending' },
    ];
    pane.setLiveTodo(steps);
    pane.toggleLiveTodoExpanded();

    const { getByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    const items = Array.from(getByTestId('live-todo-list').querySelectorAll('li'));
    const labels = items.map((li) => li.textContent?.trim() ?? '');
    // Expected order: three (inProgress); two, five (pending in
    // original order); one, four (completed in original order).
    expect(labels[0]).toContain('three');
    expect(labels[1]).toContain('two');
    expect(labels[2]).toContain('five');
    expect(labels[3]).toContain('one');
    expect(labels[4]).toContain('four');
  });

  it('truncates to 5 with a Show-more button that reveals the rest', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 's1', status: 'pending' },
      { step: 's2', status: 'pending' },
      { step: 's3', status: 'pending' },
      { step: 's4', status: 'pending' },
      { step: 's5', status: 'pending' },
      { step: 's6', status: 'pending' },
      { step: 's7', status: 'pending' },
    ]);
    pane.toggleLiveTodoExpanded();

    const { getByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    expect(getByTestId('live-todo-show-more').textContent).toContain('Show 2 more');

    let liItems = getByTestId('live-todo-list').querySelectorAll('li');
    // 5 plan rows + 1 wrapper li for the "Show more…" button.
    expect(liItems.length).toBe(6);

    await fireEvent.click(getByTestId('live-todo-show-more'));
    await tick();

    liItems = getByTestId('live-todo-list').querySelectorAll('li');
    expect(liItems.length).toBe(7);
  });

  it('does not render Show-more at exactly 5 items (the truncation limit)', async () => {
    // Boundary: a todo list that exactly fills the truncation limit must
    // not show the "Show more…" affordance. A regression that flipped
    // `> TRUNCATION_LIMIT` to `>= TRUNCATION_LIMIT` would surface here.
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 's1', status: 'pending' },
      { step: 's2', status: 'pending' },
      { step: 's3', status: 'pending' },
      { step: 's4', status: 'pending' },
      { step: 's5', status: 'pending' },
    ]);
    pane.toggleLiveTodoExpanded();

    const { getByTestId, queryByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    expect(queryByTestId('live-todo-show-more')).toBeNull();
    expect(getByTestId('live-todo-list').querySelectorAll('li').length).toBe(5);
  });

  it('shows "Show 1 more" at the first item past the truncation limit', async () => {
    // Boundary the other way: 6 items must render 5 plus a "Show 1
    // more" affordance.
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 's1', status: 'pending' },
      { step: 's2', status: 'pending' },
      { step: 's3', status: 'pending' },
      { step: 's4', status: 'pending' },
      { step: 's5', status: 'pending' },
      { step: 's6', status: 'pending' },
    ]);
    pane.toggleLiveTodoExpanded();

    const { getByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    expect(getByTestId('live-todo-show-more').textContent).toContain('Show 1 more');
  });

  it('auto-hides the panel after every step is completed', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'a', status: 'completed' },
      { step: 'b', status: 'completed' },
    ]);

    const { queryByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    expect(queryByTestId('live-todo-panel')).toBeInTheDocument();
    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS - 1);
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeInTheDocument();
    vi.advanceTimersByTime(2);
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeNull();
  });

  it('cancels a pending auto-hide when a fresh in-progress snapshot arrives', async () => {
    // Regression guard: if an all-completed snapshot scheduled a timer
    // and a follow-up snapshot adds a new in-progress step, the timer
    // must not fire and clear the (now non-empty) todo list.
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'a', status: 'completed' },
      { step: 'b', status: 'completed' },
    ]);

    const { queryByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeInTheDocument();

    // Halfway through the auto-hide window, the agent emits a new
    // step that flips the todo list back to in-progress.
    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS / 2);
    pane.setLiveTodo([
      { step: 'a', status: 'completed' },
      { step: 'b', status: 'completed' },
      { step: 'c', status: 'inProgress' },
    ]);
    await tick();

    // Past the original timer's deadline — the panel must still be
    // mounted because setLiveTodo cancels the pending timeout.
    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS);
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeInTheDocument();
  });

  it('clearLiveTodo cancels a pending auto-hide timer', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'a', status: 'completed' },
    ]);

    const { queryByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeInTheDocument();

    pane.clearLiveTodo();
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeNull();

    // Even after the original deadline elapses, no orphan callback
    // should fire (advancing past the deadline is a no-op).
    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS * 2);
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeNull();
  });

  it('does not render an empty todo list from the live state', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([]);

    const { queryByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    expect(queryByTestId('live-todo-panel')).toBeNull();
  });

  it('shows the in-progress step preview in the collapsed header', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'one', status: 'completed' },
      { step: 'two', status: 'inProgress' },
      { step: 'three', status: 'pending' },
    ]);

    const { getByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    expect(getByTestId('live-todo-in-progress-preview').textContent?.trim()).toBe('two');
  });

  it('truncates a long in-progress step in the collapsed header', async () => {
    const long = 'A'.repeat(120);
    const pane = await buildPane();
    pane.setLiveTodo([{ step: long, status: 'inProgress' }]);

    const { getByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    const preview = getByTestId('live-todo-in-progress-preview').textContent?.trim() ?? '';
    expect(preview.endsWith('…')).toBe(true);
    expect(preview.length).toBeLessThan(long.length);
  });

  it('drops completed steps the previous all-completed cycle already cleared', async () => {
    // Simulates the agent re-emitting the full todo list on each
    // update: after a cycle hits all-completed and auto-hides, the
    // next snapshot still includes those same completed rows. The
    // panel must filter them so the new logical todo cycle starts fresh.
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'old-1', status: 'completed' },
      { step: 'old-2', status: 'completed' },
    ]);

    const { queryByTestId, getByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeInTheDocument();

    // Wait past the auto-hide deadline so the cleared-cycle snapshot
    // is taken and the panel unmounts.
    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS + 1);
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeNull();

    // Agent re-emits with the prior two completed plus a new
    // in-progress task. Only the new task should render.
    pane.setLiveTodo([
      { step: 'old-1', status: 'completed' },
      { step: 'old-2', status: 'completed' },
      { step: 'new-1', status: 'inProgress' },
    ]);
    pane.toggleLiveTodoExpanded();
    await tick();

    const list = getByTestId('live-todo-list');
    const items = list.querySelectorAll('li');
    expect(items.length).toBe(1);
    expect(items[0].textContent).toContain('new-1');
    // Counts header reflects the filtered todo list, not the wire size.
    expect(getByTestId('live-todo-counts').textContent).toBe(
      '1 in progress, 0 pending, 0 completed',
    );
  });

  it('clearLiveTodo resets the cleared-cycle set so identical completed items can reappear', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([{ step: 'old-1', status: 'completed' }]);
    const { queryByTestId } = render(LiveTodoPanel, { props: { pane } });
    await tick();

    // Trigger the auto-hide cycle so 'old-1' lands in the cleared set.
    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS + 1);
    await tick();
    expect(queryByTestId('live-todo-panel')).toBeNull();

    // Explicit clear must wipe the cleared set so the SAME step text
    // can render again as completed in a fresh cycle.
    pane.clearLiveTodo();
    pane.setLiveTodo([{ step: 'old-1', status: 'completed' }]);
    await tick();

    // The single completed step is back on screen (will auto-hide
    // again, but the assertion captures the rendered frame).
    expect(queryByTestId('live-todo-panel')).toBeInTheDocument();
  });
});
