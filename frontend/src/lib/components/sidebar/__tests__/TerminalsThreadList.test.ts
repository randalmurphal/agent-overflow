import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import TerminalsThreadList from '../TerminalsThreadList.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import { resetSidebarForTest } from '../../../stores/sidebar.svelte';
import {
  THREAD_PREVIEW_LIMIT,
  THREAD_REVEAL_INCREMENT,
} from '../../../utils/sidebarThreadLimits';
import type { Thread } from '../../../types/models';

// Stub ThreadRow so we exercise the list's own structure (empty state, rail,
// row count, Show More / Show Less) without ThreadRow's heavy store graph.
vi.mock('../ThreadRow.svelte', async () => ({
  default: (await import('../../../../test/mocks/StubThreadRow.svelte')).default,
}));

function mkTerminal(id: string): Thread {
  return {
    id,
    title: `term ${id}`,
    provider: 'claude',
    workspacePath: '/home/me',
    projectPath: '',
    mode: 'terminal',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function mkTerminals(n: number): Thread[] {
  return Array.from({ length: n }, (_, i) => mkTerminal(`t${i}`));
}

describe('<TerminalsThreadList>', () => {
  beforeEach(() => {
    resetSidebarForTest();
  });

  it('shows a "New Terminal" empty-state button when there are no terminals', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(TerminalsThreadList, {
      props: { terminals: [], pane, onNewTerminal: vi.fn() },
    });
    expect(getByTestId('terminals-thread-list-empty')).toHaveTextContent(/New Terminal/i);
  });

  it('empty-state button calls onNewTerminal', async () => {
    const onNewTerminal = vi.fn();
    const pane = createThreadPane();
    const { getByTestId } = render(TerminalsThreadList, {
      props: { terminals: [], pane, onNewTerminal },
    });
    await fireEvent.click(getByTestId('terminals-thread-list-empty'));
    expect(onNewTerminal).toHaveBeenCalledTimes(1);
  });

  it('renders a row per terminal under the indent rail, in order', () => {
    const pane = createThreadPane();
    const { getByTestId, getAllByTestId } = render(TerminalsThreadList, {
      props: { terminals: [mkTerminal('a'), mkTerminal('b')], pane, onNewTerminal: vi.fn() },
    });
    expect(getByTestId('terminals-thread-list')).toBeInTheDocument();
    const rows = getAllByTestId('stub-thread-row');
    expect(rows.map((r) => r.getAttribute('data-thread-id'))).toEqual(['a', 'b']);
  });

  it('truncates to the preview limit and reveals more on "Show More" / "Show Less"', async () => {
    const pane = createThreadPane();
    const total = THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT + 1;
    const { getByTestId, getAllByTestId, queryByTestId } = render(TerminalsThreadList, {
      props: { terminals: mkTerminals(total), pane, onNewTerminal: vi.fn() },
    });

    // Only the preview slice is visible at first.
    expect(getAllByTestId('stub-thread-row')).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(getByTestId('terminals-thread-list-show-more')).toBeInTheDocument();
    // Nothing to collapse yet.
    expect(queryByTestId('terminals-thread-list-show-less')).toBeNull();

    await fireEvent.click(getByTestId('terminals-thread-list-show-more'));

    // One reveal increment more rows, and Show Less now appears.
    expect(getAllByTestId('stub-thread-row')).toHaveLength(
      THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT,
    );
    expect(getByTestId('terminals-thread-list-show-less')).toBeInTheDocument();

    await fireEvent.click(getByTestId('terminals-thread-list-show-less'));

    // Collapsed back to the preview.
    expect(getAllByTestId('stub-thread-row')).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(queryByTestId('terminals-thread-list-show-less')).toBeNull();
  });

  it('shows no reveal controls when within the preview limit', () => {
    const pane = createThreadPane();
    const { queryByTestId, getAllByTestId } = render(TerminalsThreadList, {
      props: { terminals: mkTerminals(THREAD_PREVIEW_LIMIT), pane, onNewTerminal: vi.fn() },
    });
    expect(getAllByTestId('stub-thread-row')).toHaveLength(THREAD_PREVIEW_LIMIT);
    expect(queryByTestId('terminals-thread-list-show-more')).toBeNull();
    expect(queryByTestId('terminals-thread-list-show-less')).toBeNull();
  });
});
