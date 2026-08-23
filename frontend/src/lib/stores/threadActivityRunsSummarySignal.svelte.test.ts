// The two O(1) signals `ActivityRunHeader` keys its summary on.
//
// The header used to rebuild a (id, kind, status, toolName, completionOf)
// tuple for every summary dependency on every reveal tick — an O(members)
// walk and string build at ~50Hz, on the longest runs in the thread. It now
// reads two counters instead:
//
//   membershipEpoch        stamped when identity or summary dependencies
//                          change, carried out on the node
//   memberContentRevision  bumped by the pane's row-write chokepoints when
//                          a dependency's summary tuple actually moves
//
// Between them they have to cover everything the tuple covered, which means
// each needs transition coverage in BOTH directions: it must move when the
// summary would differ, and hold when it would not. A signal that only ever
// moved would be correct and useless; one that only ever held would be fast
// and wrong.

import { describe, expect, it } from 'vitest';
import { pass, registry } from '../../test/helpers/activityRuns';

describe('membershipEpoch', () => {
  it('is stamped on first resolve and holds across an identical pass', () => {
    const runs = registry();
    const first = pass(runs, [['a', 'b']])[0];
    expect(first.membershipEpoch).toBeGreaterThan(0);

    expect(pass(runs, [['a', 'b']])[0].membershipEpoch).toBe(first.membershipEpoch);
    expect(pass(runs, [['a', 'b']])[0].membershipEpoch).toBe(first.membershipEpoch);
  });

  it('moves when a row joins and when a row leaves', () => {
    const runs = registry();
    const start = pass(runs, [['a', 'b']])[0].membershipEpoch;

    const grown = pass(runs, [['a', 'b', 'c']])[0].membershipEpoch;
    expect(grown).toBeGreaterThan(start);

    const trimmed = pass(runs, [['b', 'c']])[0].membershipEpoch;
    expect(trimmed).toBeGreaterThan(grown);
  });

  it('moves when a member is REPLACED at the same count', () => {
    // The count-only stamp this replaced could not see a swap. It happens:
    // a row's id changes when an optimistic row is laundered, and the run
    // keeps its identity because the surrounding members still match.
    const runs = registry();
    const before = pass(runs, [['a', 'b', 'c']])[0];

    const after = pass(runs, [['a', 'x', 'c']])[0];
    expect(after.runId).toBe(before.runId);
    expect(after.membershipEpoch).toBeGreaterThan(before.membershipEpoch);
  });

  it('moves when the SAME members arrive in a different order', () => {
    // Membership is ordered, not a set: the summary's running label is the
    // last active member in iteration order, so a reorder is a different
    // summary even though nothing joined or left. A set-equality test read
    // this as unchanged and left the header naming a tool that had already
    // stopped being the one in flight.
    const runs = registry();
    const before = pass(runs, [['a', 'b', 'c']])[0];

    const after = pass(runs, [['a', 'c', 'b']])[0];
    expect(after.runId).toBe(before.runId);
    expect(after.membershipEpoch).toBeGreaterThan(before.membershipEpoch);
  });

  it('holds when the same members arrive in the same order across rows', () => {
    // The other direction, and the one that keeps the signal useful: a run
    // whose rows are re-grouped without moving any member (a group row
    // splitting into two single-item rows carries the same ids in the same
    // order) must not re-summarize every header on screen.
    const runs = registry();
    const before = pass(runs, [[['a', 'b'], 'c']])[0];

    const after = pass(runs, [['a', 'b', 'c']])[0];
    expect(after.runId).toBe(before.runId);
    expect(after.membershipEpoch).toBe(before.membershipEpoch);
  });

  it('is per run, not global', () => {
    const runs = registry();
    const [left, right] = pass(runs, [['a'], ['b']]);
    expect(left.runId).not.toBe(right.runId);

    const [leftAfter, rightAfter] = pass(runs, [['a', 'a2'], ['b']]);
    expect(leftAfter.membershipEpoch).toBeGreaterThan(left.membershipEpoch);
    expect(rightAfter.membershipEpoch).toBe(right.membershipEpoch);
  });

  it('moves when a detached launch gains its completion dependency', () => {
    const runs = registry();
    runs.beginPass();
    const before = runs.resolve([['launch']], 'thread-1', ['launch']);
    runs.endPass();

    runs.beginPass();
    const after = runs.resolve(
      [['launch']],
      'thread-1',
      ['launch', 'completion'],
    );
    runs.endPass();

    expect(after.runId).toBe(before.runId);
    expect(after.membershipEpoch).toBeGreaterThan(before.membershipEpoch);
  });
});

