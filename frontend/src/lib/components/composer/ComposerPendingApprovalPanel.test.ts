import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import ComposerPendingApprovalPanel from './ComposerPendingApprovalPanel.svelte';
import type { ApprovalResponse } from '../../stores/bindings';
import type { ApprovalRequest } from '../../types/events';

// A tracker write naming several tickets, as the app raises it
// (internal/app/app_tracker_tools_host.go): its input is the change.
function trackerWrite(): ApprovalRequest {
  return {
    requestId: 'tracker:call-1',
    threadId: 'thread-1',
    toolName: 'ticket_update',
    title: 'Change 2 tickets',
    description: '',
    kind: 'tracker-write',
    occurrence: '7',
    availableDecisions: ['accept', 'decline'],
    input: {
      id: 'call-1',
      tickets: [
        { key: 'WEB-12', title: 'Login fails on Safari', changes: ['Status → In Review', 'Sprint → Sprint 15'] },
        { key: 'WEB-18', title: 'Session cookie lost', changes: ['Status → In Review'] },
      ],
      comment: 'Ready for review.\nChecked on Safari 17.',
    },
  } as ApprovalRequest;
}

describe('<ComposerPendingApprovalPanel> tracker write', () => {
  it('shows each ticket with its changes and the comment, and offers approve and decline alone', async () => {
    const onResolve = vi.fn<(r: ApprovalResponse) => Promise<void>>(async () => {});
    const { getByTestId, getAllByTestId, queryByTestId } = render(ComposerPendingApprovalPanel, {
      props: { approval: trackerWrite(), count: 1, onResolve, onError: vi.fn() },
    });
    const tickets = getAllByTestId('tracker-write-approval-ticket').map((t) => [
      t.firstElementChild?.textContent?.replace(/\s+/g, ' ').trim(),
      [...t.querySelectorAll('[data-testid="tracker-write-approval-change"]')].map((c) => c.textContent),
    ]);
    expect(tickets).toEqual([
      ['WEB-12 Login fails on Safari', ['Status → In Review', 'Sprint → Sprint 15']],
      ['WEB-18 Session cookie lost', ['Status → In Review']],
    ]);
    expect(getByTestId('tracker-write-approval-comment').textContent).toContain('Ready for review.\nChecked on Safari 17.');
    expect(queryByTestId('approval-allow-session')).toBeNull();
    expect(queryByTestId('approval-cancel')).toBeNull();
    expect(queryByTestId('approval-edit-toggle')).toBeNull();
    await fireEvent.click(getByTestId('approval-allow'));
    await waitFor(() => expect(onResolve).toHaveBeenCalledTimes(1));
    expect(onResolve.mock.calls[0]![0]).toMatchObject({ requestId: 'tracker:call-1', decision: 'accept', occurrence: '7' });
  });

  it('declines', async () => {
    const onResolve = vi.fn<(r: ApprovalResponse) => Promise<void>>(async () => {});
    const { getByTestId } = render(ComposerPendingApprovalPanel, {
      props: { approval: trackerWrite(), count: 1, onResolve, onError: vi.fn() },
    });
    await fireEvent.click(getByTestId('approval-deny'));
    await waitFor(() => expect(onResolve).toHaveBeenCalledTimes(1));
    expect(onResolve.mock.calls[0]![0]).toMatchObject({ requestId: 'tracker:call-1', decision: 'decline' });
  });
});
