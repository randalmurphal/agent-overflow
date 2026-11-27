// A dashboard widget as rendered: a number with its change against the
// range before, a gap said in words, a widget that counts now never with
// a change mark, a metric's failure said in its place, the table's person
// narrowing and its cells opening their rows, a list's tickets opening,
// and How this is counted in the widget's place.

import { fireEvent, render, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import type { RangeView, WidgetValue, WidgetView } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
import type { DrillState } from '../../stores/trackerDashboard.svelte';
import DashboardDrill from './DashboardDrill.svelte';
import DashboardWidget from './DashboardWidget.svelte';

const HOW = { words: ['The estimate of the tickets finished in each sprint, averaged.'], counts: 'The points of each sprint added up', range: 'It counts the range\'s sprints.', rules: 'none', sql: 'SELECT 1', reads: ['tickets'] };
const RANGE = { id: 'sprints_5', label: 'Last 5 sprints', periods: [], previous: 'the 5 sprints before' } as unknown as RangeView;

function view(over: Record<string, unknown>): WidgetView {
  return { id: 'pace', title: 'Usual pace', form: 'number', width: 4, unit: 'estimate', good: 'up', how: HOW, ...over } as unknown as WidgetView;
}

function draw(v: WidgetView, value: WidgetValue | undefined, range: RangeView = RANGE) {
  const onDrill = vi.fn();
  const onPerson = vi.fn();
  const onEpic = vi.fn();
  const onTicket = vi.fn();
  const r = render(DashboardWidget, { props: { view: v, value, range, estimates: 'points', onDrill, onPerson, onEpic, onTicket } });
  return { ...r, onDrill, onPerson, onEpic, onTicket };
}

describe('a number', () => {
  it('shows its value and its change from the range before, colored by its good direction, and opens its rows', async () => {
    const { getByTestId, onDrill } = draw(view({}), { id: 'pace', value: { v: 12 }, previous: { v: 9 }, trend: [{ v: 10 }, { v: 14 }] } as WidgetValue);
    expect(getByTestId('dashboard-number-value').textContent).toBe('12');
    const mark = getByTestId('dashboard-change');
    expect(mark.dataset.tone).toBe('good');
    expect(mark.textContent).toContain('Up 3 points from 9, the 5 sprints before');
    await fireEvent.click(getByTestId('dashboard-number'));
    expect(onDrill).toHaveBeenCalledWith({ widget: 'pace' });
  });

  it('says how many tickets the range before did not count, which could turn its change', () => {
    const { getByTestId } = draw(view({ unit: 'tickets' }), { id: 'pace', value: { v: 1 }, previous: { v: 0.5, nc: 2 } } as WidgetValue);
    expect(getByTestId('dashboard-change').textContent?.trim()).toBe('Up 0.5 tickets from 0.5, the 5 sprints before, 2 not counted then');
  });

  it('says why it is not compared when the range before is a gap', () => {
    const why = 'This computer keeps fewer than 5 finished sprints before these to compare with.';
    const { getByTestId, queryByTestId } = draw(view({}), { id: 'pace', value: { v: 12 }, previous: { gap: true } } as WidgetValue, { ...RANGE, previous_gap: why } as RangeView);
    expect(getByTestId('dashboard-change-gap').textContent).toBe(`Not compared: ${why}`);
    expect(queryByTestId('dashboard-change')).toBeNull();
  });

  it('that counts now shows "Now" and never a change mark, whatever its answer holds', () => {
    const { getByTestId, queryByTestId } = draw(view({ id: 'stuck', now: true, good: 'down', unit: 'tickets' }), { id: 'stuck', value: { v: 4 }, previous: { v: 2 } } as WidgetValue);
    expect(getByTestId('dashboard-now')).toBeTruthy();
    expect(queryByTestId('dashboard-change')).toBeNull();
    expect(queryByTestId('dashboard-change-gap')).toBeNull();
  });
});

describe('a number with tickets not counted', () => {
  function drill(value: object, over: object = {}) {
    const state = {
      target: { widget: 'stuck' },
      page: 0,
      loading: false,
      error: null,
      answer: { title: 'Stuck', value, unit: 'tickets', estimates: 'points', count: 0, tickets: 0, counted: 0, page: 0, pages: 1, rows: [], at: 0, ...over },
    } as unknown as DrillState;
    return render(DashboardDrill, { props: { drill: state, onPage: vi.fn(), onTicket: vi.fn(), onClose: vi.fn() } });
  }

  it('reads in the same words on the tile, a cell\'s tip, the list and the drill', () => {
    const n = { v: 3, nc: 2 };
    const tile = draw(view({ id: 'stuck', unit: 'tickets', now: true }), { id: 'stuck', value: n } as unknown as WidgetValue);
    expect(tile.getByTestId('dashboard-number-value').textContent).toBe('3');
    expect(tile.getByTestId('dashboard-number-not-counted').textContent).toBe('2 not counted');
    expect(tile.getByTestId('dashboard-number').getAttribute('aria-label')).toBe('Usual pace: 3 tickets, 2 not counted. Show its tickets');
    tile.unmount();

    const table = draw(view({ id: 'carrying', form: 'table', unit: 'tickets', bucket: 'person', series: ['To do'] }), {
      id: 'carrying', series: ['To do'], buckets: [{ key: 'acct-1', label: 'User 1', cells: [n], total: n }],
    } as unknown as WidgetValue);
    expect(table.getByTestId('dashboard-cell').getAttribute('title')).toBe('User 1, To do: 3 tickets, 2 not counted');
    table.unmount();

    const list = draw(view({ id: 'stuck_list', form: 'list', unit: 'tickets', now: true }), { id: 'stuck_list', tickets: [], not_counted: 2 } as unknown as WidgetValue);
    expect(list.getByTestId('dashboard-not-counted').textContent).toBe('2 not counted');
    list.unmount();

    const d = drill(n);
    expect(d.getByTestId('dashboard-drill-value').textContent).toBe('3 tickets');
    expect(d.getByTestId('dashboard-drill-value-not-counted').textContent).toBe('2 not counted');
  });

  it('lists a ticket that counts twice as two rows with their periods, and says so in its header', () => {
    // A key that two rows share is repaired and reported, never drawn
    // silently; each row has its own.
    const reported = vi.fn();
    vi.stubGlobal('reportError', reported);
    const row = { id: '10002', key: 'WEB-2', title: 'Second story', status: 'Done', counted: true, value: 5 };
    const d = drill({ v: 10 }, {
      unit: 'estimate',
      count: 2,
      tickets: 1,
      counted: 2,
      rows: [{ ...row, row: 0, period: 'WEB Sprint 5' }, { ...row, row: 1, period: 'WEB Sprint 6' }],
    });
    expect(d.getByTestId('dashboard-drill-count').textContent).toBe('2 rows, 1 ticket');
    expect(d.getAllByTestId('dashboard-drill-period').map((e) => e.textContent)).toEqual(['WEB Sprint 5', 'WEB Sprint 6']);
    expect(d.getAllByTestId('dashboard-drill-row-value').map((e) => e.textContent)).toEqual(['5 points', '5 points']);
    expect(reported).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it('says in the drill that the board\'s first read has not finished, from its answer', () => {
    const d = drill({ v: 1 }, { reading: true });
    expect(d.getByTestId('dashboard-drill-reading').textContent).toBe("This board's first read has not finished: these numbers count the tickets read so far.");
    d.unmount();
    expect(drill({ v: 1 }).queryByTestId('dashboard-drill-reading')).toBeNull();
  });

  it('is not known when it counted nothing, with no change mark, on the tile and in the drill', () => {
    const tile = draw(view({ unit: 'tickets' }), { id: 'pace', value: { nc: 2 }, previous: { v: 1 } } as unknown as WidgetValue);
    expect(tile.getByTestId('dashboard-number-value').textContent).toBe('Not known');
    expect(tile.getByTestId('dashboard-number-not-counted').textContent).toBe('2 not counted');
    expect(tile.queryByTestId('dashboard-change')).toBeNull();
    tile.unmount();
    const d = drill({ nc: 2 });
    expect(d.getByTestId('dashboard-drill-value').textContent).toBe('Not known');
    expect(d.getByTestId('dashboard-drill-value-not-counted').textContent).toBe('2 not counted');
  });
});

it('says why its metric failed, in its place', () => {
  const { getByTestId, queryByTestId } = draw(view({}), { id: 'pace', error: 'The query read more than 20,000 rows, the limit for one widget.' } as WidgetValue);
  expect(getByTestId('dashboard-widget-error').textContent).toBe('The query read more than 20,000 rows, the limit for one widget.');
  expect(queryByTestId('dashboard-number')).toBeNull();
});

it('shows How this is counted in its place, and its numbers again when turned off', async () => {
  const { getByTestId, queryByTestId } = draw(view({}), { id: 'pace', value: { v: 12 } } as WidgetValue);
  await fireEvent.click(getByTestId('dashboard-how-toggle'));
  expect(getByTestId('dashboard-how').textContent).toContain('Rules it reads: none.');
  expect(getByTestId('dashboard-how-sql').textContent).toBe('SELECT 1');
  expect(queryByTestId('dashboard-number')).toBeNull();
  await fireEvent.click(getByTestId('dashboard-how-toggle'));
  expect(getByTestId('dashboard-number')).toBeTruthy();
});

it('narrows the page to a table\'s person and opens a cell\'s rows', async () => {
  const v = view({ id: 'carrying', title: 'Who is carrying what', form: 'table', bucket: 'person', series: ['To do', 'Doing', 'Done'] });
  const value = {
    id: 'carrying',
    series: ['To do', 'Doing', 'Done'],
    buckets: [{ key: 'acct-1', label: 'User 1', cells: [{ v: 3 }, { v: 2 }, {}], total: { v: 5 } }, { key: '', label: 'Unassigned', cells: [{ v: 1 }, {}, {}], total: { v: 1 } }],
    more: 2,
    not_counted: 1,
  } as unknown as WidgetValue;
  const { getAllByTestId, getByTestId, onPerson, onDrill } = draw(v, value);
  const rows = getAllByTestId('dashboard-table-row');
  // Unassigned narrows nothing.
  expect(within(rows[1]).queryByTestId('dashboard-table-label')).toBeNull();
  await fireEvent.click(within(rows[0]).getByTestId('dashboard-table-label'));
  expect(onPerson).toHaveBeenCalledWith({ id: 'acct-1', label: 'User 1' });
  await fireEvent.click(within(rows[0]).getAllByTestId('dashboard-cell')[1]);
  expect(onDrill).toHaveBeenCalledWith({ widget: 'carrying', cell: { bucket: 'acct-1', series: 'Doing' } });
  await fireEvent.click(within(rows[0]).getByTestId('dashboard-cell-total'));
  expect(onDrill).toHaveBeenLastCalledWith({ widget: 'carrying', cell: { bucket: 'acct-1', series: '' } });
  expect(getByTestId('dashboard-more').textContent).toBe('2 more people');
  // More opens the rows of the people the table left out.
  await fireEvent.click(getByTestId('dashboard-more'));
  expect(onDrill).toHaveBeenLastCalledWith({ widget: 'carrying', rest: true });
  // Its "not counted" opens the whole table.
  await fireEvent.click(getByTestId('dashboard-not-counted'));
  expect(onDrill).toHaveBeenLastCalledWith({ widget: 'carrying' });
  // Unassigned's cell opens Unassigned's rows of its series, its key empty.
  await fireEvent.click(within(rows[1]).getAllByTestId('dashboard-cell')[0]);
  expect(onDrill).toHaveBeenLastCalledWith({ widget: 'carrying', cell: { bucket: '', series: 'To do' } });
});

it('opens a listed ticket, and the rest and the uncounted in the drill', async () => {
  const v = view({ id: 'stuck_list', title: 'Stuck tickets', form: 'list', now: true, unit: 'tickets' });
  const value = {
    id: 'stuck_list',
    tickets: [{ row: 0, id: '10001', key: 'WEB-1', title: 'First story', status: 'In Progress', counted: true, note: '9 days in In Progress' }],
    more: 3,
    not_counted: 1,
  } as unknown as WidgetValue;
  const { getByTestId, onTicket, onDrill } = draw(v, value);
  await fireEvent.click(getByTestId('dashboard-list-row'));
  expect(onTicket).toHaveBeenCalledWith(expect.objectContaining({ id: '10001', key: 'WEB-1' }));
  expect(getByTestId('dashboard-not-counted').textContent).toBe('1 not counted');
  await fireEvent.click(getByTestId('dashboard-more'));
  expect(onDrill).toHaveBeenCalledWith({ widget: 'stuck_list' });
});

describe('a chart of the sprint\'s days', () => {
  const day = (key: string, label: string, v: object, over: object = {}) => ({ key, label, cells: [v], total: v, ...over });
  const burn = view({ id: 'burndown', title: 'Burndown', form: 'line', bucket: 'day', unit: 'estimate', good: 'down', width: 6 });
  const buckets = [
    day('day:start', 'Start', { v: 12 }),
    day('day:2026-11-23', 'Nov 23', { v: 10, nc: 1 }),
    day('day:2026-11-24', 'Nov 24', { v: 7 }, { partial: true }),
    day('day:2026-11-25', 'Nov 25', {}, { last: true }),
  ];

  it('names the sprint it counted in How this is counted, and says why it has no days', async () => {
    const counted = draw(burn, { id: 'burndown', series: [''], buckets, range_words: 'It counts WEB Sprint 9 at its start and at the end of each working day; the range does not change it.' } as unknown as WidgetValue);
    await fireEvent.click(counted.getByTestId('dashboard-how-toggle'));
    expect(counted.getByTestId('dashboard-how').textContent).toContain('It counts WEB Sprint 9 at its start');
    counted.unmount();
    const none = draw(burn, { id: 'burndown', note: 'No sprint is active on this board.' } as unknown as WidgetValue);
    expect(none.getByTestId('dashboard-widget-note').textContent).toBe('No sprint is active on this board.');
    expect(none.queryByTestId('dashboard-line')).toBeNull();
  });
});

it('compares a state with what held when the range began, not with the range before', () => {
  const { getByTestId } = draw(view({ id: 'stuck', state: true, good: 'down', unit: 'tickets' }), { id: 'stuck', value: { v: 4 }, previous: { v: 2, nc: 1 } } as WidgetValue);
  expect(getByTestId('dashboard-change').textContent?.trim()).toBe('Up 2 tickets from 2, when the range began, 1 not counted then');
  expect(getByTestId('dashboard-change').dataset.tone).toBe('bad');
});
