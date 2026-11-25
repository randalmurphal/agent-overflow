// Regression test for the "flip-phases" hunk of
// frontend/patches/svelte@5.57.0.patch.
//
// Pristine svelte applies FLIP animations after a keyed-each reorder by
// calling `apply()` per animated item: abort the in-flight animation
// (style write), getBoundingClientRect (forced read), create the new
// animation (style write). Looping that over N items interleaves reads
// with the previous items' writes, forcing up to N style/layout recalc
// passes in one microtask — the dominant slice of sidebar-reorder burst
// frames on a 6ms frame budget.
//
// The patch splits the manager into abort() / measure_to() / apply() and
// the each-block microtask into three phased loops (all aborts, then all
// reads, then all creates). Per-item ordering (abort_i before read_i
// before create_i) is preserved and sibling transforms never affect each
// other's rects, so the animations are identical; the whole set now pays
// one forced pass. Drop the hunk if upstream batches these phases.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { get, set, state } from 'svelte/internal/client';
import FlipRowsHost from './svelte-patch-fixtures/FlipRowsHost.svelte';
import FlipSiblingListsHost from './svelte-patch-fixtures/FlipSiblingListsHost.svelte';
import FlipNestedListsHost, { type NestedGroup } from './svelte-patch-fixtures/FlipNestedListsHost.svelte';

/** Interleaving log: `read:<id>`, `create:<id>`, `cancel:<id>`, `mark:*`. */
let log: string[] = [];

function idOf(el: Element): string {
  return el.getAttribute('data-id') ?? el.tagName.toLowerCase();
}

/** Replaces setup.ts's WAAPI stub with a recording one. */
function installAnimateStub() {
  vi.spyOn(Element.prototype, 'animate').mockImplementation(function (this: Element) {
    const id = idOf(this);
    log.push(`create:${id}`);
    return {
      onfinish: null,
      effect: null,
      cancel: () => {
        log.push(`cancel:${id}`);
      },
    } as unknown as Animation;
  });
}

/** Rects derived from live DOM order, so a moved row's rect changes. */
function installRectStub() {
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(
    function (this: Element) {
      log.push(`read:${idOf(this)}`);
      const parent = this.parentElement;
      const idx = parent
        ? Array.prototype.indexOf.call(parent.children, this)
        : 0;
      return new DOMRect(0, idx * 10, 100, 10);
    },
  );
}

async function drainMicrotasks() {
  await Promise.resolve();
  await Promise.resolve();
}

/** Log entries after the last `mark:` entry. */
function afterMark(): string[] {
  const i = log.map((l) => l.startsWith('mark:')).lastIndexOf(true);
  return log.slice(i + 1);
}

function phaseIndices(entries: string[], phase: string): number[] {
  return entries.flatMap((l, i) => (l.startsWith(`${phase}:`) ? [i] : []));
}

