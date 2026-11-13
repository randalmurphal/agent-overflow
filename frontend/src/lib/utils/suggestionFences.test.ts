import { describe, expect, it } from 'vitest';
import { insertSuggestion, suggestionFence } from './suggestionFences';

// What `Suggest change` writes: the block on lines of its own at the
// caret, the caret at the end of its content, and a fence longer than any
// backtick run in the lines. That the backend reads it back as one block
// holding exactly those lines is end to end
// (e2e/tests/review-suggestions-reactions.spec.ts).

describe('insertSuggestion', () => {
  it('inserts one line at the caret on lines of its own, the caret at the end of its content', () => {
    const out = insertSuggestion('Use this:', 9, ['return nil']);
    expect(out.body).toBe('Use this:\n```suggestion\nreturn nil\n```');
    expect(out.body.slice(0, out.caret)).toBe('Use this:\n```suggestion\nreturn nil');
  });

  it('inserts a range into an empty body', () => {
    const out = insertSuggestion('', 0, ['a', '  b', '']);
    expect(out.body).toBe('```suggestion\na\n  b\n\n```');
    expect(out.caret).toBe('```suggestion\na\n  b\n'.length);
  });

  it('keeps the text after the caret on its own line', () => {
    const out = insertSuggestion('before\nafter', 6, ['x']);
    expect(out.body).toBe('before\n```suggestion\nx\n```\nafter');
  });

  it('clamps a caret past the end', () => {
    expect(insertSuggestion('ab', 9, ['x']).body).toBe('ab\n```suggestion\nx\n```');
  });

  it('opens with a longer fence when a line itself holds a fence', () => {
    const out = insertSuggestion('', 0, ['```go', 'x := 1', '```']);
    expect(out.body).toBe('````suggestion\n```go\nx := 1\n```\n````');
  });

  it('a fence of four inside the lines needs five', () => {
    expect(suggestionFence(['a ```` b'])).toBe('`````');
    expect(suggestionFence(['no ticks'])).toBe('```');
  });

  it('writes LF only into a CRLF body, the caret counted in the text the textarea holds', () => {
    const out = insertSuggestion('Use:\r\nthis\r\n', 'Use:\nthis\n'.length, ['a\r', 'b\r']);
    expect(out.body).toBe('Use:\nthis\n```suggestion\na\nb\n```');
    expect(out.body.slice(0, out.caret)).toBe('Use:\nthis\n```suggestion\na\nb');
  });

  it('keeps a lone CR inside a line, at the length the textarea gives it', () => {
    const out = insertSuggestion('', 0, ['in\rside']);
    expect(out.body).toBe('```suggestion\nin\rside\n```');
    expect(out.caret).toBe('```suggestion\nin\rside'.length);
  });
});
