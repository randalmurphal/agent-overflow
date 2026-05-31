import { describe, expect, it } from 'vitest';

import { nonOverlappingSuffix, revealedSuffix } from './textOverlap';

describe('nonOverlappingSuffix', () => {
  it('returns the whole delta when there is no overlap', () => {
    expect(nonOverlappingSuffix('abc', 'def')).toBe('def');
  });

  it('drops the part of delta that continues the end of existing', () => {
    // existing ends with "lo wor", delta starts with "lo wor"
    expect(nonOverlappingSuffix('hello wor', 'lo world')).toBe('ld');
  });

  it('returns empty when delta is entirely a trailing overlap', () => {
    expect(nonOverlappingSuffix('hello', 'lo')).toBe('');
  });

  it('passes the delta through untouched when either side is empty', () => {
    expect(nonOverlappingSuffix('', 'delta')).toBe('delta');
    expect(nonOverlappingSuffix('existing', '')).toBe('');
  });

  it('is containment-blind: a delta wholly contained as a prefix is re-emitted', () => {
    // The defect revealedSuffix exists to guard. "The quick brown" does not END
    // with any prefix of "The quick" (it ends "brown"), so the full delta returns.
    expect(nonOverlappingSuffix('The quick brown', 'The quick')).toBe('The quick');
  });
});

describe('revealedSuffix', () => {
  it('returns empty when existing already starts with revealed (snapshot ahead)', () => {
    // Routine mid-stream-expand: the flushed snapshot leads the smoother reveal.
    expect(revealedSuffix('The quick brown fox ', 'The quick ')).toBe('');
  });

  it('returns empty when existing equals revealed', () => {
    expect(revealedSuffix('same text', 'same text')).toBe('');
  });

  it('returns the continuation tail when revealed extends existing (snapshot behind)', () => {
    expect(revealedSuffix('The quick ', 'The quick brown')).toBe('brown');
  });

  it('returns the non-overlapping tail for a streamed continuation', () => {
    expect(revealedSuffix('hello wor', 'lo world')).toBe('ld');
  });

  it('returns the whole revealed text when there is no shared content', () => {
    expect(revealedSuffix('full payload before ', 'live tail')).toBe('live tail');
  });

  it('returns empty for a reconnect interior window already contained in the snapshot', () => {
    // On reconnect the smoother reseeds from the bounded tail, so its revealed
    // slice ('gamma delta') is an interior substring of the flushed snapshot,
    // not a prefix. startsWith misses it; containment catches it; nothing is
    // re-appended. Without this the snapshot's interior gets duplicated.
    expect(revealedSuffix('alpha beta gamma delta epsilon', 'gamma delta')).toBe('');
  });

  it('appends only the new tail when an interior reveal overtakes the snapshot', () => {
    // The reveal starts inside the flush ('gamma ' is a suffix of existing) but
    // extends past it with genuinely-new text ('delta'). Containment is false,
    // so the suffix scan trims the in-flush overlap and appends exactly the new
    // bytes — the new tail is never dropped.
    expect(revealedSuffix('alpha beta gamma ', 'gamma delta')).toBe('delta');
  });

  it('appends everything when existing is empty', () => {
    expect(revealedSuffix('', 'first reveal')).toBe('first reveal');
  });

  it('appends nothing when revealed is empty', () => {
    expect(revealedSuffix('anything', '')).toBe('');
  });
});
