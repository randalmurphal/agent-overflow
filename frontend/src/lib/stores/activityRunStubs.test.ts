// The client run record (docs/architecture/timeline-window-pages.md §6):
// what a pane knows about a run it holds only part of, and the arithmetic
// it does over the three descriptions it holds — loaded rows, shed copies,
// and the server's stub for the rest.
import { describe, expect, it } from 'vitest';
import {
  applyMembersStub,
  foldPageStub,
  foldedStub,
  heldRunsFold,
  mergeRunStubs,
  noteSpanMoved,
  physicalCount,
  shedOlderMembers,
  shedRowOf,
  stubFacts,
  windowDigestContribution,
  type ActivityRunRecords,
} from './activityRunStubs';
import { windowDigest } from './threadWindowDigest';
import { formatFnv1a64, parseFnv1a64, xorFnv1a64 } from '../utils/fnv1a';
import { groupActivityRunSpans } from '../utils/activityRunSpans';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';

function row(id: string, index: number, overrides: Partial<Item> = {}): Item {
  return makeItem({
    id,
    turnIndex: 0,
    itemIndex: index,
    kind: 'tool_call',
    toolName: 'Bash',
    rev: index + 1,
    ...overrides,
  });
}

function span(items: Item[]) {
  return groupActivityRunSpans(items)[0];
}

function stub(overrides: Partial<ActivityRunStub> = {}): ActivityRunStub {
  return {
    firstItemId: 'a',
    lastItemId: 'e',
    firstTurnIndex: 0,
    firstItemIndex: 1,
    lastTurnIndex: 0,
    lastItemIndex: 5,
    memberCount: 5,
    loadedFirstItemId: 'b',
    loadedLastItemId: 'd',
    unshippedBefore: 1,
    unshippedAfter: 1,
    unshippedDigest: windowDigest([{ id: 'a', rev: 1 }, { id: 'e', rev: 5 }]),
    unshippedGroups: [{ kind: 'tool_call', toolName: 'Bash', mcp: '', rows: 2 }],
    unshippedPairedLaunchIds: [],
    shippedSupersededLaunchIds: [],
    unshippedFailed: false,
    runningBefore: null,
    runningAfter: null,
    ...overrides,
  } as ActivityRunStub;
}

const loaded = [row('b', 1), row('c', 2), row('d', 3)];

describe('foldPageStub', () => {
  it('records a stub that describes the span the pane holds', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub(), span(loaded));
    expect(record).toMatchObject({
      runFirstItemId: 'a',
      loadedFirstItemId: 'b',
      loadedLastItemId: 'd',
      dirty: false,
    });
    expect(records.get('a')).toBe(record);
  });

  it('keeps the pane\'s span and goes dirty when the stub disagrees', () => {
    // A cursor page that crossed into a held run ships a different slice
    // of it. The pane's rows are the fact; the stub is a claim about a
    // window that no longer exists.
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub({ loadedLastItemId: 'c' }), span(loaded));
    expect(record.loadedLastItemId).toBe('d');
    expect(record.dirty).toBe(true);
  });

  it('clears the shed list only when the stub describes the pane', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub({ loadedFirstItemId: 'a' }), span([row('a', 0), ...loaded]));
    shedOlderMembers(record, [row('a', 0)], 'b', 'd');
    expect(record.shed).toHaveLength(1);

    foldPageStub(records, stub({ loadedLastItemId: 'c' }), span(loaded));
    expect(records.get('a')!.shed).toHaveLength(1);

    foldPageStub(records, stub(), span(loaded));
    expect(records.get('a')!.shed).toHaveLength(0);
    expect(records.get('a')!.dirty).toBe(false);
  });

  it('records a run the pane holds none of', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(
      records,
      stub({ loadedFirstItemId: '', loadedLastItemId: '', unshippedBefore: 5, unshippedAfter: 0 }),
      null,
    );
    expect(record).toMatchObject({ loadedFirstItemId: '', loadedLastItemId: '', dirty: false });
    expect(physicalCount(record)).toBe(5);
  });
});

describe('applyMembersStub', () => {
  it('supersedes everything the record carried', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub({ loadedFirstItemId: 'a' }), span([row('a', 0), ...loaded]));
    shedOlderMembers(record, [row('a', 0)], 'b', 'd');
    record.dirty = true;

    const applied = applyMembersStub(records, stub({ loadedFirstItemId: 'a', loadedLastItemId: 'e', unshippedBefore: 0, unshippedAfter: 0 }));
    expect(applied).toBe(record);
    expect(applied).toMatchObject({
      loadedFirstItemId: 'a',
      loadedLastItemId: 'e',
      dirty: false,
    });
    expect(applied.shed).toEqual([]);
  });

  it('creates a record for a run the pane had none for', () => {
    const records: ActivityRunRecords = new Map();
    applyMembersStub(records, stub());
    expect(records.get('a')?.loadedFirstItemId).toBe('b');
  });
});

