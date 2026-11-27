// The Time screen over mocked bindings: the sentences where the week
// would be, the week by day and by ticket, logging from a cell, changing
// and deleting an entry, a stale refusal, a refused read, and compact.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick, type ComponentProps } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PostTimeView, TimeEntryView, WorklogView, WriteView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TimeScreen from './TimeScreen.svelte';
import { methodNotFound } from '../../../test/helpers/olderComputer';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { applyTrackerConnectionsChanged } from '../../stores/trackerConnections.svelte';
import { trackerTimeEntriesUpdated } from '../../stores/trackerBoardEvents';
import { DRAFT_REREAD_DELAY_MS } from '../../stores/trackerTime.svelte';

const BASE: ComponentProps<typeof TimeScreen> = {
  backend: 'peer-1',
  connection: 'c1',
  connectionKind: 'jira',
  connectionOn: true,
  author: 'acct-1',
  computerName: 'Laptop',
  offline: false,
  compact: false,
  shown: true,
  openIssue: null,
  onOpenTicket: () => {},
};

function log(id: string, day: string, spent: number, extra: Partial<WorklogView> = {}): WorklogView {
  return { id, issue: '10001', key: 'WEB-1', title: 'First story', author: 'acct-1', day, spent_seconds: spent, updated: 1, ...extra };
}

const WEEK = [
  log('501', '2026-11-09', 5400, { description: 'Review' }),
  log('502', '2026-11-10', 1800, { issue: '10002', key: 'WEB-2', title: 'Second story' }),
];
const APPLIED = { parts: [{ step: 'worklog', state: 'applied' }] } as WriteView;
const body = () => within(document.body);

function answerWeek(worklogs: WorklogView[], unread: string[] = []): void {
  setBindingMock('TrackerWorklogs', async () => ({ worklogs, unread }));
  setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs, unread }));
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date(2026, 10, 12, 10, 0));
});

afterEach(() => {
  vi.useRealTimers();
});

describe('where the week would be', () => {
  const cases: Array<[string, Partial<typeof BASE>, string, string]> = [
    ['the computer is not connected', { offline: true }, 'time-offline', 'Laptop is not connected. Your time shows when it connects.'],
    ['a Linear board', { connectionKind: 'linear' }, 'time-linear', "Linear has no time tracking, so this board's connection has no logged time."],
    ['a connection turned off', { connectionOn: false }, 'time-connection-off', "This board's connection is turned off. Turn it on in Settings to see your time."],
    ['no verified account', { author: null }, 'time-no-account', 'The app does not know which account is yours on this connection. Verify the connection in Settings.'],
  ];
  for (const [name, props, testId, sentence] of cases) {
    it(`says one sentence and reads nothing for ${name}`, () => {
      const view = render(TimeScreen, { ...BASE, ...props });
      expect(view.getByTestId(testId)).toHaveTextContent(sentence);
      expect(view.queryByTestId('time-toolbar')).toBeNull();
      expect(getBindingMock('TrackerWorklogs')?.mock.calls ?? []).toHaveLength(0);
      expect(getBindingMock('RefreshTrackerWorklogs')?.mock.calls ?? []).toHaveLength(0);
    });
  }

  it('shows a read the computer refused as the computer\'s own message', async () => {
    const refused = methodNotFound();
    setBindingMock('TrackerWorklogs', async () => { throw refused; });
    setBindingMock('RefreshTrackerWorklogs', async () => { throw refused; });
    const view = render(TimeScreen, BASE);
    expect(await view.findByTestId('time-read-error')).toHaveTextContent('Could not read your logged time: The computer this reached does not support this action. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update.');
  });

  it('reads nothing while the pane does not show it', async () => {
    answerWeek(WEEK);
    const view = render(TimeScreen, { ...BASE, shown: false });
    expect(view.getByTestId('time-reading')).toBeInTheDocument();
    expect(getBindingMock('TrackerWorklogs')!.mock.calls).toHaveLength(0);
    await view.rerender({ ...BASE, shown: true });
    await view.findAllByTestId('time-entry');
    expect(getBindingMock('TrackerWorklogs')!.mock.calls).toEqual([['c1', 'acct-1', '2026-11-09', '2026-11-15']]);
  });
});

