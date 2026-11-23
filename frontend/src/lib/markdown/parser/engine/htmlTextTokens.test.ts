import { describe, expect, it } from 'vitest';
import { Lexer } from './Lexer';
import type { HtmlTextExtension } from './Lexer';

// `Lexer.htmlTextTokens`: a text run of HTML as the tokens the given inline
// extensions claim and text between, with no Markdown rule.

/** Claims `@name`; reports where one may open. Counts its calls. */
function mentions() {
  const calls = { tokenizer: 0, start: 0 };
  const extension: HtmlTextExtension = {
    tokenizer(src) {
      calls.tokenizer++;
      const match = /^@[a-z]+/.exec(src);
      return match ? { type: 'mention', raw: match[0] } : undefined;
    },
    start(src) {
      calls.start++;
      const at = src.indexOf('@');
      return at === -1 ? undefined : at;
    },
  };
  return { extension, calls };
}

/** Never claims; reports a candidate at every `:`. */
const colons: HtmlTextExtension = {
  tokenizer: () => undefined,
  start(src) {
    const at = src.indexOf(':');
    return at === -1 ? undefined : at;
  },
};

describe('Lexer.htmlTextTokens', () => {
  it('returns the claims and the text between, whose raws sum to the run', () => {
    const { extension } = mentions();
    const run = 'hi @ann and *@bob* [x](y) @ end';
    const tokens = new Lexer().htmlTextTokens(run, [extension], true)!;
    expect(tokens.map((token) => [token.type, token.raw])).toEqual([
      ['text', 'hi '],
      ['mention', '@ann'],
      ['text', ' and *'],
      ['mention', '@bob'],
      ['text', '* [x](y) @ end'],
    ]);
    expect(tokens.map((token) => token.raw).join('')).toBe(run);
  });

  it('returns null for a run nothing claims, after one scan per extension', () => {
    const { extension, calls } = mentions();
    expect(new Lexer().htmlTextTokens('plain *text* with no candidate', [extension], true)).toBeNull();
    expect(calls).toEqual({ tokenizer: 1, start: 1 });
  });

  it('scans for each extension once per candidate of its own', () => {
    const { extension, calls } = mentions();
    // Many candidates of another extension, none of this one's.
    const run = 'a: '.repeat(2000) + '@end';
    const tokens = new Lexer().htmlTextTokens(run, [colons, extension], true)!;
    expect(tokens.at(-1)).toMatchObject({ type: 'mention', raw: '@end' });
    expect(calls.start).toBeLessThanOrEqual(2);
  });

  it('sets inHtmlText, and inLink unless linkable, for the run only', () => {
    const lexer = new Lexer();
    const seen: [boolean, boolean][] = [];
    const probe: HtmlTextExtension = {
      tokenizer() {
        seen.push([this.lexer.state.inHtmlText, this.lexer.state.inLink]);
        return undefined;
      },
    };
    lexer.htmlTextTokens('x', [probe], true);
    lexer.htmlTextTokens('x', [probe], false);
    expect(seen).toEqual([[true, false], [true, true]]);
    expect(lexer.state).toMatchObject({ inHtmlText: false, inLink: false });
  });
});
