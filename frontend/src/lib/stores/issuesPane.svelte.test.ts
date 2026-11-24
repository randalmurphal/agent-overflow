// The Issues pane's view state: which board it opens on, Display per
// board, Only mine, and what another window of this client wrote.

import { beforeEach, describe, expect, it } from 'vitest';
import type { BoardChoice } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  compactColumn,
  hiddenCardFields,
  lastShownBoard,
  onlyMineOn,
  openingBoard,
  resetIssuesPaneForTest,
  setCardFieldShown,
  setCompactColumn,
  setOnlyMine,
  showBoard,
} from './issuesPane.svelte';
import type { ListedBoardChoice } from './trackerBoardChoices.svelte';
import type { TrackerBoardRef } from './trackerBoard.svelte';

function choice(backend: string, project: string, board: string, opensFirst = false): ListedBoardChoice {
  return {
    backend,
    choice: {
      project, connection: 'c1', connection_kind: 'jira', connection_label: 'Work', connection_enabled: true,
      verified_account: 'acc-user1', board, name: `Board ${board}`, tracker_project: 'WEB', opens_first: opensFirst,
    } as BoardChoice,
  };
}

function ref(backend: string, projectId: string, board: string): TrackerBoardRef {
  return { backend, projectId, connection: 'c1', board };
}

const noOrder = () => 0;

beforeEach(() => {
  localStorage.clear();
  resetIssuesPaneForTest();
});

describe('the board the pane opens on', () => {
  const choices = [
    choice('', 'p1', '1'), choice('', 'p1', '2', true),
    choice('', 'p2', '3'),
    choice('laptop', 'p3', '4'), choice('laptop', 'p3', '5'),
  ];

  it('is the board shown last while a project still lists it', () => {
    expect(openingBoard(choices, ref('', 'p2', '3'), 'p1', noOrder)).toEqual(ref('', 'p2', '3'));
    // Another project's listing of the same board is another board.
    expect(openingBoard(choices, ref('', 'p1', '3'), null, noOrder)).toEqual(ref('', 'p1', '2'));
  });

  it("is otherwise the board that opens first of the project it was opened from, else that project's first", () => {
    expect(openingBoard(choices, null, 'p1', noOrder)).toEqual(ref('', 'p1', '2'));
    expect(openingBoard(choices, ref('', 'p9', '9'), 'p3', noOrder)).toEqual(ref('laptop', 'p3', '4'));
  });

  it('is otherwise the first board of the first project in sidebar order, on any computer', () => {
    const order = (_backend: string, project: string) => ({ p1: 3, p2: 2, p3: 1 })[project] ?? 99;
    expect(openingBoard(choices, null, null, order)).toEqual(ref('laptop', 'p3', '4'));
    expect(openingBoard(choices, null, 'unlisted', (_b, p) => (p === 'p2' ? 0 : 5))).toEqual(ref('', 'p2', '3'));
  });

  it('is none when no project lists a board', () => {
    expect(openingBoard([], ref('', 'p1', '1'), 'p1', noOrder)).toBeNull();
  });
});

describe('what the pane remembers', () => {
  const board = ref('', 'p1', '1');

  it('keeps the board shown, and reads one another window of this client wrote', () => {
    showBoard(board);
    expect(lastShownBoard()).toEqual(board);
    const other = ref('laptop', 'p3', '4');
    localStorage.setItem('agent-overflow:frontend:issues-board', JSON.stringify(other));
    window.dispatchEvent(new StorageEvent('storage', { key: 'agent-overflow:frontend:issues-board', storageArea: localStorage }));
    expect(lastShownBoard()).toEqual(other);
    localStorage.setItem('agent-overflow:frontend:issues-board', JSON.stringify({ backend: '', projectId: '', connection: 'c1', board: '1' }));
    window.dispatchEvent(new StorageEvent('storage', { key: 'agent-overflow:frontend:issues-board', storageArea: localStorage }));
    expect(lastShownBoard()).toBeNull();
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
    setOnlyMine(true);
    expect(onlyMineOn()).toBe(true);
    expect(localStorage.getItem('agent-overflow:frontend:issues-only-mine')).toBe('true');
    expect(compactColumn(board)).toBeNull();
    setCompactColumn(board, 2);
    expect(compactColumn(board)).toBe(2);
    expect(compactColumn(ref('', 'p1', '2'))).toBeNull();
  });
});
