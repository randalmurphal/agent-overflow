import { describe, expect, it } from 'vitest';
import type { ActivityRunResolution } from '../utils/activityRunGrouping';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';

function registry(overrides: { defaultCollapsed?: boolean; windowRows?: number } = {}) {
  return createThreadActivityRuns({
    defaultCollapsed: () => overrides.defaultCollapsed ?? false,
    windowRows: () => overrides.windowRows ?? 30,
  });
}

/**
 * One run, described row by row. A bare id is a one-item row; an array is a
 * group row carrying several items, which is the case that makes a run's row
 * count differ from its member count.
 */
type RunSpec = (string | string[])[];

/** One projection pass. */
function pass(
  runs: ReturnType<typeof registry>,
  specs: RunSpec[],
): ActivityRunResolution[] {
  runs.beginPass();
  const out = specs.map((spec) =>
    runs.resolve(spec.map((row) => (typeof row === 'string' ? [row] : row))),
  );
  runs.endPass();
  return out;
}

/** A run of `n` single-item rows, with ids stable across passes. */
function rows(n: number): RunSpec {
  return Array.from({ length: n }, (_, i) => `i${i}`);
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

describe('mount window', () => {
  it('rests on the run tail, sized by the setting and capped by the length', () => {
    const runs = registry({ windowRows: 30 });

    expect(pass(runs, [rows(100)])[0]).toMatchObject({ mountedFrom: 70, mountedRows: 30 });
    expect(pass(runs, [rows(7)])[0]).toMatchObject({ mountedFrom: 0, mountedRows: 7 });
  });

  it('an explicit size survives the next pass', () => {
    const runs = registry({ windowRows: 30 });
    const [run] = pass(runs, [rows(100)]);

    runs.setMountWindow(run.runId, { rows: 55, startItemId: null });

    expect(pass(runs, [rows(100)])[0]).toMatchObject({ mountedFrom: 45, mountedRows: 55 });
  });

  it('an explicit size is still capped by the run length', () => {
    const runs = registry({ windowRows: 30 });
    const [run] = pass(runs, [rows(100)]);
    runs.setMountWindow(run.runId, { rows: 55, startItemId: null });

    expect(pass(runs, [rows(20)])[0]).toMatchObject({ mountedFrom: 0, mountedRows: 20 });
  });

  it('tracks the setting until the user pulls in another chunk', () => {
    let windowRows = 30;
    const runs = createThreadActivityRuns({
      defaultCollapsed: () => false,
      windowRows: () => windowRows,
    });
    pass(runs, [rows(100)]);

    windowRows = 50;

    expect(pass(runs, [rows(100)])[0].mountedRows).toBe(50);
  });

  it('never mounts zero rows', () => {
    const runs = registry({ windowRows: 0 });

    expect(pass(runs, [rows(10)])[0].mountedRows).toBe(1);
  });

  it('starts at the anchored row when one is set', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(100)]);

    runs.setMountWindow(run.runId, { rows: 10, startItemId: 'i20' });

    expect(pass(runs, [rows(100)])[0]).toMatchObject({ mountedFrom: 20, mountedRows: 10 });
  });

  it('holds its anchored rows as the run grows underneath it', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(60)]);
    runs.setMountWindow(run.runId, { rows: 10, startItemId: 'i20' });
    pass(runs, [rows(60)]);

    // 40 more rows stream in below. The reader is not at the tail, so the
    // window must not slide off what they are reading.
    expect(pass(runs, [rows(100)])[0].mountedFrom).toBe(20);
  });

  it('holds them across a head prune that shifts every row index', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(100)]);
    runs.setMountWindow(run.runId, { rows: 10, startItemId: 'i20' });
    pass(runs, [rows(100)]);

    // The live-window prune drops i0..i9, so the anchored row moves from
    // index 20 to index 10 — which an index-keyed window would not survive.
    const pruned = rows(100).slice(10);

    expect(pass(runs, [pruned])[0].mountedFrom).toBe(10);
  });

  it('returns to the tail once the window reaches the run end', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(100)]);
    // Anchored where nothing is left below it: there is nothing to hold on
    // to, so the window resumes following new activity.
    runs.setMountWindow(run.runId, { rows: 10, startItemId: 'i95' });
    expect(pass(runs, [rows(100)])[0].mountedFrom).toBe(90);

    expect(pass(runs, [rows(110)])[0].mountedFrom).toBe(100);
  });

  it('returns to the tail when its anchored row leaves the run', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(100)]);
    runs.setMountWindow(run.runId, { rows: 10, startItemId: 'i20' });
    pass(runs, [rows(100)]);

    const withoutAnchor = rows(100).filter((id) => id !== 'i20');

    expect(pass(runs, [withoutAnchor])[0].mountedFrom).toBe(89);
  });

  it('counts a group row once, not once per member', () => {
    const runs = registry({ windowRows: 2 });

    // Three rows, five items: the window is in row space.
    expect(pass(runs, [['a', ['b', 'c', 'd'], 'e']])[0])
      .toMatchObject({ mountedFrom: 1, mountedRows: 2 });
  });

  it('anchors on any item a group row carries', () => {
    const runs = registry({ windowRows: 1 });
    const [run] = pass(runs, [['a', ['b', 'c'], 'd', 'e']]);

    // A jump into a subagent card resolves to the card's launch item, which
    // is not the first id its row carries.
    runs.setMountWindow(run.runId, { rows: 1, startItemId: 'c' });

    expect(pass(runs, [['a', ['b', 'c'], 'd', 'e']])[0].mountedFrom).toBe(1);
  });
});

