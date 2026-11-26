// Real-Chromium suite for carrying a Board card (BoardScreen and
// listCarry.ts): while a card is carried over a board whose first column
// holds 2,000 cards, nothing under the columns changes but the drop slot's
// line, the header counts and the column under the card saying whether it
// takes it, so no card renders per pointer move; a drop in the card's own
// column ranks it between the cards either side of the slot.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { flushSync, mount, unmount } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import '../../../app.css';
import BoardScreen from './BoardScreen.svelte';
import type { BoardView, RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { CardField } from '../../stores/issuesPane.svelte';
import { attachTrackerBoard } from '../../stores/trackerBoard.svelte';
import { trackerBoardUpdated } from '../../stores/trackerBoardEvents';
import { HOME_BACKEND } from '../../transport/backendKey';
import { waitFor } from '../../../test/helpers/browserFrames';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const mounted: { app: object; host: HTMLElement }[] = [];
const releases: (() => void)[] = [];

// The board's 1200 by 700 px within the page, so a release's hit test
// finds what is under it.
beforeEach(async () => {
  await page.viewport(1280, 800);
});

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
  for (const release of releases.splice(0)) release();
});

const REF = { backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: '1' };

function row(i: number, status: string): RowView {
  return {
    order: `k${String(i).padStart(5, '0')}`,
    estimate: 2,
    card: { id: String(i), key: `WEB-${i}`, title: `Issue ${i}`, status: { id: status, name: status }, created: 0, updated: 0 },
  } as RowView;
}

const TODO = Array.from({ length: 2_000 }, (_, i) => row(i, 's1'));
const DOING = [row(5_001, 's2'), row(5_002, 's2'), row(5_003, 's2')];

function board(): BoardView {
  const columns = [
    { name: 'To Do', statuses: [{ id: 's1', group: 'todo' }], total: { rows: 2_000, estimate: 4_000 } },
    { name: 'In Progress', statuses: [{ id: 's2', group: 'in_progress' }], total: { rows: 3, estimate: 6 } },
    { name: 'Done', statuses: [{ id: 's3', group: 'done' }], total: { rows: 0, estimate: 0 } },
  ];
  return {
    id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'WEB board', plans_sprints: false, estimate: { unit: 'points' },
    columns, sprints: [], all: { rows: 2_003, estimate: 4_006 }, backlog: { rows: 0, estimate: 0 }, sync: { read_at: 1 },
    screen: [0, 1, 2].map((column) => ({ kind: 'column', column })),
    sections: [
      { section: { kind: 'column', column: 0 }, rows: TODO, next: null },
      { section: { kind: 'column', column: 1 }, rows: DOING, next: null },
      { section: { kind: 'column', column: 2 }, rows: [], next: null },
    ],
  } as unknown as BoardView;
}

/** The Board screen over the board store holding `view`, as the pane
 * mounts it. */
async function mountBoard(view: BoardView = board()): Promise<HTMLElement> {
  setBindingMock('SubscribeTrackerBoard', async () => ({ id: 'sub', seq: 10, loaded: true, view, poll_every: 10_000 }));
  setBindingMock('SetTrackerSubscriptionVisible', async () => {});
  setBindingMock('ReleaseTrackerSubscription', async () => {});
  const held = attachTrackerBoard(REF, true);
  releases.push(() => held.release());
  await waitFor(() => held.current?.held != null, 'the board held');
  const host = document.createElement('div');
  host.style.cssText = 'position: fixed; top: 0; left: 0; width: 1200px; height: 700px; display: flex; flex-direction: column;';
  document.body.appendChild(host);
  const app = mount(BoardScreen, {
    target: host,
    props: {
      boardKey: held.key, boardRef: REF, get held() { return held.current!.held!; }, paging: new Set<string>(), sectionErrors: new Map(),
      filter: { mine: null, words: [] }, hidden: new Set<CardField>() as ReadonlySet<CardField>, compact: false, shown: true,
      openIssue: null, typed: true, onOpenCard: () => {}, onMoveWithFields: () => {}, ticketProject: () => null, onStartWork: () => {},
    },
  });
  mounted.push({ app, host });
  return host;
}

