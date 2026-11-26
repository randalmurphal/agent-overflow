// The ticket screen's rules as tables: the comment and history windows,
// the rail's and the sections' order by the project's pins, the side a new link sends, a poker round's result, the
// attachment a file in the text names, and the row a ticket shows its
// points from.

import { describe, expect, it } from 'vitest';
import type { AttachmentView, CommentView, DetailView, EventView, FieldView, PokerVoteView, Setup } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  commentWindow,
  heldComments,
  historyWindow,
  PREVIEW_MAX_BYTES,
  linkRelations,
  linkRequest,
  mediaAttachment,
  railFields,
  newestHistory,
  pokerResult,
  ticketPinsOf,
  ticketSections,
  withTicketPin,
  previewsInApp,
  sprintNames,
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

  const ids = (list: readonly FieldView[]) => list.map((f) => f.id);

  it('lists the fields that are not sections, filled first, each group in the tracker\'s order, narrowed by find', () => {
    expect(ids(railFields(fields, null, '').more)).toEqual(['c', 'e', 'a', 'd']);
    expect(ids(railFields(fields, null, 'field D').more)).toEqual(['d']);
    expect(railFields(fields, null, '')).toMatchObject({ pinned: [], missing: [] });
  });

  it('shows pinned fields first in pin order, the rest behind the control, and names a pin this ticket lacks', () => {
    const rail = railFields(fields, ['d', 'gone', 'c', 'b'], '');
    expect(ids(rail.pinned), 'a section is never a rail field').toEqual(['d', 'c']);
    expect(ids(rail.more)).toEqual(['e', 'a']);
    expect(rail.missing).toEqual(['gone', 'b']);
    expect(ids(railFields(fields, ['d'], 'field').more), 'find narrows the rest only').toEqual(['c', 'e', 'a']);
  });

  it('opens the description, links and attachments and folds the other sections while nothing is pinned', () => {
    expect(ticketSections(fields, null, true)).toEqual({ shown: ['description', 'links', 'attachments'], folded: ['b'], missing: [] });
    expect(ticketSections(fields, null, false), 'a ticket with no place for files shows none').toEqual({ shown: ['description', 'links'], folded: ['b'], missing: [] });
  });

  it('opens pinned sections in pin order and folds the rest in the ticket\'s order', () => {
    expect(ticketSections(fields, ['b', 'links', 'gone'], true)).toEqual({ shown: ['b', 'links'], folded: ['description', 'attachments'], missing: ['gone'] });
    expect(ticketSections(fields, [], true)).toEqual({ shown: [], folded: ['description', 'b', 'links', 'attachments'], missing: [] });
    expect(ticketSections(fields, ['attachments'], false), 'a pinned section the ticket has no place for is not missing').toEqual({ shown: [], folded: ['description', 'b', 'links'], missing: [] });
  });

  it('pins last, unpins, starts an unset list from what the ticket shows, and keeps the rest of the setup', () => {
    const site = 'https://example.atlassian.net';
    const base: Setup = { status_groups: [{ tracker: 'jira', site, statuses: { s1: 'done' } }], ticket_pins: [{ tracker: 'linear', site: 'example', sections: [] }], start_work: [] };
    const field = withTicketPin(base, 'jira', site, 'fields', 'c', true);
    expect(field.ticket_pins).toEqual([{ tracker: 'linear', site: 'example', sections: [] }, { tracker: 'jira', site, fields: ['c'] }]);
    expect(field.status_groups).toBe(base.status_groups);
    const section = withTicketPin(field, 'jira', site, 'sections', 'b', true);
    expect(ticketPinsOf(section, 'jira', site)).toEqual({ fields: ['c'], sections: ['description', 'links', 'attachments', 'b'] });
    const unpinned = withTicketPin(withTicketPin(section, 'jira', site, 'sections', 'description', false), 'jira', site, 'fields', 'c', false);
    expect(ticketPinsOf(unpinned, 'jira', site)).toEqual({ fields: [], sections: ['links', 'attachments', 'b'] });
    expect(ticketPinsOf(withTicketPin(base, 'jira', site, 'fields', 'c', true), 'jira', site).sections, 'a list not touched stays unset').toBeNull();
    expect(withTicketPin(field, 'jira', site, 'fields', 'c', true).ticket_pins?.[1].fields, 'pinned once').toEqual(['c']);
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

describe('sprint names', () => {
  it('names the sprints the boards hold and says the others in words, after them', () => {
    expect(sprintNames([])).toBe('');
    expect(sprintNames([{ id: '8', name: 'Sprint 8', state: 'active' }])).toBe('Sprint 8');
    expect(sprintNames([{ id: '8', name: 'Sprint 8', state: 'active' }, { id: '9', name: 'Sprint 9', state: 'future' }, { id: '90' }])).toBe('Sprint 8, Sprint 9, a sprint on another board');
    expect(sprintNames([{ id: '90' }, { id: '91' }])).toBe('2 sprints on other boards');
  });
});
