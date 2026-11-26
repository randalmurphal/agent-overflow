// A dashboard widget as rendered: a number with its change against the
// range before, a gap said in words, a widget that counts now never with
// a change mark, a metric's failure said in its place, the table's person
// narrowing and its cells opening their rows, a list's tickets opening,
// and How this is counted in the widget's place.

import { fireEvent, render, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import type { RangeView, WidgetValue, WidgetView } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
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
  } as unknown as WidgetValue;
  const { getAllByTestId, getByTestId, onPerson, onDrill } = draw(v, value);
  const rows = getAllByTestId('dashboard-table-row');
  // Unassigned narrows nothing.
  expect(within(rows[1]).queryByTestId('dashboard-table-label')).toBeNull();
  await fireEvent.click(within(rows[0]).getByTestId('dashboard-table-label'));
  expect(onPerson).toHaveBeenCalledWith({ id: 'acct-1', label: 'User 1' });
  await fireEvent.click(within(rows[0]).getAllByTestId('dashboard-cell')[1]);
  expect(onDrill).toHaveBeenCalledWith({ widget: 'carrying', bucket: 'acct-1', series: 'Doing' });
  await fireEvent.click(within(rows[0]).getByTestId('dashboard-cell-total'));
  expect(onDrill).toHaveBeenLastCalledWith({ widget: 'carrying', bucket: 'acct-1', series: '' });
  expect(getByTestId('dashboard-more').textContent).toBe('2 more people');
});

it('opens a listed ticket, and the rest and the uncounted in the drill', async () => {
  const v = view({ id: 'stuck_list', title: 'Stuck tickets', form: 'list', now: true, unit: 'tickets' });
  const value = {
    id: 'stuck_list',
    tickets: [{ id: '10001', key: 'WEB-1', title: 'First story', status: 'In Progress', counted: true, note: '9 days in In Progress' }],
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
