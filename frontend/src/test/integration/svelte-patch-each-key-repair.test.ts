// Regression test for the "each-key-repair" hunk of
// frontend/patches/svelte@5.57.0.patch.
//
// Pristine svelte throws `each_key_duplicate` the moment a keyed {#each}
// sees a repeated key. The throw happens INSIDE the update flush, so it
// aborts the whole batch: every effect the traversal had not reached keeps
// its stale DOM for good — a composer that will not clear after its send
// went through, a reveal stopped mid-message (production incidents
// 2026-08-29 and 2026-09-04). One bad row in one list froze the page.
//
// The patch repairs the repeat to a unique key instead (`key\u0000#n` for
// the n-th repeat, so a string or number key's repaired row keeps its
// identity across runs; any other key type gets a fresh Symbol per run,
// which remounts that row but never freezes the page), reconciles with the
// repaired keys, and reports ONCE per each-block instance by calling
// `reportError` — where an uncaught throw would have landed, so
// `utils/frontendErrorCapture.ts` still records the key value.
//
// Unlike the other hunks this one is a DELIBERATE DIVERGENCE: upstream
// throws by design, so this suite will never "pass unpatched" and the hunk
// will never drop. Carry it forward and re-evaluate on every svelte bump,
// the way the ownerless-roots hunk is carried.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { get, set, state } from 'svelte/internal/client';
import DuplicateKeyHost from './svelte-patch-fixtures/DuplicateKeyHost.svelte';
import type { DuplicateKeyItem } from './svelte-patch-fixtures/types';

interface Host {
  itemsSig: ReturnType<typeof state<DuplicateKeyItem[]>>;
  labelSig: ReturnType<typeof state<string>>;
  target: HTMLElement;
}

let mounted: Array<{ target: HTMLElement; app: Record<string, unknown> }> = [];

function mountHost(items: DuplicateKeyItem[], label = 'sibling-before'): Host {
  const itemsSig = state<DuplicateKeyItem[]>(items);
  const labelSig = state<string>(label);
  const target = document.body.appendChild(document.createElement('div'));
  const app = mount(DuplicateKeyHost, {
    target,
    props: { getItems: () => get(itemsSig), getLabel: () => get(labelSig) },
  });
  mounted.push({ target, app });
  flushSync();
  return { itemsSig, labelSig, target };
}

/** Run `fn`, returning whatever it threw (or null). */
function captureThrow(fn: () => void): unknown {
  try {
    fn();
    return null;
  } catch (err) {
    return err;
  }
}

function rows(target: HTMLElement): HTMLLIElement[] {
  return [...target.querySelectorAll('li')];
}

function labels(target: HTMLElement): string[] {
  return rows(target).map((li) => li.textContent ?? '');
}

function siblingText(target: HTMLElement): string {
  return target.querySelector('[data-testid="sibling"]')?.textContent ?? '';
}

/** Distinct-label rows keyed 'a', 'b', 'a' — the repeat is the last one. */
function duplicateItems(): DuplicateKeyItem[] {
  return [
    { key: 'a', label: 'a-first' },
    { key: 'b', label: 'b-only' },
    { key: 'a', label: 'a-repeat' },
  ];
}

// The patch reads a bare `reportError`, so the spy has to be the global.
// happy-dom may or may not define one, hence the save/restore-or-delete.
const globals = globalThis as Record<string, unknown>;
let reportErrorSpy: ReturnType<typeof vi.fn>;
let hadReportError = false;
let originalReportError: unknown;

beforeEach(() => {
  hadReportError = 'reportError' in globals;
  originalReportError = globals.reportError;
  reportErrorSpy = vi.fn();
  globals.reportError = reportErrorSpy;
});

afterEach(() => {
  for (const { target, app } of mounted) {
    unmount(app);
    target.remove();
  }
  mounted = [];
  if (hadReportError) globals.reportError = originalReportError;
  else delete globals.reportError;
});

