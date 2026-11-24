// Coverage for the two `parser/extensions/alert.ts` fixes.
//
// The alert extension owns EVERY blockquote, not just the `[!NOTE]` ones:
// it is registered ahead of marked's built-ins, matches the plain blockquote
// rule, and returns a `blockquote` token when no variant marker is present.
// So both bugs below fired on ordinary agent prose, not on an exotic corner.
//
//  1. Upstream lexed each quoted body through a MODULE-LEVEL `Lexer`. Nothing
//     drains an `inlineQueue` that `Lexer.lex()` did not create, so that
//     lexer accumulated one entry per quoted paragraph for the lifetime of
//     the page (16,511 over one test corpus), and its `tokens.links` /
//     footnote maps carried reference definitions from one document into the
//     next. The fix is a blockquote-scoped lexer.
//
//  2. marked rebuilds a blockquote's `raw` from the lines it walked, and its
//     nested-list / nested-blockquote continuation branches splice the INNER
//     token's raw into the OUTER one — so `raw` came back with bytes the
//     source never had at that offset, sometimes longer than the source. Both
//     shapes terminate, so nothing crashed; what broke is the contract every
//     incremental path leans on, that a block token's `raw` names the bytes
//     it consumed.
import { Lexer } from './parser/engine';
import { describe, expect, it } from 'vitest';
import { lex } from './index';
import { markedAlert } from './parser/extensions/alert';
import { EMBEDDED_HTML_EXTENSIONS } from './parser/extensions/embeddedHtml';

/** Run the extension's tokenizer the way marked's block loop does. */
const tokenizeBlockquote = (src: string) =>
  markedAlert.tokenizer!.call({ lexer: new Lexer({ gfm: true }) }, src, []);

describe('marked-alert raw is the consumed prefix', () => {
  // Both shapes came out of a fuzz over blockquote-flavoured sources; each
  // exercises a different arm of marked's raw-splice bug.
  const shapes: ReadonlyArray<readonly [string, string]> = [
    // Nested-list continuation, no trailing newline: marked appends the
    // continuation's own newline and returns a raw LONGER than src.
    ['nested-list continuation grows the raw', '> - a\n[^a]:'],
    // Same length, different bytes: the list splice replays the stripped
    // inner raw over the `>`-prefixed outer one.
    ['nested-list splice rewrites the bytes', '>  - - \n$$\n'],
  ];

  for (const [name, src] of shapes) {
    it(name, () => {
      const token = tokenizeBlockquote(src);
      expect(token, `expected a blockquote token for ${JSON.stringify(src)}`).toBeDefined();
      const raw = token?.raw ?? '';
      expect(raw.length).toBeLessThanOrEqual(src.length);
      expect(src.startsWith(raw)).toBe(true);
      expect(raw).toBe(src.slice(0, raw.length));
    });
  }

  it('leaves well-formed blockquotes byte-identical', () => {
    // The clamp must not shorten a blockquote that marked already reported
    // correctly — including the trailing newline it deliberately rtrims.
    const cases = [
      ['> hello\n', '> hello'],
      ['> a\n> b\n\n', '> a\n> b'],
      ['> [!NOTE]\n> Careful.\n', '> [!NOTE]\n> Careful.'],
      ['> outer\n> > inner\n', '> outer\n> > inner'],
    ] as const;
    for (const [src, expected] of cases) {
      expect(tokenizeBlockquote(src)?.raw, JSON.stringify(src)).toBe(expected);
    }
  });

  it('keeps a full lex offset-aligned, which is what the raw fix is FOR', () => {
    // The tokenizer-level cases above pin one call; this pins the property
    // through `lex()`, which is the layer `incrementalLex`'s raw-offset
    // arithmetic actually reads. Walk the token stream and place each `raw`
    // back in the source: it must be findable at or after the cursor, nothing
    // but whitespace may lie between consumed spans, and the walk may never
    // run past the end.
    //
    // Both recorded pre-fix shapes fail this outright. `'> - a\n[^a]:'` came
    // back with a raw one byte LONGER than the whole source, so it is not
    // findable at all; `'>  - - \n$$\n'` came back with bytes the source never
    // had at that offset, likewise. The gaps the walk tolerates are marked's
    // own: `Tokenizer.blockquote` rtrims trailing newlines off `raw` while
    // consuming them, which is exactly what the clamp accounts for.
    const sources = [
      '> - a\n[^a]:',
      '>  - - \n$$\n',
      '> - a\n[^a]: def\n\nNext para',
      '> quoted\n> lines\n\nplain paragraph\n\n> another\n',
    ];
    for (const src of sources) {
      const tokens = lex(src) as { raw: string }[];
      let at = 0;
      for (const token of tokens) {
        const where = `${JSON.stringify(src)} :: ${JSON.stringify(token.raw)}`;
        // A zero-length raw is the fatal shape: marked's `while (src)` loops
        // advance by `raw.length` and would never terminate.
        expect(token.raw.length, where).toBeGreaterThan(0);
        const idx = src.indexOf(token.raw, at);
        expect(idx, where).toBeGreaterThanOrEqual(at);
        expect(src.slice(at, idx).trim(), where).toBe('');
        at = idx + token.raw.length;
      }
      expect(at, JSON.stringify(src)).toBeLessThanOrEqual(src.length);
    }
  });

  it('still tokenizes the alert variant and its body', () => {
    const token = tokenizeBlockquote('> [!WARNING]\n> Mind the gap.\n') as
      | { type: string; variant?: string; text?: string }
      | undefined;
    expect(token?.type).toBe('alert');
    expect(token?.variant).toBe('warning');
    expect(token?.text).toBe('Mind the gap.');
  });
});

