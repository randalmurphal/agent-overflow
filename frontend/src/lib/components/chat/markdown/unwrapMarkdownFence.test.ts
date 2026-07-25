import { describe, expect, it } from 'vitest';
import { unwrapMarkdownFence } from './unwrapMarkdownFence';

describe('unwrapMarkdownFence', () => {
  it('unwraps a ```markdown fence with inner ```go blocks', () => {
    const input = [
      '```markdown',
      '',
      '## Heading',
      '',
      'Some text.',
      '',
      '```go',
      'func main() {}',
      '```',
      '',
      'More text.',
      '',
      '```',
    ].join('\n');

    const result = unwrapMarkdownFence(input);
    expect(result).toBe(
      [
        '## Heading',
        '',
        'Some text.',
        '',
        '```go',
        'func main() {}',
        '```',
        '',
        'More text.',
      ].join('\n'),
    );
  });

  it('unwraps ```md (lowercase short alias)', () => {
    const input = ['```md', '# Title', '```go', 'x := 1', '```', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'x := 1', '```'].join('\n'));
  });

  it('unwraps ```MARKDOWN (case-insensitive)', () => {
    const input = ['```MARKDOWN', '# Title', '```go', 'x := 1', '```', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'x := 1', '```'].join('\n'));
  });

  it('preserves up-to-3-space leading indent on the opener', () => {
    const input = ['   ```markdown', '# Title', '```go', 'code', '```', '   ```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'code', '```'].join('\n'));
  });

  it('leaves content alone when the wrapper has no language tag', () => {
    const input = ['```', '# Title', '```go', 'code', '```', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('leaves content alone when there is no inner fence', () => {
    const input = ['```markdown', '', 'Just some text with no code blocks.', '', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('leaves content alone when the closing fence is missing (mid-stream)', () => {
    const input = ['```markdown', '# Title', '```go', 'code', '```', 'More text.'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('leaves content alone when the closing fence count is less than the opener', () => {
    const input = ['````markdown', '# Title', '```go', 'code', '```', '``'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('handles a 4-backtick wrapper', () => {
    const input = ['````markdown', '# Title', '```go', 'code', '```', '````'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'code', '```'].join('\n'));
  });

  it('preserves the body inner fenced blocks verbatim', () => {
    const input = [
      '```markdown',
      '',
      '```go',
      'func main() {}',
      '```',
      '',
      '```typescript',
      'const x = 1;',
      '```',
      '',
      '```',
    ].join('\n');

    const result = unwrapMarkdownFence(input);
    expect(result).toContain('```go');
    expect(result).toContain('```typescript');
    expect(result).toContain('func main() {}');
    expect(result).toContain('const x = 1;');
  });

  it('handles trailing whitespace after the closing fence', () => {
    const input = ['```markdown', '# Title', '```go', 'code', '```', '```   '].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'code', '```'].join('\n'));
  });

  it('handles a closing fence with more backticks than the opener', () => {
    const input = ['```markdown', '# Title', '```go', 'code', '```', '`````'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'code', '```'].join('\n'));
  });

  it('returns empty string for empty input', () => {
    expect(unwrapMarkdownFence('')).toBe('');
  });

  it('returns input unchanged when source is plain text', () => {
    const input = 'Hello world.\n\nSome more text.';
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  // Fast-path equivalence: sources admitted by the "backtick within the
  // first four characters" guard that are NOT ```markdown wrappers must
  // short-circuit on the opener-line test without changing behavior.

  it('returns a message that starts with inline code unchanged', () => {
    const input = '`useTailWindow` is the hook in question.\n\nIt cuts at line starts.\n\n```go\ncode\n```';
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('returns a single-line inline-code message with no newline unchanged', () => {
    const input = '`flag` ' + 'prose '.repeat(500);
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('returns an indented non-opener first line unchanged', () => {
    const input = ' x `tick`\n```markdown\n```go\ncode\n```\n```';
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('unwraps an opener preceded by up to three blank characters', () => {
    const input = ['', '```markdown', '# Title', '```go', 'code', '```', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'code', '```'].join('\n'));
  });

  it('unwraps an opener with trailing whitespace', () => {
    const input = ['```markdown   ', '# Title', '```go', 'code', '```', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'code', '```'].join('\n'));
  });

  it('leaves an opener beyond the first four characters unchanged (guard shape)', () => {
    const input = ['', '', '', '', '```markdown', '```go', 'code', '```', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('leaves a language-tag lookalike unchanged', () => {
    const input = ['```markdownx', '```go', 'code', '```', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(input);
  });

  it('unwraps with exactly three leading blank characters (guard boundary)', () => {
    const input = ['', '', '', '```markdown', '# Title', '```go', 'code', '```', '```'].join('\n');
    expect(unwrapMarkdownFence(input)).toBe(['# Title', '```go', 'code', '```'].join('\n'));
  });

  // OPENER_SCAN_RE (the sticky fast-path pre-check) must accept/reject
  // exactly the opener lines OPENER_RE does — a divergence silently
  // breaks the unwrap (or falsely engages it). Each candidate line is
  // dropped into an otherwise-unwrappable document; whether the unwrap
  // happens is then decided solely by the opener test, exercising both
  // regexes through the public API.
  describe('fast-path equivalence over opener-line shapes', () => {
    const docFor = (opener: string, closer: string): string =>
      [opener, '# Title', '```go', 'code', '```', closer].join('\n');

    const unwrapped = ['# Title', '```go', 'code', '```'].join('\n');

    const accepted: Array<[string, string, string]> = [
      ['plain opener', '```markdown', '```'],
      ['short alias', '```md', '```'],
      ['uppercase', '```MARKDOWN', '```'],
      ['mixed case alias', '```Md', '```'],
      ['one leading space', ' ```markdown', '```'],
      ['three leading spaces', '   ```markdown', '```'],
      ['trailing spaces', '```markdown   ', '```'],
      ['trailing tab', '```markdown\t', '```'],
      ['trailing carriage return', '```markdown\r', '```'],
      ['five-backtick fence', '`````markdown', '`````'],
    ];

    const rejected: Array<[string, string]> = [
      ['no language tag', '```'],
      ['two backticks', '``md'],
      ['wrong language', '```markdwn'],
      ['tag with suffix', '```mdx'],
      ['tilde fence', '~~~markdown'],
      ['tab indentation', '\t```markdown'],
      ['inline content after tag', '```markdown # heading'],
    ];

    for (const [name, opener, closer] of accepted) {
      it(`accepts: ${name}`, () => {
        expect(unwrapMarkdownFence(docFor(opener, closer))).toBe(unwrapped);
      });
    }

    for (const [name, opener] of rejected) {
      it(`rejects: ${name}`, () => {
        const input = docFor(opener, '```');
        expect(unwrapMarkdownFence(input)).toBe(input);
      });
    }
  });
});
