// The Issues pane's view state: which board it opens on when none was
// picked, the board picked last, Display per board and Only mine.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { BoardChoice } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  compactColumn,
  hiddenCardFields,
  openingBoard,
  pickedBoard,
  resetIssuesPaneForTest,
  saveOnlyMine,
  savedOnlyMine,
  setCardFieldShown,
  setCompactColumn,
} from './issuesPane.svelte';
import type { ComputerChoices } from './trackerBoardChoices.svelte';
import type { TrackerBoardRef } from './trackerBoard.svelte';

function choice(project: string, board: string, extra: Partial<BoardChoice> = {}): BoardChoice {
  return {
    project, connection: 'c1', connection_kind: 'jira', connection_label: 'Work', connection_enabled: true,
    verified_account: 'acc-user1', board, name: `Board ${board}`, tracker_project: 'WEB', opens_first: false, ...extra,
  } as BoardChoice;
}

function answered(backend: string, rows: BoardChoice[]): { backend: string; state: ComputerChoices } {
  return { backend, state: { kind: 'answered', rows } };
}

function ref(backend: string, projectId: string, board: string): TrackerBoardRef {
  return { backend, projectId, connection: 'c1', board };
}

const noOrder = () => new Map<string, number>();
const everyListed = () => true;

beforeEach(() => {
  localStorage.clear();
  resetIssuesPaneForTest();
});

describe('the board the pane opens on when none was picked', () => {
  const home = answered('', [choice('p1', '1'), choice('p1', '2', { opens_first: true }), choice('p2', '3')]);
  const laptop = answered('laptop', [choice('p3', '4'), choice('p3', '5')]);

  it("is the board that opens first of the project it was opened from, else that project's first, once its computer answered", () => {
    expect(openingBoard([home, laptop], 'p1', everyListed, noOrder)).toEqual({ board: ref('', 'p1', '2'), waiting: false });
    expect(openingBoard([home, laptop], 'p3', everyListed, noOrder)).toEqual({ board: ref('laptop', 'p3', '4'), waiting: false });
    // The other computer has not answered: the project's own decides.
    expect(openingBoard([home, { backend: 'laptop', state: { kind: 'reading', rows: null } }], 'p1', everyListed, noOrder).board).toEqual(ref('', 'p1', '2'));
  });

  it('waits on a computer that has not answered before it falls back', () => {
    const reading = { backend: 'laptop', state: { kind: 'reading', rows: null } as ComputerChoices };
    const order = vi.fn(noOrder);
    expect(openingBoard([home, reading], 'p3', everyListed, order)).toEqual({ board: null, waiting: true });
    expect(openingBoard([home, reading], null, everyListed, order)).toEqual({ board: null, waiting: true });
    // The sidebar order is read only when the fallback decides.
    expect(order).not.toHaveBeenCalled();
  });

  it('is otherwise the first board of the first project in sidebar order, on any computer, counting a computer it could not read as answered', () => {
    const order = () => new Map([['p1', 3], ['p2', 2], ['p3', 1]]);
    expect(openingBoard([home, laptop], null, everyListed, order).board).toEqual(ref('laptop', 'p3', '4'));
    expect(openingBoard([home, laptop], 'unlisted', everyListed, () => new Map([['p2', 0]])).board).toEqual(ref('', 'p2', '3'));
    const offline = { backend: 'laptop', state: { kind: 'offline', rows: null } as ComputerChoices };
    expect(openingBoard([home, offline], null, everyListed, order)).toEqual({ board: ref('', 'p2', '3'), waiting: false });
  });

  it('waits for the sidebar to list each computer with a board to open before it reads the order', () => {
    const order = vi.fn(() => new Map([['p1', 3], ['p2', 2], ['p3', 1]]));
    const listed = (backend: string) => backend !== 'laptop';
    expect(openingBoard([home, laptop], null, listed, order)).toEqual({ board: null, waiting: true });
    expect(order).not.toHaveBeenCalled();
    // The project the pane was opened from needs no order.
    expect(openingBoard([home, laptop], 'p3', listed, order).board).toEqual(ref('laptop', 'p3', '4'));
    // A computer whose boards are all off is no candidate to wait on.
    const off = answered('laptop', [choice('p3', '4', { connection_enabled: false })]);
    expect(openingBoard([home, off], null, listed, order).board).toEqual(ref('', 'p2', '3'));
  });

  it('never opens a board whose connection is turned off', () => {
    const off = answered('', [choice('p1', '1', { opens_first: true, connection_enabled: false }), choice('p1', '2'), choice('p2', '3', { connection_enabled: false })]);
    expect(openingBoard([off], 'p1', everyListed, noOrder).board).toEqual(ref('', 'p1', '2'));
    expect(openingBoard([answered('', [choice('p2', '3', { connection_enabled: false })])], 'p2', everyListed, noOrder)).toEqual({ board: null, waiting: false });
  });

  it('is none when no project lists a board', () => {
    expect(openingBoard([answered('', [])], 'p1', everyListed, noOrder)).toEqual({ board: null, waiting: false });
  });
});

describe('what the pane remembers', () => {
  const board = ref('', 'p1', '1');

  it('reads the board picked last as storage holds it, and none from a malformed value', () => {
    expect(pickedBoard()).toBeNull();
    localStorage.setItem('agent-overflow:frontend:issues-board', JSON.stringify(board));
    expect(pickedBoard()).toEqual(board);
    localStorage.setItem('agent-overflow:frontend:issues-board', JSON.stringify({ backend: '', projectId: '', connection: 'c1', board: '1' }));
    expect(pickedBoard()).toBeNull();
  });

  it('keeps Display per board on its computer, whichever project lists it', () => {
    setCardFieldShown(board, 'labels', false);
    setCardFieldShown(board, 'assignee', false);
    expect([...hiddenCardFields(board)]).toEqual(['labels', 'assignee']);
    expect([...hiddenCardFields(ref('', 'p2', '1'))]).toEqual(['labels', 'assignee']);
    expect(hiddenCardFields(ref('laptop', 'p1', '1')).size).toBe(0);
    setCardFieldShown(board, 'labels', true);
    expect([...hiddenCardFields(board)]).toEqual(['assignee']);
    const stored = JSON.parse(localStorage.getItem('agent-overflow:frontend:issues-hidden-fields')!);
    expect(stored).toHaveLength(1);
    expect(stored[0][1]).toEqual(['assignee']);
  });

  it('remembers the boards whose Display changed last, at most 200', () => {
    for (let i = 0; i < 201; i += 1) setCardFieldShown(ref('', 'p1', String(i)), 'type', false);
    expect(hiddenCardFields(ref('', 'p1', '0')).size).toBe(0);
    expect(hiddenCardFields(ref('', 'p1', '200')).has('type')).toBe(true);
    expect(JSON.parse(localStorage.getItem('agent-overflow:frontend:issues-hidden-fields')!)).toHaveLength(200);
  });

  it('keeps Only mine for every board, and the compact column per board for the session', () => {
    expect(savedOnlyMine()).toBe(false);
    saveOnlyMine(true);
    expect(savedOnlyMine()).toBe(true);
    expect(localStorage.getItem('agent-overflow:frontend:issues-only-mine')).toBe('true');
    expect(compactColumn(board)).toBeNull();
    setCompactColumn(board, 2);
    expect(compactColumn(board)).toBe(2);
    expect(compactColumn(ref('', 'p1', '2'))).toBeNull();
  });
});
