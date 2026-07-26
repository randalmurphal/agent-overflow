import { describe, expect, it } from 'vitest';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';

function registry(overrides: { defaultCollapsed?: boolean; windowRows?: number } = {}) {
  return createThreadActivityRuns({
    defaultCollapsed: () => overrides.defaultCollapsed ?? false,
    windowRows: () => overrides.windowRows ?? 30,
  });
}

/** One projection pass over runs described by their member ids. */
function pass(
  runs: ReturnType<typeof registry>,
  members: string[][],
  rowCounts?: number[],
): { runId: string; collapsed: boolean; mountedRows: number }[] {
  runs.beginPass();
  const out = members.map((ids, i) => runs.resolve(ids, rowCounts?.[i] ?? ids.length));
  runs.endPass();
  return out;
}

describe('collapse state', () => {
  it('follows the setting default until explicitly toggled', () => {
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']]);

    expect(run.collapsed).toBe(true);
  });

  it('an explicit override wins over the setting default', () => {
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']]);
    runs.setCollapsed(run.runId, false);

    expect(pass(runs, [['a']])[0].collapsed).toBe(false);
  });

  it('an override survives a default flip in either direction', () => {
    let collapsedDefault = false;
    const runs = createThreadActivityRuns({
      defaultCollapsed: () => collapsedDefault,
      windowRows: () => 30,
    });
    const [run] = pass(runs, [['a']]);
    runs.setCollapsed(run.runId, true);

    collapsedDefault = true;
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
    collapsedDefault = false;
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
  });

  it('toggles from the effective state, not from a stale stored one', () => {
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']]);

    // Never explicitly set, so the first toggle must read the default (true)
    // and land on false rather than flipping a null to true.
    runs.toggleCollapsed(run.runId);

    expect(runs.isCollapsed(run.runId)).toBe(false);
  });
});

describe('scroll snapshots', () => {
  it('round-trips through a simulated remount', () => {
    const runs = registry();
    const [run] = pass(runs, [['a', 'b']]);

    runs.saveScrollSnapshot(run.runId, { scrollTop: 240, escaped: true });

    expect(runs.scrollSnapshot(run.runId)).toEqual({ scrollTop: 240, escaped: true });
  });

  it('reports null for a run that has never scrolled', () => {
    const runs = registry();
    const [run] = pass(runs, [['a']]);

    expect(runs.scrollSnapshot(run.runId)).toBeNull();
  });

  it('survives the window edges that re-key a run', () => {
    const runs = registry();
    const [before] = pass(runs, [['b', 'c']]);
    runs.saveScrollSnapshot(before.runId, { scrollTop: 120, escaped: false });

    // Lazy paging prepends 'a'; the run keeps its id, so its scroll position
    // is still addressable and the row does not remount under the user.
    const [after] = pass(runs, [['a', 'b', 'c']]);

    expect(after.runId).toBe(before.runId);
    expect(runs.scrollSnapshot(after.runId)).toEqual({ scrollTop: 120, escaped: false });
  });
});

describe('mounted rows', () => {
  it('defaults to the window setting, capped by the run length', () => {
    const runs = registry({ windowRows: 30 });

    expect(pass(runs, [['a']], [100])[0].mountedRows).toBe(30);
    expect(pass(runs, [['a']], [7])[0].mountedRows).toBe(7);
  });

  it('an explicit mount count survives the next pass', () => {
    const runs = registry({ windowRows: 30 });
    const [run] = pass(runs, [['a']], [100]);

    runs.setMountedRows(run.runId, 55);

    expect(pass(runs, [['a']], [100])[0].mountedRows).toBe(55);
  });

  it('an explicit mount count is still capped by the run length', () => {
    const runs = registry({ windowRows: 30 });
    const [run] = pass(runs, [['a']], [100]);
    runs.setMountedRows(run.runId, 55);

    expect(pass(runs, [['a']], [20])[0].mountedRows).toBe(20);
  });

  it('tracks the setting until the user pulls in an older chunk', () => {
    let rows = 30;
    const runs = createThreadActivityRuns({
      defaultCollapsed: () => false,
      windowRows: () => rows,
    });
    pass(runs, [['a']], [100]);

    rows = 50;

    expect(pass(runs, [['a']], [100])[0].mountedRows).toBe(50);
  });

  it('never mounts zero rows', () => {
    const runs = registry({ windowRows: 0 });

    expect(pass(runs, [['a']], [10])[0].mountedRows).toBe(1);
  });
});

describe('entry lifecycle', () => {
  it('sweeps entries whose runs no longer exist', () => {
    const runs = registry();
    const [run] = pass(runs, [['a']]);
    runs.setCollapsed(run.runId, true);

    // 'a' is gone from the window entirely; nothing can claim the entry.
    pass(runs, [['z']]);

    // A run that later reuses the same member id gets a fresh entry, not
    // the swept one's collapse state.
    expect(pass(runs, [['a']])[0].collapsed).toBe(false);
  });

  it('does not let two runs claim the same entry in one pass', () => {
    const runs = registry();
    const [first] = pass(runs, [['a', 'b']]);

    const [left, right] = pass(runs, [['a'], ['b']]);

    expect(left.runId).toBe(first.runId);
    expect(right.runId).not.toBe(first.runId);
  });

  it('clear drops every entry', () => {
    const runs = registry();
    const [run] = pass(runs, [['a']]);
    runs.setCollapsed(run.runId, true);
    runs.saveScrollSnapshot(run.runId, { scrollTop: 10, escaped: true });

    runs.clear();

    const [fresh] = pass(runs, [['a']]);
    expect(fresh.collapsed).toBe(false);
    expect(runs.scrollSnapshot(fresh.runId)).toBeNull();
  });

  it('mints distinct ids for concurrent runs', () => {
    const runs = registry();

    const [a, b] = pass(runs, [['a'], ['b']]);

    expect(a.runId).not.toBe(b.runId);
  });
});