describe('noteSpanMoved', () => {
  it('re-points the record and marks it dirty', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub(), span(loaded));
    expect(noteSpanMoved(record, span([...loaded, row('e', 4)]))).toBe(true);
    expect(record).toMatchObject({ loadedLastItemId: 'e', dirty: true });
  });

  it('is a no-op when the span did not move', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub(), span(loaded));
    expect(noteSpanMoved(record, span(loaded))).toBe(false);
    expect(record.dirty).toBe(false);
  });
});

describe('shedOlderMembers', () => {
  it('appends narrow copies and moves the span\'s older edge', () => {
    const records: ActivityRunRecords = new Map();
    const withA = [row('a', 0, { status: 'errored' }), ...loaded];
    const record = foldPageStub(records, stub({ loadedFirstItemId: 'a', unshippedBefore: 0 }), span(withA));
    shedOlderMembers(record, [withA[0]], 'b', 'd');
    expect(record.loadedFirstItemId).toBe('b');
    expect(record.shed).toEqual([
      { id: 'a', rev: 1, kind: 'tool_call', toolName: 'Bash', status: 'errored', completionOf: '', mcp: '', fileRows: 1 },
    ]);
    // The record keeps counting them: N earlier is server + shed.
    expect(stubFacts(record).unshippedBefore).toBe(1);
  });

  it('resolves the display-row count at shed time', () => {
    // `fileChangeDisplayRowCount` reads kilobytes of diff JSON, which the
    // record deliberately does not retain.
    const item = row('a', 0, {
      toolName: 'Edit',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({ inlineDiff: { totalFiles: 4 } }),
    });
    expect(shedRowOf(item).fileRows).toBe(4);
  });

  it('refuses a shed that is not contiguous with the surviving span', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub(), span(loaded));
    expect(() => shedOlderMembers(record, [loaded[1]], 'c', 'd')).toThrow(/shed rows start at c/);
  });

  it('refuses a shed that moved the span\'s newer edge', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub(), span(loaded));
    expect(() => shedOlderMembers(record, [loaded[0]], 'c', 'c')).toThrow(/newest member moved/);
  });

  it('refuses shedding every loaded member', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub(), span(loaded));
    expect(() => shedOlderMembers(record, [...loaded], '', 'd')).toThrow(/drop the record instead/);
  });
});

describe('windowDigestContribution', () => {
  it('folds the shed rows into the stub\'s unshipped digest', () => {
    const records: ActivityRunRecords = new Map();
    const withA = [row('a', 0), ...loaded];
    const record = foldPageStub(
      records,
      stub({
        loadedFirstItemId: 'a',
        unshippedBefore: 0,
        unshippedDigest: windowDigest([{ id: 'e', rev: 5 }]),
      }),
      span(withA),
    );
    shedOlderMembers(record, [withA[0]], 'b', 'd');
    expect(formatFnv1a64(windowDigestContribution(record)!)).toBe(
      windowDigest([{ id: 'e', rev: 5 }, { id: 'a', rev: 1 }]),
    );
    expect(physicalCount(record)).toBe(2);
  });

  it('states nothing for a dirty record or an unparsable digest', () => {
    const records: ActivityRunRecords = new Map();
    const dirty = foldPageStub(records, stub({ loadedLastItemId: 'c' }), span(loaded));
    expect(windowDigestContribution(dirty)).toBeNull();

    const bad = applyMembersStub(new Map(), stub({ unshippedDigest: 'not-a-digest' }));
    expect(windowDigestContribution(bad)).toBeNull();
  });

  it('states nothing when a shed row carries no revision', () => {
    const records: ActivityRunRecords = new Map();
    const withA = [row('a', 0, { rev: -1 }), ...loaded];
    const record = foldPageStub(records, stub({ loadedFirstItemId: 'a' }), span(withA));
    shedOlderMembers(record, [withA[0]], 'b', 'd');
    expect(windowDigestContribution(record)).toBeNull();
  });
});

