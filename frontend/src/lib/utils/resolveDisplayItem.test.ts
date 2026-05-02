import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import { resolveDisplayItem } from './resolveDisplayItem';

function makeItem(overrides: Partial<Item> = {}): Item {
  return {
    id: 'item-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: 'persisted',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

describe('resolveDisplayItem', () => {
  it('returns the original Item reference when no live summary is buffered', () => {
    const item = makeItem();
    expect(resolveDisplayItem(item, undefined)).toBe(item);
  });

  it('returns the original Item reference when the buffered summary equals the persisted one', () => {
    // The hot path during steady-state streaming: deltas have already
    // been concatenated into liveItemSummaries[id] and the next
    // upsert re-syncs the persisted value. Both strings are equal —
    // no need to spread a fresh object that would only churn prop
    // identity for downstream renderers.
    const item = makeItem({ summary: 'streamed text' });
    expect(resolveDisplayItem(item, 'streamed text')).toBe(item);
  });

  it('returns a spread copy with the buffered summary when the two differ', () => {
    const item = makeItem({ summary: 'persisted' });
    const result = resolveDisplayItem(item, 'live in progress');
    expect(result).not.toBe(item);
    expect(result.summary).toBe('live in progress');
    expect(result.id).toBe(item.id);
  });

  it('treats an empty string buffered summary as a real value (not equivalent to undefined)', () => {
    // An empty live summary is a legitimate buffered state right
    // after `applyLiveStateForUpsert` seeds an empty entry but
    // before any deltas arrive. If the persisted summary is also
    // empty the short-circuit returns the original; if not, we copy.
    const empty = makeItem({ summary: '' });
    expect(resolveDisplayItem(empty, '')).toBe(empty);

    const withText = makeItem({ summary: 'something' });
    const result = resolveDisplayItem(withText, '');
    expect(result).not.toBe(withText);
    expect(result.summary).toBe('');
  });
});
