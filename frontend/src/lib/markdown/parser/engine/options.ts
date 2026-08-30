/**
 * The lexer's options bag and the extension contract it dispatches through.
 *
 * Absorbed from marked 16.4.2 (`src/MarkedOptions.ts`, MIT — see
 * `../../LICENSE`) and trimmed to the lexing half. marked's `renderer`,
 * `hooks`, `walkTokens` and `async` knobs, and the `ParserOutput` /
 * `RendererOutput` generics that existed only to type them, are gone with
 * the Parser/Renderer surface this tree never absorbed.
 */
import type { Token, Tokens, TokensList } from './tokens';
import type { Lexer } from './Lexer';

/** The receiver an extension tokenizer is invoked with. */
export interface TokenizerThis {
  lexer: Lexer;
}

export type TokenizerExtensionFunction = (
  this: TokenizerThis,
  src: string,
  tokens: Token[] | TokensList
) => Tokens.Generic | undefined;

export type TokenizerStartFunction = (this: TokenizerThis, src: string) => number | void;

export interface LexerOptions {
  /** Enable GFM line breaks. Requires `gfm`. */
  breaks?: boolean;

  /** Enable GitHub flavored markdown. */
  gfm?: boolean;

  /**
   * Conform to obscure parts of markdown.pl as much as possible. Don't fix
   * any of the original markdown bugs or poor behavior.
   */
  pedantic?: boolean;

  /** Log an infinite-loop bailout instead of throwing it. */
  silent?: boolean;

  /** Registered extension tokenizers, pre-sorted by level. */
  extensions?: null | {
    inline?: TokenizerExtensionFunction[];
    block?: TokenizerExtensionFunction[];
    startInline?: TokenizerStartFunction[];
    startBlock?: TokenizerStartFunction[];
  };
}

/** marked's original defaults, minus the options this tree dropped. */
export function getDefaultOptions(): LexerOptions {
  return {
    breaks: false,
    extensions: null,
    gfm: true,
    pedantic: false,
    silent: false,
  };
}

export const defaultOptions: LexerOptions = getDefaultOptions();
