import { describe, expect, it } from 'vitest';
import { insertSuggestion, suggestionFence, suggestionSegments, type SuggestionSegment } from './suggestionFences';

function blocks(body: string): { lines: readonly string[]; above: number; below: number }[] {
  return suggestionSegments(body)
    .filter((segment): segment is Extract<SuggestionSegment, { kind: 'suggestion' }> => segment.kind === 'suggestion')
    .map(({ lines, above, below }) => ({ lines, above, below }));
}

const plain = (lines: string[]) => ({ lines, above: 0, below: 0 });

// The cases of internal/mdsuggest/parse_test.go: the backend decides what
// a comment holds, so the pane must read the same blocks.
describe('suggestionSegments reads the backend grammar', () => {
  it.each<[string, string, ReturnType<typeof blocks>]>([
    ['none', 'just text\n```go\ncode\n```\n', []],
    ['one', 'Try:\n```suggestion\nreturn nil\n```\n', [plain(['return nil'])]],
    ['several in order', '```suggestion\na\n```\nand\n```suggestion\nb\nc\n```', [plain(['a']), plain(['b', 'c'])]],
    ['empty block deletes', '```suggestion\n```\n', [plain([])]],
    ['blank line kept', '```suggestion\n\n```\n', [plain([''])]],
    ['longer fence holds a shorter one', '````suggestion\n```go\nx := 1\n```\n````\n', [plain(['```go', 'x := 1', '```'])]],
    ['a fence with an info string is content', '```suggestion\n```go\n```\n', [plain(['```go'])]],
    ['a shorter closing fence is content', '````suggestion\na\n```\nb\n````', [plain(['a', '```', 'b'])]],
    ['tildes', '~~~suggestion\nx\n~~~\n', [plain(['x'])]],
    ['backticks do not close tildes', '~~~suggestion\n```\nx\n~~~\n', [plain(['```', 'x'])]],
    ['CRLF', 'Use:\r\n```suggestion\r\nfoo()\r\nbar()\r\n```\r\n', [plain(['foo()', 'bar()'])]],
    ['indented fence strips its indent', '  ```suggestion\n  a\n    b\n  ```\n', [plain(['a', '  b'])]],
    ['four spaces is an indented code block', '    ```suggestion\n    a\n    ```\n', []],
    ['blockquote is quoted text', '> ```suggestion\n> a\n> ```\n', []],
    ['list item is nested text', '- item\n  ```suggestion\n  a\n  ```\n', []],
    ["fence on the list item's own line", '1. ```suggestion\n   a\n   ```\n', []],
    ['after a list ends', '- item\n\ntext\n```suggestion\na\n```\n', [plain(['a'])]],
    ['quoted then real', '> ```suggestion\n> old\n> ```\n\n```suggestion\nnew\n```', [plain(['new'])]],
    ['unterminated is none', '```suggestion\na\nb\n', []],
    ['terminated then unterminated', '```suggestion\na\n```\n```suggestion\nb\n', [plain(['a'])]],
    ['GitLab widened header', '```suggestion:-1+2\nx\n```\n', [{ lines: ['x'], above: 1, below: 2 }]],
    ["widened past GitLab's bound is code", '```suggestion:-101+0\nx\n```\n', []],
    ['another language is code', '```suggestions\nx\n```\n', []],
    ['info string after the word', '```suggestion extra\nx\n```\n', [plain(['x'])]],
    ['closing fence with trailing spaces', '```suggestion\nx\n```   \n', [plain(['x'])]],
    ['closing fence may be longer', '```suggestion\nx\n`````\n', [plain(['x'])]],
  ])('%s', (_name, body, want) => {
    expect(blocks(body)).toEqual(want);
  });
});

describe('suggestionSegments splits the body around the blocks', () => {
  it('keeps the markdown before, between and after each block, and counts blocks from 0', () => {
    expect(suggestionSegments('Before\n```suggestion\na\n```\nmiddle\n```suggestion\nb\n```\nafter')).toEqual([
      { kind: 'markdown', text: 'Before' },
      { kind: 'suggestion', index: 0, lines: ['a'], above: 0, below: 0 },
      { kind: 'markdown', text: 'middle' },
      { kind: 'suggestion', index: 1, lines: ['b'], above: 0, below: 0 },
      { kind: 'markdown', text: 'after' },
    ]);
  });

  it('is the whole body as markdown when it holds no block', () => {
    const body = 'text\n```suggestion\nunclosed';
    expect(suggestionSegments(body)).toEqual([{ kind: 'markdown', text: body }]);
  });

  it('drops blank markdown around a body that is only a block', () => {
    expect(suggestionSegments('\n```suggestion\nx\n```\n\n')).toEqual([{ kind: 'suggestion', index: 0, lines: ['x'], above: 0, below: 0 }]);
  });
});

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
    expect(blocks(out.body)).toEqual([plain(['x'])]);
  });

  it('opens with a longer fence when a line itself holds a fence', () => {
    const lines = ['```go', 'x := 1', '```'];
    const out = insertSuggestion('', 0, lines);
    expect(out.body.startsWith('````suggestion\n')).toBe(true);
    expect(blocks(out.body)).toEqual([plain(lines)]);
  });

  it('a fence of four inside the lines needs five', () => {
    expect(suggestionFence(['a ```` b'])).toBe('`````');
    expect(suggestionFence(['no ticks'])).toBe('```');
  });
});