function cardEl(host: HTMLElement, id: string): HTMLElement {
  return host.querySelector<HTMLElement>(`[data-testid="issues-card"][data-issue="${id}"]`)!;
}

function column(host: HTMLElement, name: string): HTMLElement {
  return host.querySelector<HTMLElement>(`[data-testid="issues-column"][data-column="${name}"]`)!;
}

function pointer(type: string, target: EventTarget, x: number, y: number): void {
  target.dispatchEvent(new PointerEvent(type, { bubbles: true, clientX: x, clientY: y, pointerId: 1, pointerType: 'mouse', button: 0, buttons: type === 'pointerup' ? 0 : 1 }));
}

function frames(n: number): Promise<void> {
  return new Promise((resolve) => {
    const step = (left: number) => (left === 0 ? resolve() : requestAnimationFrame(() => step(left - 1)));
    step(n);
  });
}

/** What changed under the columns, outside the carry's own marks. */
function watchCards(host: HTMLElement): { changes: () => string[]; stop: () => void } {
  const seen: string[] = [];
  const ours = (node: Node | null): boolean => {
    const el = node instanceof Element ? node : node?.parentElement;
    return !!el?.closest('[data-testid="issues-column-slot"], [data-testid="issues-column-drop-note"], [data-testid="issues-column-count"]')
      || (node instanceof Element && node.matches('[data-testid="issues-column-slot"], [data-testid="issues-column-drop-note"]'));
  };
  const observer = new MutationObserver((records) => {
    for (const r of records) {
      if (r.attributeName === 'data-drop' || ours(r.target) || [...r.addedNodes, ...r.removedNodes].some(ours)) continue;
      const el = r.target instanceof Element ? r.target : r.target.parentElement;
      seen.push(`${r.type} ${r.attributeName ?? ''} ${el?.getAttribute('data-testid') ?? el?.tagName}`);
    }
  });
  observer.observe(host.querySelector('[data-testid="issues-columns"]')!, { subtree: true, childList: true, attributes: true, characterData: true });
  return { changes: () => seen, stop: () => observer.disconnect() };
}

