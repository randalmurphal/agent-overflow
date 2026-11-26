import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import TicketHistory from './TicketHistory.svelte';
import { DetailView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';

describe('<TicketHistory>', () => {
  it('marks a change an agent of a thread here made with the thread', () => {
    const detail = DetailView.createFrom({
      card: { id: '10012', key: 'WEB-12', title: 'Login fails on Safari' },
      history: [
        { at: 2_000, field: 'Status', from: 'In Progress', to: 'In Review', author: { id: 'acc-user1', name: 'User 1' }, agent: { id: 'thread-1', title: 'Fix the Safari login' } },
        { at: 1_000, field: 'Priority', from: 'Low', to: 'High', author: { id: 'acc-user1', name: 'User 1' } },
        { at: 500, field: 'Labels', to: 'web', author: { id: 'acc-user1', name: 'User 1' }, agent: { id: 'thread-2' } },
      ],
    });
    const { getAllByTestId } = render(TicketHistory, { props: { detail, backend: 'home' } });
    const marks = getAllByTestId('ticket-history-event').map((e) => e.querySelector('[data-testid="ticket-history-agent"]')?.textContent?.trim() ?? '');
    expect(marks).toEqual(['Agent · Fix the Safari login', '', 'Agent · a deleted thread']);
  });
});
