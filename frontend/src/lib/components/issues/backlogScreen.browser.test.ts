// Real-Chromium suite for the Backlog screen (BacklogScreen and
// listCarry.ts) over a board whose backlog holds 2,000 rows: the DOM
// holds the rows near the viewport; while a row is carried nothing
// renders but the gap's slide and the headers' numbers; a drop into the
// active sprint places the row before the one after the gap; and a row
// whose move is out takes no second carry. It also measures the screen
// against the Board's 2,000-card column in the same run (mount, scroll,
// the menu's open, a carry's start and its frames) and logs both.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import BacklogScreen from './BacklogScreen.svelte';
import BoardScreen from './BoardScreen.svelte';
import type { BoardView, RowView, WriteView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { CardField } from '../../stores/issuesPane.svelte';
import { attachTrackerBoard, holdTrackerBoardPlan } from '../../stores/trackerBoard.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { waitFor } from '../../../test/helpers/browserFrames';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const mounted: { app: object; host: HTMLElement }[] = [];
const releases: (() => void)[] = [];

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
const ROWS = 2_000;

function row(i: number, status: string): RowView {
  return {
    order: `k${String(i).padStart(5, '0')}`,
    estimate: 2,
    card: { id: String(i), key: `WEB-${i}`, title: `Issue ${i}`, status: { id: status, name: status, group: 'todo' }, created: 0, updated: 0 },
  } as RowView;
}

const MANY = Array.from({ length: ROWS }, (_, i) => row(i, 's1'));
const SPRINT = [row(5_001, 's1'), row(5_002, 's1'), row(5_003, 's1')];

const COLUMNS = [
  { name: 'To Do', statuses: [{ id: 's1', group: 'todo' }], total: { rows: 0, estimate: 0 } },
  { name: 'Done', statuses: [{ id: 's3', group: 'done' }], total: { rows: 0, estimate: 0 } },
];

/** Sprint 8, active, with three rows, and a backlog of 2,000 read whole. */
function backlogBoard(): BoardView {
  return {
    id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'WEB board', plans_sprints: true, estimate: { unit: 'points' },
    columns: COLUMNS, sprints: [{ id: '8', name: 'Sprint 8', state: 'active', total: { rows: 3, estimate: 6 } }],
    all: { rows: ROWS + 3, estimate: (ROWS + 3) * 2 }, backlog: { rows: ROWS, estimate: ROWS * 2 }, sync: { read_at: 1 },
    screen: [{ kind: 'sprint', sprint: '8' }],
    plan: [{ kind: 'sprint', sprint: '8' }, { kind: 'backlog' }],
    sections: [
      { section: { kind: 'sprint', sprint: '8' }, rows: SPRINT, next: null },
      { section: { kind: 'backlog' }, rows: MANY, next: null },
    ],
  } as unknown as BoardView;
}

/** A kanban board whose To Do column holds the 2,000. */
function columnBoard(): BoardView {
  return {
    id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'WEB board', plans_sprints: false, estimate: { unit: 'points' },
    columns: [{ ...COLUMNS[0], total: { rows: ROWS, estimate: ROWS * 2 } }, COLUMNS[1]], sprints: [],
    all: { rows: ROWS, estimate: ROWS * 2 }, backlog: { rows: 0, estimate: 0 }, sync: { read_at: 1 },
    screen: [0, 1].map((column) => ({ kind: 'column', column })), plan: [],
    sections: [
      { section: { kind: 'column', column: 0 }, rows: MANY, next: null },
      { section: { kind: 'column', column: 1 }, rows: [], next: null },
    ],
  } as unknown as BoardView;
}

async function holdBoard(view: BoardView, plan: boolean) {
  setBindingMock('SubscribeTrackerBoard', async () => ({ id: 'sub', seq: 10, loaded: true, view, poll_every: 10_000 }));
  setBindingMock('SetTrackerSubscriptionVisible', async () => {});
  setBindingMock('ReleaseTrackerSubscription', async () => {});
  const held = attachTrackerBoard(REF, true);
  releases.push(() => held.release());
  if (plan) releases.push(holdTrackerBoardPlan(held.key));
  await waitFor(() => held.current?.held != null, 'the board held');
  return held;
}

function hostEl(): HTMLElement {
  const host = document.createElement('div');
  host.style.cssText = 'position: fixed; top: 0; left: 0; width: 1200px; height: 700px; display: flex; flex-direction: column;';
  document.body.appendChild(host);
  return host;
}

const COMMON = {
  paging: new Set<string>(), sectionErrors: new Map(), filter: { mine: null, words: [] },
  hidden: new Set<CardField>() as ReadonlySet<CardField>, compact: false, shown: true, openIssue: null,
  onOpenCard: () => {}, onMoveWithFields: () => {},
};

async function mountBacklog(): Promise<{ host: HTMLElement; mountMs: number }> {
  const held = await holdBoard(backlogBoard(), true);
  await waitFor(() => held.current!.held!.sections.length === 2, 'the backlog held');
  const host = hostEl();
  const t0 = performance.now();
  const app = mount(BacklogScreen, {
    target: host,
    props: { ...COMMON, boardKey: held.key, boardRef: REF, get held() { return held.current!.held!; }, kind: 'jira' },
  });
  flushSync();
  void host.offsetHeight;
  const mountMs = performance.now() - t0;
  mounted.push({ app, host });
  // The active sprint starts folded: unfold it.
  const head = host.querySelector<HTMLElement>('[data-testid="issues-backlog-section"][data-list="sprint:8"]')!;
  if (!head.hasAttribute('data-open')) head.querySelector<HTMLElement>('[data-testid="issues-backlog-fold"]')!.click();
  return { host, mountMs };
}

async function mountBoard(): Promise<{ host: HTMLElement; mountMs: number }> {
  const held = await holdBoard(columnBoard(), false);
  const host = hostEl();
  const t0 = performance.now();
  const app = mount(BoardScreen, {
    target: host,
    props: { ...COMMON, boardKey: held.key, boardRef: REF, get held() { return held.current!.held!; }, typed: true },
  });
  flushSync();
  void host.offsetHeight;
  const mountMs = performance.now() - t0;
  mounted.push({ app, host });
  return { host, mountMs };
}

function rowEl(host: HTMLElement, id: string): HTMLElement | null {
  return host.querySelector<HTMLElement>(`[data-testid="issues-backlog-row"][data-issue="${id}"]`);
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

/** The time `act` and the update and layout it causes take. */
function timed(act: () => void): number {
  const t0 = performance.now();
  act();
  flushSync();
  void document.body.offsetHeight;
  return performance.now() - t0;
}

/** Runs `step` once a frame `n` times and answers the longest frame. */
async function longestFrame(n: number, step: (i: number) => void): Promise<number> {
  let last = await new Promise<number>((resolve) => requestAnimationFrame(resolve));
  let longest = 0;
  for (let i = 0; i < n; i += 1) {
    step(i);
    const now = await new Promise<number>((resolve) => requestAnimationFrame(resolve));
    longest = Math.max(longest, now - last);
    last = now;
  }
  return longest;
}

interface Costs { mountMs: number; scrollMs: number; scrollMaxMs: number; scrollFrameMs: number; menuMs: number; carryStartMs: number; carryFrameMs: number; rowsMounted: number }

/** The costs of a screen: `scroller` scrolled through, `rows()` the row
 * elements mounted, `target(i)` the i-th row shown. */
async function measure(host: HTMLElement, mountMs: number, scroller: HTMLElement, rowSelector: string): Promise<Costs> {
  const rows = () => [...host.querySelectorAll<HTMLElement>(rowSelector)];
  await waitFor(() => rows().length > 0, 'rows mounted');
  const rowsMounted = rows().length;
  // A scroll step's update and layout, and the longest frame over 30 steps.
  const steps: number[] = [];
  for (let i = 1; i <= 10; i += 1) {
    steps.push(timed(() => {
      scroller.scrollTop = i * 2_000;
      scroller.dispatchEvent(new Event('scroll'));
    }));
    await frames(1);
  }
  const scrollMs = steps.sort((a, b) => a - b)[5];
  const scrollMaxMs = steps[9];
  const scrollFrameMs = await longestFrame(30, (i) => { scroller.scrollTop = 20_000 + i * 600; });
  scroller.scrollTop = 0;
  scroller.dispatchEvent(new Event('scroll'));
  await frames(3);
  // The menu's open on a row.
  const first = rows()[1];
  const r = first.getBoundingClientRect();
  const menuMs = timed(() => {
    first.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: r.left + 40, clientY: r.top + 10 }));
  });
  await waitFor(() => document.querySelector('[role="menu"]') !== null, 'the menu');
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  await waitFor(() => document.querySelector('[role="menu"]') === null, 'the menu closed');
  // A carry's start, the carry loaded already, and its frames moving down.
  await import('./listCarry');
  const title = first.querySelector<HTMLElement>('[data-testid="issues-card-title"]')!;
  const x = r.left + r.width / 2;
  const t0 = performance.now();
  pointer('pointerdown', title, x, r.top + 10);
  pointer('pointermove', window, x + 10, r.top + 14);
  await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') !== null, 'the carried copy');
  flushSync();
  const carryStartMs = performance.now() - t0;
  const carryFrameMs = await longestFrame(30, (i) => pointer('pointermove', window, x, r.top + 14 + i * 12));
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
  await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the carry cancelled');
  return { mountMs, scrollMs, scrollMaxMs, scrollFrameMs, menuMs, carryStartMs, carryFrameMs, rowsMounted };
}

