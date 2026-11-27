// A time_draft call's card in its thread over mocked bindings: the call's
// entries by the days its row names, its header for each thing left, the
// approvals of a day and of the call, Discard, a question, and Review in
// Time.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import type { ComponentProps } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import type { PostTimeView, TimeEntryView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TimeDraftCard from './TimeDraftCard.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { trackerTimeEntriesUpdated } from '../../stores/trackerBoardEvents';

const openTime = vi.hoisted(() => vi.fn(async () => {}));
vi.mock('../../stores/ticketOpen', async (importOriginal) => ({ ...(await importOriginal<typeof import('../../stores/ticketOpen')>()), openTimeInIssues: openTime }));

const BASE: ComponentProps<typeof TimeDraftCard> = { backend: 'peer-1', thread: 't1', call: 'call-1', connection: 'c1', days: ['2026-11-11', '2026-11-12'] };

function drafted(id: number, day: string, minutes: number, extra: Partial<TimeEntryView> = {}): TimeEntryView {
  return { id, revision: 2, connection: 'c1', day, issue: '10001', key: 'WEB-1', title: 'First story', minutes, reason: 'Commits', by: 'agent', thread: 't1', call: 'call-1', state: 'drafted', ...extra };
}

function asked(id: number, day: string, minutes: number): TimeEntryView {
  return drafted(id, day, minutes, { issue: undefined, key: undefined, title: undefined, reason: 'A call not on the calendar.', offers: [{ issue: '10002', key: 'WEB-2' }] });
}

const POSTED = (sentence: string): PostTimeView => ({ entries: [], posted: 2, of: 2, sentence });

describe('a time draft card', () => {
  it('reads its call on the thread\'s computer and shows its entries by day, with a day\'s approval and the call\'s', async () => {
    const read = setBindingMock('ThreadTimeEntries', async () => [drafted(11, '2026-11-11', 90), asked(13, '2026-11-12', 25), drafted(14, '2026-11-12', 30)]);
    const post = setBindingMock('PostTrackerTimeEntries', async () => POSTED('Posted 2 of 2 entries for Wed Nov 11 and Thu Nov 12. 1 question for Thu Nov 12 is still open.'));
    const view = render(TimeDraftCard, BASE);
    const header = await view.findByTestId('time-draft-card-header');
    expect(read).toHaveBeenCalledWith('t1', 'call-1');
    expect(header.dataset.state).toBe('draft');
    expect(header).toHaveTextContent('Log time 2 days 2h');
    const days = view.getAllByTestId('time-draft-card-day');
    expect(days.map((d) => d.dataset.day)).toEqual(['2026-11-11', '2026-11-12']);
    expect(within(days[1]).getAllByTestId('time-draft-question')).toHaveLength(1);
    await fireEvent.click(within(days[1]).getByTestId('time-draft-card-approve-day'));
    expect(post).toHaveBeenLastCalledWith('c1', { entries: [{ id: 13, revision: 2 }, { id: 14, revision: 2 }], alone: undefined });
    await fireEvent.click(view.getByTestId('time-draft-card-approve'));
    expect(post).toHaveBeenLastCalledWith('c1', { entries: [{ id: 11, revision: 2 }, { id: 13, revision: 2 }, { id: 14, revision: 2 }], alone: undefined });
    expect(await view.findByTestId('time-draft-card-approve-write-outcome')).toHaveTextContent('Posted 2 of 2 entries for Wed Nov 11 and Thu Nov 12. 1 question for Thu Nov 12 is still open.');
  });

  it('keeps what the call\'s approval said after its entries posted, and says the time is logged', async () => {
    let entries = [drafted(11, '2026-11-11', 90), drafted(12, '2026-11-12', 30)];
    setBindingMock('ThreadTimeEntries', async () => entries);
    setBindingMock('PostTrackerTimeEntries', async () => POSTED('Posted 2 of 2 entries for Wed Nov 11 and Thu Nov 12.'));
    const view = render(TimeDraftCard, BASE);
    await fireEvent.click(await view.findByTestId('time-draft-card-approve'));
    await view.findByTestId('time-draft-card-approve-write-outcome');
    entries = entries.map((e) => ({ ...e, state: 'posted', worklog: `w${e.id}` }));
    trackerTimeEntriesUpdated({ connection: 'c1', author: 'acct-1', days: ['2026-11-11', '2026-11-12'], threads: ['t1'] }, 'peer-1');
    // The frame's read is coalesced (DRAFT_CARD_REREAD_DELAY_MS).
    await waitFor(() => expect(view.getByTestId('time-draft-card-header').dataset.state).toBe('logged'));
    expect(view.getByTestId('time-draft-card-header')).toHaveTextContent('Logged 2h 2 worklogs');
    expect(view.queryByTestId('time-draft-card-approve')).toBeNull();
    expect(view.getByTestId('time-draft-card-approve-write-outcome')).toHaveTextContent('Posted 2 of 2 entries for Wed Nov 11 and Thu Nov 12.');
  });

  // A call can have no entries from its start (it only replaced the
  // thread's earlier ones), or lose them later: the card says only what
  // its read shows.
  it('says only that the call has no entries when it has none', async () => {
    setBindingMock('ThreadTimeEntries', async () => []);
    const view = render(TimeDraftCard, BASE);
    expect(await view.findByTestId('time-draft-card-empty')).toHaveTextContent('This draft has no entries now.');
    expect(view.getAllByTestId('time-draft-card-day-gone').map((d) => d.textContent)).toEqual(['This draft has nothing on this day now.', 'This draft has nothing on this day now.']);
    expect(view.queryByTestId('time-draft-card-approve')).toBeNull();
    expect(view.queryByTestId('time-draft-card-discard')).toBeNull();
  });

  it('asks once before Discard removes the call\'s drafted entries, and opens the draft\'s week in Time', async () => {
    setBindingMock('ThreadTimeEntries', async () => [drafted(11, '2026-11-11', 90), drafted(15, '2026-11-12', 30, { state: 'unknown', refusal: 'may already be logged' })]);
    const remove = setBindingMock('RemoveTrackerTimeEntries', async () => []);
    const view = render(TimeDraftCard, BASE);
    await fireEvent.click(await view.findByTestId('time-draft-card-discard'));
    await fireEvent.click(view.getByTestId('time-draft-card-discard-confirm-yes'));
    expect(remove).toHaveBeenCalledWith('c1', [{ id: 11, revision: 2 }]);
    // The entry of unknown outcome offers Remove and no hand log here.
    const unknown = view.getAllByTestId('time-draft-entry').find((e) => e.dataset.state === 'unknown')!;
    expect(within(unknown).getByTestId('time-draft-entry-remove')).toBeInTheDocument();
    expect(within(unknown).queryByTestId('time-draft-entry-log-by-hand')).toBeNull();
    await fireEvent.click(view.getByTestId('time-draft-card-review'));
    expect(openTime).toHaveBeenCalledWith({ backend: 'peer-1', connection: 'c1', day: '2026-11-11' });
  });

  it('says why it cannot show when the thread\'s computer is not known, and reads nothing', () => {
    const view = render(TimeDraftCard, { ...BASE, backend: null });
    expect(view.getByTestId('time-draft-card-unowned')).toBeInTheDocument();
    expect(getBindingMock('ThreadTimeEntries')?.mock.calls ?? []).toHaveLength(0);
  });
});
