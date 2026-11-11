import { describe, expect, it } from 'vitest';
import { parseIncompleteMarkdown } from './parser/incompleteMarkdown';
import { createParseBlocksCache, createProvenAppend, lex, parseBlocks } from './index';
import { splitAtBoundary } from './boundary/split';
import { StreamingBoundarySplitter } from './boundary';
import {
  __setAlignmentEnabledForTest,
  alignmentAfter,
  isAlignmentMarkerBlock,
  shortLastLine,
  nextShortLastLine,
  withoutPartialAlignmentCloser,
} from './parser/extensions/align';
import { NOT_ALIGNMENT_CORPUS } from '../../test/helpers/alignmentCorpus';


const withoutRaw = (value: unknown): unknown =>
  JSON.parse(JSON.stringify(value, (key, nested) => (key === 'raw' ? undefined : nested)));

const random = (seed: number) => () => {
  seed = (seed * 1103515245 + 12345) & 0x7fffffff;
  return seed / 0x7fffffff;
};

const chunkings = (text: string, seed: number): string[] => {
  const next = random(seed);
  const out: string[] = [];
  for (let end = 0; end < text.length;) {
    end = Math.min(text.length, end + 1 + Math.floor(next() * 8));
    out.push(text.slice(0, end));
  }
  return out;
};

/** Everything the streaming pipeline derives from one source. */
const pipeline = (prefixes: string[]) => {
  const cache = createParseBlocksCache();
  const splitter = new StreamingBoundarySplitter();
  let previous = '';
  return prefixes.map((prefix) => {
    const append = createProvenAppend(previous, prefix.slice(previous.length));
    previous = prefix;
    const split = splitter.split(prefix, append);
    return {
      prefix,
      cached: [...parseBlocks(prefix, [], cache, append)],
      fresh: parseBlocks(prefix),
      tokens: withoutRaw(lex(prefix)),
      completed: parseIncompleteMarkdown(prefix),
      committed: split.prefix,
      tail: split.tail,
    };
  });
};

const withAlignment = <T>(enabled: boolean, run: () => T): T => {
  __setAlignmentEnabledForTest(enabled);
  try {
    return run();
  } finally {
    __setAlignmentEnabledForTest(true);
  }
};

describe('alignment markers in streaming', () => {
  it('leaves a closer-like line in a fence to the fence', () => {
    expect(parseIncompleteMarkdown('```text\n[center]\n[/cen')).toBe('```text\n[center]\n[/cen\n```');
  });

  it('commits past an opener-like line in indented code', () => {
    expect(splitAtBoundary('    [center]\n\nAfter.\n\nMore').prefix).toBe('    [center]\n\nAfter.\n\n');
  });

  it.each([...NOT_ALIGNMENT_CORPUS.map((doc) => [doc]), [NOT_ALIGNMENT_CORPUS.join('\n\n')]])(
    'parses text that is no marker exactly as with alignment off: %j',
    (doc) => {
      const runs = [
        Array.from({ length: doc.length }, (_, i) => doc.slice(0, i + 1)),
        ...[1, 2, 3].map((seed) => chunkings(doc, seed)),
      ];
      for (const prefixes of runs) {
        const on = withAlignment(true, () => pipeline(prefixes));
        const off = withAlignment(false, () => pipeline(prefixes));
        on.forEach((step, i) => expect(step, JSON.stringify(step.prefix)).toEqual(off[i]));
      }
    },
  );

  it('splits markers into blocks of their own, with the alignment in force folded across them', () => {
    const source = '[center]\nA\n[/center]\n\nB\n\n[right]\n- x\n[/right]\nC';
    const blocks = parseBlocks(source);
    let align: 'center' | 'right' | null = null;
    const shown: [string, string | null][] = [];
    const markers: string[] = [];
    for (const block of blocks) {
      if (isAlignmentMarkerBlock(block, align)) markers.push(block.trim());
      else shown.push([block.trim(), align]);
      align = alignmentAfter(block, align);
    }
    expect(markers).toEqual(['[center]', '[/center]', '[right]', '[/right]']);
    expect(shown).toEqual([['A', 'center'], ['B', null], ['- x', 'right'], ['C', null]]);
  });

  it('treats a closer of an alignment not in force as text', () => {
    expect(parseBlocks('[center]\nA\n[/right]\nB')).toEqual(['[center]\n', 'A\n[/right]\nB']);
    expect(isAlignmentMarkerBlock('[/center]\n', null)).toBe(false);
    expect(isAlignmentMarkerBlock('[/center]\n', 'right')).toBe(false);
  });

  it('holds back only an incomplete closer of the alignment in force', () => {
    const held = (block: string, align: 'center' | 'right') =>
      withoutPartialAlignmentCloser(block, align, shortLastLine(block));
    expect(held('text\n[/cen', 'center')).toBe('text\n');
    expect(held('[', 'right')).toBe('');
    expect(held('text\n[/center]\r', 'center')).toBe('text\n');
    expect(held('text\n[/center]', 'center')).toBe('text\n');
    expect(held('text\n[/ri', 'center')).toBe('text\n[/ri');
    expect(held('text\n[/center]x', 'center')).toBe('text\n[/center]x');
    expect(held('text [/cen', 'center')).toBe('text [/cen');
  });

  it('follows the last line through appends without the whole source', () => {
    let line = shortLastLine('words');
    let source = 'words';
    for (const delta of [' more', '\n[', '/cen', 'ter]', '\n', 'a long line past any closer']) {
      line = nextShortLastLine(line, delta);
      source += delta;
      expect(line, JSON.stringify(source)).toBe(shortLastLine(source));
    }
  });

  it('lexes each paragraph appended inside an open block once, not the whole block again', () => {
    let lexed = 0;
    const cache = createParseBlocksCache((_path, length) => { lexed += length; });
    const paragraph = 'A paragraph of aligned words that keeps streaming.\n\n';
    let source = '[center]\n';
    parseBlocks(source, [], cache);
    const count = 200;
    for (let i = 0; i < count; i++) {
      for (const word of paragraph.split(/(?<= )/)) {
        const append = createProvenAppend(source, word);
        source = append.next;
        parseBlocks(source, [], cache, append);
      }
    }
    // Each append re-lexes at most the last couple of blocks.
    expect(lexed).toBeLessThan(count * paragraph.length * 30);
  });
});
