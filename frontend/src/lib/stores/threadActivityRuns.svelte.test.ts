import { beforeEach, describe, expect, it } from 'vitest';
import type { ActivityRunResolution } from '../utils/activityRunGrouping';
import type { ActivityRunNode } from '../utils/subagentGrouping';
import { revealActivityRunItem } from '../utils/activityRunWindow';
import {
  createThreadActivityRuns,
  resetActivityRunAnchorReportsForTest,
} from './threadActivityRuns.svelte';
import type { PaneScrollController } from './threadPaneShared';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';
import { makeItem } from '../../test/helpers/chat';
import { pass, registry, rows } from '../../test/helpers/activityRuns';

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
      scrollController: () => null,
    });
    const [run] = pass(runs, [['a']]);
    runs.setCollapsed(run.runId, true);

    collapsedDefault = true;
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
    collapsedDefault = false;
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
  });

});

describe('collapse state while a run is still working', () => {
  it('renders open while live, and stays open once settled until released', () => {
    // The defaults say how a run should SIT, and one still filling has not
    // settled into anything yet — collapsing by default must not mean going
    // blind to work that is still arriving. And losing the tail is NOT what
    // closes it: snapping shut on the settle frame would remove a viewport of
    // content in front of whoever watched it stream, so the open-because-live
    // hold outlives liveness until the timeline's gate releases it off-screen
    // (`releaseOpenedLive`).
    const runs = registry({ defaultCollapsed: true });
    const [live] = pass(runs, [['a']], 'thread-1', 0);
    expect(live.collapsed).toBe(false);

    const [settled] = pass(runs, [['a']]);
    expect(settled.collapsed).toBe(false);

    runs.releaseOpenedLive([settled.runId]);
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
  });

  it('a run that arrives already settled follows the default outright', () => {
    // No pass ever resolved it open-because-live — a history load, or a
    // thread switch back — so there is no hold and nothing owed: nobody was
    // watching this run stream.
    const runs = registry({ defaultCollapsed: true });

    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
    expect(runs.openedLiveRunIds()).toEqual([]);
  });

  it('closes now when the reader collapses it, and stays closed as it settles', () => {
    // The click is about this run, right now. A collapse that waited for the run
    // to finish would leave the reader with nothing to show the click landed —
    // which is the entire visible effect of collapsing.
    const runs = registry({ defaultCollapsed: false });
    const [run] = pass(runs, [['a']], 'thread-1', 0);

    runs.setCollapsed(run.runId, true);

    expect(pass(runs, [['a']], 'thread-1', 0)[0].collapsed).toBe(true);
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
  });

  it('keeps an explicitly expanded run open once it settles', () => {
    // The other direction of the same rule: an answer about the run outlives
    // the liveness that would otherwise have decided for it.
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']], 'thread-1', 0);
    runs.setCollapsed(run.runId, false);

    expect(pass(runs, [['a']])[0].collapsed).toBe(false);
  });

  it('is not closed by a bulk collapse, which is a default and not an answer', () => {
    // `setAllCollapsed` sets the thread's default and DROPS per-run overrides
    // (that is how a later flip reaches every run), so it is the same kind of
    // fact as the setting: it governs runs that have settled. The one still
    // working keeps showing its work — it re-records its open-because-live
    // hold on the very next pass — and once it settles, the gate collapses it
    // off-screen like any other held-open run.
    const runs = registry({ defaultCollapsed: false });
    pass(runs, [['a']], 'thread-1', 0);

    runs.setAllCollapsed(true);

    expect(pass(runs, [['a']], 'thread-1', 0)[0].collapsed).toBe(false);
    const [settled] = pass(runs, [['a']]);
    expect(settled.collapsed).toBe(false);

    runs.releaseOpenedLive([settled.runId]);
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
  });
});