/** What changed under the list outside a carry's own marks: the slide of
 * an item (its style) and the headers' numbers. */
function watchRows(host: HTMLElement): { changes: () => string[]; stop: () => void } {
  const seen: string[] = [];
  const ours = (node: Node | null): boolean => {
    const el = node instanceof Element ? node : node?.parentElement;
    return !!el?.closest('[data-testid="issues-backlog-section"]');
  };
  const observer = new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === 'attributes' && (r.attributeName === 'style' || r.attributeName === 'class') && (r.target as Element).hasAttribute('data-list')) continue;
      if (ours(r.target) || [...r.addedNodes, ...r.removedNodes].some(ours)) continue;
      const el = r.target instanceof Element ? r.target : r.target.parentElement;
      seen.push(`${r.type} ${r.attributeName ?? ''} ${el?.getAttribute('data-testid') ?? el?.tagName}`);
    }
  });
  observer.observe(host.querySelector('[data-testid="issues-backlog"]')!, { subtree: true, childList: true, attributes: true, characterData: true });
  return { changes: () => seen, stop: () => observer.disconnect() };
}

describe('the Backlog screen over 2,000 rows', () => {
  it('mounts the rows near the viewport, slides them aside for a carry with nothing else rendering, and places a drop before the row after the gap', async () => {
    const moved = setBindingMock('MoveTrackerIssuesToSprint', async () => ({ parts: [{ step: 'sprint', state: 'applied' }] }));
    const { host } = await mountBacklog();
    await waitFor(() => rowEl(host, '5003') !== null && rowEl(host, '0') !== null, 'the sprint\'s and the backlog\'s first rows');
    const sprintHead = host.querySelector<HTMLElement>('[data-testid="issues-backlog-section"][data-list="sprint:8"]')!;
    expect(host.querySelectorAll('[data-testid="issues-backlog-row"]').length).toBeLessThan(80);

    const carried = rowEl(host, '0')!;
    const target = rowEl(host, '5002')!.getBoundingClientRect();
    const start = carried.getBoundingClientRect();
    const x = start.left + start.width / 2;
    pointer('pointerdown', carried.querySelector('[data-testid="issues-card-title"]')!, x, start.top + 10);
    pointer('pointermove', window, x + 10, start.top + 14);
    await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') !== null, 'the carried copy');
    await frames(2);
    const watch = watchRows(host);
    // Up into Sprint 8, one move a frame, to the upper half of its
    // second row.
    for (let y = start.top + 14; y > target.top + 8; y -= 10) {
      pointer('pointermove', window, x, y);
      await frames(1);
    }
    pointer('pointermove', window, x, target.top + 6);
    const second = () => rowEl(host, '5002')!.closest<HTMLElement>('[data-list]')!;
    await waitFor(() => second().style.transform.startsWith('translateY('), 'the gap before the second row');
    const to = () => sprintHead.querySelector('[data-testid="issues-backlog-to"]')?.textContent;
    await waitFor(() => to() === '→ 8 points done', 'Sprint 8 counting the row in');
    expect(rowEl(host, '5001')!.closest<HTMLElement>('[data-list]')!.style.transform).toBe('');
    await frames(3);
    expect(watch.changes()).toEqual([]);
    watch.stop();
    // Let go over the gap: the row goes before the row after it.
    const gap = second().getBoundingClientRect();
    pointer('pointerup', window, x, target.top + 6);
    expect(gap.top).toBeGreaterThan(target.top + 6);
    await waitFor(() => moved.mock.calls.length === 1, 'the move');
    expect(moved.mock.calls[0]).toEqual(['c1', { board: '1', sprint: '8', issues: ['0'], prev: '5001', next: '5002' }]);
    await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the copy gone');
  });

  it('takes no second carry of a row whose move is out: two quick drops send one move', async () => {
    let answer: ((v: WriteView) => void) | null = null;
    const moved = setBindingMock('MoveTrackerIssuesToSprint', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const { host } = await mountBacklog();
    await waitFor(() => rowEl(host, '5003') !== null && rowEl(host, '0') !== null, 'the rows');
    const drop = async () => {
      const r = rowEl(host, '0')!.getBoundingClientRect();
      const t = rowEl(host, '5002')!.getBoundingClientRect();
      const x = r.left + r.width / 2;
      pointer('pointerdown', rowEl(host, '0')!.querySelector('[data-testid="issues-card-title"]')!, x, r.top + 10);
      for (let k = 1; k <= 8; k += 1) {
        pointer('pointermove', window, x, r.top + 10 + (t.top + 6 - r.top - 10) * k / 8);
        await frames(1);
      }
      pointer('pointerup', window, x, t.top + 6);
      await frames(2);
    };
    await drop();
    await waitFor(() => moved.mock.calls.length === 1, 'the first move');
    await waitFor(() => document.querySelector('[data-testid="issues-card-carried"]') === null, 'the copy landed');
    await drop();
    expect(document.querySelector('[data-testid="issues-card-carried"]')).toBeNull();
    expect(moved).toHaveBeenCalledTimes(1);
    answer!({ parts: [{ step: 'sprint', state: 'applied' }] } as WriteView);
  });

  it('costs about what the Board\'s 2,000-card column costs: mount, scroll, menu, carry', async () => {
    setBindingMock('TrackerMoves', async () => []);
    const clear = () => {
      for (const { app, host } of mounted.splice(0)) {
        unmount(app);
        host.remove();
      }
      for (const release of releases.splice(0)) release();
    };
    const runBacklog = async () => {
      const m = await mountBacklog();
      const c = await measure(m.host, m.mountMs, m.host.querySelector<HTMLElement>('[data-testid="issues-backlog"]')!, '[data-testid="issues-backlog-row"]');
      clear();
      return c;
    };
    const runBoard = async () => {
      const m = await mountBoard();
      const c = await measure(m.host, m.mountMs, m.host.querySelector<HTMLElement>('[data-testid="issues-column-cards"]')!, '[data-testid="issues-card"]');
      clear();
      return c;
    };
    // Each once to warm the page, then each measured, in turn.
    await runBoard();
    await runBacklog();
    const boardCosts = await runBoard();
    const backlogCosts = await runBacklog();
    console.log(`BACKLOG_COSTS ${JSON.stringify({ backlog: backlogCosts, board: boardCosts })}`);
    // Both virtualize: a fraction of the 2,000 is mounted.
    expect(backlogCosts.rowsMounted).toBeLessThan(80);
    expect(boardCosts.rowsMounted).toBeLessThan(80);
  });
});
