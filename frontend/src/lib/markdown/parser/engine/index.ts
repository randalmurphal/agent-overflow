/**
 * The absorbed markdown engine: marked 16.4.2's lexing half as first-party
 * TypeScript. See `Lexer.ts` for what diverges and why, and `../../LICENSE`
 * for the MIT notice this code carries.
 *
 * Nothing outside `parser/` should import from here — `parser/lexer.ts` owns
 * the configured entry points (`lex`, `lexCapture`, `lexFootnoteDefinitions`)
 * and the extension registries.
 */
export { Lexer } from './Lexer';
export { Tokenizer } from './Tokenizer';
export { block, inline, other } from './rules';
export type { Rules } from './rules';
export type {
  LexerOptions,
  TokenizerThis,
  TokenizerExtensionFunction,
  TokenizerStartFunction,
} from './options';
export type { Links, MarkedToken, Token, Tokens, TokensList } from './tokens';
