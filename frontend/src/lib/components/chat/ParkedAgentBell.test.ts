import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import ParkedAgentBell from './ParkedAgentBell.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { Item } from '../../types/models';

function bell(meta: Record<string, unknown>): Item {
  return {
    id: 'task-notification:task-agent:u1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 4,
    kind: 'notification',
    role: 'system',
    status: 'completed',
    summary: 'Agent "Spike agent" reported and is waiting on 2 background commands',
    meta: JSON.stringify({ task_id: 'task-agent', kind: 'parked_agent', parked_commands: 2, ...meta }),
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}

const report: Item = {
  id: 'text:0:agent:1',
  threadId: 'thread-1',
  turnIndex: 0,
  itemIndex: 3,
  kind: 'assistant_text',
  role: 'assistant',
  status: 'completed',
  summary: 'Found the race in **fork_moves.go**.\n\nThe log is keyed by transaction.',
  parentId: 'agent',
  createdAt: 0,
  updatedAt: 0,
} as Item;

describe('<ParkedAgentBell>', () => {
  it('shows the bell line and the report head at rest, then the full report on demand, loaded once', async () => {
    const read = setBindingMock('GetThreadItem', vi.fn(async () => report));
    const { getByTestId, queryByTestId } = render(ParkedAgentBell, {
      props: { item: bell({ parked_report_item_id: report.id, parked_report_preview: 'Found the race in **fork_moves.go**.' }) },
    });
    expect(getByTestId('parked-agent-bell').textContent).toContain('reported and is waiting on 2 background commands');
    expect(getByTestId('parked-agent-bell-preview').textContent).toBe('Found the race in **fork_moves.go**.');
    expect(queryByTestId('parked-agent-bell-report')).toBeNull();
    expect(read).not.toHaveBeenCalled();

    await fireEvent.click(getByTestId('parked-agent-bell-report-toggle'));
    await waitFor(() => expect(getByTestId('parked-agent-bell-report')).toBeInTheDocument());
    expect(read).toHaveBeenCalledWith('thread-1', report.id);
    expect(getByTestId('parked-agent-bell-report').textContent).toContain('The log is keyed by transaction.');
    expect(getByTestId('parked-agent-bell')).toHaveAttribute('data-expanded', 'true');

    await fireEvent.click(getByTestId('parked-agent-bell-report-toggle'));
    await waitFor(() => expect(queryByTestId('parked-agent-bell-report')).toBeNull());
    expect(getByTestId('parked-agent-bell-preview')).toBeInTheDocument();
    await fireEvent.click(getByTestId('parked-agent-bell-report-toggle'));
    await waitFor(() => expect(getByTestId('parked-agent-bell-report')).toBeInTheDocument());
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('renders only the line when the agent had written no report', () => {
    const read = setBindingMock('GetThreadItem', vi.fn(async () => report));
    const { getByTestId, queryByTestId } = render(ParkedAgentBell, { props: { item: bell({}) } });
    expect(getByTestId('parked-agent-bell').textContent).toContain('reported and is waiting');
    expect(queryByTestId('parked-agent-bell-report-toggle')).toBeNull();
    expect(read).not.toHaveBeenCalled();
  });

  it('shows a failed report load and retries on the next click', async () => {
    const read = setBindingMock('GetThreadItem', vi.fn()
      .mockRejectedValueOnce(new Error('backend unreachable'))
      .mockResolvedValueOnce(report));
    const { getByTestId, queryByTestId } = render(ParkedAgentBell, {
      props: { item: bell({ parked_report_item_id: report.id, parked_report_preview: 'Found the race.' }) },
    });
    await fireEvent.click(getByTestId('parked-agent-bell-report-toggle'));
    await waitFor(() => expect(getByTestId('parked-agent-bell-error').textContent).toContain('backend unreachable'));
    expect(queryByTestId('parked-agent-bell-report')).toBeNull();

    await fireEvent.click(getByTestId('parked-agent-bell-report-toggle'));
    await waitFor(() => expect(getByTestId('parked-agent-bell-report')).toBeInTheDocument());
    expect(queryByTestId('parked-agent-bell-error')).toBeNull();
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('reports a report row that no longer exists instead of opening nothing', async () => {
    setBindingMock('GetThreadItem', vi.fn(async () => null));
    const { getByTestId, queryByTestId } = render(ParkedAgentBell, {
      props: { item: bell({ parked_report_item_id: 'gone', parked_report_preview: 'Found the race.' }) },
    });
    await fireEvent.click(getByTestId('parked-agent-bell-report-toggle'));
    await waitFor(() => expect(getByTestId('parked-agent-bell-error').textContent).toContain('no longer in this thread'));
    expect(queryByTestId('parked-agent-bell-report')).toBeNull();
  });
});