describe('carrying a Board card', () => {
  it('renders no card per pointer move while carried, and ranks it at the slot it is dropped on', async () => {
    const moves = setBindingMock('TrackerMoves', async () => [{ id: '11', name: 'To Do', to: { id: 's1', name: 'To Do' } }]);
    const rank = setBindingMock('RankTrackerIssues', async () => ({ parts: [{ step: 'rank', state: 'applied' }] }));
    const host = await mountBoard();
    await waitFor(() => cardEl(host, '5003') !== null, 'the In Progress cards');
    const carried = cardEl(host, '5003');
    const first = cardEl(host, '5001').getBoundingClientRect();
    const start = carried.getBoundingClientRect();
    const x = start.left + start.width / 2;
    pointer('pointerdown', carried.querySelector('[data-testid="issues-card-title"]')!, x, start.top + 20);
    pointer('pointermove', window, x + 10, start.top + 24);
    await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') !== null, 'the carried copy');
    await waitFor(() => carried.hasAttribute('data-carried'), 'the card dimmed in its place');
    await frames(2);

    // Up the column to above the first card, one move a frame: only the
    // slot line moves.
    const watch = watchCards(host);
    for (let y = start.top + 24; y > first.top + 4; y -= 8) {
      pointer('pointermove', window, x, y);
      await frames(1);
    }
    pointer('pointermove', window, x, first.top + 4);
    await waitFor(() => column(host, 'In Progress').querySelector('[data-testid="issues-column-slot"]') !== null, 'the slot line');
    await frames(2);
    // Over To Do, its 2,000 cards: the header counts the card in, and the
    // column, which a move reaches, says nothing.
    const todo = column(host, 'To Do').getBoundingClientRect();
    pointer('pointermove', window, todo.left + todo.width / 2, todo.top + 200);
    await waitFor(() => column(host, 'To Do').querySelector('[data-testid="issues-column-count"]')!.textContent!.match(/^2,?001 /) !== null, 'To Do counting the card');
    expect(column(host, 'In Progress').querySelector('[data-testid="issues-column-count"]')!.textContent).toMatch(/^2 /);
    expect(moves).toHaveBeenCalledTimes(1);
    expect(column(host, 'To Do').querySelector('[data-testid="issues-column-drop-note"]')).toBeNull();
    await frames(3);
    expect(watch.changes()).toEqual([]);
    watch.stop();

    // Back above the first card of its own column, and let go.
    pointer('pointermove', window, x, first.top + 4);
    await waitFor(() => column(host, 'In Progress').querySelector('[data-testid="issues-column-slot"]') !== null, 'the slot line again');
    pointer('pointerup', window, x, first.top + 4);
    await waitFor(() => rank.mock.calls.length === 1, 'the rank');
    expect(rank.mock.calls[0]).toEqual(['c1', { board: '1', issues: ['5003'], prev: undefined, next: '5001' }]);
    await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the copy gone');
    flushSync();
  });

  it('keeps a card carried while the copy of the one dropped before it glides home', async () => {
    setBindingMock('TrackerMoves', async () => []);
    const host = await mountBoard();
    await waitFor(() => cardEl(host, '5003') !== null, 'the In Progress cards');
    const pickUp = async (id: string) => {
      const r = cardEl(host, id).getBoundingClientRect();
      pointer('pointerdown', cardEl(host, id).querySelector('[data-testid="issues-card-title"]')!, r.left + 20, r.top + 20);
      pointer('pointermove', window, r.left + 40, r.top + 30);
      await waitFor(() => cardEl(host, id).hasAttribute('data-carried'), `${id} carried`);
      return r;
    };
    // Let go over Done, which no move reaches: its copy glides home.
    await pickUp('5003');
    const done = column(host, 'Done').getBoundingClientRect();
    pointer('pointermove', window, done.left + done.width / 2, done.top + 200);
    await waitFor(() => column(host, 'Done').querySelector('[data-testid="issues-column-drop-note"]')?.textContent?.includes('No move') === true, 'Done refusing it');
    pointer('pointerup', window, done.left + done.width / 2, done.top + 200);
    await frames(2);
    expect(document.querySelectorAll('[data-testid="issues-card-carried"]')).toHaveLength(1);
    // Another card picked up while that copy is on its way stays carried
    // after the copy lands.
    await pickUp('5001');
    await waitFor(() => document.querySelectorAll('[data-testid="issues-card-carried"]').length === 1, 'the first copy landed');
    await frames(2);
    expect(cardEl(host, '5001').hasAttribute('data-carried')).toBe(true);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await waitFor(() => !cardEl(host, '5001').hasAttribute('data-carried'), 'the carry cancelled');
  });

  it('sends nothing for a card let go above the cards, over another column or its own', async () => {
    const moves = setBindingMock('TrackerMoves', async () => [{ id: '31', name: 'Done', to: { id: 's3', name: 'Done' } }]);
    const move = setBindingMock('MoveTrackerIssue', async () => ({ parts: [{ step: 'move', state: 'applied' }] }));
    const rank = setBindingMock('RankTrackerIssues', async () => ({ parts: [{ step: 'rank', state: 'applied' }] }));
    const host = await mountBoard();
    await waitFor(() => cardEl(host, '5003') !== null, 'the In Progress cards');
    for (const over of ['Done', 'In Progress']) {
      const start = cardEl(host, '5003').getBoundingClientRect();
      pointer('pointerdown', cardEl(host, '5003').querySelector('[data-testid="issues-card-title"]')!, start.left + 20, start.top + 20);
      pointer('pointermove', window, start.left + 40, start.top + 30);
      await waitFor(() => cardEl(host, '5003').hasAttribute('data-carried'), 'the card carried');
      // Up over the column's header, above its cards.
      const head = column(host, over).getBoundingClientRect();
      const cards = column(host, over).querySelector('[data-testid="issues-column-cards"]')!.getBoundingClientRect();
      const x = head.left + head.width / 2;
      const y = (head.top + cards.top) / 2;
      expect(y).toBeLessThan(cards.top);
      // Straight up out of its own cards, then along above the columns.
      for (let k = 1; k <= 6; k += 1) {
        pointer('pointermove', window, start.left + 40, start.top + 30 + (y - start.top - 30) * k / 6);
        await frames(1);
      }
      for (let k = 1; k <= 6; k += 1) {
        pointer('pointermove', window, start.left + 40 + (x - start.left - 40) * k / 6, y);
        await frames(1);
      }
      await frames(2);
      pointer('pointerup', window, x, y);
      await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the copy home');
      expect(cardEl(host, '5003').hasAttribute('data-carried')).toBe(false);
    }
    expect(move).not.toHaveBeenCalled();
    expect(rank).not.toHaveBeenCalled();
    expect(moves).not.toHaveBeenCalled();

    // Over Done's cards, where a toast came up during the carry: on screen
    // the release is over the toast, so nothing is sent either.
    const start = cardEl(host, '5003').getBoundingClientRect();
    pointer('pointerdown', cardEl(host, '5003').querySelector('[data-testid="issues-card-title"]')!, start.left + 20, start.top + 20);
    pointer('pointermove', window, start.left + 40, start.top + 30);
    await waitFor(() => cardEl(host, '5003').hasAttribute('data-carried'), 'the card carried');
    const done = column(host, 'Done').querySelector('[data-testid="issues-column-cards"]')!.getBoundingClientRect();
    const toast = document.createElement('div');
    toast.style.cssText = `position: fixed; left: ${done.left}px; top: ${done.top + 50}px; width: ${done.width}px; height: 80px; z-index: 100;`;
    document.body.appendChild(toast);
    try {
      pointer('pointermove', window, done.left + done.width / 2, done.top + 90);
      await frames(3);
      pointer('pointerup', window, done.left + done.width / 2, done.top + 90);
      await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the copy home');
      await frames(2);
    } finally {
      toast.remove();
    }
    expect(move).not.toHaveBeenCalled();
  });

  it('lets a card be picked up again only once its drop is decided, and sends the one move it decided', async () => {
    let answer: (moves: unknown[]) => void = () => {};
    const moves = setBindingMock('TrackerMoves', () => new Promise((resolve) => { answer = resolve; }));
    const move = setBindingMock('MoveTrackerIssue', async () => ({ parts: [{ step: 'move', state: 'applied' }] }));
    const host = await mountBoard();
    await waitFor(() => cardEl(host, '5003') !== null, 'the In Progress cards');
    const carry = async (to: string) => {
      const start = cardEl(host, '5003').getBoundingClientRect();
      pointer('pointerdown', cardEl(host, '5003').querySelector('[data-testid="issues-card-title"]')!, start.left + 20, start.top + 20);
      pointer('pointermove', window, start.left + 40, start.top + 30);
      await frames(4);
      const r = column(host, to).querySelector('[data-testid="issues-column-cards"]')!.getBoundingClientRect();
      pointer('pointermove', window, r.left + r.width / 2, r.top + 100);
      await frames(2);
      pointer('pointerup', window, r.left + r.width / 2, r.top + 100);
    };
    // Let go over Done while its moves are being read: the drop waits.
    await carry('Done');
    await waitFor(() => moves.mock.calls.length === 1, 'the moves read');
    await frames(2);
    expect(document.querySelectorAll('[data-testid="issues-card-carried"]')).toHaveLength(1);
    // Picked up again meanwhile and let go over To Do: no second carry.
    await carry('To Do');
    await frames(4);
    expect(document.querySelectorAll('[data-testid="issues-card-carried"]')).toHaveLength(1);
    expect(moves).toHaveBeenCalledTimes(1);
    answer([{ id: '11', name: 'To Do', to: { id: 's1', name: 'To Do' } }, { id: '31', name: 'Done', to: { id: 's3', name: 'Done' } }]);
    await waitFor(() => move.mock.calls.length === 1, 'the move');
    expect(move.mock.calls[0]).toEqual(['c1', { issue: '5003', move: '31', fields: undefined, accept_loss: false }]);
    await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the copy landed');
    await frames(4);
    expect(move).toHaveBeenCalledTimes(1);
  });

  it('reads no layout per frame while the card is over the end of its column', async () => {
    setBindingMock('TrackerMoves', async () => []);
    const host = await mountBoard();
    await waitFor(() => cardEl(host, '5003') !== null, 'the In Progress cards');
    const start = cardEl(host, '5001').getBoundingClientRect();
    pointer('pointerdown', cardEl(host, '5001').querySelector('[data-testid="issues-card-title"]')!, start.left + 20, start.top + 20);
    pointer('pointermove', window, start.left + 40, start.top + 30);
    await waitFor(() => cardEl(host, '5001').hasAttribute('data-carried'), 'the card carried');
    const last = cardEl(host, '5003').getBoundingClientRect();
    pointer('pointermove', window, start.left + 40, last.bottom - 4);
    await waitFor(() => column(host, 'In Progress').querySelector('[data-testid="issues-column-slot"]') !== null, 'the slot at the end');
    const height = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetHeight')!;
    let reads = 0;
    Object.defineProperty(HTMLElement.prototype, 'offsetHeight', {
      configurable: true,
      get(this: HTMLElement) {
        reads += 1;
        return height.get!.call(this);
      },
    });
    try {
      for (let k = 0; k < 10; k += 1) {
        pointer('pointermove', window, start.left + 40, last.bottom - 4 - (k % 2));
        await frames(1);
      }
    } finally {
      Object.defineProperty(HTMLElement.prototype, 'offsetHeight', height);
    }
    expect(reads).toBe(0);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the carry cancelled');
  });
});

