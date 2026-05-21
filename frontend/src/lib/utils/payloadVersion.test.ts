import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import {
  boundedPayloadVersionString,
  payloadVersionForItem,
  thinkingPayloadVersionForItem,
} from './payloadVersion';

describe('payloadVersion', () => {
  it('derives payload versions from stable payload signatures before timestamps', () => {
    expect(payloadVersionForItem(makeItem({
      id: 'payload',
      payloadId: 'payload-id',
      payloadMeta: JSON.stringify({ signature: 'sha:abc' }),
      updatedAt: 123,
    }))).toBe('sha:abc');
    expect(payloadVersionForItem(makeItem({
      id: 'payload-id-only',
      payloadId: 'payload-id',
      updatedAt: 456,
    }))).toBe('payload-id');
    expect(payloadVersionForItem(makeItem({
      id: 'timestamp-fallback',
      updatedAt: 789,
    }))).toBe(789);
  });

  it('bounds provider-controlled metadata versions', () => {
    const largeMeta = JSON.stringify({
      preview: 'x'.repeat(10_000),
      command: 'pnpm test',
    });

    const version = payloadVersionForItem(makeItem({
      id: 'metadata-only',
      payloadId: undefined,
      inputPayloadId: undefined,
      payloadMeta: largeMeta,
      updatedAt: 123,
    }));

    expect(typeof version).toBe('string');
    expect((version as string).length).toBeLessThan(180);
    expect(version).not.toBe(largeMeta);
  });

  it('keeps bounded strings stable and distinguishes same-length edge collisions', () => {
    const first = `${'a'.repeat(100)}middle-a${'z'.repeat(100)}`;
    const second = `${'a'.repeat(100)}middle-b${'z'.repeat(100)}`;

    expect(boundedPayloadVersionString(first)).toBe(boundedPayloadVersionString(first));
    expect(boundedPayloadVersionString(first)).not.toBe(boundedPayloadVersionString(second));
  });

  it('keeps streaming thinking versions stable across deltas', () => {
    const first = thinkingPayloadVersionForItem(makeItem({
      kind: 'thinking',
      payloadId: 'thinking-payload',
      status: 'streaming',
      updatedAt: 1,
    }));
    const second = thinkingPayloadVersionForItem(makeItem({
      kind: 'thinking',
      payloadId: 'thinking-payload',
      status: 'streaming',
      updatedAt: 2,
    }));

    expect(second).toBe(first);
  });

  it('changes settled thinking versions when the payload timestamp changes', () => {
    const first = thinkingPayloadVersionForItem(makeItem({
      kind: 'thinking',
      payloadId: 'thinking-payload',
      status: 'completed',
      updatedAt: 1,
    }));
    const second = thinkingPayloadVersionForItem(makeItem({
      kind: 'thinking',
      payloadId: 'thinking-payload',
      status: 'completed',
      updatedAt: 2,
    }));

    expect(second).not.toBe(first);
  });
});