describe('the week', () => {
  it('shows each day by day with its entries and total, an unread day with no total, and the week\'s', async () => {
    answerWeek(WEEK, ['2026-11-11']);
    const view = render(TimeScreen, BASE);
    await view.findAllByTestId('time-entry');
    const days = view.getAllByTestId('time-day');
    expect(days.map((d) => d.dataset.day)).toEqual(['2026-11-09', '2026-11-10', '2026-11-11', '2026-11-12', '2026-11-13']);
    expect(within(days[0]).getByTestId('time-day-total')).toHaveTextContent('1h 30m');
    expect(within(days[0]).getByTestId('time-entry-description')).toHaveTextContent('Review');
    expect(within(days[2]).getByTestId('time-day-uncounted')).toHaveTextContent('Not read yet');
    expect(within(days[2]).queryByTestId('time-day-total')).toBeNull();
    expect(view.getByTestId('time-week-total')).toHaveTextContent('2h, leaving out 1 day with no total');
  });

  it('moves a week back and forward and to this week', async () => {
    answerWeek([]);
    const view = render(TimeScreen, BASE);
    const label = view.getByTestId('time-week').textContent;
    expect(view.queryByTestId('time-this-week')).toBeNull();
    await fireEvent.click(view.getByTestId('time-previous'));
    await waitFor(() => expect(getBindingMock('TrackerWorklogs')!.mock.calls.at(-1)).toEqual(['c1', 'acct-1', '2026-11-02', '2026-11-08']));
    await fireEvent.click(view.getByTestId('time-this-week'));
    expect(view.getByTestId('time-week').textContent).toBe(label);
  });

  it('shows By ticket as a row per ticket with its cells and totals, and remembers the choice', async () => {
    answerWeek(WEEK);
    const view = render(TimeScreen, BASE);
    await view.findAllByTestId('time-entry');
    await fireEvent.click(view.getByRole('radio', { name: 'By ticket' }));
    const rows = view.getAllByTestId('time-ticket-row');
    expect(rows.map((r) => r.dataset.issue)).toEqual(['10001', '10002']);
    expect(within(rows[0]).getAllByTestId('time-cell').map((c) => c.textContent?.trim())).toEqual(['1h 30m', '', '', '', '']);
    expect(within(rows[1]).getByTestId('time-ticket-total')).toHaveTextContent('30m');
    view.unmount();
    const again = render(TimeScreen, BASE);
    expect(await again.findByTestId('time-by-ticket')).toBeInTheDocument();
  });
});