describe('svelte patch: a duplicate each key is repaired, never thrown', () => {
  it('renders every row when the initial array already repeats a key', () => {
    let host!: Host;
    expect(captureThrow(() => {
      host = mountHost(duplicateItems());
    })).toBeNull();

    // The row count is the assertion the hunk exists for: pristine svelte
    // renders nothing at all here, because the throw escapes the mount.
    expect(rows(host.target)).toHaveLength(3);
    expect(labels(host.target)).toEqual(['a-first', 'b-only', 'a-repeat']);
    expect(reportErrorSpy).toHaveBeenCalledTimes(1);
  });

  it('finishes the batch a duplicate arrives in, so later effects still run', () => {
    const host = mountHost(
      [
        { key: 'a', label: 'a-first' },
        { key: 'b', label: 'b-only' },
      ],
      'sibling-before',
    );
    expect(siblingText(host.target)).toBe('sibling-before');

    // One batch: the each block gains a repeat AND the sibling text
    // changes. The sibling is downstream of the each in tree order, so
    // pristine svelte's throw would abort before it updated.
    set(host.itemsSig, duplicateItems());
    set(host.labelSig, 'sibling-after');
    expect(captureThrow(flushSync)).toBeNull();

    expect(siblingText(host.target)).toBe('sibling-after');
    expect(labels(host.target)).toEqual(['a-first', 'b-only', 'a-repeat']);
  });

  it('keeps the repaired row on the same element across runs', () => {
    // `key\u0000#2` is derived from the key, not minted per run, so the
    // repaired row is reconciled rather than remounted — it keeps its DOM
    // node, its state and its scroll position like any other row.
    const host = mountHost(duplicateItems());
    const before = rows(host.target);

    set(host.itemsSig, duplicateItems());
    flushSync();

    const after = rows(host.target);
    expect(after).toHaveLength(3);
    expect(after[0]).toBe(before[0]);
    expect(after[1]).toBe(before[1]);
    expect(after[2], 'the repaired row is the one at risk').toBe(before[2]);
    expect(labels(host.target)).toEqual(['a-first', 'b-only', 'a-repeat']);
  });

  it('reconciles back to the unique rows when the duplicate goes away', () => {
    const host = mountHost(duplicateItems());
    expect(rows(host.target)).toHaveLength(3);

    set(host.itemsSig, [
      { key: 'a', label: 'a-first' },
      { key: 'b', label: 'b-only' },
    ]);
    expect(captureThrow(flushSync)).toBeNull();

    // The row that had been living under the repaired key is destroyed
    // like any removed item — no stray left behind.
    expect(rows(host.target)).toHaveLength(2);
    expect(labels(host.target)).toEqual(['a-first', 'b-only']);
  });

  it('reports once per each-block instance, naming the key and the index', () => {
    const host = mountHost([
      { key: 'row-7', label: 'first' },
      { key: 'row-7', label: 'second' },
    ]);

    expect(reportErrorSpy).toHaveBeenCalledTimes(1);
    const reported = reportErrorSpy.mock.calls[0][0] as Error;
    expect(reported).toBeInstanceOf(Error);
    expect(reported.message).toContain('each_key_duplicate');
    expect(reported.message).toContain('row-7');
    expect(reported.message).toContain('index 1');
    expect(reported.message).toContain('repaired to a unique key');

    // A repeat every run is the shape a real bug takes (a list that keeps
    // re-rendering with the same collision). One record, not one per frame.
    for (const label of ['second-run', 'third-run']) {
      set(host.itemsSig, [
        { key: 'row-7', label: 'first' },
        { key: 'row-7', label },
      ]);
      flushSync();
    }
    expect(labels(host.target)).toEqual(['first', 'third-run']);
    expect(reportErrorSpy).toHaveBeenCalledTimes(1);

    // Per INSTANCE, though: a second block with its own collision is its
    // own bug and reports on its own.
    mountHost([
      { key: 'other', label: 'first' },
      { key: 'other', label: 'second' },
    ]);
    expect(reportErrorSpy).toHaveBeenCalledTimes(2);
  });

  it('repairs a repeated non-string key too, with a fresh symbol per run', () => {
    // Anything but a string or number gets a Symbol, so the repaired row
    // remounts on every run. That is a worse outcome than the stable path
    // and a fine one next to a frozen page — what matters is that no run
    // throws and every row is on screen.
    const shared = { id: 'shared' };
    let host!: Host;
    expect(captureThrow(() => {
      host = mountHost([
        { key: shared, label: 'one' },
        { key: { id: 'other' }, label: 'two' },
        { key: shared, label: 'three' },
      ]);
    })).toBeNull();

    expect(labels(host.target)).toEqual(['one', 'two', 'three']);
    expect(reportErrorSpy).toHaveBeenCalledTimes(1);

    set(host.itemsSig, [
      { key: shared, label: 'one' },
      { key: shared, label: 'three' },
    ]);
    expect(captureThrow(flushSync)).toBeNull();
    expect(labels(host.target)).toEqual(['one', 'three']);
    expect(reportErrorSpy).toHaveBeenCalledTimes(1);
  });
});
