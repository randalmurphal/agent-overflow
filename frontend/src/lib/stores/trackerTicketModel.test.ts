// The ticket screen's rules as tables: the comment and history windows, the rail's order, and the row a
// ticket shows its points from.

import { describe, expect, it } from 'vitest';
import type { CommentView, DetailView, EventView, FieldView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  commentWindow,
  heldComments,
  historyWindow,
  moreFields,
  newestHistory,
  sectionFields,
  ticketRow,
} from './trackerTicketModel';

function comment(id: string, created: number): CommentView {
  return { id, author: { id: 'u1', name: 'User 1' }, body: `Comment ${id}`, created } as CommentView;
}

describe('comments', () => {
  const first = [comment('9', 9), comment('8', 8), comment('7', 7)];
  const detail = { comments: first } as unknown as DetailView;

  it('holds the first page, then the earlier pages, each comment once', () => {
    expect(heldComments(detail, [{ comments: [comment('7', 7)] }, { comments: [comment('6', 6)] }]).map((c) => c.id)).toEqual(['9', '8', '7', '6']);
    expect(heldComments(detail, [])).toBe(first);
  });

  it('mounts the newest of them, in the order they read', () => {
    const held = heldComments(detail, [{ comments: [comment('6', 6), comment('5', 5)] }]);
    expect(commentWindow(held, 2, 'oldest')).toEqual({ comments: [held[1], held[0]], hidden: 3 });
    expect(commentWindow(held, 2, 'newest')).toEqual({ comments: [held[0], held[1]], hidden: 3 });
    expect(commentWindow(held, 50, 'oldest').hidden).toBe(0);
  });

  it('mounts the newest changes first', () => {
    const history = [{ at: 1, field: 'status' }, { at: 3, field: 'assignee' }, { at: 2, field: 'priority' }] as EventView[];
    expect(historyWindow(newestHistory(history), 2)).toEqual({ events: [history[1], history[2]], hidden: 1 });
  });
});

describe('the rail', () => {
  const field = (id: string, kind: string, filled: boolean): FieldView => ({ id, name: `Field ${id}`, kind, value: filled ? { kind, texts: ['x'] } : {} } as FieldView);
  const fields = [field('a', 'text', false), field('b', 'rich_text', true), field('c', 'option', true), field('d', 'number', false), field('e', 'text', true)];

  it('lists the fields that are not sections, filled first, each group in the tracker\'s order, narrowed by find', () => {
    expect(moreFields(fields, '').map((f) => f.id)).toEqual(['c', 'e', 'a', 'd']);
    expect(moreFields(fields, 'field D').map((f) => f.id)).toEqual(['d']);
    expect(sectionFields(fields).map((f) => f.id)).toEqual(['b']);
  });

  it('shows the points of the board the ticket was opened from, else of the first board', () => {
    const rows = [{ board: '1', estimate: 3, unit: 'points' }, { board: '2', estimate: 5, unit: 'points' }];
    expect(ticketRow(rows, '2')).toBe(rows[1]);
    expect(ticketRow(rows, '9')).toBe(rows[0]);
    expect(ticketRow([], '1')).toBeNull();
  });
});
