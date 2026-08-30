/**
 * The markdown renderer's public surface.
 *
 * Exactly what the app imports and nothing more: the barrel is the seam
 * between `chat/`'s hosts and this tree, so a symbol reaching it is a
 * deliberate widening. Everything else stays module-local — reach into
 * `parser/` or `render/` directly only from inside this directory.
 */
export { default as Streamdown } from './render/Streamdown.svelte';
export { useStreamdown } from './render/context.svelte';
export type { Theme } from './render/theme';
export type { Footnote } from './parser/extensions/footnotes';
export {
  Lexer,
  lex,
  lexFootnoteDefinitions,
  parseBlocks,
  createParseBlocksCache,
  updateParseBlockStringMaterialization,
  incrementalLex,
  createIncrementalLexCache,
  createProvenAppend,
  createMaterializedProvenAppend,
  matchesProvenAppend,
} from './parser/index';
export type {
  Extension,
  LexerOptions,
  Token,
  Tokens,
  TokensList,
  StreamdownToken,
  ParseBlocksCache,
  ParseBlocksLexPath,
  IncrementalLexCache,
  ProvenAppend,
} from './parser/index';
export { parseIncompleteMarkdown } from './parser/incompleteMarkdown';
export { acquireDocumentInteraction } from './render/documentInteraction';
export type { DocumentInteraction } from './render/documentInteraction';
export { attachStreamdownLiteralHost, streamdownLiteralHostOf } from './render/literalHost';
export type {
  StreamdownLiteralHost,
  StreamdownLiteralHostHandle,
} from './render/literalHost';
