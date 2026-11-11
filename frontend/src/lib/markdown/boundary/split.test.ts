import { describe, it, expect } from 'vitest';
import { splitAtBoundary } from './split';

describe('splitAtBoundary', () => {
  it('returns empty prefix and empty tail for empty input', () => {
    expect(splitAtBoundary('')).toEqual({ prefix: '', tail: '' });
  });

  it('keeps a single unfinished paragraph entirely in the tail', () => {
    const text = 'A single paragraph still being typed.';
    expect(splitAtBoundary(text)).toEqual({ prefix: '', tail: text });
  });

  it('does not commit a paragraph until a real blank line follows it', () => {
    const text = 'Paragraph one.\n';
    // Single trailing newline must NOT create a phantom boundary.
    expect(splitAtBoundary(text)).toEqual({ prefix: '', tail: text });
  });

  it('commits the first paragraph once a blank line + content follows', () => {
    const text = 'Paragraph one.\n\nParagraph two starting…';
    const split = splitAtBoundary(text);
    // The blank-line separator stays with the committed prefix so the
    // volatile tail always starts on a fresh block boundary.
    expect(split.prefix).toBe('Paragraph one.\n\n');
    expect(split.tail).toBe('Paragraph two starting…');
    expect(split.prefix + split.tail).toBe(text);
  });

  it('holds back the trailing block as lookahead with two completed blocks', () => {
    const text = 'Block one.\n\nBlock two.\n\nBlock three still streaming';
    const split = splitAtBoundary(text);
    // Lookahead rule: block three stays in the tail.
    expect(split.tail.includes('Block three')).toBe(true);
    // Block one is definitely committed.
    expect(split.prefix.startsWith('Block one.')).toBe(true);
  });

  it('does not split inside an open fenced code block', () => {
    const text = 'Intro.\n\n```ts\nconst x = 1;\n\nconst y = 2;\n';
    const split = splitAtBoundary(text);
    // Intro committed (with its trailing blank-line separator); the open
    // fence cannot commit and stays in the volatile tail.
    expect(split.prefix).toBe('Intro.\n\n');
    expect(split.tail.includes('```ts')).toBe(true);
    expect(split.tail.includes('const y = 2;')).toBe(true);
    expect(split.prefix + split.tail).toBe(text);
  });

  it('commits a closed code fence as a stable block', () => {
    const text = 'Before.\n\n```ts\nconst x = 1;\n```\n\nAfter…';
    const split = splitAtBoundary(text);
    expect(split.prefix.includes('```ts')).toBe(true);
    expect(split.prefix.includes('```\n')).toBe(true);
    expect(split.tail.includes('After')).toBe(true);
  });

  it('defers commit when the next line could be a setext underline', () => {
    // Paragraph followed by ===== retroactively becomes an H1. The
    // detector must not commit the paragraph until the next line is
    // known to NOT be a setext underline.
    const setextLikely = 'Title goes here\n';
    const split = splitAtBoundary(setextLikely);
    expect(split.prefix).toBe('');
  });

  it('keeps an unterminated bullet list in the tail until it ends', () => {
    const text = 'Heading\n\n- item one\n- item two\n- item three';
    const split = splitAtBoundary(text);
    // Heading committed (with its blank-line separator); list still
    // streaming → in tail.
    expect(split.prefix).toBe('Heading\n\n');
    expect(split.tail.includes('- item one')).toBe(true);
    expect(split.prefix + split.tail).toBe(text);
  });

  it('commits a list before a non-list block that follows its blank line', () => {
    const list = '- first item\n- second item\n\n';
    for (const nextBlock of [
      '| Partial table row | still growing',
      'Ordinary paragraph still growing',
      '> A blockquote still growing',
      '```ts\nconst value = true;',
      '## A heading still growing',
    ]) {
      const split = splitAtBoundary(list + nextBlock);
      expect(split.prefix).toBe(list);
      expect(split.tail).toBe(nextBlock);
    }
  });

  it('commits a footnote before an unindented block after its blank line', () => {
    const footnote = '[^note]: Footnote body\n\n';
    for (const nextBlock of [
      'Ordinary paragraph still growing',
      '- a list still growing',
      '```ts\nconst value = true;',
      '## A heading still growing',
    ]) {
      const split = splitAtBoundary(footnote + nextBlock);
      expect(split.prefix).toBe(footnote);
      expect(split.tail).toBe(nextBlock);
    }
  });

  it('keeps an indented footnote continuation together across a blank line', () => {
    const footnote = '[^note]: Footnote body\n\n    continued body';
    expect(splitAtBoundary(footnote)).toEqual({ prefix: '', tail: footnote });

    const separator = '\n\n';
    const nextBlock = 'Ordinary paragraph still growing';
    const split = splitAtBoundary(footnote + separator + nextBlock);
    expect(split.prefix).toBe(footnote + separator);
    expect(split.tail).toBe(nextBlock);
  });

  it('does not regress the committed prefix on the monotonic guard', () => {
    // Simulate a hypothetical detector that returned a shorter prefix
    // on a later call. The caller passes the previously committed
    // length and gets exactly that much back.
    const text = 'Para one.\n\nPara two start';
    const baseline = splitAtBoundary(text);
    expect(baseline.prefix.length).toBeGreaterThan(0);
    // Pass a previousPrefixLength larger than what the detector would
    // produce by itself, but still inside `text` so we exercise the
    // real branch (not the slice-clamps-to-text-length degenerate
    // case). The guard must produce a prefix of exactly that length.
    const fakedPrevious = baseline.prefix.length + 10;
    expect(fakedPrevious).toBeLessThan(text.length);
    const guarded = splitAtBoundary(text, fakedPrevious);
    expect(guarded.prefix.length).toBe(fakedPrevious);
    // Invariant: prefix + tail must always reconstruct the source.
    expect(guarded.prefix + guarded.tail).toBe(text);
  });

  it('handles trailing whitespace-only lines without phantom boundaries', () => {
    // A real "\n\n" inside the text DOES commit; a trailing single "\n"
    // does NOT. This pair must not produce an oscillation as characters
    // arrive one at a time.
    const noTrail = 'Para.\n\nPara two';
    const withTrail = noTrail + '\n';
    const a = splitAtBoundary(noTrail);
    const b = splitAtBoundary(withTrail);
    // Adding the trailing newline must not RETRACT the already-
    // committed prefix.
    expect(b.prefix.length).toBeGreaterThanOrEqual(a.prefix.length);
  });
});
