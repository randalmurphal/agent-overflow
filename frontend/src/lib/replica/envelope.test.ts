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
