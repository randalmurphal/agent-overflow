import { describe, expect, it } from 'vitest';
import {
  MAX_ENVELOPE_ITEMS,
  REPLICA_SCHEMA_VERSION,
  bodyFitsCaps,
  estimateBodyChars,
  metaMatches,
  normalizeBody,
  readEnvelope,
  wrapEnvelope,
  type ReplicaBody,
} from './envelope';
import type { Item } from '../types/models';

function item(overrides: Partial<Item> = {}): Item {
  return {
    rev: 0,
    id: 'i-1',
    threadId: 't-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: 'hello',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

function body(overrides: Partial<ReplicaBody> = {}): ReplicaBody {
  return {
    epoch: 2,
    rev: 7,
    savedAt: 1_000,
    items: [item()],
    oldestCursor: { turnIndex: 0, itemIndex: 0, itemId: 'i-1' },
    newestCursor: { turnIndex: 0, itemIndex: 0, itemId: 'i-1' },
    hasMoreOlder: true,
    hasMoreNewer: false,
    latestSettledTurn: null,
    subagentFolds: null,
    runs: [],
    ...overrides,
  };
}

describe('replica envelope', () => {
  it('round-trips a wrapped body through readEnvelope', () => {
    const read = readEnvelope(wrapEnvelope(normalizeBody(body())));
    expect(read).not.toBeNull();
    expect(read?.rev).toBe(7);
    expect(read?.epoch).toBe(2);
    expect(read?.items.map((it) => it.id)).toEqual(['i-1']);
    expect(read?.hasMoreOlder).toBe(true);
    expect(read?.hasMoreNewer).toBe(false);
  });

  it('drops a record whose envelope version this build does not write', () => {
    const envelope = wrapEnvelope(normalizeBody(body())) as { v: number };
    envelope.v = 99;
    expect(readEnvelope(envelope)).toBeNull();
  });

  it('drops a record whose cipher this build cannot read', () => {
    const envelope = wrapEnvelope(normalizeBody(body())) as unknown as { cipher: string };
    envelope.cipher = 'aes-gcm';
    expect(readEnvelope(envelope)).toBeNull();
  });

  it('drops a body with missing stamps or malformed rows', () => {
    expect(readEnvelope({ v: 1, cipher: 'none', body: { items: [] } })).toBeNull();
    expect(
      readEnvelope({
        v: 1,
        cipher: 'none',
        body: { epoch: 1, rev: 1, savedAt: 1, items: [{ id: 5 }] },
      }),
    ).toBeNull();
    expect(readEnvelope(null)).toBeNull();
  });

  it('round-trips the activity-run stubs the window needs', () => {
    // A window is only paintable with the stubs describing the run
    // members it does not hold (timeline-window-pages §6): without them
    // the restored pane would render every run as complete.
    const stub = {
      firstItemId: 'a',
      lastItemId: 'e',
      firstTurnIndex: 3,
      firstItemIndex: 1,
      lastTurnIndex: 3,
      lastItemIndex: 5,
      memberCount: 5,
      loadedFirstItemId: 'b',
      loadedLastItemId: 'd',
      unshippedBefore: 1,
      unshippedAfter: 1,
      unshippedDigest: '0123456789abcdef',
      unshippedGroups: [{ kind: 'tool_call', toolName: 'Bash', mcp: '', rows: 2 }],
      unshippedPairedLaunchIds: ['a'],
      shippedSupersededLaunchIds: [],
      unshippedFailed: true,
      runningBefore: { kind: 'tool_call', toolName: 'Task', mcp: '' },
      runningAfter: null,
    } as unknown as ReplicaBody['runs'][number];
    const read = readEnvelope(wrapEnvelope(normalizeBody(body({ runs: [stub] }))));
    expect(read?.runs).toEqual([stub]);
  });

  it('drops a body written before the runs field existed', () => {
    // Never migrate, always drop: a stored window with no stubs was
    // written by a build that could not describe its runs.
    const legacy = normalizeBody(body()) as unknown as { runs?: unknown };
    delete legacy.runs;
    expect(readEnvelope({ v: 1, cipher: 'none', body: legacy })).toBeNull();
  });

  it('drops a body with a malformed stub', () => {
    const broken = normalizeBody(body()) as unknown as { runs: unknown[] };
    broken.runs = [{ firstItemId: 'a' }];
    expect(readEnvelope({ v: 1, cipher: 'none', body: broken })).toBeNull();
  });

  it('drops a body whose rows predate the item rev', () => {
    const stale = normalizeBody(body()) as unknown as { items: Array<Partial<Item>> };
    for (const row of stale.items) delete row.rev;
    expect(readEnvelope({ v: 1, cipher: 'none', body: stale })).toBeNull();
  });

  it('normalizes a reactive proxy into structured-clone-safe plain data', () => {
    const proxied = new Proxy([item()], {});
    const normalized = normalizeBody(body({ items: proxied }));
    // The pane hands `items` over through a Svelte $state proxy, and
    // structuredClone throws DataCloneError on one — the write path must
    // never see it.
    expect(() => structuredClone(normalized)).not.toThrow();
    expect(normalized.items).not.toBe(proxied);
  });

  it('counts the fold against the same char budget as the rows', () => {
    const withFold = body({
      subagentFolds: {
        anchors: [
          {
            anchorId: 'anchor',
            evictedIds: ['abc', 'def'],
            terminalPreview: '1234567890',
            terminalTurnIndex: 0,
            terminalItemIndex: 1,
          },
        ],
      },
    });
    expect(estimateBodyChars(withFold)).toBe(
      'hello'.length + 'abc'.length + 'def'.length + '1234567890'.length,
    );
  });

  it('refuses a window past the per-envelope item cap', () => {
    const many = Array.from({ length: MAX_ENVELOPE_ITEMS + 1 }, (_, index) =>
      item({ id: `i-${index}`, itemIndex: index }),
    );
    expect(bodyFitsCaps(body({ items: many }))).toBe(false);
    expect(bodyFitsCaps(body())).toBe(true);
  });

  it('accepts a meta record only at the current schema version and generation', () => {
    expect(metaMatches({ generation: 'g1', schemaVersion: REPLICA_SCHEMA_VERSION }, 'g1')).toBe(true);
    expect(metaMatches({ generation: 'g2', schemaVersion: REPLICA_SCHEMA_VERSION }, 'g1')).toBe(false);
    expect(metaMatches({ generation: 'g1', schemaVersion: REPLICA_SCHEMA_VERSION + 1 }, 'g1')).toBe(false);
    expect(metaMatches(undefined, 'g1')).toBe(false);
  });
});

it('includes attached launch context in replica accounting and snapshots', () => {
  const launch = new Proxy(item({ id: 'launch', summary: 'context' }), {});
  const plain = body({ items: [item({ id: 'done' })] });
  const attached = body({ items: [item({ id: 'done', completionLaunch: launch })] });
  expect(estimateBodyChars(attached) - estimateBodyChars(plain)).toBe('context'.length);
  const snapshot = normalizeBody(attached);
  launch.summary = 'changed';
  expect(snapshot.items[0].completionLaunch?.summary).toBe('context');
  expect(structuredClone(snapshot).items[0].completionLaunch?.id).toBe('launch');
});
