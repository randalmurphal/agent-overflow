import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import TerminalTabStrip from './TerminalTabStrip.svelte';
import {
  createThreadTerminalState,
  type ThreadTerminalStateHandle,
} from './terminalStore.svelte';
import type { TerminalSessionSummary } from '../../types/terminal';

// TerminalTabStrip is purely presentational: props in, callbacks out. It never
// touches the Wails bindings (TerminalSurface owns the RefreshTerminal call), so
// we render it in isolation with a real (but headless) terminal-state handle and
// spy callbacks — no xterm, no binding mocks. Omitting `workspacePath` keeps the
// EditorLink (and its bindings) out of the tree.

function makeSummary(terminalID: string): TerminalSessionSummary {
  return {
    terminalID,
    threadID: 'thread-A',
    shell: '/bin/bash',
    cwd: '/tmp',
    rows: 24,
    cols: 80,
    pid: 1,
    startedAt: 0,
    running: true,
    exitCode: 0,
    exitReason: '',
  };
}

function handleWithTabs(...ids: string[]): ThreadTerminalStateHandle {
  const handle = createThreadTerminalState();
  for (const id of ids) handle.addTab(makeSummary(id));
  return handle;
}

const noop = () => {};

afterEach(() => cleanup());

describe('TerminalTabStrip refresh affordance', () => {
  it('renders the refresh button and invokes onRefresh on click when a terminal is open', async () => {
    const onRefresh = vi.fn();
    const { getByTestId } = render(TerminalTabStrip, {
      handle: handleWithTabs('t1'),
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
      onRefresh,
    });

    await fireEvent.click(getByTestId('terminal-refresh'));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it('hides the refresh button when there are no tabs to refresh', () => {
    const { queryByTestId } = render(TerminalTabStrip, {
      handle: handleWithTabs(),
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
      onRefresh: vi.fn(),
    });

    expect(queryByTestId('terminal-refresh')).toBeNull();
  });

  it('hides the refresh button when the host does not wire onRefresh', () => {
    const { queryByTestId } = render(TerminalTabStrip, {
      handle: handleWithTabs('t1'),
      onOpen: noop,
      onClose: noop,
      onSelect: noop,
    });

    expect(queryByTestId('terminal-refresh')).toBeNull();
  });
});