describe('writes', () => {
  it('logs from an empty cell with its ticket and day filled in', async () => {
    answerWeek(WEEK);
    const add = setBindingMock('AddTrackerWorklog', async () => APPLIED);
    localStorage.setItem('agent-overflow:frontend:issues-time-layout', JSON.stringify('ticket'));
    const view = render(TimeScreen, BASE);
    const row = (await view.findAllByTestId('time-ticket-row'))[0];
    await fireEvent.click(within(row).getAllByTestId('time-cell')[2]);
    const form = await body().findByTestId('worklog-form');
    expect(within(form).getByTestId('worklog-form-ticket')).toHaveTextContent('WEB-1');
    expect(within(form).getByTestId('worklog-form-day')).toHaveValue('2026-11-11');
    expect(within(form).getByTestId('worklog-form-time')).toHaveValue('');
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: 'an hour' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    expect(within(form).getByTestId('worklog-form-problem')).toHaveTextContent('Type the time as 1h 30m, 45m or a number of minutes.');
    expect(add).not.toHaveBeenCalled();
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: '1h 30m' } });
    await fireEvent.input(within(form).getByTestId('worklog-form-description'), { target: { value: 'Pairing' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await waitFor(() => expect(body().queryByTestId('worklog-form')).toBeNull());
    expect(add).toHaveBeenCalledWith('c1', { issue: '10001', day: '2026-11-11', spent_seconds: 5400, description: 'Pairing' });
  });

  it('logs on another ticket found from the week\'s tickets, with nothing filled in', async () => {
    answerWeek(WEEK);
    const add = setBindingMock('AddTrackerWorklog', async () => APPLIED);
    localStorage.setItem('agent-overflow:frontend:issues-time-layout', JSON.stringify('ticket'));
    const view = render(TimeScreen, BASE);
    await view.findAllByTestId('time-ticket-row');
    await fireEvent.click(view.getByTestId('time-log-other'));
    const form = await body().findByTestId('worklog-form');
    expect(within(form).getByTestId('worklog-form-day')).toHaveValue('');
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    expect(within(form).getByTestId('worklog-form-problem')).toHaveTextContent('Pick the ticket to log time on.');
    await fireEvent.click(within(form).getByTestId('worklog-form-find'));
    await waitFor(() => expect(body().getAllByTestId('ticket-picker-item').map((i) => i.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['WEB-1 First story', 'WEB-2 Second story']));
    await fireEvent.click(body().getAllByTestId('ticket-picker-item')[1]);
    expect(within(form).getByTestId('worklog-form-ticket')).toHaveTextContent('WEB-2');
    await fireEvent.input(within(form).getByTestId('worklog-form-day'), { target: { value: '2026-11-13' } });
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: '45' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await waitFor(() => expect(add).toHaveBeenCalledWith('c1', { issue: '10002', day: '2026-11-13', spent_seconds: 2700, description: undefined }));
  });

  it('changes an entry sending only what changed, and says when it landed outside the week', async () => {
    answerWeek(WEEK);
    const edit = setBindingMock('EditTrackerWorklog', async () => APPLIED);
    const view = render(TimeScreen, BASE);
    const entry = (await view.findAllByTestId('time-entry'))[0];
    await fireEvent.click(within(entry).getByTestId('time-entry-hours'));
    const form = await body().findByTestId('worklog-form');
    expect(within(form).getByTestId('worklog-form-time')).toHaveValue('1h 30m');
    await fireEvent.input(within(form).getByTestId('worklog-form-day'), { target: { value: '2026-11-17' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await waitFor(() => expect(body().queryByTestId('worklog-form')).toBeNull());
    expect(edit).toHaveBeenCalledWith('c1', expect.objectContaining({ from: WEEK[0], day: '2026-11-17', spent_seconds: undefined, description: undefined }));
    expect(view.getByTestId('time-saved-elsewhere')).toHaveTextContent('Logged on');
    expect(view.getByTestId('time-week').textContent).not.toMatch(/17/);
  });

  it('deletes from the entry\'s menu only after Delete is pressed again, and Keep sends nothing', async () => {
    answerWeek(WEEK);
    const del = setBindingMock('DeleteTrackerWorklog', async () => APPLIED);
    const view = render(TimeScreen, BASE);
    const entry = (await view.findAllByTestId('time-entry'))[0];
    await fireEvent.contextMenu(entry);
    await fireEvent.click(body().getByTestId('time-menu-delete'));
    const confirm = await body().findByTestId('worklog-form-confirm-delete');
    await fireEvent.click(within(confirm).getByTestId('worklog-form-delete-keep'));
    expect(body().queryByTestId('worklog-form-confirm-delete')).toBeNull();
    expect(del).not.toHaveBeenCalled();
    await fireEvent.click(body().getByTestId('worklog-form-delete'));
    await fireEvent.click(body().getByTestId('worklog-form-delete-confirm'));
    await waitFor(() => expect(del).toHaveBeenCalledWith('c1', { from: WEEK[0] }));
  });

  it('says an entry changed on the tracker and reads the week again', async () => {
    answerWeek(WEEK);
    setBindingMock('EditTrackerWorklog', async () => ({ parts: [{ step: 'worklog', state: 'refused', refusal: { reason: 'stale', message: 'worklog 501 changed since it was read' } }] }) as WriteView);
    const view = render(TimeScreen, BASE);
    const entry = (await view.findAllByTestId('time-entry'))[0];
    const refreshes = getBindingMock('RefreshTrackerWorklogs')!.mock.calls.length;
    await fireEvent.click(within(entry).getByTestId('time-entry-hours'));
    const form = await body().findByTestId('worklog-form');
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: '2h' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    const note = await within(form).findByTestId('worklog-form-write-outcome');
    await waitFor(() => expect(note).toHaveTextContent('This entry changed on the tracker since it was read. The week was read again; look at it before you change it.'));
    expect(getBindingMock('RefreshTrackerWorklogs')!.mock.calls.length).toBe(refreshes + 1);
  });
});

describe('compact', () => {
  it('shows one day at a time, picked from the week\'s strip', async () => {
    answerWeek(WEEK);
    const view = render(TimeScreen, { ...BASE, compact: true });
    await view.findAllByTestId('time-day-tab');
    expect(view.getAllByTestId('time-day').map((d) => d.dataset.day)).toEqual(['2026-11-12']);
    const tabs = view.getAllByTestId('time-day-tab');
    expect(tabs.map((t) => t.textContent?.replace(/\s+/g, ' ').trim())).toHaveLength(5);
    await fireEvent.click(tabs[0]);
    expect(view.getAllByTestId('time-day').map((d) => d.dataset.day)).toEqual(['2026-11-09']);
    expect(view.getByTestId('time-entry')).toHaveAttribute('data-worklog', '501');
  });

  it('keeps the grid by ticket with keys and no titles', async () => {
    answerWeek(WEEK);
    localStorage.setItem('agent-overflow:frontend:issues-time-layout', JSON.stringify('ticket'));
    const view = render(TimeScreen, { ...BASE, compact: true });
    const rows = await view.findAllByTestId('time-ticket-row');
    expect(within(rows[0]).getByTestId('time-ticket-open').textContent?.trim()).toBe('WEB-1');
  });
});

const LOSS = { parts: [{ step: 'worklog', state: 'refused', refusal: { reason: 'loss', message: 'This change would lose formatting.', lost: ['a table'] } }] } as WriteView;
const STALE = { parts: [{ step: 'worklog', state: 'refused', refusal: { reason: 'stale', message: 'worklog 501 changed since it was read' } }] } as WriteView;

/** Opens the form of the week's first entry from its hours. */
async function changeFirst(view: { findAllByTestId: (id: string) => Promise<HTMLElement[]> }): Promise<HTMLElement> {
  const entry = (await view.findAllByTestId('time-entry'))[0];
  await fireEvent.click(within(entry).getByTestId('time-entry-hours'));
  return body().findByTestId('worklog-form');
}

describe('Save anyway', () => {
  it('is withdrawn when the person changes the refused draft, so the next Save sends what the form holds', async () => {
    answerWeek(WEEK);
    const edit = setBindingMock('EditTrackerWorklog', async () => LOSS);
    const view = render(TimeScreen, BASE);
    const form = await changeFirst(view);
    await fireEvent.input(within(form).getByTestId('worklog-form-description'), { target: { value: 'Review, with a table' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await within(form).findByTestId('worklog-form-write-save-anyway');
    await fireEvent.input(within(form).getByTestId('worklog-form-description'), { target: { value: 'Review, corrected' } });
    expect(within(form).queryByTestId('worklog-form-write-save-anyway')).toBeNull();
    expect(edit).toHaveBeenCalledTimes(1);
  });

  it('is withdrawn when the form closes, and the entry never offers it', async () => {
    answerWeek(WEEK);
    setBindingMock('EditTrackerWorklog', async () => LOSS);
    const view = render(TimeScreen, BASE);
    const form = await changeFirst(view);
    await fireEvent.input(within(form).getByTestId('worklog-form-description'), { target: { value: 'Review, with a table' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await within(form).findByTestId('worklog-form-write-save-anyway');
    const entry = view.getAllByTestId('time-entry')[0];
    expect(within(entry).queryByTestId('time-entry-write-save-anyway')).toBeNull();
    await fireEvent.click(within(form).getByTestId('worklog-form-cancel'));
    await waitFor(() => expect(body().queryByTestId('worklog-form')).toBeNull());
    expect(within(entry).queryByTestId('time-entry-write-save-anyway')).toBeNull();
    // Opened again, the form holds the entry's values, not the refused draft.
    const again = await changeFirst(view);
    expect(within(again).getByTestId('worklog-form-description')).toHaveValue('Review');
    expect(within(again).queryByTestId('worklog-form-write-save-anyway')).toBeNull();
  });
});

describe('Save anyway, for a form closed while its write was out', () => {
  it('is not offered when the refusal lands after the form closed', async () => {
    answerWeek(WEEK);
    let refuse!: (v: WriteView) => void;
    setBindingMock('EditTrackerWorklog', () => new Promise<WriteView>((r) => { refuse = r; }));
    const view = render(TimeScreen, BASE);
    const form = await changeFirst(view);
    await fireEvent.input(within(form).getByTestId('worklog-form-description'), { target: { value: 'Review, with a table' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await within(form).findByTestId('worklog-form-write-pending');
    await fireEvent.click(within(form).getByTestId('worklog-form-cancel'));
    await waitFor(() => expect(body().queryByTestId('worklog-form')).toBeNull());
    refuse(LOSS);
    const entry = view.getAllByTestId('time-entry')[0];
    await within(entry).findByTestId('time-entry-write-outcome');
    const again = await changeFirst(view);
    expect(within(again).getByTestId('worklog-form-write-outcome')).toHaveAttribute('data-kind', 'refused');
    expect(within(again).queryByTestId('worklog-form-write-save-anyway')).toBeNull();
  });
});

describe('a write the week has not caught up with', () => {
  it('leaves a receipt at the week after an add whose read failed, until a refresh reads the week', async () => {
    answerWeek(WEEK);
    const stale = { connection: 'c1', resources: [{ kind: 'worklogs', author: 'acct-1', day: '2026-11-11' }], failure: { kind: 'transient', message: 'Jira could not be reached; try again', at: 1 } };
    const add = setBindingMock('AddTrackerWorklog', async () => ({ ...APPLIED, stale: [stale] }) as unknown as WriteView);
    localStorage.setItem('agent-overflow:frontend:issues-time-layout', JSON.stringify('ticket'));
    const view = render(TimeScreen, BASE);
    const row = (await view.findAllByTestId('time-ticket-row'))[0];
    await fireEvent.click(within(row).getAllByTestId('time-cell')[2]);
    const form = await body().findByTestId('worklog-form');
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: '1h 30m' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await waitFor(() => expect(body().queryByTestId('worklog-form')).toBeNull());
    expect(add).toHaveBeenCalledTimes(1);
    const receipt = await view.findByTestId('time-receipt');
    expect(receipt).toHaveTextContent('1h 30m logged on WEB-1 for Nov 11');
    expect(receipt).toHaveTextContent('Jira could not be reached; try again');
    // The stored copy does not have it yet: the cell is still empty.
    expect(within(view.getAllByTestId('time-ticket-row')[0]).getAllByTestId('time-cell')[2]).toHaveAttribute('data-count', '0');
    await fireEvent.click(within(receipt).getByTestId('time-receipt-refresh'));
    await waitFor(() => expect(view.queryByTestId('time-receipt')).toBeNull());
  });
});

describe('a stale refusal', () => {
  it('says the week is being read again until that read lands, then edits the entry as it was read', async () => {
    answerWeek(WEEK);
    const edit = setBindingMock('EditTrackerWorklog', vi.fn().mockResolvedValueOnce(STALE).mockResolvedValue(APPLIED));
    const view = render(TimeScreen, BASE);
    await view.findAllByTestId('time-entry');
    const changed = log('501', '2026-11-09', 7200, { description: 'Review', updated: 8 });
    let answer!: (v: { worklogs: WorklogView[] }) => void;
    setBindingMock('RefreshTrackerWorklogs', () => new Promise((r) => { answer = r; }));
    setBindingMock('TrackerWorklogs', async () => ({ worklogs: [changed, WEEK[1]] }));
    const form = await changeFirst(view);
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: '2h 30m' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    const note = await within(form).findByTestId('worklog-form-write-outcome');
    expect(note).toHaveTextContent('This entry changed on the tracker since it was read.');
    expect(note).not.toHaveTextContent('The week was read again');
    answer({ worklogs: [changed, WEEK[1]] });
    await waitFor(() => expect(note).toHaveTextContent('The week was read again'));
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await waitFor(() => expect(edit).toHaveBeenCalledTimes(2));
    expect(edit.mock.calls[1][1]).toEqual(expect.objectContaining({ from: changed, spent_seconds: 9000 }));
  });

  it('says the week could not be read again when that read fails', async () => {
    answerWeek(WEEK);
    setBindingMock('EditTrackerWorklog', async () => STALE);
    const view = render(TimeScreen, BASE);
    await view.findAllByTestId('time-entry');
    setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: WEEK, failure: { kind: 'transient', message: 'Jira could not be reached; try again', at: 1 } }));
    const form = await changeFirst(view);
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: '2h' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    const note = await within(form).findByTestId('worklog-form-write-outcome');
    await waitFor(() => expect(note).toHaveTextContent('The week could not be read again: Jira could not be reached; try again'));
  });
});

describe('the form sends only what the person changed', () => {
  it('leaves a description it did not touch out of a change of the hours', async () => {
    const spaced = log('501', '2026-11-09', 5400, { description: '  Review\n' });
    answerWeek([spaced]);
    const edit = setBindingMock('EditTrackerWorklog', async () => APPLIED);
    const view = render(TimeScreen, BASE);
    const form = await changeFirst(view);
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: '2h' } });
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await waitFor(() => expect(edit).toHaveBeenCalledTimes(1));
    expect(edit.mock.calls[0][1]).toEqual(expect.objectContaining({ spent_seconds: 7200, description: undefined, day: undefined }));
  });
});

describe('this week', () => {
  it('moves with the clock: a screen open from Sunday into Monday goes to the new week', async () => {
    vi.useRealTimers();
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] });
    vi.setSystemTime(new Date(2026, 10, 15, 23, 0));
    answerWeek([]);
    const view = render(TimeScreen, BASE);
    expect(view.queryByTestId('time-this-week')).toBeNull();
    const sunday = view.getByTestId('time-week').textContent;
    await vi.advanceTimersByTimeAsync(2 * 3600_000);
    // Timers are fake here, so the test reads the DOM as it stands.
    await fireEvent.click(view.getByTestId('time-this-week'));
    await vi.advanceTimersByTimeAsync(0);
    expect(view.getByTestId('time-week').textContent).not.toBe(sunday);
    expect(getBindingMock('TrackerWorklogs')!.mock.calls.at(-1)).toEqual(['c1', 'acct-1', '2026-11-16', '2026-11-22']);
  });
});

describe('a change of the computer\'s connections', () => {
  it('keeps the week and an open form, and reads nothing', async () => {
    answerWeek(WEEK);
    const view = render(TimeScreen, BASE);
    const form = await changeFirst(view);
    await fireEvent.input(within(form).getByTestId('worklog-form-description'), { target: { value: 'Review, half done' } });
    const reads = () => [getBindingMock('TrackerWorklogs')!.mock.calls.length, getBindingMock('RefreshTrackerWorklogs')!.mock.calls.length];
    const before = reads();
    applyTrackerConnectionsChanged('peer-1');
    await tick();
    await tick();
    expect(view.getAllByTestId('time-entry')).toHaveLength(WEEK.length);
    expect(view.queryByTestId('time-reading')).toBeNull();
    expect(within(body().getByTestId('worklog-form')).getByTestId('worklog-form-description')).toHaveValue('Review, half done');
    expect(reads()).toEqual(before);
  });
});

function drafted(id: number, day: string, minutes: number, extra: Partial<TimeEntryView> = {}): TimeEntryView {
  return { id, revision: 2, connection: 'c1', day, issue: '10001', key: 'WEB-1', title: 'First story', minutes, reason: 'Commits on the branch', by: 'agent', thread: 't1', call: 'call-1', state: 'drafted', ...extra };
}

function asked(id: number, day: string, minutes: number, extra: Partial<TimeEntryView> = {}): TimeEntryView {
  return drafted(id, day, minutes, { issue: undefined, key: undefined, title: undefined, reason: 'A 25 minute call at 14:05 is not on your calendar.', offers: [{ issue: '10002', key: 'WEB-2', title: 'Second story' }], ...extra });
}

function answerDraft(worklogs: WorklogView[], entries: TimeEntryView[]): void {
  setBindingMock('TrackerWorklogs', async () => ({ worklogs, drafted: entries }));
  setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs, drafted: entries }));
}

const POSTED = (sentence: string): PostTimeView => ({ entries: [], posted: 1, of: 1, sentence });

describe('the agent\'s draft', () => {
  const DRAFT = [
    drafted(11, '2026-11-11', 150),
    drafted(12, '2026-11-11', 60, { issue: '10002', key: 'WEB-2', title: 'Second story', revision: 7 }),
    asked(13, '2026-11-12', 25),
    drafted(14, '2026-11-12', 45),
  ];

  it('shows the week\'s draft in a banner and approves the whole week shown, questions included', async () => {
    answerDraft(WEEK, DRAFT);
    const post = setBindingMock('PostTrackerTimeEntries', async () => POSTED('Posted 3 of 3 entries for Wed Nov 11 and Thu Nov 12. 1 question for Thu Nov 12 is still open.'));
    const view = render(TimeScreen, BASE);
    const banner = await view.findByTestId('time-draft-banner');
    expect(within(banner).getByTestId('time-draft-banner-title')).toHaveTextContent('Your agent drafted 2 days');
    expect(within(banner).getByTestId('time-draft-banner-detail')).toHaveTextContent('3 entries · 4h 15m · 1 question · from a thread');
    await fireEvent.click(within(banner).getByTestId('time-draft-approve-week'));
    expect(post).toHaveBeenCalledWith('c1', { entries: [{ id: 11, revision: 2 }, { id: 12, revision: 7 }, { id: 14, revision: 2 }, { id: 13, revision: 2 }], alone: undefined });
    expect(await view.findByTestId('time-draft-approve-week-write-outcome')).toHaveTextContent('Posted 3 of 3 entries for Wed Nov 11 and Thu Nov 12. 1 question for Thu Nov 12 is still open.');
  });

  it('asks once before Discard removes the week\'s drafted entries', async () => {
    answerDraft(WEEK, DRAFT);
    const remove = setBindingMock('RemoveTrackerTimeEntries', async () => DRAFT);
    const view = render(TimeScreen, BASE);
    await fireEvent.click(await view.findByTestId('time-draft-discard-week'));
    expect(remove).not.toHaveBeenCalled();
    expect(view.getByTestId('time-draft-discard-week-confirm')).toHaveTextContent('Remove the 4 entries of this draft? Nothing is logged.');
    await fireEvent.click(view.getByTestId('time-draft-discard-week-confirm-keep'));
    expect(remove).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('time-draft-discard-week'));
    await fireEvent.click(view.getByTestId('time-draft-discard-week-confirm-yes'));
    expect(remove).toHaveBeenCalledWith('c1', [{ id: 11, revision: 2 }, { id: 12, revision: 7 }, { id: 14, revision: 2 }, { id: 13, revision: 2 }]);
  });

  it('shows each day\'s draft dashed under its logged time and approves one day alone', async () => {
    answerDraft(WEEK, DRAFT);
    const post = setBindingMock('PostTrackerTimeEntries', async () => POSTED('Posted 2 of 2 entries for Wed Nov 11.'));
    const view = render(TimeScreen, BASE);
    await view.findByTestId('time-draft-banner');
    const wed = view.getAllByTestId('time-day').find((d) => d.dataset.day === '2026-11-11')!;
    expect(within(wed).getAllByTestId('time-draft-entry').map((e) => e.dataset.entry)).toEqual(['11', '12']);
    expect(within(wed).getByTestId('time-day-drafted')).toHaveTextContent('+3h 30m');
    expect(within(wed).queryByTestId('time-day-empty')).toBeNull();
    await fireEvent.click(within(wed).getByTestId('time-draft-approve-day'));
    expect(post).toHaveBeenCalledWith('c1', { entries: [{ id: 11, revision: 2 }, { id: 12, revision: 7 }], alone: undefined });
  });

  it('changes an entry, logs it alone as changed, keeps it, and removes it from its form', async () => {
    answerDraft([], [drafted(11, '2026-11-11', 150)]);
    const edit = setBindingMock('EditTrackerTimeEntry', async () => drafted(11, '2026-11-11', 120, { revision: 3 }));
    const post = setBindingMock('PostTrackerTimeEntries', async () => POSTED('Posted 1 of 1 entries for Wed Nov 11.'));
    const remove = setBindingMock('RemoveTrackerTimeEntries', async () => [drafted(11, '2026-11-11', 150)]);
    const view = render(TimeScreen, BASE);
    await fireEvent.click(await view.findByTestId('time-draft-entry-hours'));
    const form = await body().findByTestId('time-draft-form');
    await fireEvent.input(within(form).getByTestId('time-draft-form-time'), { target: { value: '2h' } });
    await fireEvent.click(within(form).getByTestId('time-draft-form-log'));
    await waitFor(() => expect(post).toHaveBeenCalledWith('c1', { entries: [{ id: 11, revision: 3 }], alone: true }));
    expect(edit).toHaveBeenCalledWith('c1', { id: 11, revision: 2 }, { hours: 2 });
    await waitFor(() => expect(body().queryByTestId('time-draft-form')).toBeNull());
    // Keep as draft with nothing changed sends nothing.
    await fireEvent.click(view.getByTestId('time-draft-entry-hours'));
    await fireEvent.click(within(await body().findByTestId('time-draft-form')).getByTestId('time-draft-form-keep'));
    expect(edit).toHaveBeenCalledTimes(1);
    await fireEvent.click(view.getByTestId('time-draft-entry-hours'));
    await fireEvent.click(within(await body().findByTestId('time-draft-form')).getByTestId('time-draft-form-remove'));
    expect(remove).toHaveBeenCalledWith('c1', [{ id: 11, revision: 2 }]);
  });

  it('answers a question with an offered ticket or leaves it out, with one press', async () => {
    answerDraft([], [asked(13, '2026-11-12', 25)]);
    const edit = setBindingMock('EditTrackerTimeEntry', async () => drafted(13, '2026-11-12', 25));
    const view = render(TimeScreen, BASE);
    const q = await view.findByTestId('time-draft-question');
    expect(within(q).getByTestId('time-draft-question-text')).toHaveTextContent('A 25 minute call at 14:05 is not on your calendar. Which ticket was it for?');
    await fireEvent.click(within(q).getByTestId('time-draft-question-offer'));
    expect(edit).toHaveBeenLastCalledWith('c1', { id: 13, revision: 2 }, { ticket: { issue: '10002', key: 'WEB-2', title: 'Second story' } });
    await fireEvent.click(within(q).getByTestId('time-draft-question-leave-out'));
    expect(edit).toHaveBeenLastCalledWith('c1', { id: 13, revision: 2 }, { leave_out: true });
  });

  it('shows an entry of unknown outcome with the computer\'s sentence and Remove, never a way to post it, and logs it by hand', async () => {
    const sentence = "may already be logged, so it is not sent again; look at the day's logged time, and remove the entry if the time is there, or log the time by hand and then remove the entry";
    const unknown = drafted(15, '2026-11-11', 90, { state: 'unknown', refusal: sentence });
    answerDraft([], [unknown]);
    const add = setBindingMock('AddTrackerWorklog', async () => APPLIED);
    const remove = setBindingMock('RemoveTrackerTimeEntries', async () => [unknown]);
    const view = render(TimeScreen, BASE);
    const row = await view.findByTestId('time-draft-entry');
    expect(within(row).getByTestId('time-draft-entry-unknown')).toHaveTextContent(`This entry ${sentence}.`);
    expect(within(row).getByTestId('time-draft-entry-hours').tagName).toBe('SPAN');
    expect(view.queryByTestId('time-draft-approve-day')).toBeNull();
    expect(view.queryByTestId('time-draft-banner')).toBeNull();
    await fireEvent.click(within(row).getByTestId('time-draft-entry-log-by-hand'));
    const form = await body().findByTestId('worklog-form');
    expect(within(form).getByTestId('worklog-form-time')).toHaveValue('1h 30m');
    expect(within(form).getByTestId('worklog-form-description')).toHaveValue('Commits on the branch');
    expect(within(form).getByTestId('worklog-form-day')).toHaveValue('2026-11-11');
    expect(remove).not.toHaveBeenCalled();
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await waitFor(() => expect(remove).toHaveBeenCalledWith('c1', [{ id: 15, revision: 2 }]));
    expect(add).toHaveBeenCalledWith('c1', { issue: '10001', day: '2026-11-11', spent_seconds: 5400, description: 'Commits on the branch' });
  });

  it('keeps an entry of unknown outcome when its time could not be logged by hand', async () => {
    const unknown = drafted(15, '2026-11-11', 90, { state: 'unknown', refusal: 'may already be logged' });
    answerDraft([], [unknown]);
    setBindingMock('AddTrackerWorklog', async () => ({ parts: [{ step: 'worklog', state: 'refused', refusal: { reason: 'refused', message: 'Worklog is closed.' } }] }) as WriteView);
    const remove = setBindingMock('RemoveTrackerTimeEntries', async () => [unknown]);
    const view = render(TimeScreen, BASE);
    await fireEvent.click(within(await view.findByTestId('time-draft-entry')).getByTestId('time-draft-entry-log-by-hand'));
    await fireEvent.click(within(await body().findByTestId('worklog-form')).getByTestId('worklog-form-save'));
    await body().findByTestId('worklog-form-write-outcome');
    expect(remove).not.toHaveBeenCalled();
  });

  it('shows the draft dashed in its tickets\' cells and the questions in a last Not placed row', async () => {
    answerDraft(WEEK, DRAFT);
    const view = render(TimeScreen, BASE);
    await view.findByTestId('time-draft-banner');
    await fireEvent.click(view.getByRole('radio', { name: 'By ticket' }));
    const web1 = view.getAllByTestId('time-ticket-row').find((r) => r.dataset.issue === '10001')!;
    const wed = within(web1).getAllByTestId('time-cell').find((c) => c.dataset.day === '2026-11-11')!;
    expect(within(wed).getByTestId('time-cell-drafted')).toHaveTextContent('2h 30m');
    const notPlaced = view.getByTestId('time-not-placed-row');
    expect(within(notPlaced).getAllByTestId('time-not-placed-cell').map((c) => c.dataset.day)).toEqual(['2026-11-12']);
    await fireEvent.click(within(notPlaced).getByTestId('time-not-placed-cell'));
    expect(within(await body().findByTestId('time-draft-questions')).getAllByTestId('time-draft-question').map((q) => q.dataset.entry)).toEqual(['13']);
    await fireEvent.click(wed);
    expect(await body().findByTestId('time-draft-form')).toBeInTheDocument();
    expect(view.getByTestId('time-draft-note')).toHaveTextContent("Dashed cells are your agent's draft. Nothing is posted until you approve.");
  });

  it('marks each day of the compact strip with what it holds', async () => {
    answerDraft(WEEK, DRAFT);
    const view = render(TimeScreen, { ...BASE, compact: true });
    await view.findByTestId('time-draft-banner');
    expect(view.getAllByTestId('time-day-tab').map((t) => [t.dataset.day, t.dataset.mark, t.title])).toEqual([
      ['2026-11-09', 'logged', 'Logged'],
      ['2026-11-10', 'logged', 'Logged'],
      ['2026-11-11', 'drafted', 'Drafted, not logged'],
      ['2026-11-12', 'question', 'A question is waiting'],
      ['2026-11-13', 'none', 'Nothing yet'],
    ]);
  });

  it('moves to a week another surface asks for', async () => {
    answerDraft([], []);
    const view = render(TimeScreen, BASE);
    await view.findByTestId('time-week');
    await view.rerender({ ...BASE, askedWeek: { monday: '2026-11-02' } });
    await waitFor(() => expect(getBindingMock('TrackerWorklogs')!.mock.calls.at(-1)).toEqual(['c1', 'acct-1', '2026-11-02', '2026-11-08']));
  });
});

