import { describe, expect, it, vi } from 'vitest';
import { reasoningBodyText, type ReasoningBodyTextInput } from './reasoningTailSource';

// reasoningBodyText is the shared body-text selector for the two reasoning-tail
// rows (ThinkingBlock + CompactionReasoning). These cover the three branches,
// including which ones read the whole live text.
function input(overrides: Partial<ReasoningBodyTextInput>): ReasoningBodyTextInput {
  return {
    summary: 'trimmed summary',
    liveWindow: null,
    liveText: () => null,
    persisted: '',
    expanded: false,
    isStreaming: true,
    ...overrides,
  };
}

describe('reasoningBodyText', () => {
  describe('collapsed', () => {
    it('returns the live window without reading the whole live text', () => {
      const liveText = vi.fn(() => 'whole live text');
      expect(
        reasoningBodyText(input({
          liveWindow: { text: 'live text', start: 6 },
          liveText,
          persisted: 'loaded payload',
        })),
      ).toEqual({ text: 'live text', start: 6 });
      expect(liveText).not.toHaveBeenCalled();
    });

    it('falls back to the trimmed summary once the smoother disposes (window null)', () => {
      expect(
        reasoningBodyText(input({ persisted: 'loaded payload', isStreaming: false })),
      ).toEqual({ text: 'trimmed summary', start: 0 });
    });
  });

  describe('expanded + streaming', () => {
    it('appends only the continuation tail when the snapshot is behind the reveal', () => {
      // persisted leads with "A"; the reveal "ABC" continues it → append "BC".
      expect(
        reasoningBodyText(input({ liveText: () => 'ABC', persisted: 'A', expanded: true })),
      ).toEqual({ text: 'ABC', start: 0 });
    });

    it('returns the whole live text, not its window, when nothing is loaded yet', () => {
      expect(
        reasoningBodyText(input({
          liveWindow: { text: 'so far', start: 7 },
          liveText: () => 'reveal so far',
          expanded: true,
        })),
      ).toEqual({ text: 'reveal so far', start: 0 });
    });

    it('appends nothing when the loaded snapshot already leads the reveal (snapshot ahead)', () => {
      // GetPayloadData flushes the live buffer before reading, so the fetched
      // body can lead the smoother reveal; the merge must not duplicate it.
      expect(
        reasoningBodyText(input({ liveText: () => 'AB', persisted: 'ABC', expanded: true })),
      ).toEqual({ text: 'ABC', start: 0 });
    });

    it('appends the summary to a stale snapshot when no live text is held', () => {
      expect(
        reasoningBodyText(input({ summary: 'live tail', persisted: 'full before ', expanded: true })),
      ).toEqual({ text: 'full before live tail', start: 0 });
    });
  });

  describe('expanded + settled', () => {
    it('keeps the longer loaded payload over a shorter live remnant', () => {
      expect(
        reasoningBodyText(input({
          liveText: () => 'short',
          persisted: 'the full loaded payload body',
          expanded: true,
          isStreaming: false,
        })),
      ).toEqual({ text: 'the full loaded payload body', start: 0 });
    });

    it('keeps the live text when it is longer than the loaded payload', () => {
      expect(
        reasoningBodyText(input({
          liveText: () => 'the longer live tail body',
          persisted: 'short',
          expanded: true,
          isStreaming: false,
        })),
      ).toEqual({ text: 'the longer live tail body', start: 0 });
    });
  });
});
