import { describe, expect, it } from 'vitest';
import { findableBody, layoutBody, layoutText, suggestionRanges } from './suggestionPieces';
import type { CommentSuggestion } from '../types/models';

// A body cut at the fence lines the backend reports: markdown between,
// each block in place; a body any block is not placed in renders whole
// with every block after it.

const BODY = 'Before.\n```suggestion\nx\n```\nBetween.\n```suggestion\ny\n```\nAfter.';

function suggestion(over: Partial<CommentSuggestion>): CommentSuggestion {
  return { index: 0, to: [], appliable: false, bodyStartLine: 0, bodyEndLine: 0, bodyPlaced: false, digest: '0'.repeat(64), ...over };
}

describe('layoutBody', () => {
  it('cuts the body at each placed block, the markdown around them kept in order', () => {
    const layout = layoutBody(BODY, [{ startLine: 1, endLine: 3 }, { startLine: 5, endLine: 7 }]);
    expect(layout.pieces).toEqual([
      { kind: 'markdown', text: 'Before.', key: 'before:0' },
      { kind: 'block', at: 0, key: 'block:0' },
      { kind: 'markdown', text: 'Between.', key: 'before:1' },
      { kind: 'block', at: 1, key: 'block:1' },
      { kind: 'markdown', text: 'After.', key: 'tail' },
    ]);
    expect(layout.after).toEqual([]);
    expect(layoutText(layout)).toBe('Before.\n\nBetween.\n\nAfter.');
  });

  it('drops blank and comment-only markdown between blocks', () => {
    const body = '```suggestion\nx\n```\n<!-- marker -->\n```suggestion\ny\n```\n';
    expect(layoutBody(body, [{ startLine: 0, endLine: 2 }, { startLine: 4, endLine: 6 }]).pieces.map((piece) => piece.kind)).toEqual(['block', 'block']);
  });

  it('counts lines of a CRLF body by "\\n"', () => {
    const layout = layoutBody('A\r\n```suggestion\r\nx\r\n```\r\nB', [{ startLine: 1, endLine: 3 }]);
    expect(layoutText(layout)).toBe('A\n\nB');
  });

  it('renders the body whole with every block after it when one is not placed', () => {
    const layout = layoutBody(BODY, [{ startLine: 1, endLine: 3 }, null]);
    expect(layout.pieces).toEqual([{ kind: 'markdown', text: BODY, key: 'body' }]);
    expect(layout.after).toEqual([0, 1]);
  });

  it('renders the body whole when the ranges overlap, run backwards or leave the body', () => {
    for (const ranges of [
      [{ startLine: 1, endLine: 5 }, { startLine: 5, endLine: 7 }],
      [{ startLine: 5, endLine: 7 }, { startLine: 1, endLine: 3 }],
      [{ startLine: 3, endLine: 1 }],
      [{ startLine: 7, endLine: 9 }],
      [{ startLine: -1, endLine: 1 }],
    ]) {
      expect(layoutBody(BODY, ranges).after.length, JSON.stringify(ranges)).toBe(ranges.length);
    }
  });

  it('is the visible body with no blocks', () => {
    expect(layoutBody('Hi <!-- x -->', [])).toEqual({ pieces: [{ kind: 'markdown', text: 'Hi', key: 'body' }], after: [] });
  });
});

describe('findableBody', () => {
  it('reads a sent comment without its placed blocks, and whole when they are not placed', () => {
    const placed = [suggestion({ bodyPlaced: true, bodyStartLine: 1, bodyEndLine: 3 }), suggestion({ index: 1, bodyPlaced: true, bodyStartLine: 5, bodyEndLine: 7 })];
    expect(findableBody({ body: BODY, suggestions: placed })).toBe('Before.\n\nBetween.\n\nAfter.');
    expect(findableBody({ body: BODY, suggestions: [suggestion({})] })).toBe(BODY);
    expect(suggestionRanges(undefined)).toEqual([]);
  });
});