// What another window changes while a form or a question is open: the
// person's press acts on what the screen showed them.
describe('a draft changed while the person looks at it', () => {
  // The week's next stored read answers `entries`, and a frame names the
  // day, as the computer sends one after another window's change.
  async function changedElsewhere(worklogs: WorklogView[], entries: TimeEntryView[], day = '2026-11-11'): Promise<void> {
    answerDraft(worklogs, entries);
    trackerTimeEntriesUpdated({ connection: 'c1', author: 'acct-1', days: [day], threads: ['t1'] }, 'peer-1');
    // The frame's read is coalesced on the clock, which these tests hold.
    vi.setSystemTime(Date.now() + 6 * DRAFT_REREAD_DELAY_MS);
    await waitFor(() => expect(getBindingMock('TrackerWorklogs')!.mock.calls.length).toBeGreaterThan(0));
  }

  it('logs and removes from its form the entry as the form shows it, not a later change', async () => {
    answerDraft([], [drafted(11, '2026-11-11', 60)]);
    const post = setBindingMock('PostTrackerTimeEntries', async () => POSTED('Entry 11 changed since it was read and was not posted.'));
    const remove = setBindingMock('RemoveTrackerTimeEntries', async () => []);
    const view = render(TimeScreen, BASE);
    await fireEvent.click(await view.findByTestId('time-draft-entry-hours'));
    const form = await body().findByTestId('time-draft-form');
    await changedElsewhere([], [drafted(11, '2026-11-11', 180, { revision: 3, issue: '10002', key: 'WEB-2', title: 'Second story' })]);
    await waitFor(() => expect(view.getByTestId('time-draft-entry-hours')).toHaveTextContent('3h'));
    expect(within(form).getByTestId('time-draft-form-ticket')).toHaveTextContent('WEB-1');
    await fireEvent.click(within(form).getByTestId('time-draft-form-log'));
    await waitFor(() => expect(post).toHaveBeenCalled());
    expect(post).toHaveBeenCalledWith('c1', { entries: [{ id: 11, revision: 2 }], alone: true });
    await fireEvent.click(within(form).getByTestId('time-draft-form-remove'));
    await waitFor(() => expect(remove).toHaveBeenCalled());
    expect(remove).toHaveBeenCalledWith('c1', [{ id: 11, revision: 2 }]);
  });

  it('discards the entries its question named, not ones drafted after it asked', async () => {
    answerDraft([], [drafted(11, '2026-11-11', 60)]);
    const remove = setBindingMock('RemoveTrackerTimeEntries', async () => []);
    const view = render(TimeScreen, BASE);
    await fireEvent.click(await view.findByTestId('time-draft-discard-week'));
    expect(view.getByTestId('time-draft-discard-week-confirm')).toHaveTextContent('Remove the 1 entry of this draft? Nothing is logged.');
    await changedElsewhere([], [drafted(21, '2026-11-11', 120)]);
    await waitFor(() => expect(view.getByTestId('time-draft-entry').dataset.entry).toBe('21'));
    await fireEvent.click(view.getByTestId('time-draft-discard-week-confirm-yes'));
    await waitFor(() => expect(remove).toHaveBeenCalled());
    expect(remove).toHaveBeenCalledWith('c1', [{ id: 11, revision: 2 }]);
  });

  it('logs nothing by hand for an entry of unknown outcome that was settled elsewhere', async () => {
    const unknown = drafted(15, '2026-11-11', 90, { state: 'unknown', refusal: 'may already be logged' });
    answerDraft([], [unknown]);
    const add = setBindingMock('AddTrackerWorklog', async () => APPLIED);
    setBindingMock('RemoveTrackerTimeEntries', async () => [unknown]);
    const view = render(TimeScreen, BASE);
    await fireEvent.click(within(await view.findByTestId('time-draft-entry')).getByTestId('time-draft-entry-log-by-hand'));
    const form = await body().findByTestId('worklog-form');
    await changedElsewhere([log('601', '2026-11-11', 5400)], []);
    await waitFor(() => expect(view.queryByTestId('time-draft-entry')).toBeNull());
    await fireEvent.click(within(form).getByTestId('worklog-form-save'));
    await tick();
    expect(add).not.toHaveBeenCalled();
    expect(within(form).getByTestId('worklog-form-waiting')).toHaveTextContent('This entry was removed or logged elsewhere, so its time is not logged from here.');
  });
});

