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
import { Lexer } from 'marked';
import { describe, expect, it } from 'vitest';
import { lex } from './index';
import { markedAlert } from './parser/extensions/alert';

/** Run the extension's tokenizer the way marked's block loop does. */
const tokenizeBlockquote = (src: string) =>
  markedAlert.tokenizer.call({ lexer: new Lexer({ gfm: true }) }, src, []);

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
  type Probe = { inlineQueue: unknown[] };
  const first = new Set<Probe>();
  const later = new Set<Probe>();
  let sink = first;
  const proto = Lexer.prototype as unknown as {
    blockTokens: (this: unknown, ...args: unknown[]) => unknown;
  };
  const original = proto.blockTokens;
  proto.blockTokens = function (this: unknown, ...args: unknown[]) {
    sink.add(this as Probe);
    return original.apply(this, args);
  };
  try {
    lex(CORPUS);
    sink = later;
    for (let i = 1; i < parses; i += 1) lex(CORPUS);
  } finally {
    proto.blockTokens = original;
  }
  const shared = [...first].filter((lexer) => later.has(lexer));
  return {
    shared: shared.length,
    queued: shared.reduce((total, lexer) => total + lexer.inlineQueue.length, 0),
  };
};

describe('marked-alert holds no lexer between documents', () => {
  it('never reaches through a lexer a previous parse used', () => {
    expect(probeSharedLexers(6).shared).toBe(0);
  });

  it('retains no inline-queue entries, and retains no more of them as parses accumulate', () => {
    const short = probeSharedLexers(2);
    const long = probeSharedLexers(30);
    // Upstream: one shared lexer, its queue growing by ~200 entries per parse.
    expect(short.queued).toBe(0);
    expect(long.queued).toBe(short.queued);
  });
});
