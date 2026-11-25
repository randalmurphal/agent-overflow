// Real-Chromium suite for carrying a Board card (BoardScreen and
// boardCarry.ts): while a card is carried over a board whose first column
// holds 2,000 cards, nothing under the columns changes but the drop slot's
// line, the header counts and the column under the card saying whether it
// takes it, so no card renders per pointer move; a drop in the card's own
// column ranks it between the cards either side of the slot.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import BoardScreen from './BoardScreen.svelte';
import type { BoardView, RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { CardField } from '../../stores/issuesPane.svelte';
import { heldFromView } from '../../stores/trackerBoardModel';
import { HOME_BACKEND } from '../../transport/backendKey';
import { waitFor } from '../../../test/helpers/browserFrames';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
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

function mountBoard() {
  const host = document.createElement('div');
  host.style.cssText = 'position: fixed; top: 0; left: 0; width: 1200px; height: 700px; display: flex; flex-direction: column;';
  document.body.appendChild(host);
  const app = mount(BoardScreen, {
    target: host,
    props: {
      boardKey: 'board', boardRef: REF, held: heldFromView(board(), (r) => r), paging: new Set<string>(), sectionErrors: new Map(),
      filter: { mine: null, words: [] }, hidden: new Set<CardField>() as ReadonlySet<CardField>, compact: false, shown: true,
      openIssue: null, typed: true, onOpenCard: () => {}, onMoveWithFields: () => {},
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
    const host = mountBoard();
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
    const host = mountBoard();
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
});
