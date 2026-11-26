// A pull request row's ticket chips: one per ticket the row names, each
// opening its ticket without opening the row.

import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import PullRow from './PullRow.svelte';
import type { PullRowView } from './pullRowView';

const PULL: PullRowView = {
  key: 'k5', forge: 'github', number: 5, title: 'WEB-1 Retry cursor', url: '', state: 'open', group: 'waiting',
  label: 'Open', tone: 'muted', projectName: 'Repo', author: 'alice', viewerIsAuthor: false, age: '2h', times: '', draft: false,
  crossRepository: false, ci: '', approvals: null, unresolved: null, projectId: 'p1',
  tickets: [
    { key: 'WEB-1', connectionId: 'c1', issueId: '10101', title: 'Retry cursor' },
    { key: 'WEB-77', connectionId: 'c1' },
  ],
};

describe('the ticket chips', () => {
  it('shows a chip per ticket named, titled with the ticket, that opens it and not the row', async () => {
    const onOpen = vi.fn();
    const onTicket = vi.fn();
    const view = render(PullRow, { row: PULL, selected: false, onOpen, onTicket });
    const chips = view.getAllByTestId('pull-ticket');
    expect(chips.map((c) => c.textContent)).toEqual(['WEB-1', 'WEB-77']);
    expect(chips[0].getAttribute('title')).toBe('WEB-1 Retry cursor');
    expect(chips[1].getAttribute('title')).toBe('Open WEB-77');
    await fireEvent.click(chips[1]);
    expect(onTicket).toHaveBeenCalledWith(PULL, PULL.tickets[1]);
    expect(onOpen).not.toHaveBeenCalled();
    view.unmount();
  });

  it('adds nothing to a row that names no ticket', () => {
    const view = render(PullRow, { row: { ...PULL, tickets: [] }, selected: false, onOpen: () => {}, onTicket: () => {} });
    expect(view.queryByTestId('pull-tickets')).toBeNull();
    view.unmount();
  });
});
