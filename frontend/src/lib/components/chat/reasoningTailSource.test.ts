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
    it('returns the whole live text, not its window, when nothing is loaded yet', () => {
      expect(
        reasoningBodyText(input({
          liveWindow: { text: 'so far', start: 7 },
          liveText: () => 'reveal so far',
          expanded: true,
        })),
      ).toEqual({ text: 'reveal so far', start: 0 });
    });

    // `live` null is no live text held: the summary stands in for it.
    it.each([
      // persisted leads with "A"; the reveal "ABC" continues it → append "BC".
      { name: 'appends only the continuation tail when the snapshot is behind the reveal', live: 'ABC', summary: 'trimmed summary', persisted: 'A', text: 'ABC' },
      // GetPayloadData flushes the live buffer before reading, so the fetched
      // body can lead the smoother reveal. The text past the reveal waits
      // for it rather than landing in one block.
      { name: 'ends at the reveal when the loaded snapshot leads it (snapshot ahead)', live: 'AB', summary: 'trimmed summary', persisted: 'ABC', text: 'AB' },
      // A smoother reseeded from the row's trimmed tail reveals an interior
      // slice of the whole text: the body is the snapshot up to its end.
      { name: 'ends at the reveal when a reseeded reveal sits inside the snapshot', live: 'CD', summary: 'trimmed summary', persisted: 'ABCDEF', text: 'ABCD' },
      { name: 'shows nothing past an empty reveal', live: null, summary: '', persisted: 'ABC', text: '' },
      { name: 'appends the summary to a stale snapshot when no live text is held', live: null, summary: 'live tail', persisted: 'full before ', text: 'full before live tail' },
    ])('$name', ({ live, summary, persisted, text }) => {
      expect(
        reasoningBodyText(input({ liveText: () => live, summary, persisted, expanded: true })),
      ).toEqual({ text, start: 0 });
    });
  });

  describe('expanded + settled', () => {
    it.each([
      { name: 'keeps the longer loaded payload over a shorter live remnant', live: 'short', persisted: 'the full loaded payload body', text: 'the full loaded payload body' },
      { name: 'keeps the live text when it is longer than the loaded payload', live: 'the longer live tail body', persisted: 'short', text: 'the longer live tail body' },
    ])('$name', ({ live, persisted, text }) => {
      expect(
        reasoningBodyText(input({ liveText: () => live, persisted, expanded: true, isStreaming: false })),
      ).toEqual({ text, start: 0 });
    });
  });
});