describe('a drop whose move is being looked up', () => {
  // Two moves reach Done, so a drop there asks which.
  const INTO_DONE = [{ id: '31', name: 'Done', to: { id: 's3', name: 'Done' } }, { id: '32', name: 'Close', to: { id: 's3', name: 'Done' } }];
  const REF2 = { ...REF, board: '2' };

  /** The moves reads, each answered by the test in turn. */
  function heldLookups(): { asked: () => number; answer: (k: number) => void } {
    const answers: ((moves: unknown[]) => void)[] = [];
    setBindingMock('TrackerMoves', () => new Promise((resolve) => answers.push(resolve)));
    return { asked: () => answers.length, answer: (k) => answers[k](INTO_DONE) };
  }

  /** The Board screen over the board store, showing board 1 or 2 as
   * `show` says, as the pane does when another board is picked. */
  async function mountBoards(): Promise<{ host: HTMLElement; show: (ref: typeof REF) => void; remount: () => Promise<HTMLElement> }> {
    setBindingMock('SubscribeTrackerBoard', async (...args: unknown[]) => ({ id: `sub-${String(args[2])}`, seq: 10, loaded: true, view: board(), poll_every: 10_000 }));
    setBindingMock('SetTrackerSubscriptionVisible', async () => {});
    setBindingMock('ReleaseTrackerSubscription', async () => {});
    const boards = [attachTrackerBoard(REF, true), attachTrackerBoard(REF2, true)];
    for (const b of boards) releases.push(() => b.release());
    await waitFor(() => boards.every((b) => b.current?.held != null), 'the boards held');
    const showing = new SvelteMap([['board', 0]]);
    const at = () => boards[showing.get('board')!];
    const mountOne = (): HTMLElement => {
      const host = document.createElement('div');
      host.style.cssText = 'position: fixed; top: 0; left: 0; width: 1200px; height: 700px; display: flex; flex-direction: column;';
      document.body.appendChild(host);
      const app = mount(BoardScreen, {
        target: host,
        props: {
          get boardKey() { return at().key; }, get boardRef() { return showing.get('board') === 0 ? REF : REF2; }, get held() { return at().current!.held!; },
          paging: new Set<string>(), sectionErrors: new Map(), filter: { mine: null, words: [] }, hidden: new Set<CardField>() as ReadonlySet<CardField>,
          compact: false, shown: true, openIssue: null, typed: true, onOpenCard: () => {}, onMoveWithFields: () => {}, ticketProject: () => null, onStartWork: () => {},
        },
      });
      mounted.push({ app, host });
      return host;
    };
    const host = mountOne();
    await waitFor(() => cardEl(host, '5003') !== null, 'the In Progress cards');
    return {
      host,
      show: (ref) => {
        showing.set('board', ref === REF ? 0 : 1);
        flushSync();
      },
      // The screen left (Time, another pane) and shown again.
      remount: async () => {
        const { app, host: old } = mounted.pop()!;
        unmount(app);
        old.remove();
        const host = mountOne();
        await waitFor(() => cardEl(host, '5003') !== null, 'the In Progress cards again');
        return host;
      },
    };
  }

  async function dropOn(host: HTMLElement, id: string, to: string): Promise<void> {
    const start = cardEl(host, id).getBoundingClientRect();
    pointer('pointerdown', cardEl(host, id).querySelector('[data-testid="issues-card-title"]')!, start.left + 20, start.top + 20);
    pointer('pointermove', window, start.left + 40, start.top + 30);
    await waitFor(() => cardEl(host, id).hasAttribute('data-carried'), `${id} carried`);
    const r = column(host, to).querySelector('[data-testid="issues-column-cards"]')!.getBoundingClientRect();
    pointer('pointermove', window, r.left + r.width / 2, r.top + 100);
    await frames(2);
    pointer('pointerup', window, r.left + r.width / 2, r.top + 100);
  }

  /** Whether a press and a pull pick the card up: a card held by a drop
   * stays where it is. */
  async function picksUp(host: HTMLElement, id: string): Promise<boolean> {
    await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'no copy on its way');
    const start = cardEl(host, id).getBoundingClientRect();
    pointer('pointerdown', cardEl(host, id).querySelector('[data-testid="issues-card-title"]')!, start.left + 20, start.top + 20);
    pointer('pointermove', window, start.left + 40, start.top + 30);
    await frames(4);
    const carried = cardEl(host, id).hasAttribute('data-carried');
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    pointer('pointerup', window, start.left + 40, start.top + 30);
    await waitFor(() => !cardEl(host, id).hasAttribute('data-carried'), `${id} put back`);
    return carried;
  }

  const choice = (key: string) => document.querySelector(`[role="menu"][aria-label="Move ${key} by"]`);
  const dismiss = () => document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));

  it('lets both cards move again when a second drop asks which move while the first does', async () => {
    const lookups = heldLookups();
    const move = setBindingMock('MoveTrackerIssue', async () => ({ parts: [{ step: 'move', state: 'applied' }] }));
    const { host } = await mountBoards();
    await dropOn(host, '5003', 'Done');
    await dropOn(host, '5002', 'Done');
    await waitFor(() => lookups.asked() === 2, 'both lookups out');
    lookups.answer(0);
    await waitFor(() => choice('WEB-5003') !== null, "the first card's choice");
    lookups.answer(1);
    await waitFor(() => choice('WEB-5002') !== null, "the second card's choice");
    // One choice shows: the second, which took the first's place.
    expect(choice('WEB-5003')).toBeNull();
    dismiss();
    await waitFor(() => choice('WEB-5002') === null, 'the choice dismissed');
    expect(await picksUp(host, '5003')).toBe(true);
    expect(await picksUp(host, '5002')).toBe(true);
    expect(move).not.toHaveBeenCalled();
  });

  it('lets the card move again when its lookup answers after the Board screen was left', async () => {
    const lookups = heldLookups();
    const move = setBindingMock('MoveTrackerIssue', async () => ({ parts: [{ step: 'move', state: 'applied' }] }));
    const boards = await mountBoards();
    await dropOn(boards.host, '5003', 'Done');
    await waitFor(() => lookups.asked() === 1, 'the lookup out');
    const host = await boards.remount();
    lookups.answer(0);
    await frames(4);
    expect(choice('WEB-5003')).toBeNull();
    expect(await picksUp(host, '5003')).toBe(true);
    expect(move).not.toHaveBeenCalled();
  });

  it('lets the card move again when its lookup answers after another board was shown', async () => {
    const lookups = heldLookups();
    const move = setBindingMock('MoveTrackerIssue', async () => ({ parts: [{ step: 'move', state: 'applied' }] }));
    const { host, show } = await mountBoards();
    await dropOn(host, '5003', 'Done');
    await waitFor(() => lookups.asked() === 1, 'the lookup out');
    show(REF2);
    lookups.answer(0);
    await frames(4);
    expect(choice('WEB-5003')).toBeNull();
    show(REF);
    await waitFor(() => cardEl(host, '5003') !== null, 'board 1 again');
    expect(await picksUp(host, '5003')).toBe(true);
    expect(move).not.toHaveBeenCalled();
  });
});

