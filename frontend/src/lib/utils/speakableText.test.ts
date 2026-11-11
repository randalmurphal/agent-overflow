import { describe, expect, it } from 'vitest';
import { lex } from '../markdown';
import {
  MAX_PIECE_CHARS,
  MAX_SECTION_CHARS,
  MIN_SECTION_CHARS,
  SECTION_CHARS,
  capPiece,
  renderedBlocks,
  speakableSections,
  splitSentences,
} from './speakableText';

/** Every sentence a device voice reads, in order. */
function sentences(markdown: string): string[] {
  return speakableSections(markdown).flatMap((section) => section.sentences);
}

/** A paragraph of `count` short sentences, `length` characters long or a little under. */
function paragraph(length: number, word = 'word'): string {
  const sentence = `The ${word} goes on here.`;
  return Array.from({ length: Math.floor((length + 1) / (sentence.length + 1)) }, () => sentence).join(' ');
}

describe('what is spoken', () => {
  it('reads each sentence of prose as its own device sentence', () => {
    expect(sentences('The tests pass. Two were flaky! Want me to retry them?')).toEqual([
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
    expect(sentences(markdown)).toEqual(['Before the code.', 'After the table.']);
  });

  it('keeps the words of links, inline code and emphasis, and drops their markup', () => {
    expect(sentences('Run **`make test`** in [the repo](https://example.com/repo) _first_.')).toEqual([
      'Run make test in the repo first.',
    ]);
  });

  it('drops bare URLs, images, footnote markers and emoji', () => {
    expect(sentences('See https://example.com/x for details 🚀. ![diagram](d.png) Done[^1].\n\n[^1]: note')).toEqual([
      'See for details.',
      'Done.',
    ]);
  });

  it('drops a URL written as inline code', () => {
    expect(sentences('Open `https://example.com/docs` to read more.')).toEqual(['Open to read more.']);
  });

  it('reads headings and each list item as their own sentences, nested lists in order', () => {
    expect(sentences('## Summary\n\n- first item\n  - nested item\n- second item\n\n1. step one\n2. step two')).toEqual([
      'Summary',
      'first item',
      'nested item',
      'second item',
      'step one',
      'step two',
    ]);
  });

  it('reads the text inside quotes and alerts', () => {
    expect(sentences('> Quoted line.\n\n> [!NOTE]\n> Heads up.')).toEqual(['Quoted line.', 'Heads up.']);
  });

  it('keeps code and math silent inside an MDX container', () => {
    const markdown = '<Aside>\nInside the aside.\n\n```sh\nrm -rf build\n```\n\n$$\nx^2\n$$\n\nStill inside.\n</Aside>\n\nAfter it.';
    expect(sentences(markdown)).toEqual(['Inside the aside.', 'Still inside.', 'After it.']);
  });

  it('reads nothing from a message that is only code', () => {
    expect(speakableSections('```\nls -la\n```')).toEqual([]);
    expect(speakableSections('   ')).toEqual([]);
  });

  it('keeps every device sentence within the engine limit', () => {
    const long = Array.from({ length: 80 }, (_, i) => `clause ${i}`).join(', ') + '.';
    const pieces = sentences(long);
    expect(pieces.length).toBeGreaterThan(1);
    for (const piece of pieces) expect(piece.length).toBeLessThanOrEqual(MAX_PIECE_CHARS);
    expect(pieces.join(' ').replace(/\s+/g, ' ')).toBe(long);
  });
});

describe('speakableSections', () => {
  it('packs short blocks into one section joined by blank lines, and list items by line', () => {
    expect(speakableSections('First para.\n\nSecond para.\n\n- one\n- two')).toEqual([{
      text: 'First para.\n\nSecond para.\n\none\ntwo',
      sentences: ['First para.', 'Second para.', 'one', 'two'],
      firstBlock: 0,
      lastBlock: 2,
    }]);
  });

  it('starts a new section when the next block would pass SECTION_CHARS', () => {
    const a = paragraph(250, 'first');
    const b = paragraph(250, 'second');
    const sections = speakableSections(`${a}\n\n${b}`);
    expect(sections.map((s) => [s.text, s.firstBlock, s.lastBlock])).toEqual([[a, 0, 0], [b, 1, 1]]);
    // Exactly at the limit still packs.
    const c = paragraph(SECTION_CHARS - a.length - 2, 'third');
    expect(a.length + 2 + c.length).toBeLessThanOrEqual(SECTION_CHARS);
    expect(speakableSections(`${a}\n\n${c}`)).toHaveLength(1);
  });

  it('always starts a section at a heading', () => {
    const sections = speakableSections('Intro line that is long enough to stand on its own as a section here.\n\n## Next\n\nBody.');
    expect(sections.map((s) => [s.text, s.firstBlock, s.lastBlock])).toEqual([
      ['Intro line that is long enough to stand on its own as a section here.', 0, 0],
      ['Next\n\nBody.', 1, 2],
    ]);
  });

  it('lets a short section take the next block past SECTION_CHARS, but not past MAX_SECTION_CHARS or a heading', () => {
    const opener = 'Here is the plan.';
    expect(opener.length).toBeLessThan(MIN_SECTION_CHARS);
    const long = paragraph(600);
    expect(speakableSections(`${opener}\n\n${long}`)).toMatchObject([{ text: `${opener}\n\n${long}`, firstBlock: 0, lastBlock: 1 }]);

    const tooLong = paragraph(MAX_SECTION_CHARS, 'wordy');
    expect(tooLong.length).toBeLessThanOrEqual(MAX_SECTION_CHARS);
    expect(opener.length + 2 + tooLong.length).toBeGreaterThan(MAX_SECTION_CHARS);
    expect(speakableSections(`${opener}\n\n${tooLong}`)).toHaveLength(2);

    const heading = speakableSections(`## Plan\n\n${long}`);
    expect(heading).toMatchObject([{ text: `Plan\n\n${long}`, firstBlock: 0, lastBlock: 1 }]);
    expect(speakableSections(`${opener}\n\n## Plan`)).toHaveLength(2);
  });

  it('ends a section at a block with nothing to say, so it never covers one', () => {
    const sections = speakableSections('Before.\n\n```sh\nls\n```\n\nAfter.\n\n---\n\nLast.');
    expect(sections.map((s) => [s.text, s.firstBlock, s.lastBlock])).toEqual([
      ['Before.', 0, 0],
      ['After.', 2, 2],
      ['Last.', 4, 4],
    ]);
  });

  it('splits a block past MAX_SECTION_CHARS at sentences into parts of about equal length', () => {
    const long = paragraph(2000);
    const sections = speakableSections(`Short.\n\n${long}\n\nTail.`);
    const parts = sections.filter((s) => s.firstBlock === 1);
    expect(parts).toHaveLength(3);
    for (const part of parts) {
      expect(part.lastBlock).toBe(1);
      expect(part.text.length).toBeLessThanOrEqual(MAX_SECTION_CHARS);
      expect(part.text.length).toBeGreaterThan(2000 / 3 - 40);
      expect(part.text).toMatch(/^The word.*here\.$/);
    }
    expect(parts.map((p) => p.text).join(' ')).toBe(long);
    // The split block is packed with nothing on either side.
    expect(sections[0]).toMatchObject({ text: 'Short.', firstBlock: 0, lastBlock: 0 });
    expect(sections.at(-1)).toMatchObject({ text: 'Tail.', firstBlock: 2, lastBlock: 2 });
  });

  it('cuts a single sentence past MAX_SECTION_CHARS at clause punctuation', () => {
    const long = `${Array.from({ length: 150 }, (_, i) => `clause ${i}`).join(', ')}.`;
    const sections = speakableSections(long);
    expect(sections.length).toBeGreaterThan(1);
    for (const section of sections) {
      expect(section.text.length).toBeLessThanOrEqual(MAX_SECTION_CHARS);
      for (const sentence of section.sentences) expect(sentence.length).toBeLessThanOrEqual(MAX_PIECE_CHARS);
    }
    expect(sections.slice(0, -1).every((s) => s.text.endsWith(','))).toBe(true);
    expect(sections.map((s) => s.text).join(' ')).toBe(long);
  });

  it('indexes blocks as rendered: definitions, raw HTML and alignment take none, an MDX container its children', () => {
    const markdown = [
      'Zero.', '', '[ref]: https://example.com', '', '<div>raw</div>', '', '[center]', 'Centered.', '[/center]', '',
      '<Aside>', 'One.', '', 'Two.', '</Aside>', '', '```ts', 'x', '```', '', 'Four.',
    ].join('\n');
    expect(renderedBlocks(lex(markdown)).map((t) => t.type)).toEqual(['paragraph', 'paragraph', 'paragraph', 'code', 'paragraph']);
    expect(speakableSections(markdown).map((s) => [s.text, s.firstBlock, s.lastBlock])).toEqual([
      ['Zero.\n\nOne.\n\nTwo.', 0, 2],
      ['Four.', 4, 4],
    ]);
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

  it('cuts an unbroken run at the limit, the default or a given one', () => {
    expect(capPiece('x'.repeat(MAX_PIECE_CHARS * 2 + 5)).map((p) => p.length)).toEqual([MAX_PIECE_CHARS, MAX_PIECE_CHARS, 5]);
    expect(capPiece('x'.repeat(25), 10).map((p) => p.length)).toEqual([10, 10, 5]);
  });
});
