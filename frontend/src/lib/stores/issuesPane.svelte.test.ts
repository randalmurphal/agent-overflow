// The Issues pane's view state: which board it opens on when none was
// picked, the board picked last, Display per board and Only mine.

import { beforeEach, describe, expect, it } from 'vitest';
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


beforeEach(() => {
  localStorage.clear();
  resetIssuesPaneForTest();
});

describe('the board the pane opens on when none was picked', () => {
  const home = answered('', [choice('p1', '1'), choice('p1', '2', { opens_first: true }), choice('p2', '3')]);
  const laptop = answered('laptop', [choice('p3', '4'), choice('p3', '5')]);

  it("is the board that opens first of the project it was opened from, else that project's first, once its computer answered", () => {
    expect(openingBoard([home, laptop], 'p1')).toEqual({ board: ref('', 'p1', '2'), waiting: false });
    expect(openingBoard([home, laptop], 'p3')).toEqual({ board: ref('laptop', 'p3', '4'), waiting: false });
    // The other computer has not answered: the project's own decides.
    expect(openingBoard([home, { backend: 'laptop', state: { kind: 'reading', rows: null } }], 'p1').board).toEqual(ref('', 'p1', '2'));
  });

  it('waits on a computer that has not answered before it falls back', () => {
    const reading = { backend: 'laptop', state: { kind: 'reading', rows: null } as ComputerChoices };
    expect(openingBoard([home, reading], 'p3')).toEqual({ board: null, waiting: true });
    expect(openingBoard([home, reading], null)).toEqual({ board: null, waiting: true });
  });

  it("is otherwise the first project's board in the computers' order and each one's own, counting a computer it could not read as answered", () => {
    expect(openingBoard([home, laptop], null)).toEqual({ board: ref('', 'p1', '2'), waiting: false });
    expect(openingBoard([laptop, home], 'unlisted').board).toEqual(ref('laptop', 'p3', '4'));
    const offline = { backend: '', state: { kind: 'offline', rows: null } as ComputerChoices };
    expect(openingBoard([offline, laptop], null)).toEqual({ board: ref('laptop', 'p3', '4'), waiting: false });
  });

  it('never opens a board whose connection is turned off', () => {
    const off = answered('', [choice('p1', '1', { opens_first: true, connection_enabled: false }), choice('p1', '2'), choice('p2', '3', { connection_enabled: false })]);
    expect(openingBoard([off], 'p1').board).toEqual(ref('', 'p1', '2'));
    expect(openingBoard([answered('', [choice('p2', '3', { connection_enabled: false })])], 'p2')).toEqual({ board: null, waiting: false });
    // A project whose boards are all off is passed over.
    expect(openingBoard([answered('', [choice('p1', '1', { connection_enabled: false })]), laptop], null).board).toEqual(ref('laptop', 'p3', '4'));
  });

  it("passes over an archived project's boards on every computer, unless no other project lists one", () => {
    const archivedHome = answered('', [choice('p1', '1', { project_archived: true }), choice('p2', '3', { connection_enabled: false })]);
    expect(openingBoard([archivedHome, laptop], null).board).toEqual(ref('laptop', 'p3', '4'));
    expect(openingBoard([archivedHome], null).board).toEqual(ref('', 'p1', '1'));
    // The project it was opened from opens, archived or not.
    expect(openingBoard([archivedHome, laptop], 'p1').board).toEqual(ref('', 'p1', '1'));
  });

  it('is none when no project lists a board', () => {
    expect(openingBoard([answered('', [])], 'p1')).toEqual({ board: null, waiting: false });
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