describe('ranking cards one after another in a column', () => {
  const COLUMN = { kind: 'column', column: 0 } as const;
  const A = row(1, 's1');
  const B = row(2, 's1');
  const C = row(3, 's1');
  const D = row(4, 's1');

  function rankedBoard(rows: RowView[]): BoardView {
    return {
      id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'WEB board', plans_sprints: false, estimate: { unit: 'points' },
      columns: [{ name: 'To Do', statuses: [{ id: 's1', group: 'todo' }], total: { rows: 4, estimate: 8 } }],
      sprints: [], all: { rows: 4, estimate: 8 }, backlog: { rows: 0, estimate: 0 }, sync: { read_at: 1 },
      screen: [COLUMN], sections: [{ section: COLUMN, rows, next: null }],
    } as unknown as BoardView;
  }

  it('sends a rank only once the one before it in the column settled, naming the neighbors the board then holds', async () => {
    // The first rank's refresh tells the board D moved between A and B.
    const reordered = { ...D, order: 'k00001m' } as RowView;
    setBindingMock('TrackerBoardPatch', async () => ({ lifetime: 1, revision: 2, sections: [{ section: COLUMN, rows: [reordered] }] }));
    const answers: ((view: unknown) => void)[] = [];
    const rank = setBindingMock('RankTrackerIssues', () => new Promise((resolve) => answers.push(resolve)));
    const host = await mountBoard(rankedBoard([A, B, C, D]));
    await waitFor(() => cardEl(host, '4') !== null, 'the cards');
    const drag = async (id: string, before: string) => {
      const start = cardEl(host, id).getBoundingClientRect();
      pointer('pointerdown', cardEl(host, id).querySelector('[data-testid="issues-card-title"]')!, start.left + 20, start.top + 20);
      pointer('pointermove', window, start.left + 40, start.top + 30);
      await waitFor(() => cardEl(host, id).hasAttribute('data-carried'), `${id} carried`);
      const to = cardEl(host, before).getBoundingClientRect();
      pointer('pointermove', window, start.left + 40, to.top + 4);
      await waitFor(() => host.querySelector('[data-testid="issues-column-slot"]') !== null, 'the slot');
      await frames(2);
      pointer('pointerup', window, start.left + 40, to.top + 4);
      await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the copy landed');
    };
    // D between A and B; its rank is out.
    await drag('4', '2');
    await waitFor(() => rank.mock.calls.length === 1, 'the first rank');
    expect(rank.mock.calls[0]).toEqual(['c1', { board: '1', issues: ['4'], prev: '1', next: '2' }]);
    // C between D, drawn where it waits, and B: nothing goes yet.
    await drag('3', '2');
    await frames(4);
    expect(rank).toHaveBeenCalledTimes(1);
    expect(cardEl(host, '3').hasAttribute('data-moving')).toBe(true);
    // The first lands, its event read first; then the second names D and B.
    trackerBoardUpdated({ scope: { connection: 'c1', board: '1' }, seq: 11, changed: ['4'] }, HOME_BACKEND);
    answers[0]({ parts: [{ step: 'rank', state: 'applied' }] });
    await waitFor(() => rank.mock.calls.length === 2, 'the second rank');
    expect(rank.mock.calls[1]).toEqual(['c1', { board: '1', issues: ['3'], prev: '4', next: '2' }]);
    answers[1]({ parts: [{ step: 'rank', state: 'applied' }] });
    await waitFor(() => !cardEl(host, '3').hasAttribute('data-moving'), 'the second settled');
  });
});
