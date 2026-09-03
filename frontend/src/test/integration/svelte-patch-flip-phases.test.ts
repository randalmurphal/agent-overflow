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

/** Interleaving log: `read:<id>`, `create:<id>`, `cancel:<id>`, `mark:*`. */
let log: string[] = [];

function idOf(el: Element): string {
  return el.getAttribute('data-id') ?? el.tagName.toLowerCase();
}

/** jsdom has no WAAPI; install a recording stub. */
function installAnimateStub() {
  (Element.prototype as unknown as { animate: unknown }).animate = function (
    this: Element,
  ) {
    const id = idOf(this);
    log.push(`create:${id}`);
    return {
      onfinish: null,
      effect: null,
      cancel: () => {
        log.push(`cancel:${id}`);
      },
    };
  };
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
  delete (Element.prototype as { animate?: unknown }).animate;
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
});
