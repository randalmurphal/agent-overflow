import { describe, expect, it } from 'vitest';
import { MAX_PIECE_CHARS, capPiece, speakablePieces, splitSentences } from './speakableText';

describe('speakablePieces', () => {
  it('reads each sentence of prose as its own piece', () => {
    expect(speakablePieces('The tests pass. Two were flaky! Want me to retry them?')).toEqual([
      'The tests pass.',
      'Two were flaky!',
      'Want me to retry them?',
    ]);
  });

  it('skips code blocks, display math, tables, raw HTML and rules silently', () => {
    const markdown = [
      'Before the code.',
      '',
      '```ts',
      'const x = 1;',
      '```',
      '',
      '$$',
      'e = mc^2',
      '$$',
      '',
      '| a | b |',
      '|---|---|',
      '| 1 | 2 |',
      '',
      '<div>hidden</div>',
      '',
      '---',
      '',
      'After the table.',
    ].join('\n');
    expect(speakablePieces(markdown)).toEqual(['Before the code.', 'After the table.']);
  });

  it('keeps the words of links, inline code and emphasis, and drops their markup', () => {
    expect(speakablePieces('Run **`make test`** in [the repo](https://example.com/repo) _first_.')).toEqual([
      'Run make test in the repo first.',
    ]);
  });

  it('drops bare URLs, images, footnote markers and emoji', () => {
    expect(speakablePieces('See https://example.com/x for details 🚀. ![diagram](d.png) Done[^1].\n\n[^1]: note')).toEqual([
      'See for details.',
      'Done.',
    ]);
  });

  it('reads headings and each list item as their own pieces', () => {
    expect(speakablePieces('## Summary\n\n- first item\n- second item\n\n1. step one\n2. step two')).toEqual([
      'Summary',
      'first item',
      'second item',
      'step one',
      'step two',
    ]);
  });

  it('reads the text inside quotes and alerts', () => {
    expect(speakablePieces('> Quoted line.\n\n> [!NOTE]\n> Heads up.')).toEqual(['Quoted line.', 'Heads up.']);
  });

  it('keeps code and math silent inside an MDX container', () => {
    const markdown = '<Aside>\nInside the aside.\n\n```sh\nrm -rf build\n```\n\n$$\nx^2\n$$\n\nStill inside.\n</Aside>\n\nAfter it.';
    expect(speakablePieces(markdown)).toEqual(['Inside the aside.', 'Still inside.', 'After it.']);
  });

  it('reads nothing from a message that is only code', () => {
    expect(speakablePieces('```\nls -la\n```')).toEqual([]);
    expect(speakablePieces('   ')).toEqual([]);
  });

  it('keeps every piece within the engine limit', () => {
    const long = Array.from({ length: 80 }, (_, i) => `clause ${i}`).join(', ') + '.';
    const pieces = speakablePieces(long);
    expect(pieces.length).toBeGreaterThan(1);
    for (const piece of pieces) expect(piece.length).toBeLessThanOrEqual(MAX_PIECE_CHARS);
    expect(pieces.join(' ').replace(/\s+/g, ' ')).toBe(long);
  });
});

describe('splitSentences', () => {
  it('splits on sentence ends and drops blanks', () => {
    expect(splitSentences('One. Two?  Three!')).toEqual(['One.', 'Two?', 'Three!']);
  });
});

describe('capPiece', () => {
  it('cuts after clause punctuation before falling back to whitespace', () => {
    const head = 'a'.repeat(200);
    const pieces = capPiece(`${head}, ${'b '.repeat(60)}end`);
    expect(pieces[0]).toBe(`${head},`);
  });

  it('cuts an unbroken run at the limit', () => {
    const pieces = capPiece('x'.repeat(MAX_PIECE_CHARS * 2 + 5));
    expect(pieces.map((p) => p.length)).toEqual([MAX_PIECE_CHARS, MAX_PIECE_CHARS, 5]);
  });
});
