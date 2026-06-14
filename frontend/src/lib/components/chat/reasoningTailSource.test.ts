import { describe, expect, it } from 'vitest';
import { reasoningBodyText } from './reasoningTailSource';

// reasoningBodyText is the shared body-text selector for the two reasoning-tail
// rows (ThinkingBlock + CompactionReasoning). These cover the three branches and
// the containment-aware merge that the components themselves don't exercise
// directly — the case most likely to duplicate or drop text if it regresses.
describe('reasoningBodyText', () => {
  describe('collapsed', () => {
    it('returns the live smoother tail when present', () => {
      expect(
        reasoningBodyText({
          summary: 'trimmed summary',
          liveTail: 'live tail text',
          persisted: 'loaded payload',
          expanded: false,
          isStreaming: true,
        }),
      ).toBe('live tail text');
    });

    it('falls back to the trimmed summary once the smoother disposes (liveTail null)', () => {
      expect(
        reasoningBodyText({
          summary: 'trimmed summary',
          liveTail: null,
          persisted: 'loaded payload',
          expanded: false,
          isStreaming: false,
        }),
      ).toBe('trimmed summary');
    });
  });

  describe('expanded + streaming', () => {
    it('appends only the continuation tail when the snapshot is behind the reveal', () => {
      // persisted leads with "A"; the reveal "ABC" continues it → append "BC".
      expect(
        reasoningBodyText({
          summary: '',
          liveTail: 'ABC',
          persisted: 'A',
          expanded: true,
          isStreaming: true,
        }),
      ).toBe('ABC');
    });

    it('returns the live reveal verbatim when nothing is loaded yet (persisted empty)', () => {
      expect(
        reasoningBodyText({
          summary: '',
          liveTail: 'reveal so far',
          persisted: '',
          expanded: true,
          isStreaming: true,
        }),
      ).toBe('reveal so far');
    });

    it('appends nothing when the loaded snapshot already leads the reveal (snapshot ahead)', () => {
      // GetPayloadData flushes the live buffer before reading, so the fetched
      // body can lead the smoother reveal; the merge must not duplicate it.
      expect(
        reasoningBodyText({
          summary: '',
          liveTail: 'AB',
          persisted: 'ABC',
          expanded: true,
          isStreaming: true,
        }),
      ).toBe('ABC');
    });
  });

  describe('expanded + settled', () => {
    it('keeps the longer loaded payload over a shorter live remnant', () => {
      expect(
        reasoningBodyText({
          summary: '',
          liveTail: 'short',
          persisted: 'the full loaded payload body',
          expanded: true,
          isStreaming: false,
        }),
      ).toBe('the full loaded payload body');
    });

    it('keeps the live tail when it is longer than the loaded payload', () => {
      expect(
        reasoningBodyText({
          summary: '',
          liveTail: 'the longer live tail body',
          persisted: 'short',
          expanded: true,
          isStreaming: false,
        }),
      ).toBe('the longer live tail body');
    });
  });
});