describe('what the week shows of a posted draft', () => {
  it('draws a posted entry whose worklog the read does not hold as nothing, and the logged time as the worklogs say', async () => {
    answerDraft([], [drafted(11, '2026-11-11', 60, { state: 'posted', worklog: '701' })]);
    const view = render(TimeScreen, BASE);
    const wed = (await view.findAllByTestId('time-day')).find((d) => d.dataset.day === '2026-11-11')!;
    expect(within(wed).queryByTestId('time-draft-entry')).toBeNull();
    expect(within(wed).getByTestId('time-day-empty')).toBeInTheDocument();
  });

  it('says in the banner what the draft logged once nothing of it is left to approve', async () => {
    answerDraft([log('701', '2026-11-11', 3600), log('702', '2026-11-12', 1800)], [
      drafted(11, '2026-11-11', 60, { state: 'posted', worklog: '701' }),
      drafted(12, '2026-11-12', 30, { state: 'posted', worklog: '702' }),
    ]);
    const view = render(TimeScreen, BASE);
    expect(await view.findByTestId('time-draft-banner-done')).toHaveTextContent('Logged 1h 30m in 2 worklogs');
  });

  it('says beside a question left what the draft logged', async () => {
    answerDraft([log('701', '2026-11-11', 3600)], [drafted(11, '2026-11-11', 60, { state: 'posted', worklog: '701' }), asked(13, '2026-11-12', 25)]);
    const view = render(TimeScreen, BASE);
    expect(await view.findByTestId('time-draft-banner')).toHaveTextContent('1h is logged. Answer it in that day\'s column.');
  });
});
