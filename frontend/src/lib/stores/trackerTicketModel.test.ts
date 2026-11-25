// The ticket screen's rules as tables: the comment and history windows,
// the rail's order, the side a new link sends, a poker round's result, the
// attachment a file in the text names, and the row a ticket shows its
// points from.

import { describe, expect, it } from 'vitest';
import type { AttachmentView, CommentView, DetailView, EventView, FieldView, PokerVoteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  commentWindow,
  heldComments,
  historyWindow,
  PREVIEW_MAX_BYTES,
  linkRelations,
  linkRequest,
  mediaAttachment,
  moreFields,
  newestHistory,
  pokerResult,
  sectionFields,
  previewsInApp,
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
    expect(commentWindow(held, 2, 'oldest', new Set())).toEqual({ comments: [held[1], held[0]], hidden: 3 });
    expect(commentWindow(held, 2, 'newest', new Set())).toEqual({ comments: [held[0], held[1]], hidden: 3 });
    expect(commentWindow(held, 50, 'oldest', new Set()).hidden).toBe(0);
  });

  it('widens to the oldest comment holding unsent text, keeping the window one run', () => {
    const held = heldComments(detail, [{ comments: [comment('6', 6), comment('5', 5)] }]);
    expect(commentWindow(held, 2, 'newest', new Set(['6'])).comments.map((c) => c.id)).toEqual(['9', '8', '7', '6']);
    expect(commentWindow(held, 2, 'oldest', new Set(['6', '7'])).comments.map((c) => c.id)).toEqual(['6', '7', '8', '9']);
    // A draft already in the window, or on a comment no longer held, widens nothing.
    expect(commentWindow(held, 2, 'newest', new Set(['8', 'gone'])).hidden).toBe(3);
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

describe('links', () => {
  const types = [
    { id: '10000', name: 'Blocks', outward: 'blocks', inward: 'is blocked by' },
    { id: '10003', name: 'Relates', outward: 'relates to', inward: 'relates to' },
  ];

  it('offers each side of a type once, a symmetric type once', () => {
    expect(linkRelations(types).map((r) => r.phrase)).toEqual(['blocks', 'is blocked by', 'relates to']);
  });

  it('sends the ticket on the side its phrase reads from', () => {
    const [blocks, blockedBy] = linkRelations(types);
    expect(linkRequest(blocks, '10101', '10102')).toEqual({ type: '10000', outward: '10101', inward: '10102' });
    expect(linkRequest(blockedBy, '10101', '10102')).toEqual({ type: '10000', outward: '10102', inward: '10101' });
  });
});

describe('poker', () => {
  const vote = (value: number | null): PokerVoteView => ({ voter: { id: 'u', name: 'User' }, value } as PokerVoteView);

  it('counts each value shown, most votes first, the lower value first among equals', () => {
    expect(pokerResult([vote(5), vote(3), vote(5), vote(8), vote(3), vote(null)])).toEqual([
      { value: 3, count: 2 }, { value: 5, count: 2 }, { value: 8, count: 1 },
    ]);
    expect(pokerResult([vote(null)])).toEqual([]);
  });
});

describe('files in the text', () => {
  const file = (id: string, name: string, mime: string, size: number): AttachmentView => ({ id, name, mime_type: mime, size } as AttachmentView);

  it('names the one attachment of that name, and none when several share it', () => {
    const a = file('1', 'diagram.png', 'image/png', 10);
    expect(mediaAttachment([a, file('2', 'notes.txt', 'text/plain', 5)], 'diagram.png')).toBe(a);
    expect(mediaAttachment([a, file('3', 'diagram.png', 'image/png', 10)], 'diagram.png')).toBeNull();
    expect(mediaAttachment([a], 'missing.png')).toBeNull();
  });

  it('previews an image up to the cap, and nothing else', () => {
    expect(previewsInApp(file('1', 'a.png', 'image/png', PREVIEW_MAX_BYTES))).toBe(true);
    expect(previewsInApp(file('1', 'a.png', 'image/png', PREVIEW_MAX_BYTES + 1))).toBe(false);
    expect(previewsInApp(file('1', 'a.pdf', 'application/pdf', 10))).toBe(false);
  });
});