// A quote's body is lexed by the active lexer through the engine's blockquote
// rule: a block extension claims a run that spans blank quoted lines as it
// does outside a quote, and a lazy line stays where the engine puts it.
describe('a quoted body is lexed by the active lexer', () => {
  type Node = { type: string; raw?: string; text?: string; tokens?: Node[] };
  const kinds = (tokens: Node[] | undefined) => (tokens ?? []).filter((token) => token.type !== 'space').map((token) => token.type);
  const DETAILS = '<details>\n> <summary>More</summary>\n>\n> - one\n> - two\n>\n> </details>';

  it.each([
    ['keeps a <details> run whole inside a quote',
      `> before\n>\n> ${DETAILS}\n>\n> after\n`, 'blockquote', ['paragraph', 'details', 'paragraph'], 'before'],
    ['keeps a <details> run whole inside an alert, the marker stripped',
      `> [!NOTE]\n> Processing.\n>\n> ${DETAILS}\n`, 'alert', ['paragraph', 'details'], 'Processing.'],
  ])('%s', (_name, source, type, children, firstText) => {
    const [block] = lex(source, EMBEDDED_HTML_EXTENSIONS) as Node[];
    expect(block.type).toBe(type);
    expect(kinds(block.tokens)).toEqual(children);
    expect(block.tokens?.[0].text).toBe(firstText);
    const details = block.tokens?.find((token) => token.type === 'details');
    expect(kinds(details?.tokens)).toEqual(['detailsSummary', 'list']);
  });

  it('keeps display math whole across a blank quoted line', () => {
    const [quote] = lex('> $$\n> x = 1\n>\n> y = 2\n> $$\n') as Node[];
    expect(kinds(quote.tokens)).toEqual(['math']);
  });

  it('keeps every block of a quoted body', () => {
    // One child per body block, none dropped: a paragraph, a list and code.
    const [quote] = lex('> lead\n>\n> - a\n> - b\n>\n> ```\n> code\n> ```\n') as Node[];
    expect(kinds(quote.tokens)).toEqual(['paragraph', 'list', 'code']);
  });

  it('keeps a lazy line after a non-paragraph as its own block', () => {
    // The html block ends at the quote's last `>` line; lexing the body as
    // one string would hand `y` to it.
    for (const source of ['> x\n> <div>\ny', '> [!NOTE]\n> x\n> <div>\ny']) {
      const [quote] = lex(source) as Node[];
      expect(quote.raw).toBe(source);
      expect(quote.tokens?.map((token) => [token.type, token.raw])).toEqual([
        ['paragraph', 'x\n'],
        ['html', '<div>'],
        ['paragraph', 'y']
      ]);
    }
  });

  it('builds a lazily continued nested list or quote through its extension', () => {
    // The list extension's token carries `tokens`; the engine's own carries `items`.
    const [inList] = lex('> - a\nb') as Node[];
    expect(inList.tokens?.[0]).toMatchObject({ type: 'list', raw: '- a\nb' });
    expect(inList.tokens?.[0].tokens?.map((token) => token.type)).toEqual(['list_item']);
    expect(inList.tokens?.[0]).not.toHaveProperty('items');

    const [inAlert] = lex('> [!TIP] Title\n> - a\nb\n\nout') as Node[];
    expect(inAlert).toMatchObject({ type: 'alert', raw: '> [!TIP] Title\n> - a\nb' });
    expect(inAlert.tokens?.[1].tokens?.map((token) => token.type)).toEqual(['list_item']);

    const [outer] = lex('> > [!WARNING]\n> > in\nlazy') as Node[];
    expect(outer.tokens).toHaveLength(1);
    expect(outer.tokens?.[0]).toMatchObject({ type: 'alert', raw: '> [!WARNING]\n> in\nlazy' });
    expect(outer.tokens?.[0].tokens?.map((token) => token.text)).toEqual(['in\nlazy']);
  });
});