describe('the open-because-live hold', () => {
  // The auto-collapse gate's registry half. Sequences, not states: the hold
  // is persistent side-state recorded by `collapsedFor`, so every path that
  // retires it has to be exercised as a transition from a pass that set it.

  it('lists held runs for the gate, including the one still live', () => {
    // The registry cannot know liveness, so it cannot filter the live run
    // out — the gate does, from `node.live`.
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']], 'thread-1', 0);

    expect(runs.openedLiveRunIds()).toEqual([run.runId]);

    pass(runs, [['a']]);
    expect(runs.openedLiveRunIds()).toEqual([run.runId]);

    runs.releaseOpenedLive([run.runId]);
    expect(runs.openedLiveRunIds()).toEqual([]);
  });

  it('release applies the default, forgets the inner position, and rebuilds', () => {
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a', 'b']], 'thread-1', 0);
    pass(runs, [['a', 'b']]);
    // Held open, so the clip is real and its position recordable.
    runs.saveScrollSnapshot(run.runId, { scrollTop: 240, escaped: false });
    expect(runs.scrollSnapshot(run.runId)).toEqual({ scrollTop: 240, escaped: false });
    const before = runs.revision;

    runs.releaseOpenedLive([run.runId]);

    // The run became a chip: same forgetting as a clicked collapse, and a
    // revision bump so the projection actually renders the change.
    expect(runs.revision).toBeGreaterThan(before);
    expect(runs.scrollSnapshot(run.runId)).toBeNull();
    expect(pass(runs, [['a', 'b']])[0].collapsed).toBe(true);
  });

  it('release under an expanded default changes nothing it would have to undo', () => {
    // The run keeps rendering exactly as it was, so dropping its inner
    // position would yank a still-open clip to its tail, and a revision bump
    // would rebuild the projection to change nothing.
    const overrides = { defaultCollapsed: false };
    const runs = registry(overrides);
    const [run] = pass(runs, [['a', 'b']], 'thread-1', 0);
    pass(runs, [['a', 'b']]);
    runs.saveScrollSnapshot(run.runId, { scrollTop: 240, escaped: true });
    const before = runs.revision;

    runs.releaseOpenedLive([run.runId]);

    expect(runs.revision).toBe(before);
    expect(runs.scrollSnapshot(run.runId)).toEqual({ scrollTop: 240, escaped: true });
    expect(pass(runs, [['a', 'b']])[0].collapsed).toBe(false);
    // Retired all the same: a later default flip treats this run like any
    // other settled one instead of finding a years-old hold.
    expect(runs.openedLiveRunIds()).toEqual([]);
    overrides.defaultCollapsed = true;
    expect(pass(runs, [['a', 'b']])[0].collapsed).toBe(true);
  });

  it('release is idempotent and ignores a run that does not exist', () => {
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']], 'thread-1', 0);
    pass(runs, [['a']]);

    runs.releaseOpenedLive([run.runId]);
    const after = runs.revision;
    runs.releaseOpenedLive([run.runId]);
    runs.releaseOpenedLive(['r99']);

    expect(runs.revision).toBe(after);
  });

  it('a reader collapse retires the hold, in both directions of the click', () => {
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']], 'thread-1', 0);
    pass(runs, [['a']]);

    runs.setCollapsed(run.runId, true);
    expect(runs.openedLiveRunIds()).toEqual([]);
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);

    // Expanding afterwards is an override, not a revived hold.
    runs.setCollapsed(run.runId, false);
    expect(runs.openedLiveRunIds()).toEqual([]);
    expect(pass(runs, [['a']])[0].collapsed).toBe(false);
  });

  it('a bulk action retires every hold it can reach', () => {
    const runs = registry({ defaultCollapsed: true });
    pass(runs, [['a']], 'thread-1', 0);
    pass(runs, [['a']]);

    runs.setAllCollapsed(true);

    expect(runs.openedLiveRunIds()).toEqual([]);
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
  });

  it('re-arms when a released run becomes live again', () => {
    // A completion pairing into a settled run can hand it the tail back. The
    // hold is not a one-shot: opening because live is the same commitment the
    // second time.
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']], 'thread-1', 0);
    pass(runs, [['a']]);
    runs.releaseOpenedLive([run.runId]);
    expect(pass(runs, [['a']])[0].collapsed).toBe(true);

    expect(pass(runs, [['a']], 'thread-1', 0)[0].collapsed).toBe(false);
    const [settled] = pass(runs, [['a']]);
    expect(settled.collapsed).toBe(false);
    expect(runs.openedLiveRunIds()).toEqual([settled.runId]);
  });

  it('does not survive a sweep — a revived run follows the default', () => {
    // A run coming back from the archive mounts fresh rows nobody was
    // watching stream; holding it open would preserve a courtesy for a
    // reader who is no longer there.
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a', 'b']], 'thread-1', 0);
    pass(runs, [['a', 'b']]);
    // Something explicit so the sweep archives the entry at all.
    runs.saveScrollSnapshot(run.runId, { scrollTop: 120, escaped: false });

    pass(runs, [['z']]);
    const [back] = pass(runs, [['a', 'b']]);

    expect(back.collapsed).toBe(true);
    expect(runs.openedLiveRunIds()).toEqual([]);
  });
});

