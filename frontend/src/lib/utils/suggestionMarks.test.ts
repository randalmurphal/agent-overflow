import { describe, expect, it } from 'vitest';
import { findableBody, isSuggestionMark, markSuggestionFences, sentBlocks, suggestionMarkIndex } from './suggestionMarks';
import type { CommentSuggestion } from '../types/models';

// The fences the backend placed are marked in place, by line, when the
// fence there still holds the block's lines; every other block renders
// after the body.

function marks(source: string): (number | null)[] {
  return source.split('\n').map((line) => {
    const info = /^ {0,3}(?:`{3,}|~{3,})(.*)$/.exec(line.replace(/\r$/, ''))?.[1].trim() ?? '';
    return suggestionMarkIndex(info);
  });
}

function suggestion(over: Partial<CommentSuggestion>): CommentSuggestion {
  return { index: 0, to: [], appliable: false, bodyStartLine: 0, bodyEndLine: 0, bodyPlaced: false, digest: '0'.repeat(64), ...over };
}

describe('markSuggestionFences', () => {
  it('marks each placed fence by its line, identical blocks each at their own', () => {
    const body = 'A\n```suggestion\nx\n```\nB\n```suggestion\nx\n```';
    const marked = markSuggestionFences(body, [{ startLine: 1, endLine: 3, lines: ['x'] }, { startLine: 5, endLine: 7, lines: ['x'] }]);
    expect(marks(marked.source)).toEqual([null, 0, null, null, null, 1, null, null]);
    expect(marked.after).toEqual([]);
  });

  it('accepts tildes, longer closers, a range info string, indent and CRLF', () => {
    for (const body of ['~~~suggestion\nx\n~~~~', '```suggestion:-1+2\nx\n```', '  ```suggestion\n  x\n  ```', '```suggestion\r\nx\r\n```\r\n']) {
      const marked = markSuggestionFences(body, [{ startLine: 0, endLine: 2, lines: ['x'] }]);
      expect(marks(marked.source)[0], JSON.stringify(body)).toBe(0);
      expect(marked.after, JSON.stringify(body)).toEqual([]);
    }
  });

  it('keeps the line ending of a CRLF opening line', () => {
    expect(markSuggestionFences('```suggestion\r\nx\r\n```', [{ startLine: 0, endLine: 2, lines: ['x'] }]).source).toMatch(/-0\r\nx\r\n```$/);
  });

  it('renders after the body a block that is unplaced, out of range, reused or no longer matches', () => {
    const body = '```suggestion\nx\n```\n```ts\nx\n```';
    for (const block of [
      null,
      { startLine: -1, endLine: 2, lines: ['x'] },
      { startLine: 0, endLine: 9, lines: ['x'] },
      { startLine: 2, endLine: 0, lines: ['x'] },
      { startLine: 0, endLine: 2, lines: ['y'] },
      { startLine: 0, endLine: 2, lines: ['x', 'x'] },
      { startLine: 3, endLine: 5, lines: ['x'] },
      { startLine: 0, endLine: 1, lines: [] },
    ]) {
      const marked = markSuggestionFences(body, [block]);
      expect(marked.after, JSON.stringify(block)).toEqual([0]);
      expect(marks(marked.source).every((mark) => mark === null)).toBe(true);
    }
    const twice = markSuggestionFences(body, [{ startLine: 0, endLine: 2, lines: ['x'] }, { startLine: 0, endLine: 2, lines: ['x'] }]);
    expect(twice.after).toEqual([1]);
  });

  it('cannot be marked by the body itself', () => {
    const forged = '```suggestion ao-suggestion-0\nx\n```';
    expect(marks(markSuggestionFences(forged, []).source)).toEqual([null, null, null]);
    expect(isSuggestionMark('suggestion ao-suggestion-0')).toBe(false);
  });

  it('drops HTML comments from the rendered source', () => {
    expect(markSuggestionFences('Hi <!-- x -->', []).source).toBe('Hi');
  });
});

describe('sentBlocks and findableBody', () => {
  it('places only what the forge placed, and find reads the marked source', () => {
    const suggestions = [
      suggestion({ to: ['x'], bodyStartLine: 0, bodyEndLine: 2, bodyPlaced: true }),
      suggestion({ index: 1, to: ['y'] }),
    ];
    expect(sentBlocks(suggestions)).toEqual([{ startLine: 0, endLine: 2, lines: ['x'] }, null]);
    expect(sentBlocks(null)).toEqual([]);
    const shown = findableBody({ body: '```suggestion\nx\n```', suggestions });
    expect(marks(shown)[0]).toBe(0);
  });
});