describe('memberContentRevision', () => {
  it('starts at zero and moves when a member is reported changed', () => {
    const runs = registry();
    const [run] = pass(runs, [['a', 'b']]);
    expect(runs.memberContentRevision(run.runId)).toBe(0);

    runs.noteMemberContentChanged('b');
    expect(runs.memberContentRevision(run.runId)).toBe(1);
    runs.noteMemberContentChanged('b');
    expect(runs.memberContentRevision(run.runId)).toBe(2);
  });

  it('only moves the run that summarizes the item', () => {
    const runs = registry();
    const [left, right] = pass(runs, [['a'], ['b']]);

    runs.noteMemberContentChanged('a');

    expect(runs.memberContentRevision(left.runId)).toBe(1);
    expect(runs.memberContentRevision(right.runId)).toBe(0);
  });

  it('moves every header that summarizes a shared completion', () => {
    const runs = registry();
    runs.beginPass();
    const launch = runs.resolve(
      [['launch']],
      'thread-1',
      ['launch', 'completion'],
    );
    const card = runs.resolve(
      [['completion']],
      'thread-1',
      ['completion'],
    );
    runs.endPass();

    runs.noteMemberContentChanged('completion');

    expect(runs.memberContentRevision(launch.runId)).toBe(1);
    expect(runs.memberContentRevision(card.runId)).toBe(1);
  });

  it('ignores an id no run summarizes', () => {
    const runs = registry();
    const [run] = pass(runs, [['a']]);

    runs.noteMemberContentChanged('not-a-member');

    expect(runs.memberContentRevision(run.runId)).toBe(0);
  });

  it('survives passes that keep the run and resets with the run itself', () => {
    // Transition coverage across the entry lifecycle: the revision means
    // "something changed since you last rendered", so it must NOT reset
    // under a live run, and must not outlive one either.
    const runs = registry();
    const [run] = pass(runs, [['a', 'b']]);
    runs.noteMemberContentChanged('a');
    expect(runs.memberContentRevision(run.runId)).toBe(1);

    pass(runs, [['a', 'b']]);
    expect(runs.memberContentRevision(run.runId)).toBe(1);

    // A pass that no longer contains the run sweeps its entry; its member
    // is no longer indexed, so nothing can bump the swept id again.
    pass(runs, [['z']]);
    expect(runs.memberContentRevision(run.runId)).toBe(0);
    runs.noteMemberContentChanged('a');
    expect(runs.memberContentRevision(run.runId)).toBe(0);
  });

  it('drops with the thread on clear()', () => {
    const runs = registry();
    const [run] = pass(runs, [['a']]);
    runs.noteMemberContentChanged('a');
    expect(runs.memberContentRevision(run.runId)).toBe(1);

    runs.clear();

    expect(runs.memberContentRevision(run.runId)).toBe(0);
  });
});

// The third signal, and the one neither of the others can stand in for: a
// WHOLESALE item replacement (paged load, cache paint reconciled by
// SyncThreadWindow, window prune) rewrites rows without going through the
// per-item write path, and it can leave run membership identical while
// every summary-relevant field on those rows moved.
describe('wholesaleGeneration', () => {
  it('starts at zero and holds across passes that only re-resolve runs', () => {
    const runs = registry();
    expect(runs.wholesaleGeneration).toBe(0);

    pass(runs, [['a', 'b']]);
    pass(runs, [['a', 'b', 'c']]);
    runs.noteMemberContentChanged('a');

    expect(runs.wholesaleGeneration).toBe(0);
  });

  it('moves on a replacement whose membership is unchanged', () => {
    // The case the per-run signals miss entirely: same ids, same order, and
    // no `noteMemberContentChanged` — but a tool the cache painted as
    // running came back from the attested window as completed.
    const runs = registry();
    const before = pass(runs, [['a', 'b']])[0];
    const beforeGeneration = runs.wholesaleGeneration;

    runs.noteWholesaleReplace();

    const after = pass(runs, [['a', 'b']])[0];
    expect(after.runId).toBe(before.runId);
    expect(after.membershipEpoch).toBe(before.membershipEpoch);
    expect(runs.memberContentRevision(after.runId)).toBe(0);
    expect(runs.wholesaleGeneration).toBeGreaterThan(beforeGeneration);
  });
});