describe('heldRunsFold', () => {
  it('composes every record into one count and digest', () => {
    const records: ActivityRunRecords = new Map();
    foldPageStub(records, stub({ unshippedDigest: windowDigest([{ id: 'a', rev: 1 }]), unshippedBefore: 1, unshippedAfter: 0 }), span(loaded));
    foldPageStub(
      records,
      stub({
        firstItemId: 'x',
        lastItemId: 'z',
        loadedFirstItemId: 'y',
        loadedLastItemId: 'y',
        unshippedBefore: 1,
        unshippedAfter: 1,
        unshippedDigest: windowDigest([{ id: 'x', rev: 9 }, { id: 'z', rev: 9 }]),
      }),
      span([row('y', 8)]),
    );
    const fold = heldRunsFold(records)!;
    expect(fold.count).toBe(3);
    expect(formatFnv1a64(fold.digest)).toBe(
      windowDigest([{ id: 'a', rev: 1 }, { id: 'x', rev: 9 }, { id: 'z', rev: 9 }]),
    );
  });

  it('states nothing when any record cannot state its own', () => {
    const records: ActivityRunRecords = new Map();
    foldPageStub(records, stub({ loadedLastItemId: 'c' }), span(loaded));
    expect(heldRunsFold(records)).toBeNull();
  });

  it('is zero for no records at all', () => {
    expect(heldRunsFold(new Map())).toEqual({ count: 0, digest: { hi: 0, lo: 0 } });
  });
});

