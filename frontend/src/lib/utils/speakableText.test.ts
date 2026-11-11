import { describe, expect, it, vi } from 'vitest';
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
  spokenLength,
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

  it('packs and splits by spoken length, so dense scripts and numbers get shorter sections', () => {
    // 61 characters, said in about as long as 241 letters.
    const zh = `${'这是一个用来测试朗读长度的句子'.repeat(4)}。`;
    expect(zh).toHaveLength(61);
    expect(spokenLength(zh)).toBe(241);
    // Two fit SECTION_CHARS as characters; as speech they do not.
    expect(speakableSections(`${zh}\n\n${zh}`).map((s) => [s.firstBlock, s.lastBlock])).toEqual([[0, 0], [1, 1]]);

    // One 700-character paragraph is under MAX_SECTION_CHARS as characters.
    const sentence = '我们今天把所有的测试都重新运行了一遍，结果全部通过。';
    const long = sentence.repeat(Math.ceil(700 / sentence.length));
    expect(long.length).toBeLessThan(MAX_SECTION_CHARS);
    const parts = speakableSections(long);
    expect(parts.length).toBeGreaterThanOrEqual(4);
    for (const part of parts) {
      expect(spokenLength(part.text)).toBeLessThanOrEqual(MAX_SECTION_CHARS);
      expect(part.text.endsWith('。')).toBe(true);
      for (const piece of part.sentences) expect(spokenLength(piece)).toBeLessThanOrEqual(MAX_PIECE_CHARS);
      expect([part.firstBlock, part.lastBlock]).toEqual([0, 0]);
    }
    // Sentences inside a part are joined by a space.
    expect(parts.map((p) => p.text.replaceAll(' ', '')).join('')).toBe(long);

    const figures = Array.from({ length: 60 }, (_, i) => `Row ${i + 1} holds ${123456 + i}.`).join(' ');
    expect(figures.length).toBeLessThan(MAX_SECTION_CHARS * 2);
    const rows = speakableSections(figures);
    expect(rows.length).toBeGreaterThan(4);
    for (const row of rows) expect(spokenLength(row.text)).toBeLessThanOrEqual(MAX_SECTION_CHARS);
    expect(rows.map((r) => r.text).join(' ')).toBe(figures);
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

describe('renderedBlocks', () => {
  it('takes a container of any size, and nested ones in order', () => {
    const paragraphs = Array.from({ length: 500_000 }, () => ({ type: 'paragraph' }));
    expect(renderedBlocks([{ type: 'mdx', tokens: paragraphs }])).toHaveLength(500_000);
    const nested = [
      { type: 'heading', id: 0 },
      { type: 'mdx', tokens: [{ type: 'paragraph', id: 1 }, { type: 'mdx', tokens: [{ type: 'code', id: 2 }, { type: 'space', id: -1 }] }, { type: 'list', id: 3 }] },
      { type: 'html', id: -1 },
      { type: 'hr', id: 4 },
    ];
    expect(renderedBlocks(nested).map((token) => (token as { id: number }).id)).toEqual([0, 1, 2, 3, 4]);
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

  it('measures the limit as spoken length and cuts after wide clause punctuation', () => {
    // 4 a character: 20 fits five.
    expect(capPiece('天'.repeat(12), 20)).toEqual(['天天天天天', '天天天天天', '天天']);
    expect(capPiece('一二三，四五六七八九', 20)).toEqual(['一二三，', '四五六七八', '九']);
    expect(capPiece('12345 6789', 40)).toEqual(['12345', '6789']);
    // A piece is never only the clause mark it starts with.
    expect(capPiece('，一二三四', 9)).toEqual(['，一二', '三四']);
    // A character past the basic plane is never cut in half, and one that
    // alone passes the limit is still a piece.
    expect(capPiece('𠀀𠀁𠀂', 8)).toEqual(['𠀀𠀁', '𠀂']);
    expect(capPiece('😀😀😀', 5)).toEqual(['😀😀', '😀']);
    expect(capPiece('𠀀𠀁', 3)).toEqual(['𠀀', '𠀁']);
    expect(capPiece('77', 3)).toEqual(['7', '7']);
    // Nothing is lost or reordered around the cuts.
    expect(capPiece('ab, cd  ef,gh ij', 4)).toEqual(['ab,', 'cd', 'ef,g', 'h ij']);
    expect(capPiece(' a\t\t\t\tb ', 2)).toEqual(['a', 'b']);
  });

  it('cuts plain text where counting its characters does', () => {
    // The cut by character count, which spoken length equals for text
    // with no digit and no dense script.
    const byCharacters = (sentence: string, max: number): string[] => {
      const pieces: string[] = [];
      let rest = sentence;
      while (rest.length > max) {
        const window = rest.slice(0, max + 1);
        const clause = Math.max(window.lastIndexOf('; '), window.lastIndexOf(': '), window.lastIndexOf(', '));
        const space = window.lastIndexOf(' ');
        const cut = clause > 0 ? clause + 1 : space > 0 ? space : max;
        pieces.push(rest.slice(0, cut).trim());
        rest = rest.slice(cut).trim();
      }
      if (rest) pieces.push(rest);
      return pieces;
    };
    let seed = 20261110;
    const next = (below: number): number => {
      seed = (seed * 1103515245 + 12345) % 2147483648;
      return seed % below;
    };
    const alphabet = 'abcde   ,;:.';
    for (let round = 0; round < 3000; round += 1) {
      const text = Array.from({ length: 1 + next(70) }, () => alphabet[next(alphabet.length)]).join('').trim();
      const max = 1 + next(14);
      expect(capPiece(text, max), `${JSON.stringify(text)} at ${max}`).toEqual(byCharacters(text, max));
    }
  });

  it('keeps a word that ends exactly at the limit in its piece', () => {
    expect(capPiece('aa bb cc', 5)).toEqual(['aa bb', 'cc']);
    expect(capPiece('aa bb, cc', 6)).toEqual(['aa bb,', 'cc']);
    expect(capPiece('aa bbb cc', 5)).toEqual(['aa', 'bbb', 'cc']);
  });

  it('reads each character a bounded number of times, however long the run', () => {
    const digits = '7'.repeat(100_000);
    const original = String.prototype.codePointAt;
    let budget = 0;
    // Stops the work at the budget, so a rescan fails here and fast.
    const read = vi.spyOn(String.prototype, 'codePointAt').mockImplementation(function (this: string, at: number) {
      if (read.mock.calls.length > budget) throw new Error(`read more than ${budget} characters`);
      return original.call(this, at);
    });
    try {
      budget = digits.length * 1.5;
      expect(capPiece(digits, MAX_SECTION_CHARS)).toHaveLength(1000);
      read.mockClear();
      // The same run as a reply: measured, split into sections and into
      // device pieces.
      budget = digits.length * 8;
      const sections = speakableSections(`\`${digits}\``);
      expect(sections).toHaveLength(1000);
      expect(sections.map((section) => section.text).join('')).toBe(digits);
    } finally {
      read.mockRestore();
    }
  });
});

describe('spokenLength', () => {
  it('counts letters once, and digits and dense scripts by how long they take to say', () => {
    expect(spokenLength('Plain words, here.')).toBe(18);
    expect(spokenLength('2026')).toBe(32);
    expect(spokenLength('２０')).toBe(16);
    expect(spokenLength('中文')).toBe(8);
    expect(spokenLength('𠀀')).toBe(4);
    expect(spokenLength('한국')).toBe(6);
    expect(spokenLength('かなカナ')).toBe(8);
    expect(spokenLength('ไทย')).toBe(3);
    expect(spokenLength('😀')).toBe(2);
  });

  it('counts a decimal digit of any script as a digit', () => {
    // Arabic-Indic, Eastern Arabic-Indic, Devanagari, Thai, mathematical bold.
    for (const digit of ['٣', '۳', '३', '๓', '𝟑']) expect(spokenLength(digit), digit).toBe(8);
    const ascii = '123456 '.repeat(100).trim();
    const sectionsOf = (text: string) => speakableSections(text).length;
    expect(sectionsOf(ascii)).toBeGreaterThan(5);
    expect(sectionsOf(ascii.replace(/\d/g, (d) => '٠١٢٣٤٥٦٧٨٩'[Number(d)]))).toBe(sectionsOf(ascii));
    expect(sectionsOf(ascii.replace(/\d/g, (d) => '०१२३४५६७८९'[Number(d)]))).toBe(sectionsOf(ascii));
  });
});
