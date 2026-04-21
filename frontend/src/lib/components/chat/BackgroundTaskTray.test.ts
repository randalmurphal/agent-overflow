import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';
import BackgroundTaskTray from './BackgroundTaskTray.svelte';
import type { Item } from '../../types/models';

/**
 * Test fixtures mirror the real `Item` shape produced by the backend:
 * launches land as `tool_call`, completions as `tool_completion` with
 * `completionOf` pointing at the launch id. `status` uses the real
 * vocabulary from the store (`streaming | running | completed |
 * errored | declined`).
 */
function item(overrides: Partial<Item> & { id: string }): Item {
  return {
    threadId: 't1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'running',
    summary: '',
    highlightedContent: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

describe('<BackgroundTaskTray>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(1_000_000));
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it('renders nothing when no items match', () => {
    const { container, queryByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          // Not flagged as background — should be ignored.
          item({ id: 'a', status: 'running' }),
          // Flagged but already completed with no matching launch + past retention.
          item({
            id: 'b',
            kind: 'tool_completion',
            isBackground: true,
            status: 'completed',
            completionOf: 'ghost',
            createdAt: 0,
          }),
        ],
      },
    });
    expect(queryByTestId('background-task-tray')).toBeNull();
    expect(container.textContent?.trim() ?? '').toBe('');
  });

  it('renders the header with a count when there is at least one background task', () => {
    const { getByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({
            id: 'launch-a',
            isBackground: true,
            status: 'running',
            summary: 'Bash',
            createdAt: 1_000_000 - 5_000,
          }),
          item({
            id: 'launch-b',
            isBackground: true,
            status: 'running',
            summary: 'Grep',
            createdAt: 1_000_000 - 2_000,
          }),
        ],
      },
    });
    expect(getByTestId('background-task-tray')).toBeInTheDocument();
    expect(getByTestId('background-task-tray-count').textContent).toBe('2');
  });

  it('shows the pulsing dot when at least one task is running and hides it otherwise', () => {
    const { getByTestId, queryByTestId, rerender } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({
            id: 'launch-a',
            isBackground: true,
            status: 'running',
            summary: 'Bash',
            createdAt: 1_000_000 - 1_000,
          }),
        ],
      },
    });
    expect(getByTestId('background-task-tray-pulse')).toBeInTheDocument();

    // Swap in a lone recent completion — no running task, no pulse.
    rerender({
      items: [
        item({
          id: 'done-a',
          kind: 'tool_completion',
          isBackground: true,
          status: 'completed',
          completionOf: 'launch-a',
          summary: 'Bash',
          createdAt: 1_000_000 - 500,
        }),
      ],
    });
    expect(queryByTestId('background-task-tray-pulse')).toBeNull();
    // The completion is still within the 2 s retention window, so the
    // tray keeps rendering a row.
    expect(getByTestId('background-task-tray-count').textContent).toBe('1');
  });

  it('prunes a standalone completion 2 seconds after its createdAt', async () => {
    const { getByTestId, queryByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({
            id: 'done-a',
            kind: 'tool_completion',
            isBackground: true,
            status: 'completed',
            completionOf: 'ghost-launch',
            summary: 'Bash',
            // createdAt == now; retention window is fresh.
            createdAt: 1_000_000,
          }),
        ],
      },
    });
    expect(getByTestId('background-task-tray')).toBeInTheDocument();
    expect(getByTestId('background-task-tray-count').textContent).toBe('1');

    // Advance past the 2 s retention window — the 1 s interval fires
    // twice, flushing the derivation twice with new `now` values.
    await vi.advanceTimersByTimeAsync(2_500);

    expect(queryByTestId('background-task-tray')).toBeNull();
  });

  it('removes a launch+completion pair 2 seconds after the completion lands', async () => {
    // Regression: for backgrounded tools the launch row stays
    // `status='running'` forever (spec invariant — the backend never
    // flips it; the completion is a sibling row). If retention only
    // prunes the completion from the tray's view, the launch gets
    // orphaned and re-renders as "Running" indefinitely. The tray
    // must drop the whole pair once the completion ages past the
    // retention window.
    const { getByTestId, queryByTestId, getAllByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({
            id: 'launch-a',
            isBackground: true,
            status: 'running',
            summary: 'Bash',
            createdAt: 1_000_000 - 10_000,
            updatedAt: 1_000_000 - 10_000,
          }),
          item({
            id: 'done-a',
            kind: 'tool_completion',
            isBackground: true,
            status: 'completed',
            completionOf: 'launch-a',
            summary: 'Bash -> done',
            createdAt: 1_000_000,
            updatedAt: 1_000_000,
          }),
        ],
      },
    });
    // Completion just landed — row shows as completed.
    const [row] = getAllByTestId('background-task-tray-row');
    expect(row.getAttribute('data-row-id')).toBe('launch-a');
    expect(getByTestId('background-task-tray-row-status').getAttribute('data-status')).toBe('completed');

    await vi.advanceTimersByTimeAsync(2_500);

    // Both the launch and the completion must be gone from the tray.
    expect(queryByTestId('background-task-tray')).toBeNull();
  });

  it('renders a declined completion with error styling, not as a green checkmark', () => {
    // Backend emits three terminal completion statuses — completed,
    // errored, declined — and ToolDecisionChip.svelte already
    // colours a declined run in error-red elsewhere in the UI. The
    // tray used to collapse declined into completed (green ✓); this
    // test pins the distinct rendering.
    const { getByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({
            id: 'launch-a',
            isBackground: true,
            status: 'running',
            summary: 'Bash',
            createdAt: 1_000_000 - 5_000,
            updatedAt: 1_000_000 - 5_000,
          }),
          item({
            id: 'done-a',
            kind: 'tool_completion',
            isBackground: true,
            status: 'declined',
            completionOf: 'launch-a',
            summary: 'Bash -> declined',
            createdAt: 1_000_000 - 100,
            updatedAt: 1_000_000 - 100,
          }),
        ],
      },
    });
    const status = getByTestId('background-task-tray-row-status');
    expect(status.getAttribute('data-status')).toBe('declined');
    expect(status.className).toContain('text-error');
    expect(status.textContent).toContain('Declined');
  });

  it('suppresses the elapsed label for an orphan completion (no launch)', () => {
    // An orphan completion has no matching launch in `items`, so
    // there's no meaningful "task started at" timestamp — counting
    // elapsed from the completion's createdAt would show a misleading
    // "0s" for a task that actually ran for minutes. The row still
    // renders (the user sees the result land) but the elapsed label
    // is hidden.
    const { getByTestId, queryByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({
            id: 'done-a',
            kind: 'tool_completion',
            isBackground: true,
            status: 'completed',
            completionOf: 'ghost-launch',
            summary: 'Bash',
            createdAt: 1_000_000 - 100,
          }),
        ],
      },
    });
    expect(getByTestId('background-task-tray-row')).toBeInTheDocument();
    expect(queryByTestId('background-task-tray-row-elapsed')).toBeNull();
  });

  it('clicking a row invokes onExpand with the item id', async () => {
    const onExpand = vi.fn();
    const { getAllByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({
            id: 'launch-a',
            isBackground: true,
            status: 'running',
            summary: 'Bash',
            createdAt: 1_000_000 - 3_000,
          }),
        ],
        onExpand,
      },
    });
    const rows = getAllByTestId('background-task-tray-row');
    expect(rows).toHaveLength(1);
    await fireEvent.click(rows[0]);
    expect(onExpand).toHaveBeenCalledTimes(1);
    expect(onExpand).toHaveBeenCalledWith('launch-a');
  });

  it('caps visible rows at three and shows the hidden count', () => {
    const { getAllByTestId, getByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({ id: 'a', isBackground: true, status: 'running', summary: 'A', createdAt: 1_000_000 - 1_000 }),
          item({ id: 'b', isBackground: true, status: 'running', summary: 'B', createdAt: 1_000_000 - 2_000 }),
          item({ id: 'c', isBackground: true, status: 'running', summary: 'C', createdAt: 1_000_000 - 3_000 }),
          item({ id: 'd', isBackground: true, status: 'running', summary: 'D', createdAt: 1_000_000 - 4_000 }),
        ],
      },
    });

    expect(getAllByTestId('background-task-tray-row')).toHaveLength(3);
    expect(getByTestId('background-task-tray-more').textContent).toContain('+1 more');
  });

  it('orders rows by latest activity first', () => {
    const { getAllByTestId } = render(BackgroundTaskTray, {
      props: {
        items: [
          item({ id: 'older', isBackground: true, status: 'running', summary: 'Older', createdAt: 1_000_000 - 4_000, updatedAt: 1_000_000 - 4_000 }),
          item({ id: 'newer', isBackground: true, status: 'running', summary: 'Newer', createdAt: 1_000_000 - 1_000, updatedAt: 1_000_000 - 1_000 }),
        ],
      },
    });

    const rows = getAllByTestId('background-task-tray-row');
    expect(rows[0]?.getAttribute('data-row-id')).toBe('newer');
    expect(rows[1]?.getAttribute('data-row-id')).toBe('older');
  });
});