describe('collapsing all', () => {
  it('reports the thread default the control renders from', () => {
    const runs = registry({ defaultCollapsed: false });

    expect(runs.bulkCollapsed).toBe(false);
    runs.setAllCollapsed(true);
    expect(runs.bulkCollapsed).toBe(true);
  });

  it('takes runs that had their own state with it, in both directions', () => {
    const runs = registry({ defaultCollapsed: false });
    const [kept, flipped] = pass(runs, [['a'], ['b']]);
    runs.setCollapsed(flipped.runId, true);

    runs.setAllCollapsed(true);
    expect(pass(runs, [['a'], ['b']]).map((r) => r.collapsed)).toEqual([true, true]);

    // And back: the override that said "collapsed" was dropped, not inverted,
    // so it cannot survive to contradict the next bulk action either.
    runs.setAllCollapsed(false);
    expect(pass(runs, [['a'], ['b']]).map((r) => r.collapsed)).toEqual([false, false]);
    expect(kept.runId).not.toBe(flipped.runId);
  });

  it('governs a run that does not exist yet', () => {
    // Older history pages in after the action. A bulk state that only reached
    // the runs loaded at the time would not be "all".
    const runs = registry({ defaultCollapsed: false });
    pass(runs, [['b']]);
    runs.setAllCollapsed(true);

    expect(pass(runs, [['b'], ['c']])[1].collapsed).toBe(true);
  });

  it('governs a run that comes back from the archive', () => {
    const runs = registry({ defaultCollapsed: false });
    const [run] = pass(runs, [['a']]);
    // An explicit expand, archived when the run leaves the loaded window.
    runs.setCollapsed(run.runId, false);
    pass(runs, [['z']]);

    runs.setAllCollapsed(true);

    expect(pass(runs, [['a']])[0].collapsed).toBe(true);
  });

  it('leaves a single run free to disagree afterwards', () => {
    const runs = registry({ defaultCollapsed: false });
    const [run] = pass(runs, [['a'], ['b']]);
    runs.setAllCollapsed(true);

    runs.setCollapsed(run.runId, false);

    expect(pass(runs, [['a'], ['b']]).map((r) => r.collapsed)).toEqual([false, true]);
  });

  it('is scoped to its thread', () => {
    const runs = registry({ defaultCollapsed: false });
    pass(runs, [['a']]);
    runs.setAllCollapsed(true);

    runs.clear();

    expect(runs.bulkCollapsed).toBe(false);
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

  it('is dropped when the run collapses, so it reopens at its newest row', () => {
    const runs = registry();
    const [run] = pass(runs, [rows(60)]);
    runs.setWindowAnchor(run.runId, 'i10');
    runs.saveScrollSnapshot(run.runId, { scrollTop: 240, escaped: true });

    runs.setCollapsed(run.runId, true);

    // Both halves of "where the reader was", because the chip replaced the
    // inside of the run: an offset would reopen it mid-list, and the window
    // pin would hold the mounted rows away from the tail it now lands on.
    expect(runs.scrollSnapshot(run.runId)).toBeNull();
    expect(runs.windowAnchor(run.runId)).toBeNull();
    expect(pass(runs, [rows(60)])[0].mountedFrom).toBe(30);
  });

  it('refuses a save from the clip a collapse is tearing down', () => {
    // The row that becomes a chip unmounts its clip THROUGH saveScrollSnapshot,
    // and a detached element reports scrollTop 0 — so without the refusal every
    // collapse-then-expand would reopen the run at its first row.
    const runs = registry();
    const [run] = pass(runs, [['a', 'b']]);
    runs.setCollapsed(run.runId, true);

    runs.saveScrollSnapshot(run.runId, { scrollTop: 0, escaped: false });

    expect(runs.scrollSnapshot(run.runId)).toBeNull();
  });

  it('refuses a save for a run that renders without a clip', () => {
    // Presence is recorded by `collapsedFor` itself — the resolution IS what
    // the row renders — so a run sitting collapsed under the defaults has no
    // clip to have produced this save, whoever claims otherwise.
    const runs = registry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a', 'b']]);

    runs.saveScrollSnapshot(run.runId, { scrollTop: 120, escaped: false });

    expect(runs.scrollSnapshot(run.runId)).toBeNull();
  });

  it('keeps the rows the reader paged in across a collapse', () => {
    // The row-count override is what they ASKED for, which the chip does not
    // contradict — unlike the position, which the chip replaced.
    const runs = registry({ windowRows: 30 });
    const [run] = pass(runs, [rows(100)]);
    runs.setMountWindow(run.runId, { rows: 55, startItemId: null });

    runs.setCollapsed(run.runId, true);

    expect(pass(runs, [rows(100)])[0].mountedRows).toBe(55);
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
      scrollController: () => null,
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

  it('clamps an anchor too late to fit a window, and keeps holding it', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(100)]);
    // Only five rows left below the anchor, so the window starts earlier than
    // asked rather than mounting five rows and blank space.
    runs.setMountWindow(run.runId, { rows: 10, startItemId: 'i95' });
    expect(pass(runs, [rows(100)])[0].mountedFrom).toBe(90);

    // Growth does NOT hand the window back to the tail. An anchor means the
    // reader is up there, and a window that resumed following on its own
    // would slide the rows they are reading out from under them.
    expect(pass(runs, [rows(110)])[0].mountedFrom).toBe(95);
  });

  it('follows the tail again when the anchor is released, in either direction', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(100)]);

    runs.setWindowAnchor(run.runId, 'i40');
    expect(pass(runs, [rows(100)])[0].mountedFrom).toBe(40);

    // The reader returned to the clip's bottom. Releasing has to hand the
    // window back to the tail, or a live run stays stranded behind its "N
    // later" boundary while it keeps streaming.
    runs.setWindowAnchor(run.runId, null);
    expect(pass(runs, [rows(100)])[0].mountedFrom).toBe(90);

    // And re-pinning after a release still works: the flag is state, not a
    // one-shot.
    runs.setWindowAnchor(run.runId, 'i40');
    expect(pass(runs, [rows(100)])[0].mountedFrom).toBe(40);
  });

  it('an anchored window keeps tracking the size setting', () => {
    let windowRows = 30;
    const runs = createThreadActivityRuns({
      defaultCollapsed: () => false,
      windowRows: () => windowRows,
      scrollController: () => null,
    });
    const [run] = pass(runs, [rows(100)]);

    // A jump anchors without stating a size, so this run still inherits.
    // Writing the size it already had would pin it, and every jump would pin
    // one more run against a later change to the setting.
    runs.setWindowAnchor(run.runId, 'i40');
    expect(pass(runs, [rows(100)])[0].mountedRows).toBe(30);

    windowRows = 50;
    const [wider] = pass(runs, [rows(100)]);
    expect(wider.mountedRows).toBe(50);
    expect(wider.mountedFrom).toBe(40);
  });

  it('anchoring leaves the window size alone', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(100)]);
    // Mounting an older chunk is a size change; pinning is not. Folding the
    // two together would freeze this run's size against a later change to the
    // `activityRunWindowRows` setting.
    runs.setWindowAnchor(run.runId, 'i40');

    // Both halves: the window moved (or the size claim would hold for a call
    // that did nothing at all), and it moved without resizing.
    expect(pass(runs, [rows(100)])[0])
      .toMatchObject({ mountedFrom: 40, mountedRows: 10 });
  });

  it('reports the pin the row has to carry across a remount', () => {
    const runs = registry({ windowRows: 10 });
    const [run] = pass(runs, [rows(100)]);

    expect(runs.windowAnchor(run.runId)).toBeNull();

    // The row's controller reads this to know the reader is parked up here:
    // a run pinned while it had no controller has no escape flag to restore.
    runs.setWindowAnchor(run.runId, 'i40');
    expect(runs.windowAnchor(run.runId)).toBe('i40');

    runs.setWindowAnchor(run.runId, null);
    expect(runs.windowAnchor(run.runId)).toBeNull();
    expect(runs.windowAnchor('r99')).toBeNull();
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

    expect(runs.requestFocus(run.runId, { itemId: 'i5', relocated: true })).toBe(true);

    expect(runs.takeFocus(run.runId)).toEqual({ itemId: 'i5', relocated: true });
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

    runs.requestFocus(run.runId, { itemId: 'i39', relocated: false });

    expect(runs.revision).toBeGreaterThan(before);
  });

  it('survives until the run is mounted, across passes', () => {
    const runs = registry();
    const [run] = pass(runs, [rows(40)]);
    runs.requestFocus(run.runId, { itemId: 'i5', relocated: true });

    pass(runs, [rows(40)]);

    expect(runs.takeFocus(run.runId)?.itemId).toBe('i5');
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

  it('ignores mutations naming a run that does not exist', () => {
    const runs = registry({ windowRows: 10 });

    // `resolve` is the only thing that creates an entry. A stale call — a
    // click landing after the pass that swept its run, or a typo — must not
    // seed state for whichever run later mints that id.
    runs.setCollapsed('r1', true);
    runs.saveScrollSnapshot('r1', { scrollTop: 400, escaped: true });
    runs.setWindowAnchor('r1', 'i40');
    // The one mutator that reports: a jump has a gesture to abandon, while a
    // teardown save has nothing left to do either way.
    expect(runs.requestFocus('r1', { itemId: 'i40', relocated: true })).toBe(false);

    // A hundred rows and an interior anchor, so a stored ghost would move the
    // window somewhere the tail default cannot reach on its own.
    const [run] = pass(runs, [rows(100)]);

    expect(run.runId).toBe('r1');
    expect(run.collapsed).toBe(false);
    expect(run.mountedFrom).toBe(90);
    expect(runs.windowAnchor('r1')).toBeNull();
    expect(runs.scrollSnapshot('r1')).toBeNull();
    expect(runs.takeFocus('r1')).toBeNull();
  });

  it('drops a snapshot saved after the registry was cleared', () => {
    const runs = registry();
    const [run] = pass(runs, [['a']]);
    runs.saveScrollSnapshot(run.runId, { scrollTop: 120, escaped: true });

    // The row tears down AFTER the thread switch cleared the registry. Its
    // last position is already archived; re-creating the entry here would
    // leave a memberless ghost that outlives the thread it came from.
    runs.clear();
    runs.saveScrollSnapshot(run.runId, { scrollTop: 999, escaped: false });
    expect(runs.scrollSnapshot(run.runId)).toBeNull();

    const [back] = pass(runs, [['a']]);
    expect(runs.scrollSnapshot(back.runId)).toEqual({ scrollTop: 120, escaped: true });
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
  // Deliberately two tests over one combined case: a collapsed run has no
  // inner position by construction, so a single run carrying both a collapse
  // override and a scroll snapshot is a state that cannot occur.
  it('brings a collapsed run back collapsed', () => {
    const runs = registry();
    const [run] = pass(runs, [['a', 'b', 'c']]);
    runs.setCollapsed(run.runId, true);

    // The live-window prune takes every item this run had.
    pass(runs, [['z']]);
    // Load-older brings them back.
    const [back] = pass(runs, [['a', 'b', 'c']]);

    expect(back.collapsed).toBe(true);
  });

  it('brings an expanded run back where the reader left it', () => {
    const runs = registry();
    const [run] = pass(runs, [['a', 'b', 'c']]);
    runs.saveScrollSnapshot(run.runId, { scrollTop: 240, escaped: true });

    pass(runs, [['z']]);
    const [back] = pass(runs, [['a', 'b', 'c']]);

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
    pass(runs, [['x', 'y']], 'thread-2');
    runs.clear();

    const [back] = pass(runs, [rows(50)]);
    expect(back.mountedRows).toBe(12);
    expect(runs.scrollSnapshot(back.runId)).toEqual({ scrollTop: 90, escaped: true });
  });

  it('does not hand one thread the state another thread archived', () => {
    const runs = registry();
    const [run] = pass(runs, [['think:0:0', 'think:0:1']]);
    runs.setCollapsed(run.runId, true);

    // Item ids are unique only WITHIN a thread — the store's key is
    // (thread_id, item_id), and synthesized ids are deterministic per turn —
    // so the incoming thread's first run really can present the exact ids the
    // outgoing one just archived.
    runs.clear();
    const [other] = pass(runs, [['think:0:0', 'think:0:1']], 'thread-2');
    expect(other.collapsed).toBe(false);

    // And the thread it belongs to still finds it.
    runs.clear();
    expect(pass(runs, [['think:0:0', 'think:0:1']])[0].collapsed).toBe(true);
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

// ── Window-anchor contract guard ──────────────────────────────────────────
//
// Every anchor writer names `timelineNodeItemId(run.children[from])`, and that
// id is only resolvable back to a row because `buildRun` yields it into
// `rowMemberIds[from]`. That is a contract across two files and every
// `TimelineNode` kind; the contract itself is pinned in
// `utils/activityRunAnchorContract.test.ts`. What is pinned HERE is the
// registry's structural defence when it breaks: an unresolvable anchor is
// coerced to tail-follow at the write, so a second write of the same id
// matches the stored `null` and does NOT bump `revision` — which is what stops
// resolve()-nulls-it / effect-rewrites-it becoming an infinite rebuild.
//
// Asserted against the REAL capture pipeline rather than a spy: the claim is
// that a broken invariant lands in `ui-trace/frontend-errors.jsonl`.

/** A projected node for a run, as a jump site would be holding it. */
function nodeFor(resolved: ActivityRunResolution, ids: readonly string[]): ActivityRunNode {
  return {
    kind: 'activity_run',
    runId: resolved.runId,
    threadId: 'thread-1',
    children: ids.map((id) => ({ kind: 'leaf' as const, item: makeItem({ id }) })),
    collapsed: false,
    live: false,
    atTail: false,
    mountedFrom: resolved.mountedFrom,
    mountedRows: resolved.mountedRows,
    membershipEpoch: resolved.membershipEpoch,
    memberItemIds: [...ids],
  };
}

describe('window-anchor guard', () => {
  const diagnostics = installDiagnosticsCapture();

  beforeEach(() => {
    // The once-per-run report ledger is module state.
    resetActivityRunAnchorReportsForTest();
  });

  function fixture() {
    const runs = registry({ windowRows: 2 });
    const [run] = pass(runs, [['a', 'b', 'c']]);
    return { runs, run };
  }

  it('reaches a fixpoint in two iterations for a contract-resolvable anchor', async () => {
    const { runs, run } = fixture();

    const before = runs.revision;
    runs.setWindowAnchor(run.runId, 'a');
    const afterFirst = runs.revision;
    // Second write of the same anchor: the registry already holds it, so the
    // projection is not invalidated again. That is the fixpoint — without it
    // every ActivityRun effect pass would rebuild the run node.
    runs.setWindowAnchor(run.runId, 'a');

    expect(afterFirst).toBe(before + 1);
    expect(runs.revision).toBe(afterFirst);
    expect(runs.windowAnchor(run.runId)).toBe('a');
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('refuses an anchor the run does not contain, reports once, and does not spin', async () => {
    // The shape a broken node kind produces: the writer names an id that is
    // not in the run's membership. Accepting it would null on the next
    // resolve(), the effect would rewrite it, and revision would bump every
    // lap forever.
    const { runs, run } = fixture();

    const before = runs.revision;
    runs.setWindowAnchor(run.runId, 'not-a-member');
    runs.setWindowAnchor(run.runId, 'not-a-member');
    runs.setWindowAnchor(run.runId, 'not-a-member');

    // Coerced to tail-follow, which the entry already was — so no rebuild at
    // all, however many times the effect re-asserts it.
    expect(runs.revision).toBe(before);
    expect(runs.windowAnchor(run.runId)).toBeNull();

    // ONE report for three writes. The re-assertion is an `$effect` that runs
    // per reveal tick, so reporting per write buries the finding under its own
    // repetition and burns the ui-trace rotation budget.
    const records = await diagnostics.all();
    expect(records).toHaveLength(1);
    expect(records[0].message).toContain('threadActivityRuns');
    // Constant message, variables in the detail — an id in the message would
    // mint a signature per run and walk past the per-signature cap.
    expect(records[0].message).not.toContain('not-a-member');
    expect(records[0].detail).toContain('not-a-member');
    // Console fallback: a remote session cannot persist at all.
    expect(diagnostics.warnings().join('\n')).toContain('not-a-member');
  });

  it('setMountWindow stores a resolvable anchor together with its row count', async () => {
    const { runs, run } = fixture();

    runs.setMountWindow(run.runId, { rows: 3, startItemId: 'a' });

    expect(runs.windowAnchor(run.runId)).toBe('a');
    expect(pass(runs, [['a', 'b', 'c']])[0]).toMatchObject({ mountedFrom: 0, mountedRows: 3 });
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('setMountWindow applies the same guard while still honouring the row count', async () => {
    const { runs, run } = fixture();

    runs.setMountWindow(run.runId, { rows: 3, startItemId: 'not-a-member' });

    expect(runs.windowAnchor(run.runId)).toBeNull();
    expect((await diagnostics.messages()).length).toBe(1);
    // The size is a separate, valid instruction — dropping it would silently
    // revert a chunk the reader explicitly paged in.
    expect(pass(runs, [['a', 'b', 'c']])[0].mountedRows).toBe(3);
  });

  it('an invalid write after a valid one releases the pin rather than keeping it', async () => {
    // Transition coverage: the guard is not only about the first write. A run
    // that legitimately pinned a row and is then handed a broken id must end
    // up following its tail — keeping the old pin would leave the reader
    // parked while the effect kept trying to move them.
    const { runs, run } = fixture();
    runs.setWindowAnchor(run.runId, 'a');
    const pinned = runs.revision;

    runs.setWindowAnchor(run.runId, 'not-a-member');

    expect(runs.windowAnchor(run.runId)).toBeNull();
    expect(runs.revision).toBe(pinned + 1);
    expect((await diagnostics.messages()).length).toBe(1);
  });

  it('releasing the anchor (null) is never reported', async () => {
    const { runs, run } = fixture();
    runs.setWindowAnchor(run.runId, 'a');
    runs.setWindowAnchor(run.runId, null);

    expect(runs.windowAnchor(run.runId)).toBeNull();
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('a jump from a stale run node degrades to tail-follow without reporting', async () => {
    // `revealActivityRunItem` is the one anchor writer that can legitimately
    // hold a node older than the registry: a jump is resolved when the reader
    // clicks, and an older-side prune can have run since the node was built.
    // Reporting that as a broken node kind would be a false positive on the
    // only path that produces it — so the jump asks about membership first.
    const runs = registry({ windowRows: 10 });
    const ids = rows(100) as string[];
    const [resolved] = pass(runs, [ids]);
    const stale = nodeFor(resolved, ids);

    // The older half is pruned; the node the jump site holds still names it.
    pass(runs, [ids.slice(50)]);

    // Target row 10 => anchor row 5 => 'i5', gone with the prune.
    expect(revealActivityRunItem(runs, stale, 'i10')).toBe(true);
    expect(runs.windowAnchor(resolved.runId)).toBeNull();
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('still reports a write of that same unresolvable anchor from anywhere else', async () => {
    // The contrast that makes the previous case a narrowing rather than a
    // hole: the id really is unresolvable, and a writer reading from a LIVE
    // node naming it is a broken contract, not a stale node.
    const runs = registry({ windowRows: 10 });
    const ids = rows(100) as string[];
    const [resolved] = pass(runs, [ids]);
    pass(runs, [ids.slice(50)]);

    runs.setWindowAnchor(resolved.runId, 'i5');

    expect(runs.windowAnchor(resolved.runId)).toBeNull();
    expect((await diagnostics.messages()).length).toBe(1);
  });
});

describe('viewport hold ownership', () => {
  // The hold moved INTO the mutators (incident 2026-08-17, chat/AGENTS.md
  // "Every collapse/expand"): a call site cannot forget it, and the one
  // hold-free verb cannot collapse. These pin which writes ride the
  // transaction and with which takeover, against a controller stub that
  // records the calls.
  function heldRegistry(overrides: { defaultCollapsed?: boolean } = {}) {
    const holds: { takeover: string; ranInside: boolean }[] = [];
    const controller: PaneScrollController = {
      pauseAutoScroll: () => () => {},
      autoScrollInFlight: () => false,
      observe: () => {},
      markStructuralContentPending: () => {},
      armWarmup: () => {},
      preserveScrollAnchor: async (_anchor, action) => {
        await action();
      },
      preserveViewportBottom: (change, opts) => {
        const record = { takeover: opts?.takeover ?? 'claim', ranInside: false };
        holds.push(record);
        change();
        record.ranInside = true;
      },
    };
    const runs = createThreadActivityRuns({
      defaultCollapsed: () => overrides.defaultCollapsed ?? false,
      windowRows: () => 30,
      scrollController: () => controller,
    });
    return { runs, holds };
  }

  it('setCollapsed writes inside a claim-takeover hold', () => {
    const { runs, holds } = heldRegistry();
    const [run] = pass(runs, [['a']]);
    const revisionBefore = runs.revision;

    runs.setCollapsed(run.runId, true);

    expect(holds).toEqual([{ takeover: 'claim', ranInside: true }]);
    expect(runs.revision, 'the write must land inside the hold').toBe(revisionBefore + 1);
  });

  it('setAllCollapsed writes inside a claim-takeover hold', () => {
    const { runs, holds } = heldRegistry();
    pass(runs, [['a']]);

    runs.setAllCollapsed(true);

    expect(holds).toEqual([{ takeover: 'claim', ranInside: true }]);
    expect(runs.bulkCollapsed).toBe(true);
  });

  it('releaseOpenedLive runs the whole batch inside ONE yield-takeover hold', () => {
    const { runs, holds } = heldRegistry({ defaultCollapsed: true });
    // Both runs held open: each streamed as the tail once, so each recorded
    // an open-because-live hold that outlives its liveness.
    pass(runs, [['a']], 'thread-1', 0);
    pass(runs, [['a'], ['b']], 'thread-1', 1);
    const [first, second] = pass(runs, [['a'], ['b']]);
    expect(runs.openedLiveRunIds().sort()).toEqual(
      [first.runId, second.runId].sort(),
    );

    runs.releaseOpenedLive([first.runId, second.runId]);

    expect(holds).toEqual([{ takeover: 'yield', ranInside: true }]);
    expect(runs.openedLiveRunIds()).toEqual([]);
  });

  it('releaseOpenedLive with nothing releasable opens no transaction', () => {
    // The transaction pauses the spring and burns a restore token — the
    // counter thread-switch restores guard on — so a batch that changes
    // nothing must not run one. The gate pre-filters too, but the property
    // belongs to the API: entries can be swept between capture and release.
    const { runs, holds } = heldRegistry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']]);
    expect(runs.openedLiveRunIds()).toEqual([]);

    runs.releaseOpenedLive([]);
    runs.releaseOpenedLive([run.runId, 'r99']);

    expect(holds).toEqual([]);
  });

  it('releaseOpenedLive with expanded defaults retires holds without a transaction', () => {
    // Nothing visible changes on this path — the run keeps rendering exactly
    // as it was — so there is no geometry for a viewport hold to guard.
    const { runs, holds } = heldRegistry({ defaultCollapsed: false });
    pass(runs, [['a']], 'thread-1', 0);
    const [run] = pass(runs, [['a']]);
    expect(runs.openedLiveRunIds()).toEqual([run.runId]);

    runs.releaseOpenedLive([run.runId]);

    expect(holds).toEqual([]);
    expect(runs.openedLiveRunIds()).toEqual([]);
    expect(pass(runs, [['a']])[0].collapsed).toBe(false);
  });

  it('expandForReveal writes with NO hold — the jump owns the viewport', () => {
    const { runs, holds } = heldRegistry({ defaultCollapsed: true });
    const [run] = pass(runs, [['a']]);
    expect(run.collapsed).toBe(true);

    runs.expandForReveal(run.runId);

    expect(holds).toEqual([]);
    expect(pass(runs, [['a']])[0].collapsed).toBe(false);
  });
});
