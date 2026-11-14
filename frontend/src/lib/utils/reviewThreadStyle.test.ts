import { describe, expect, it } from 'vitest';
import type { ReviewThread } from '../types/models';
import {
  outdatedTitle,
  reviewThreadOutdated,
  reviewThreadState,
  threadBodyClass,
  type ReviewThreadState,
} from './reviewThreadStyle';

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
  it('reads unresolved and resolved from the thread', () => {
    expect(reviewThreadState(thread())).toBe('unresolved');
    expect(reviewThreadState(thread({ isResolved: true }))).toBe('resolved');
  });

  it('keeps an outdated thread in its resolve state: the code moved, the question did not close', () => {
    expect(reviewThreadState(thread({ isOutdated: true }))).toBe('unresolved');
    expect(reviewThreadState(thread({ isResolved: true, isOutdated: true }))).toBe('resolved');
  });

  it('gives a non-resolvable thread no state, whatever its flags say', () => {
    expect(reviewThreadState(thread({ isResolvable: false }))).toBe('none');
    expect(reviewThreadState(thread({ isResolvable: false, isResolved: true, isOutdated: true }))).toBe('none');
  });
});

describe('reviewThreadOutdated', () => {
  it('marks the forge flag and an orphaned anchor alike', () => {
    expect(reviewThreadOutdated(thread())).toBe(false);
    expect(reviewThreadOutdated(thread({ isOutdated: true }))).toBe(true);
    expect(reviewThreadOutdated(thread(), true)).toBe(true);
  });
});

describe('outdatedTitle', () => {
  it('names the line when the forge still knows it', () => {
    expect(outdatedTitle(thread({ line: 12 }))).toBe('Written against an earlier revision, at line 12 then');
    expect(outdatedTitle(thread({ line: null, originalLine: 7 }))).toBe('Written against an earlier revision, at line 7 then');
    expect(outdatedTitle(thread({ line: null }))).toBe('Written against an earlier revision');
  });
});

describe('threadBodyClass', () => {
  it('dims only resolved threads', () => {
    const expected: Record<ReviewThreadState, boolean> = {
      unresolved: false,
      resolved: true,
      none: false,
    };
    for (const [state, dimmed] of Object.entries(expected) as [ReviewThreadState, boolean][]) {
      expect(threadBodyClass(state) !== '', state).toBe(dimmed);
    }
  });
});
