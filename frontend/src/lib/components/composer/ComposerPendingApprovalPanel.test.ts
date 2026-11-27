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
        { key: 'WEB-12', title: 'Login fails on Safari', changes: [{ line: 'Status → In Review' }, { line: 'Sprint → Sprint 15' }] },
        { key: 'WEB-18', title: 'Session cookie lost', changes: [{ line: 'Status → In Review' }, { line: 'description → new text (31 characters)', value: 'description' }] },
      ],
      comment: 'Ready for review.\nChecked on Safari 17.',
      values: [{ name: 'description', value: 'Steps:\n1. Open the login page.' }, { name: 'status', value: 'In Review' }],
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
      ['WEB-18 Session cookie lost', ['Status → In Review', 'description → new text (31 characters)']],
    ]);
    expect(getByTestId('tracker-write-approval-comment').textContent).toContain('Ready for review.\nChecked on Safari 17.');
    expect(queryByTestId('approval-allow-session')).toBeNull();
    expect(queryByTestId('approval-cancel')).toBeNull();
    expect(queryByTestId('approval-edit-toggle')).toBeNull();
    await fireEvent.click(getByTestId('approval-allow'));
    await waitFor(() => expect(onResolve).toHaveBeenCalledTimes(1));
    expect(onResolve.mock.calls[0]![0]).toMatchObject({ requestId: 'tracker:call-1', decision: 'accept', occurrence: '7', shown: true });
  });

  it('approves without saying it was shown when the changes could not all be drawn', async () => {
    for (const input of [
      // A ticket the list cannot draw is not drawn as a smaller list.
      { ...(trackerWrite().input as object), tickets: [{ key: 'WEB-12', changes: [{ line: 'Status → In Review' }] }, { title: 'no key' }] },
      { ...(trackerWrite().input as object), tickets: [{ key: 'WEB-12', changes: [{ line: 'Status → In Review' }, { value: 'description' }] }] },
      { id: 'call-1', tickets: [], notes: ['Tue Nov 24, 15m: Standup', 7] },
      // Nothing to draw.
      { id: 'call-1', tickets: [] },
      'not an approval',
    ]) {
      const onResolve = vi.fn<(r: ApprovalResponse) => Promise<void>>(async () => {});
      const { getByTestId, unmount } = render(ComposerPendingApprovalPanel, {
        props: { approval: { ...trackerWrite(), input } as ApprovalRequest, count: 1, onResolve, onError: vi.fn() },
      });
      await fireEvent.click(getByTestId('approval-allow'));
      await waitFor(() => expect(onResolve).toHaveBeenCalledTimes(1));
      expect(onResolve.mock.calls[0]![0].shown, JSON.stringify(input)).toBeFalsy();
      unmount();
    }
  });

  it('says it was shown only for the prompt it drew, and never for another kind of approval', async () => {
    const onResolve = vi.fn<(r: ApprovalResponse) => Promise<void>>(async () => {});
    const { getByTestId, rerender } = render(ComposerPendingApprovalPanel, {
      props: { approval: trackerWrite(), count: 1, onResolve, onError: vi.fn() },
    });
    // The same request asked again is another prompt, drawn again.
    await rerender({ approval: { ...trackerWrite(), occurrence: '8' }, count: 1, onResolve, onError: vi.fn() });
    await fireEvent.click(getByTestId('approval-allow'));
    await waitFor(() => expect(onResolve).toHaveBeenCalledTimes(1));
    expect(onResolve.mock.calls[0]![0]).toMatchObject({ occurrence: '8', shown: true });

    const tool = vi.fn<(r: ApprovalResponse) => Promise<void>>(async () => {});
    const other = render(ComposerPendingApprovalPanel, {
      props: {
        approval: { ...trackerWrite(), requestId: 'call-9', kind: 'command', toolName: 'Bash', title: 'Run a command' } as ApprovalRequest,
        count: 1,
        onResolve: tool,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(other.getAllByTestId('approval-allow').at(-1)!);
    await waitFor(() => expect(tool).toHaveBeenCalledTimes(1));
    expect(tool.mock.calls[0]![0].shown).toBeUndefined();
  });

  it('names the connection beside a ticket two connections of one site change', () => {
    const approval = {
      ...trackerWrite(),
      input: {
        id: 'call-3',
        tickets: [
          { key: 'WEB-1', account: 'Work', changes: [{ line: 'Tue Nov 24, 1h 30m: Built the form' }] },
          { key: 'WEB-1', account: 'user2@example.com', changes: [{ line: 'Tue Nov 24, 30m: Paired' }] },
        ],
      },
    } as ApprovalRequest;
    const { getAllByTestId } = render(ComposerPendingApprovalPanel, { props: { approval, count: 1, onResolve: vi.fn(), onError: vi.fn() } });
    expect(getAllByTestId('tracker-write-approval-ticket').map((t) => t.firstElementChild?.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      'WEB-1 Work',
      'WEB-1 user2@example.com',
    ]);
    expect(getAllByTestId('tracker-write-approval-change').map((c) => c.textContent)).toEqual(['Tue Nov 24, 1h 30m: Built the form', 'Tue Nov 24, 30m: Paired']);
  });

  it('opens the whole of a value a line shows in part, from what the approval carries', async () => {
    const { getAllByTestId, getByTestId, queryByTestId } = render(ComposerPendingApprovalPanel, {
      props: { approval: trackerWrite(), count: 1, onResolve: vi.fn(), onError: vi.fn() },
    });
    // Only the line that shows its value in part offers it.
    expect(getAllByTestId('tracker-write-approval-show-all')).toHaveLength(1);
    expect(queryByTestId('tracker-write-approval-value')).toBeNull();
    await fireEvent.click(getByTestId('tracker-write-approval-show-all'));
    expect(getByTestId('tracker-write-approval-value').textContent).toBe('Steps:\n1. Open the login page.');
    await fireEvent.click(getByTestId('tracker-write-approval-show-all'));
    expect(queryByTestId('tracker-write-approval-value')).toBeNull();
  });

  it('is headed by its title, not the tool, with no second line saying it again', () => {
    const { getByTestId, getByText, queryAllByText } = render(ComposerPendingApprovalPanel, {
      props: { approval: trackerWrite(), count: 1, onResolve: vi.fn(), onError: vi.fn() },
    });
    expect(getByTestId('composer-pending-approval-heading').textContent).toBe('Change 2 tickets');
    expect(queryAllByText('ticket_update')).toHaveLength(0);
    expect(getByText('Change 2 tickets')).toBe(getByTestId('composer-pending-approval-heading'));
  });

  it("shows a time post's entries under their tickets and its open questions as notes", () => {
    const post = {
      ...trackerWrite(),
      toolName: 'time_draft',
      title: 'Log time for Tue Nov 24',
      input: {
        id: 'call-2',
        tickets: [
          { key: 'WEB-1', title: 'Checkout form', changes: [{ line: 'Tue Nov 24, 1h 30m: Built the form' }] },
          { key: 'WEB-3', changes: [{ line: "Tue Nov 24, 1h: Triaged (may already be logged, so it is not sent again; look at the day's logged time, and remove the entry if the time is there, or log the time by hand and then remove the entry)" }] },
        ],
        notes: ['Tue Nov 24, 15m: Standup (its ticket is not picked yet, so it is not posted)'],
      },
    } as ApprovalRequest;
    const { getByTestId, getAllByTestId, queryByTestId } = render(ComposerPendingApprovalPanel, {
      props: { approval: post, count: 1, onResolve: vi.fn(), onError: vi.fn() },
    });
    expect(getByTestId('composer-pending-approval-heading').textContent).toBe('Log time for Tue Nov 24');
    expect(getAllByTestId('tracker-write-approval-change').map((c) => c.textContent)).toEqual([
      'Tue Nov 24, 1h 30m: Built the form',
      "Tue Nov 24, 1h: Triaged (may already be logged, so it is not sent again; look at the day's logged time, and remove the entry if the time is there, or log the time by hand and then remove the entry)",
    ]);
    expect(getAllByTestId('tracker-write-approval-note').map((n) => n.textContent)).toEqual([
      'Tue Nov 24, 15m: Standup (its ticket is not picked yet, so it is not posted)',
    ]);
    expect(queryByTestId('tracker-write-approval-comment')).toBeNull();
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
