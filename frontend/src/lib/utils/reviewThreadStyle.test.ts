import { describe, expect, it } from 'vitest';
import type { ReviewThread } from '../types/models';
import { reviewThreadState, threadBodyClass, type ReviewThreadState } from './reviewThreadStyle';

function thread(overrides: Partial<ReviewThread> = {}): ReviewThread {
  return {
    id: 't1',
    path: 'src/a.ts',
    line: 3,
    side: 'RIGHT',
    isResolvable: true,
    isResolved: false,
    isOutdated: false,
    comments: [],
    ...overrides,
  };
}

describe('reviewThreadState', () => {
  it('reads unresolved, resolved and outdated from the thread', () => {
    expect(reviewThreadState(thread())).toBe('unresolved');
    expect(reviewThreadState(thread({ isResolved: true }))).toBe('resolved');
    expect(reviewThreadState(thread({ isOutdated: true }))).toBe('outdated');
  });

  it('reads outdated over resolved: the edge says the line moved', () => {
    expect(reviewThreadState(thread({ isResolved: true, isOutdated: true }))).toBe('outdated');
  });

  it('reads an orphaned thread (line gone from the diff) as outdated', () => {
    expect(reviewThreadState(thread(), true)).toBe('outdated');
    expect(reviewThreadState(thread({ isResolved: true }), true)).toBe('outdated');
  });

  it('gives a non-resolvable thread no state, whatever its flags say', () => {
    expect(reviewThreadState(thread({ isResolvable: false }))).toBe('none');
    expect(reviewThreadState(thread({ isResolvable: false, isResolved: true, isOutdated: true }), true)).toBe('none');
  });
});

describe('threadBodyClass', () => {
  it('dims only settled threads', () => {
    const expected: Record<ReviewThreadState, boolean> = {
      unresolved: false,
      resolved: true,
      outdated: true,
      none: false,
    };
    for (const [state, dimmed] of Object.entries(expected) as [ReviewThreadState, boolean][]) {
      expect(threadBodyClass(state) !== '', state).toBe(dimmed);
    }
  });
});