beforeEach(() => {
  log = [];
  installAnimateStub();
  installRectStub();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('svelte patch: phased FLIP application on keyed-each reorder', () => {
  it('applies reorder animations as batched phases, never read/write interleaved', async () => {
    const items = state<number[]>([1, 2, 3, 4]);
    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(FlipRowsHost, {
      target,
      props: { getItems: () => get(items) },
    });
    flushSync();
    await drainMicrotasks();

    // First reorder: every row moves, no in-flight animations to abort.
    // svelte drains its microtask queue inside flushSync, so the whole
    // reorder (4 measure reads, then the apply pass with 4 destination
    // reads + 4 creates) lands in this one segment.
    log.push('mark:reorder-1');
    set(items, [4, 1, 2, 3]);
    flushSync();
    await drainMicrotasks();

    let entries = afterMark();
    let reads = phaseIndices(entries, 'read');
    let creates = phaseIndices(entries, 'create');
    expect(reads).toHaveLength(8);
    expect(creates).toHaveLength(4);
    // The phase assertion the patch exists for: every rect read happens
    // before any animation is created (writes never dirty a later read).
    expect(Math.min(...creates)).toBeGreaterThan(Math.max(...reads));

    // Second reorder mid-animation: aborts join the phases, also ahead of
    // every create.
    log.push('mark:reorder-2');
    set(items, [3, 4, 1, 2]);
    flushSync();
    await drainMicrotasks();

    entries = afterMark();
    const cancels = phaseIndices(entries, 'cancel');
    reads = phaseIndices(entries, 'read');
    creates = phaseIndices(entries, 'create');
    expect(cancels).toHaveLength(4);
    expect(reads).toHaveLength(8);
    expect(creates).toHaveLength(4);
    expect(Math.min(...creates)).toBeGreaterThan(Math.max(...reads, ...cancels));

    await Promise.resolve(unmount(app)).catch(() => {});
    target.remove();
  });

  it('still animates only moved rows and skips stationary ones', async () => {
    const items = state<number[]>([1, 2, 3, 4]);
    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(FlipRowsHost, {
      target,
      props: { getItems: () => get(items) },
    });
    flushSync();
    await drainMicrotasks();

    // Swap the first two rows; 3 and 4 stay put.
    log.push('mark:swap');
    set(items, [2, 1, 3, 4]);
    flushSync();
    await drainMicrotasks();

    const entries = afterMark();
    const created = entries
      .filter((l) => l.startsWith('create:'))
      .map((l) => l.slice('create:'.length))
      .sort();
    expect(created).toEqual(['1', '2']);

    await Promise.resolve(unmount(app)).catch(() => {});
    target.remove();
  });

  it('batches fix() phases across removed rows without read/write interleaving', async () => {
    const items = state<number[]>([1, 2, 3, 4]);
    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(FlipRowsHost, {
      target,
      props: { getItems: () => get(items) },
    });
    flushSync();
    await drainMicrotasks();

    const styleLog: string[] = [];
    const origGetComputedStyle = window.getComputedStyle;
    vi.spyOn(window, 'getComputedStyle').mockImplementation((el: Element, pseudo?: string | null) => {
      if (el.hasAttribute('data-id')) styleLog.push(`computed:${idOf(el)}`);
      return origGetComputedStyle.call(window, el, pseudo);
    });
    const listItems = Array.from(target.querySelectorAll('li'));
    for (const li of listItems) {
      const id = idOf(li);
      const rawStyle = li.style;
      const styleProxy = new Proxy(rawStyle, {
        set(obj, prop, value, receiver) {
          if (prop === 'position' && value === 'absolute') styleLog.push(`position:${id}`);
          if (prop === 'transform' && value) styleLog.push(`transform:${id}`);
          return Reflect.set(obj, prop, value, receiver);
        },
      });
      Object.defineProperty(li, 'style', {
        configurable: true,
        get: () => styleProxy,
      });
    }
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
      const id = idOf(this);
      if (this.hasAttribute('data-id')) {
        styleLog.push(`rect:${id}`);
        // Once position:absolute is applied, simulate collapsing into static top=0
        // so removed rows need compensating translateY transforms.
        const isAbs = (this as HTMLElement).style.position === 'absolute';
        const idx = Number(id) - 1;
        return new DOMRect(0, isAbs ? 0 : idx * 10, 100, 10);
      }
      return new DOMRect(0, 0, 100, 10);
    });

    // Remove 3 rows at once while keeping row 4.
    set(items, [4]);
    flushSync();
    await drainMicrotasks();

    const computedIdxs = phaseIndices(styleLog, 'computed');
    const posIdxs = phaseIndices(styleLog, 'position');
    const transformIdxs = phaseIndices(styleLog, 'transform');
    // Pre-fix measure rects are followed by computed reads, then position writes,
    // then post-detach rect reads, then transform writes — zero interleaving.
    expect(computedIdxs).toHaveLength(3);
    expect(posIdxs).toHaveLength(3);
    expect(transformIdxs).toHaveLength(2); // rows 2 and 3 moved from y=10,20 to y=0
    expect(Math.min(...posIdxs)).toBeGreaterThan(Math.max(...computedIdxs));
    const postPosRects = phaseIndices(styleLog, 'rect').filter((i) => i > Math.max(...posIdxs));
    expect(postPosRects.length).toBeGreaterThanOrEqual(3);
    expect(Math.min(...transformIdxs)).toBeGreaterThan(Math.max(...postPosRects.slice(0, 3)));

    await Promise.resolve(unmount(app)).catch(() => {});
    target.remove();
  });

  it('batches FLIP destination reads and animation creates across sibling animated each blocks', async () => {
    const itemsA = state<string[]>(['a1', 'a2', 'a3']);
    const itemsB = state<string[]>(['b1', 'b2', 'b3']);
    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(FlipSiblingListsHost, {
      target,
      props: {
        getItemsA: () => get(itemsA),
        getItemsB: () => get(itemsB),
      },
    });
    flushSync();
    await drainMicrotasks();

    // Reorder BOTH sibling lists in the same reactive flush.
    // Every destination read across BOTH lists must finish before ANY
    // animation is created (zero cross-block read/write interleaving in the
    // shared FLIP microtask).
    log.push('mark:sibling-reorder');
    set(itemsA, ['a3', 'a1', 'a2']);
    set(itemsB, ['b3', 'b1', 'b2']);
    flushSync();
    await drainMicrotasks();

    const reorderEntries = afterMark();
    const reorderReads = phaseIndices(reorderEntries, 'read');
    const reorderCreates = phaseIndices(reorderEntries, 'create');
    expect(reorderReads).toHaveLength(12); // 6 pre-reads + 6 post-reads
    expect(reorderCreates).toHaveLength(6);
    expect(Math.min(...reorderCreates)).toBeGreaterThan(Math.max(...reorderReads));

    await Promise.resolve(unmount(app)).catch(() => {});
    target.remove();
  });

  it('reorders nested animated each blocks in one flush without double-measuring or interleaving creates', async () => {
    const groups = state<NestedGroup[]>([
      { id: 'g1', items: ['g1-a', 'g1-b'] },
      { id: 'g2', items: ['g2-a', 'g2-b'] },
    ]);
    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(FlipNestedListsHost, {
      target,
      props: {
        getGroups: () => get(groups),
      },
    });
    flushSync();
    await drainMicrotasks();

    log.push('mark:nested-reorder');
    set(groups, [
      { id: 'g2', items: ['g2-b', 'g2-a'] },
      { id: 'g1', items: ['g1-b', 'g1-a'] },
    ]);
    flushSync();
    await drainMicrotasks();

    const entries = afterMark();
    const reads = phaseIndices(entries, 'read');
    const creates = phaseIndices(entries, 'create');
    // 2 outer groups + 4 inner items = 6 animated elements; each gets 1 pre-read
    // + 1 destination read = 12 reads, and all 6 animate once after all reads.
    expect(reads).toHaveLength(12);
    expect(creates).toHaveLength(6);
    expect(Math.min(...creates)).toBeGreaterThan(Math.max(...reads));

    await Promise.resolve(unmount(app)).catch(() => {});
    target.remove();
  });

  it('recovers cleanly if a FLIP animation callback throws during microtask drain', async () => {
    const items = state<number[]>([1, 2, 3]);
    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(FlipRowsHost, {
      target,
      props: { getItems: () => get(items) },
    });
    flushSync();
    await drainMicrotasks();

    // Force Element.prototype.animate to throw once during the microtask drain.
    let shouldThrow = true;
    vi.spyOn(Element.prototype, 'animate').mockImplementation(function (this: Element) {
      if (shouldThrow) {
        shouldThrow = false;
        throw new Error('simulated FLIP failure');
      }
      const id = idOf(this);
      log.push(`create:${id}`);
      return {
        onfinish: null,
        effect: null,
        cancel: () => {
          log.push(`cancel:${id}`);
        },
      } as unknown as Animation;
    });

    expect(() => {
      set(items, [3, 1, 2]);
      flushSync();
    }).toThrow('simulated FLIP failure');

    // A subsequent reorder must not stay wedged behind a stale scheduled flag.
    log.push('mark:after-throw');
    set(items, [2, 3, 1]);
    flushSync();
    await drainMicrotasks();

    const entries = afterMark();
    const creates = phaseIndices(entries, 'create');
    expect(creates).toHaveLength(3);

    await Promise.resolve(unmount(app)).catch(() => {});
    target.remove();
  });
});