describe('foldedStub', () => {
  it('returns the stub unchanged when nothing was shed', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub(), span(loaded));
    expect(foldedStub(record, loaded)).toBe(record.stub);
  });

  it('folds shed rows into the counts, the digest and the groups', () => {
    const records: ActivityRunRecords = new Map();
    const withA = [row('a', 0, { toolName: 'Read' }), ...loaded];
    const record = foldPageStub(
      records,
      stub({
        loadedFirstItemId: 'a',
        unshippedBefore: 0,
        unshippedDigest: windowDigest([{ id: 'e', rev: 5 }]),
        unshippedGroups: [{ kind: 'tool_call', toolName: 'Bash', mcp: '', rows: 1 }],
      }),
      span(withA),
    );
    shedOlderMembers(record, [withA[0]], 'b', 'd');

    const folded = foldedStub(record, loaded)!;
    expect(folded.unshippedBefore).toBe(1);
    expect(folded.loadedFirstItemId).toBe('b');
    expect(folded.unshippedDigest).toBe(windowDigest([{ id: 'e', rev: 5 }, { id: 'a', rev: 1 }]));
    expect(folded.unshippedGroups).toEqual([
      { kind: 'tool_call', toolName: 'Bash', mcp: '', rows: 1 },
      { kind: 'tool_call', toolName: 'Read', mcp: '', rows: 1 },
    ]);
  });

  it('pairs a shed launch with a completion the window keeps', () => {
    const records: ActivityRunRecords = new Map();
    const launch = row('a', 0);
    const completion = row('b', 1, { kind: 'tool_completion', completionOf: 'a' });
    const rows = [launch, completion, row('c', 2)];
    const record = foldPageStub(
      records,
      stub({ loadedFirstItemId: 'a', loadedLastItemId: 'c', unshippedBefore: 0, unshippedAfter: 0, unshippedDigest: windowDigest([]), unshippedGroups: [] }),
      span(rows),
    );
    shedOlderMembers(record, [launch], 'b', 'c');
    const folded = foldedStub(record, rows.slice(1))!;
    expect(folded.unshippedPairedLaunchIds).toEqual(['a']);
  });

  it('shedding a launch whose completion is unshipped leaves it in neither list', () => {
    const records: ActivityRunRecords = new Map();
    const launch = row('a', 0, { status: 'running' });
    const rows = [launch, row('b', 1)];
    // The stub ships a,b and holds the completion of `a` unshipped after them.
    const record = foldPageStub(
      records,
      stub({
        lastItemId: 'c',
        memberCount: 3,
        loadedFirstItemId: 'a',
        loadedLastItemId: 'b',
        unshippedBefore: 0,
        unshippedAfter: 1,
        unshippedDigest: windowDigest([{ id: 'c', rev: 3 }]),
        unshippedGroups: [],
        shippedSupersededLaunchIds: ['a'],
      }),
      span(rows),
    );
    expect(stubFacts(record).shippedSupersededLaunchIds).toEqual(['a']);
    shedOlderMembers(record, [launch], 'b', 'b');
    const folded = foldedStub(record, rows.slice(1))!;
    expect(folded.shippedSupersededLaunchIds).toEqual([]);
    expect(folded.unshippedPairedLaunchIds).toEqual([]);
    // Superseded by its unshipped completion, so the shed launch is not the
    // running edge and its completion still counts zero.
    expect(folded.runningBefore).toBeNull();
    expect(folded.unshippedGroups).toEqual([{ kind: 'tool_call', toolName: 'Bash', mcp: '', rows: 1 }]);
  });

  it('drops a shed completion\'s launch from the pairing list and counts it zero', () => {
    const records: ActivityRunRecords = new Map();
    const launch = row('a', 0);
    const completion = row('b', 1, { kind: 'tool_completion', completionOf: 'a' });
    const rows = [launch, completion, row('c', 2)];
    const record = foldPageStub(
      records,
      stub({
        loadedFirstItemId: 'b',
        loadedLastItemId: 'c',
        unshippedBefore: 1,
        unshippedAfter: 0,
        unshippedDigest: windowDigest([{ id: 'a', rev: 1 }]),
        unshippedGroups: [{ kind: 'tool_call', toolName: 'Bash', mcp: '', rows: 1 }],
        unshippedPairedLaunchIds: ['a'],
      }),
      span(rows.slice(1)),
    );
    shedOlderMembers(record, [completion], 'c', 'c');
    const folded = foldedStub(record, [rows[2]])!;
    expect(folded.unshippedPairedLaunchIds).toEqual([]);
    // The completion pairs with a member, so it adds no rows.
    expect(folded.unshippedGroups).toEqual([{ kind: 'tool_call', toolName: 'Bash', mcp: '', rows: 1 }]);
    expect(folded.unshippedBefore).toBe(2);
  });

  it('keeps a shed parked agent running and paired while a parked stop of it is kept', () => {
    // a: a background agent's launch; b, d: its parked stops. Shedding a
    // and b leaves d holding the pairing, and no ending stop settles a.
    const records: ActivityRunRecords = new Map();
    const launch = row('a', 0, { toolName: 'Agent', status: 'running', isBackground: true });
    const parked = (id: string, index: number) =>
      row(id, index, { kind: 'tool_completion', toolName: 'Agent', completionOf: 'a', status: 'parked' });
    const rows = [launch, parked('b', 1), row('c', 2), parked('d', 3)];
    const record = foldPageStub(
      records,
      stub({
        lastItemId: 'd',
        memberCount: 4,
        loadedFirstItemId: 'a',
        loadedLastItemId: 'd',
        unshippedBefore: 0,
        unshippedAfter: 0,
        unshippedDigest: windowDigest([]),
        unshippedGroups: [],
      }),
      span(rows),
    );
    shedOlderMembers(record, rows.slice(0, 2), 'c', 'd');
    const folded = foldedStub(record, rows.slice(2))!;
    expect(folded.unshippedPairedLaunchIds).toEqual(['a']);
    expect(folded.shippedSupersededLaunchIds).toEqual([]);
    expect(folded.runningBefore).toEqual({ kind: 'tool_call', toolName: 'Agent', mcp: '' });
    // The launch counts once; the shed parked stop pairs and counts zero.
    expect(folded.unshippedGroups).toEqual([{ kind: 'tool_call', toolName: 'Agent', mcp: '', rows: 1 }]);
  });

  it('states nothing for a dirty record', () => {
    const records: ActivityRunRecords = new Map();
    const record = foldPageStub(records, stub({ loadedLastItemId: 'c' }), span(loaded));
    expect(foldedStub(record, loaded)).toBeNull();
  });

  it('keeps a restored window\'s digest composable', () => {
    // The whole point: the pane that restores this stub composes it back
    // into the same held window the pane that saved it described.
    const records: ActivityRunRecords = new Map();
    const withA = [row('a', 0), ...loaded];
    const record = foldPageStub(
      records,
      stub({ loadedFirstItemId: 'a', unshippedBefore: 0, unshippedDigest: windowDigest([{ id: 'e', rev: 5 }]) }),
      span(withA),
    );
    shedOlderMembers(record, [withA[0]], 'b', 'd');
    const before = windowDigestContribution(record)!;

    const restored = applyMembersStub(new Map(), foldedStub(record, loaded)!);
    expect(formatFnv1a64(windowDigestContribution(restored)!)).toBe(formatFnv1a64(before));
    expect(physicalCount(restored)).toBe(physicalCount(record));
  });
});

describe('mergeRunStubs', () => {
  it('keys by firstItemId and lets the later page win', () => {
    const earlier = stub({ loadedLastItemId: 'c' });
    const later = stub({ loadedLastItemId: 'd' });
    expect(mergeRunStubs([earlier], [later])).toEqual([later]);
    expect(mergeRunStubs([earlier], [stub({ firstItemId: 'x' })])).toHaveLength(2);
    expect(mergeRunStubs(null, undefined)).toEqual([]);
  });
});

describe('the digest a pane sends', () => {
  it('is the XOR of every part, so composition order does not matter', () => {
    const rows = windowDigest([{ id: 'b', rev: 2 }, { id: 'c', rev: 3 }]);
    const runs = windowDigest([{ id: 'a', rev: 1 }]);
    expect(formatFnv1a64(xorFnv1a64(parseFnv1a64(rows)!, parseFnv1a64(runs)!))).toBe(
      windowDigest([{ id: 'a', rev: 1 }, { id: 'b', rev: 2 }, { id: 'c', rev: 3 }]),
    );
  });
});
