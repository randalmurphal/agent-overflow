// What the Backlog screen draws of a held board: its sections, each
// header's numbers with the moved rows counted where they go, the one
// list its virtualizer shows and where a carried row lands in it, and
// the sprint forms' helpers and what Complete sprint counts. The header's words are boardFormat's
// sectionLine, tested beside it.

import { describe, expect, it } from 'vitest';
import type { BoardView, CardView, RowView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { sectionLine } from '../components/issues/boardFormat';
import {
  backlogItems,
  backlogSections,
  dayAt,
  dayValue,
  gapItem,
  nextSprintName,
  sectionNumbers,
  shownSections,
  spotAt,
  completionNumbers,
  finishedStatuses,
  waitingMoves,
  type BacklogSection,
} from './trackerBacklogModel';
import { heldFromView, type HeldBoard } from './trackerBoardModel';

const GROUPS: Record<string, string> = { s1: 'todo', s2: 'done', s3: 'todo', sx: 'canceled' };

function row(id: string, status: string, estimate: number | null, extra: Partial<CardView> = {}): RowView {
  return { order: id, estimate: estimate ?? undefined, card: { id, key: `WEB-${id}`, title: `Issue ${id}`, status: { id: status, group: GROUPS[status] }, created: 0, updated: 0, ...extra } } as RowView;
}

const U1 = { id: 'u1', name: 'User 1' };
const U2 = { id: 'u2', name: 'User 2' };

/** Sprint 8 active, Sprint 9 future, the backlog, on columns To Do (s1)
 * and Done (s2) unless `columns` names each column's statuses; s3 is a
 * status the project leaves out of counts, and `regrouped` the statuses
 * it puts in other groups. */
function board(rows: { '8': RowView[]; '9': RowView[]; backlog: RowView[] }, extra: { unit?: string; counts?: string; pace?: object | null; more?: boolean; regrouped?: Record<string, string>; columns?: string[][] } = {}): HeldBoard {
  const total = (held: RowView[]) => ({
    rows: held.filter((r) => r.card.status.id !== 's3').length,
    estimate: held.filter((r) => r.card.status.id !== 's3').reduce((n, r) => n + (r.estimate ?? 0), 0),
    unestimated: held.filter((r) => r.card.status.id !== 's3' && r.estimate == null).length,
  });
  const view = {
    id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'WEB board', plans_sprints: true, estimate: { unit: extra.unit ?? 'points' }, counts: extra.counts ?? extra.unit ?? 'points',
    columns: (extra.columns ?? [['s1'], ['s2']]).map((statuses, i) => ({ name: `Column ${i}`, statuses: statuses.map((id) => ({ id, group: GROUPS[id] })), total: { rows: 0, estimate: 0 } })),
    sprints: [
      { id: '8', name: 'Sprint 8', state: 'active', total: total(rows['8']) },
      { id: '9', name: 'Sprint 9', state: 'future', total: total(rows['9']) },
    ],
    all: { rows: 0, estimate: 0 }, backlog: total(rows.backlog), sync: {}, regrouped: { s3: 'left_out', ...extra.regrouped },
    screen: [{ kind: 'sprint', sprint: '8' }],
    plan: [{ kind: 'sprint', sprint: '8' }, { kind: 'sprint', sprint: '9' }, { kind: 'backlog' }],
    ...(extra.pace === null ? {} : { pace: extra.pace ?? { sprints: 2, rows: 3, estimate: 6 } }),
    sections: [
      { section: { kind: 'sprint', sprint: '8' }, rows: rows['8'], next: null },
      { section: { kind: 'sprint', sprint: '9' }, rows: rows['9'], next: null },
      { section: { kind: 'backlog' }, rows: rows.backlog, next: extra.more ? 'next' : null },
    ],
  } as unknown as BoardView;
  return heldFromView(view, (r) => r, true);
}

const ROWS = {
  '8': [row('1', 's2', 3), row('2', 's1', 2), row('3', 's3', 5, { assignee: U1 })],
  '9': [row('4', 's1', 1, { assignee: U1 }), row('5', 's1', 4, { assignee: U2 }), row('6', 's1', 2), row('7', 's1', null, { assignee: U1 })],
  backlog: [row('10', 's1', null), row('11', 's1', 1)],
};

describe('the sections', () => {
  it('are the board\'s plan in its order, each with its kind, sprint, rows and total', () => {
    const sections = backlogSections(board(ROWS));
    expect(sections.map((s) => [s.list, s.kind, s.sprint?.name ?? null, s.rows.length, s.unread, s.more])).toEqual([
      ['sprint:8', 'active', 'Sprint 8', 3, false, false],
      ['sprint:9', 'future', 'Sprint 9', 4, false, false],
      ['backlog', 'backlog', null, 2, false, false],
    ]);
    expect(sections[2].total).toEqual({ rows: 2, estimate: 1, unestimated: 1 });
  });

  it('says a planned section the board does not hold yet is unread', () => {
    const held = board(ROWS);
    const partial: HeldBoard = { ...held, sections: held.sections.slice(0, 1) };
    expect(backlogSections(partial).map((s) => s.unread)).toEqual([false, true, true]);
  });
});

describe('a header\'s numbers', () => {
  it('count an active sprint\'s done rows, leaving out the statuses the board does', () => {
    const held = board(ROWS);
    const [active] = backlogSections(held);
    const n = sectionNumbers(held, active, []);
    expect(n.total).toEqual({ rows: 2, estimate: 5, unestimated: 0 });
    expect(n.done).toEqual({ rows: 1, estimate: 3, unestimated: 0 });
    expect(n.before).toBeNull();
    expect(n.load).toBeNull();
    expect(n.pace).toBeNull();
    expect(sectionLine('active', n, 'points').map((p) => p.text).join('')).toBe('3 of 5 points done');
  });

  it('count an active sprint\'s done rows by the project\'s status groups', () => {
    const regrouped = board(ROWS, { regrouped: { s1: 'done' } });
    const project = sectionNumbers(regrouped, backlogSections(regrouped)[0], []);
    expect(sectionLine('active', project, 'points').map((p) => p.text).join(''), 'To Do counts as done').toBe('5 of 5 points done');
    const undone = board(ROWS, { regrouped: { s2: 'in_progress' } });
    expect(sectionNumbers(undone, backlogSections(undone)[0], []).done, 'Done counts as in progress').toEqual({ rows: 0, estimate: 0, unestimated: 0 });
  });

  it('weigh a future sprint against the pace, with each person\'s load, the most first and the unassigned last', () => {
    const held = board(ROWS);
    const future = backlogSections(held)[1];
    const n = sectionNumbers(held, future, []);
    expect(n.total.estimate).toBe(7);
    expect(n.load?.map((l) => [l.person?.name ?? null, l.rows, l.estimate])).toEqual([['User 2', 1, 4], ['User 1', 2, 1], [null, 1, 2]]);
    expect(n.over).toBe(1);
    const line = sectionLine('future', n, 'points');
    expect(line.map((p) => p.text).join('')).toBe('7 of about 6 points, the last 2 sprints\' average · 1 over');
    expect(line.filter((p) => p.tone === 'warn').map((p) => p.text)).toEqual(['1 over']);
    const one = board(ROWS, { pace: { sprints: 1, rows: 3, estimate: 7.25 } });
    const level = sectionNumbers(one, backlogSections(one)[1], []);
    expect(level.over).toBe(0);
    expect(sectionLine('future', level, 'points').map((p) => p.text).join('')).toBe('7 of about 7.3 points, the last sprint');
  });

  it('say a future sprint\'s points alone on a board that finished no sprint', () => {
    const held = board(ROWS, { pace: null });
    const n = sectionNumbers(held, backlogSections(held)[1], []);
    expect(n.pace).toBeNull();
    expect(n.over).toBe(0);
    expect(sectionLine('future', n, 'points').map((p) => p.text).join('')).toBe('7 points');
  });

  it('count the backlog\'s tickets, points and those with no estimate', () => {
    const held = board(ROWS);
    const n = sectionNumbers(held, backlogSections(held)[2], []);
    expect(sectionLine('backlog', n, 'points').map((p) => p.text).join('')).toBe('2 tickets · 1 point · 1 without an estimate');
  });

  it('count tickets where points would be, on a board without estimates', () => {
    const held = board(ROWS, { unit: 'none', pace: { sprints: 2, rows: 3, estimate: 0 } });
    const sections = backlogSections(held);
    const future = sectionNumbers(held, sections[1], []);
    expect(future.over).toBe(1);
    expect(sectionLine('future', future, 'none').map((p) => p.text).join('')).toBe('4 of about 3 tickets, the last 2 sprints\' average · 1 over');
    expect(sectionLine('backlog', sectionNumbers(held, sections[2], []), 'none').map((p) => p.text).join('')).toBe('2 tickets');
  });

  it('count tickets on a board with points when the project counts tickets, each person\'s load by tickets', () => {
    const held = board(ROWS, { counts: 'none', pace: { sprints: 2, rows: 5, estimate: 6 } });
    const future = sectionNumbers(held, backlogSections(held)[1], []);
    expect(future.over, '4 tickets against a pace of 5, not 7 points against 6').toBe(0);
    expect(future.load?.map((l) => l.person?.name ?? null)).toEqual(['User 1', 'User 2', null]);
  });

  it('count a row moving out of one section and into another where it goes, with what each was before', () => {
    const held = board(ROWS);
    const sections = backlogSections(held);
    const moved = [{ row: ROWS.backlog[1], from: 'backlog', to: 'sprint:9' }];
    const into = sectionNumbers(held, sections[1], moved);
    expect(into.total.estimate).toBe(8);
    expect(into.before?.estimate).toBe(7);
    expect(into.over).toBe(2);
    // The load counts it with its person: nobody's 2 points and its 1.
    expect(into.load?.at(-1)).toEqual({ person: null, rows: 2, estimate: 3 });
    const line = sectionLine('future', into, 'points');
    expect(line.map((p) => p.text).join('')).toBe('7 → 8 of about 6 points, the last 2 sprints\' average · 2 over');
    expect(line.filter((p) => p.tone === 'to').map((p) => p.text)).toEqual(['→ 8']);
    const out = sectionNumbers(held, sections[2], moved);
    expect(out.total).toEqual({ rows: 1, estimate: 0, unestimated: 1 });
    expect(sectionLine('backlog', out, 'points').map((p) => p.text).join('')).toBe('2 → 1 ticket · 1 → 0 points · 1 without an estimate');
    // A row in a status left out of counts changes no number.
    const left = sectionNumbers(held, sections[1], [{ row: ROWS['8'][2], from: 'sprint:8', to: 'sprint:9' }]);
    expect(left.before).toBeNull();
    expect(left.total.estimate).toBe(7);
  });

  it('wait for a section to be read whole before counting what is done or each person\'s load', () => {
    const held = board(ROWS, { more: true });
    const sections = backlogSections(held);
    expect(sections[2].more).toBe(true);
    const unread: BacklogSection = { ...sections[1], unread: true };
    expect(sectionNumbers(held, unread, []).load).toBeNull();
    expect(sectionNumbers(held, { ...sections[0], more: true }, []).done).toBeNull();
  });
});

describe('the rows shown', () => {
  it('draw a waiting row where it waits, and count it there', () => {
    const held = board(ROWS);
    const sections = backlogSections(held);
    const waiting = [{ issue: '11', list: 'sprint:9', before: '5' }];
    const shown = shownSections(sections, waiting, { mine: null, words: [] });
    expect(shown.map((rows) => rows.map((r) => r.card.id))).toEqual([['1', '2', '3'], ['4', '11', '5', '6', '7'], ['10']]);
    expect(waitingMoves(sections, waiting)).toEqual([{ row: ROWS.backlog[1], from: 'backlog', to: 'sprint:9' }]);
    // A row waiting in its own section moves no number.
    expect(waitingMoves(sections, [{ issue: '11', list: 'backlog', before: '10' }])).toEqual([]);
  });
});

describe('the one list and a carry\'s place in it', () => {
  const held = board(ROWS);
  const sections = backlogSections(held);
  const shown = sections.map((s) => s.rows);
  const items = backlogItems(sections, shown, (s) => s.kind !== 'active');

  it('is each header, then an open section\'s rows and its foot; a folded section is its header', () => {
    expect(items.map((it) => it.key)).toEqual([
      'h:sprint:8',
      'h:sprint:9', 'r:4', 'r:5', 'r:6', 'r:7', 'f:sprint:9',
      'h:backlog', 'r:10', 'r:11', 'f:backlog',
    ]);
    expect(items.every((it, i) => it.at === i)).toBe(true);
  });

  it('places a carried row at a header\'s top, a row\'s upper or lower half, or a foot\'s end', () => {
    expect(spotAt(items, shown, sections, 1, false)).toEqual({ list: 'sprint:9', slot: 0 });
    expect(spotAt(items, shown, sections, 3, false)).toEqual({ list: 'sprint:9', slot: 1 });
    expect(spotAt(items, shown, sections, 3, true)).toEqual({ list: 'sprint:9', slot: 2 });
    expect(spotAt(items, shown, sections, 6, false)).toEqual({ list: 'sprint:9', slot: 4 });
    expect(spotAt(items, shown, sections, 0, true)).toEqual({ list: 'sprint:8', slot: 0 });
    expect(spotAt(items, shown, sections, 99, false)).toBeNull();
  });

  it('opens the gap at the row a slot goes before, else at the foot, and nowhere in a folded section', () => {
    expect(gapItem(items, sections, 'sprint:9', 1)).toBe(3);
    expect(gapItem(items, sections, 'sprint:9', 4)).toBe(6);
    expect(gapItem(items, sections, 'backlog', 0)).toBe(8);
    expect(gapItem(items, sections, 'sprint:8', 0)).toBe(-1);
    expect(gapItem(items, sections, 'sprint:77', 0)).toBe(-1);
  });
});

describe('the sprint forms', () => {
  it('offer the next name after the last sprint\'s number', () => {
    const sp = (name: string) => ({ id: name, name, state: 'future', total: { rows: 0, estimate: 0 } });
    expect(nextSprintName([sp('Sprint 40'), sp('WEB Sprint 41')])).toBe('WEB Sprint 42');
    expect(nextSprintName([sp('Sprint 9'), sp('Planning')])).toBe('Sprint 10');
    expect(nextSprintName([sp('Planning')])).toBe('');
    expect(nextSprintName([])).toBe('');
  });

  it('turn a date input\'s day into its start or end in this computer\'s zone and back', () => {
    const start = dayAt('2026-03-09', false)!;
    const end = dayAt('2026-03-09', true)!;
    expect(new Date(start).getHours()).toBe(0);
    expect(new Date(end).getHours()).toBe(23);
    expect(new Date(end).getMinutes()).toBe(59);
    expect(dayValue(start)).toBe('2026-03-09');
    expect(dayValue(end)).toBe('2026-03-09');
    expect(dayAt('', false)).toBeNull();
    expect(dayAt('9 March', false)).toBeNull();
  });

});

describe('what Complete sprint counts', () => {
  const line = (n: { numbers: Parameters<typeof sectionLine>[1] }) => sectionLine('active', n.numbers, 'points').map((p) => p.text).join('');

  it('counts a ticket finished when its status is in the board\'s last column that has statuses, every ticket of the sprint, whatever the project regroups', () => {
    for (const held of [board(ROWS), board(ROWS, { regrouped: { s1: 'done', s2: 'in_progress' } })]) {
      const c = completionNumbers(held, backlogSections(held)[0], []);
      // WEB-3 is in s3, which the project leaves out of its counts: Jira counts it.
      expect(line(c)).toBe('3 of 10 points done');
      expect(c.unfinished).toBe(2);
    }
  });

  it('counts a done status of an earlier column unfinished, and another of the last column finished', () => {
    const held = board(ROWS, { columns: [['s1', 's2'], ['s3']] });
    const c = completionNumbers(held, backlogSections(held)[0], []);
    expect(line(c), 'WEB-3, To Do in the last column, is finished; WEB-1, Done in the first, is not').toBe('5 of 10 points done');
    expect(c.unfinished).toBe(2);
  });

  it('counts a row moving into the sprint, and one moving out, where they go', () => {
    const held = board(ROWS);
    const sections = backlogSections(held);
    const c = completionNumbers(held, sections[0], [{ row: ROWS.backlog[1], from: 'backlog', to: 'sprint:8' }, { row: ROWS['8'][0], from: 'sprint:8', to: 'sprint:9' }]);
    expect(line(c), 'WEB-11 in, WEB-1 out').toBe('0 of 10 → 8 points done');
    expect(c.unfinished).toBe(3);
  });

  it('counts nothing until the sprint is read whole', () => {
    const held = board(ROWS);
    const c = completionNumbers(held, { ...backlogSections(held)[0], more: true }, []);
    expect(c.numbers.done).toBeNull();
    expect(c.unfinished).toBeNull();
  });

  it('takes the last column that has statuses, as Jira\'s DoneStatusIDs does', () => {
    const cols = (...statuses: string[][]) => statuses.map((ids) => ({ statuses: ids.map((id) => ({ id })) }));
    expect([...finishedStatuses(cols(['a'], ['b', 'c']))]).toEqual(['b', 'c']);
    expect([...finishedStatuses(cols(['a'], ['b'], []))], 'the last column empty').toEqual(['b']);
    expect([...finishedStatuses(cols(['a'], [], []))], 'statuses only in an earlier column').toEqual(['a']);
    expect([...finishedStatuses(cols([], []))], 'no statuses at all').toEqual([]);
    expect([...finishedStatuses([])]).toEqual([]);
  });

  it('counts every ticket unfinished on a board with no column statuses, which Jira refuses to complete', () => {
    const held = board(ROWS, { columns: [[], []] });
    const c = completionNumbers(held, backlogSections(held)[0], []);
    expect(line(c)).toBe('0 of 10 points done');
    expect(c.unfinished).toBe(3);
  });
});
