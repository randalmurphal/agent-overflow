import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

// Stub ThreadRow so we exercise the group's own structure (row, chevron,
// row count, empty state) without ThreadRow's heavy store graph. The stub
// also stands in for the ThreadRow rendered inside TerminalsThreadList.
vi.mock('../ThreadRow.svelte', async () => ({
  default: (await import('../../../../test/mocks/StubThreadRow.svelte')).default,
}));

import TerminalsGroup from '../TerminalsGroup.svelte';
import {
  resetSidebarForTest,
  isTerminalsGroupExpanded,
} from '../../../stores/sidebar.svelte';
import type { Thread } from '../../../types/models';

function makeTerminal(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'term-1',
    title: 'home',
    provider: 'claude',
    workspacePath: '/home/me',
    projectPath: '',
    mode: 'terminal',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

beforeEach(() => {
  resetSidebarForTest();
});

describe('<TerminalsGroup>', () => {
  it('renders a project-style row titled Terminals with a reachable +terminal', () => {
    const { getByTestId, getByText } = render(TerminalsGroup, {
      props: { terminals: [], pane: null, onNewTerminal: vi.fn() },
    });

    expect(getByTestId('sidebar-terminals-group')).toBeInTheDocument();
    expect(getByTestId('sidebar-terminals-row')).toBeInTheDocument();
    expect(getByText('Terminals')).toBeInTheDocument();
    // The global create button is always reachable.
    expect(getByTestId('sidebar-new-terminal-global')).toBeInTheDocument();
  });

  it('shows the empty-state New Terminal button and no rows when expanded + empty', () => {
    const { getByTestId, queryByTestId } = render(TerminalsGroup, {
      props: { terminals: [], pane: null, onNewTerminal: vi.fn() },
    });

    // Expanded by default → the empty-state "New Terminal" button, no rows.
    expect(getByTestId('terminals-thread-list-empty')).toBeInTheDocument();
    expect(queryByTestId('stub-thread-row')).toBeNull();
  });

  it('renders one row per terminal, in order, when expanded', () => {
    const { getAllByTestId, queryByTestId } = render(TerminalsGroup, {
      props: {
        terminals: [makeTerminal({ id: 'a' }), makeTerminal({ id: 'b' })],
        pane: null,
        onNewTerminal: vi.fn(),
      },
    });

    const rows = getAllByTestId('stub-thread-row');
    expect(rows.map((r) => r.getAttribute('data-thread-id'))).toEqual(['a', 'b']);
    // No empty-state button when there are rows.
    expect(queryByTestId('terminals-thread-list-empty')).toBeNull();
  });

  it('global +terminal calls onNewTerminal with no project id (home terminal)', async () => {
    const onNewTerminal = vi.fn();
    const { getByTestId } = render(TerminalsGroup, {
      props: { terminals: [], pane: null, onNewTerminal },
    });

    await fireEvent.click(getByTestId('sidebar-new-terminal-global'));

    expect(onNewTerminal).toHaveBeenCalledTimes(1);
    // No argument → ProjectsSection.handleNewTerminal forwards `undefined`,
    // and the backend roots the terminal at home.
    expect(onNewTerminal.mock.calls[0]).toHaveLength(0);
  });

  it('chevron toggles the persisted collapse state and hides the rows', async () => {
    const { getByTestId, getAllByTestId, queryByTestId } = render(TerminalsGroup, {
      props: { terminals: [makeTerminal()], pane: null, onNewTerminal: vi.fn() },
    });

    expect(isTerminalsGroupExpanded()).toBe(true);
    expect(getAllByTestId('stub-thread-row')).toHaveLength(1);

    await fireEvent.click(getByTestId('sidebar-terminals-chevron'));
    await tick();

    // Store flipped → rows gone, but the row + global button remain reachable.
    expect(isTerminalsGroupExpanded()).toBe(false);
    expect(queryByTestId('stub-thread-row')).toBeNull();
    expect(getByTestId('sidebar-terminals-group')).toBeInTheDocument();
    expect(getByTestId('sidebar-terminals-chevron').getAttribute('aria-expanded')).toBe(
      'false',
    );
  });

  it('collapsing hides the empty-state button too (collapsed + zero terminals)', async () => {
    const { getByTestId, queryByTestId } = render(TerminalsGroup, {
      props: { terminals: [], pane: null, onNewTerminal: vi.fn() },
    });

    // Expanded + empty → empty-state button visible.
    expect(getByTestId('terminals-thread-list-empty')).toBeInTheDocument();

    await fireEvent.click(getByTestId('sidebar-terminals-chevron'));
    await tick();

    // Collapsed → the empty-state button is gone too, but the row header +
    // global create button stay reachable.
    expect(queryByTestId('terminals-thread-list-empty')).toBeNull();
    expect(getByTestId('sidebar-new-terminal-global')).toBeInTheDocument();
  });
});
