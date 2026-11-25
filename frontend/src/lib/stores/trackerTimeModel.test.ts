import { describe, expect, it } from 'vitest';
import type { WorklogDays, WorklogView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { addDays, isWeekend, localDay, mondayOf, weekDays, weekShape, weekTickets } from './trackerTimeModel';

function log(id: string, day: string, spent: number, issue = '10001', key = 'WEB-1', title = 'First story'): WorklogView {
  return { id, issue, key, title, author: 'acct-1', day, spent_seconds: spent, updated: 1 };
}

function answer(worklogs: WorklogView[], extra: Partial<WorklogDays> = {}): WorklogDays {
  return { worklogs, ...extra };
}

const MONDAY = '2026-11-09';

describe('week days', () => {
  it('finds the Monday of every day of a week, across a month and a year', () => {
    expect(weekDays(MONDAY).map(mondayOf)).toEqual(Array(7).fill(MONDAY));
    expect(mondayOf('2026-12-03')).toBe('2026-11-30');
    expect(mondayOf('2027-01-03')).toBe('2026-12-28');
    expect(weekDays('2026-12-28')).toEqual(['2026-12-28', '2026-12-29', '2026-12-30', '2026-12-31', '2027-01-01', '2027-01-02', '2027-01-03']);
  });

  it('moves by days across a daylight saving change, which a day string has none of', () => {
    expect(addDays('2026-03-07', 1)).toBe('2026-03-08');
    expect(addDays('2026-03-08', 1)).toBe('2026-03-09');
    expect(addDays('2026-11-01', -1)).toBe('2026-10-31');
  });

  it('takes today from the window\'s local date', () => {
    expect(localDay(new Date(2026, 0, 5, 23, 59))).toBe('2026-01-05');
  });

  it('knows the weekend', () => {
    expect(weekDays(MONDAY).map(isWeekend)).toEqual([false, false, false, false, false, true, true]);
  });

  it('refuses what is not a day, loudly', () => {
    expect(() => mondayOf('2026-1-9')).toThrow(/is not a day/);
  });
});

describe('weekShape', () => {
  it('shows Monday to Friday with each day\'s entries and total, and the week\'s', () => {
    const shape = weekShape(MONDAY, answer([log('1', '2026-11-09', 3600), log('2', '2026-11-09', 1800, '10002', 'WEB-2', 'Second'), log('3', '2026-11-11', 5400)]));
    expect(shape.days.map((d) => d.day)).toEqual(weekDays(MONDAY).slice(0, 5));
    expect(shape.days.map((d) => d.total)).toEqual([5400, 0, 5400, 0, 0]);
    expect(shape.days[0].entries.map((w) => w.id)).toEqual(['1', '2']);
    expect(shape.total).toBe(10_800);
    expect(shape.uncounted).toBe(0);
  });

  it('adds Saturday or Sunday only for a week that holds time on it', () => {
    expect(weekShape(MONDAY, answer([log('1', '2026-11-15', 600)])).days.map((d) => d.day)).toEqual([...weekDays(MONDAY).slice(0, 5), '2026-11-15']);
    expect(weekShape(MONDAY, answer([], { unread: ['2026-11-14'] })).days).toHaveLength(5);
  });

  it('gives an unread day no total, never 0h, and leaves it out of the week\'s', () => {
    const shape = weekShape(MONDAY, answer([log('1', '2026-11-09', 3600)], { unread: ['2026-11-10', '2026-11-11'] }));
    expect(shape.days.map((d) => [d.total, d.uncounted])).toEqual([[3600, null], [null, 'unread'], [null, 'unread'], [0, null], [0, null]]);
    expect(shape.uncounted).toBe(2);
    expect(shape.total).toBe(3600);
  });

  it('with more, totals only the days before the last worklog\'s day', () => {
    const shape = weekShape(MONDAY, answer([log('1', '2026-11-09', 3600), log('2', '2026-11-10', 60), log('3', '2026-11-10', 60, '10002', 'WEB-2')], { more: true }));
    expect(shape.days.map((d) => d.uncounted)).toEqual([null, 'partial', 'partial', 'partial', 'partial']);
    expect(shape.total).toBe(3600);
    // A ticket's total counts the counted days alone, as the week's does.
    expect(weekTickets(shape).map((t) => [t.key, t.total])).toEqual([['WEB-1', 3600], ['WEB-2', 0]]);
  });

  it('makes a row per ticket by key, with each day\'s entries in its cells', () => {
    const shape = weekShape(MONDAY, answer([
      log('1', '2026-11-09', 3600, '10101', 'WEB-101'),
      log('2', '2026-11-09', 1800, '10012', 'WEB-12'),
      log('3', '2026-11-10', 900, '10012', 'WEB-12'),
      log('4', '2026-11-10', 300, '10012', 'WEB-12'),
      log('5', '2026-11-10', 60, '99', '', ''),
    ]));
    expect(weekTickets(shape).map((t) => t.key || t.issue)).toEqual(['WEB-12', 'WEB-101', '99']);
    const web12 = weekTickets(shape)[0];
    expect(web12.cells.map((c) => c.map((w) => w.id))).toEqual([['2'], ['3', '4'], [], [], []]);
    expect(web12.total).toBe(3000);
  });

  it('names a ticket from any entry that names it', () => {
    const shape = weekShape(MONDAY, answer([log('1', '2026-11-09', 60, '7', '', ''), log('2', '2026-11-10', 60, '7', 'WEB-7', 'Seventh')]));
    expect(weekTickets(shape).map((t) => [t.key, t.title])).toEqual([['WEB-7', 'Seventh']]);
  });
});
