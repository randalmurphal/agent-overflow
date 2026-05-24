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
});