describe('focus requests', () => {
  it('hands the request to the first reader and no one else', () => {
    const runs = registry();
    const [run] = pass(runs, [rows(40)]);

    runs.requestFocus(run.runId, 'i5');

    expect(runs.takeFocus(run.runId)).toBe('i5');
    expect(runs.takeFocus(run.runId)).toBeNull();
  });

  it('reports nothing for a run that was never jumped into', () => {
    const runs = registry();
    const [run] = pass(runs, [rows(40)]);

    expect(runs.takeFocus(run.runId)).toBeNull();
  });

  it('bumps the revision so a row can notice a request it changes nothing else about', () => {
    const runs = registry();
    const [run] = pass(runs, [rows(40)]);
    const before = runs.revision;

    runs.requestFocus(run.runId, 'i39');

    expect(runs.revision).toBeGreaterThan(before);
  });

  it('survives until the run is mounted, across passes', () => {
    const runs = registry();
    const [run] = pass(runs, [rows(40)]);
    runs.requestFocus(run.runId, 'i5');

    pass(runs, [rows(40)]);

    expect(runs.takeFocus(run.runId)).toBe('i5');
  });
});

describe('entry lifecycle', () => {
  it('does not let two runs claim the same entry in one pass', () => {
    const runs = registry();
    const [first] = pass(runs, [['a', 'b']]);

    const [left, right] = pass(runs, [['a'], ['b']]);

    expect(left.runId).toBe(first.runId);
    expect(right.runId).not.toBe(first.runId);
  });

  it('mints distinct ids for concurrent runs', () => {
    const runs = registry();

    const [a, b] = pass(runs, [['a'], ['b']]);

    expect(a.runId).not.toBe(b.runId);
  });

  it('drops a swept entry that was never touched', () => {
    const runs = registry();
    pass(runs, [['a']]);

    // Nothing explicit on it, so there is nothing to bring back — and the
    // archive slot stays free for a run the user actually acted on.
    pass(runs, [['z']]);

    expect(pass(runs, [['a']])[0].collapsed).toBe(false);
  });
});

describe('state across a sweep', () => {
  it('comes back when the whole run leaves the window and returns', () => {
    const runs = registry();
    const [run] = pass(runs, [['a', 'b', 'c']]);
    runs.setCollapsed(run.runId, true);
    runs.saveScrollSnapshot(run.runId, { scrollTop: 240, escaped: true });

    // The live-window prune takes every item this run had.
    pass(runs, [['z']]);
    // Load-older brings them back.
    const [back] = pass(runs, [['a', 'b', 'c']]);

    expect(back.collapsed).toBe(true);
    expect(runs.scrollSnapshot(back.runId)).toEqual({ scrollTop: 240, escaped: true });
  });

  it('is found again by the run tail when the prune cut its head', () => {
    const runs = registry();
    const [run] = pass(runs, [['a', 'b', 'c', 'd']]);
    runs.setCollapsed(run.runId, true);

    pass(runs, [['z']]);

    expect(pass(runs, [['c', 'd', 'e']])[0].collapsed).toBe(true);
  });

  it('is found again by the run head when the prune cut its tail', () => {
    const runs = registry();
    const [run] = pass(runs, [['a', 'b', 'c', 'd']]);
    runs.setCollapsed(run.runId, true);

    pass(runs, [['z']]);

    expect(pass(runs, [['a', 'b']])[0].collapsed).toBe(true);
  });

  it('survives a thread switch and back', () => {
    const runs = registry({ windowRows: 30 });
    const [run] = pass(runs, [rows(50)]);
    runs.setMountWindow(run.runId, { rows: 12, startItemId: null });
    runs.saveScrollSnapshot(run.runId, { scrollTop: 90, escaped: true });

    runs.clear();
    // The other thread's runs cannot collide: item ids are unique per thread.
    pass(runs, [['x', 'y']]);
    runs.clear();

    const [back] = pass(runs, [rows(50)]);
    expect(back.mountedRows).toBe(12);
    expect(runs.scrollSnapshot(back.runId)).toEqual({ scrollTop: 90, escaped: true });
  });

  it('gives a revived run a live id, not the swept one', () => {
    const runs = registry();
    const [run] = pass(runs, [['a']]);
    runs.setCollapsed(run.runId, true);
    pass(runs, [['z']]);

    const [back] = pass(runs, [['a']]);

    expect(back.runId).not.toBe(run.runId);
    expect(back.collapsed).toBe(true);
  });

  it('is claimed once — a split cannot revive the same state twice', () => {
    const runs = registry();
    const [run] = pass(runs, [['a', 'b']]);
    runs.setCollapsed(run.runId, true);
    pass(runs, [['z']]);

    // A `proposed_plan` arriving mid-run splits it in two. Only the half
    // holding an archived key inherits; the other starts from the default.
    const [left, right] = pass(runs, [['a'], ['b']]);

    expect(left.collapsed).toBe(true);
    expect(right.collapsed).toBe(false);
  });

  it('a live entry beats an archived one for the same member', () => {
    const runs = registry({ windowRows: 30 });
    const [archived] = pass(runs, [['b']]);
    runs.setMountWindow(archived.runId, { rows: 4, startItemId: null });
    // 'b' leaves the window and is archived; a separate run keeps 'a' live.
    const [live] = pass(runs, [['a']]);
    runs.setMountWindow(live.runId, { rows: 7, startItemId: null });

    // The merged run shares a member with both. The live entry is the same
    // run still going, so it wins.
    expect(pass(runs, [['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h']])[0].mountedRows)
      .toBe(7);
  });
});
