import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';
import BackgroundTaskTray from './BackgroundTaskTray.svelte';
import type { Item } from '../../types/models';

/**
 * Tests operate against the Item type extended by the schema agent —
 * `status`, `isBackground`, and `completionOfItemId` are optional fields
 * on the Item record and fed through by the store/adapter layer. The
 * component tolerates missing fields by design, so stubs here only set
 * what each test needs.
 */
type BgItem = Item & {
  status?: 'running' | 'completed' | 'failed';
  isBackground?: boolean;
  completionOfItemId?: string;
};

function item(overrides: Partial<BgItem> & { id: string }): BgItem {
  return {
    threadId: 't1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_use',
    role: 'assistant',
    summary: '',
    createdAt: 0,
    ...overrides,
  } as BgItem;
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
            isBackground: true,
            status: 'completed',
            completionOfItemId: 'ghost',
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
          isBackground: true,
          status: 'completed',
          completionOfItemId: 'launch-a',
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
            isBackground: true,
            status: 'completed',
            completionOfItemId: 'ghost-launch',
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
});