describe('an alert marker', () => {
  type Node = { type: string; raw?: string; text?: string; tokens?: Node[] };
  const children = (source: string) => {
    const [alert, ...after] = lex(source) as Node[];
    return { alert, after, blocks: alert.tokens?.map((token) => [token.type, token.text]) };
  };

  it('goes once, whatever its case, and leaves the body as authored', () => {
    const { alert, blocks } = children('> [!note] same line [!NOTE] again\n>\n>     indented');
    expect(alert.type).toBe('alert');
    expect(blocks).toEqual([
      ['paragraph', 'same line [!NOTE] again'],
      ['code', 'indented']
    ]);
  });

  it('on a line of its own leaves the next line first, quoted or lazy', () => {
    expect(children('> [!NOTE]\n> body').blocks).toEqual([['paragraph', 'body']]);
    expect(children('> [!NOTE]\nlazy').blocks).toEqual([['paragraph', 'lazy']]);
    expect(children('> [!NOTE]\n>\n> body').blocks).toEqual([['paragraph', 'body']]);
    expect(children('> [!NOTE]').blocks).toEqual([]);
  });

  it('reports the consumed source when the quote ends before the match does', () => {
    const { alert, after } = children('> [!CAUTION]\n> ```\n> code\nafter');
    expect(alert).toMatchObject({ type: 'alert', raw: '> [!CAUTION]\n> ```\n> code\n' });
    expect(after.map((token) => [token.type, token.raw])).toEqual([['paragraph', 'after']]);
  });
});

// A corpus dense in the shapes that push onto a lexer's inline queue: every
// quoted paragraph, heading and list item is one entry.
const CORPUS = Array.from(
  { length: 40 },
  (_, i) =>
    `> [!NOTE]\n> note body ${i} with \`code\` and *emphasis*\n\n` +
    `> plain quote ${i}\n> second line\n>\n> - item a\n> - item b\n\n` +
    `> ### quoted heading ${i}\n> tail paragraph\n`,
).join('\n');

/**
 * Parse `CORPUS` `parses` times, watching which `Lexer` instances the parse
 * reaches through. Any instance seen in BOTH the first parse and a later one
 * is module-level state surviving between documents — the leak, whatever
 * shape it takes.
 */
const probeSharedLexers = (parses: number) => {
  const first = new Set<unknown>();
  const later = new Set<unknown>();
  let sink = first;
  const proto = Lexer.prototype as unknown as {
    blockTokens: (this: unknown, ...args: unknown[]) => unknown;
  };
  const original = proto.blockTokens;
  proto.blockTokens = function (this: unknown, ...args: unknown[]) {
    sink.add(this);
    return original.apply(this, args);
  };
  try {
    lex(CORPUS);
    sink = later;
    for (let i = 1; i < parses; i += 1) lex(CORPUS);
  } finally {
    proto.blockTokens = original;
  }
  return [...first].filter((lexer) => later.has(lexer)).length;
};

describe('marked-alert holds no lexer between documents', () => {
  it('never reaches through a lexer a previous parse used', () => {
    // Upstream: one shared lexer, its inline queue growing by ~200 entries
    // per parse. With no shared lexer there is no queue left to grow.
    expect(probeSharedLexers(6)).toBe(0);
  });
});
