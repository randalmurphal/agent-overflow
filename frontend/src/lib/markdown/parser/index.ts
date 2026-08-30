/**
 * The parser's internal barrel: what `render/` and the public
 * `markdown/index.ts` are allowed to see.
 *
 * Re-exports only. The modules behind it are the real seams:
 *
 * | module                    | owns                                        |
 * |---------------------------|---------------------------------------------|
 * | `engine/`                 | the absorbed marked 16.4.2 lexer + grammar   |
 * | `lexer.ts`                | extension registries, cached options, `lex` |
 * | `provenAppend.ts`         | the append-lineage proof both layers key on |
 * | `geometry.ts`             | append-safety predicates shared by both layers |
 * | `parseBlocks.ts`          | document -> outer block boundaries           |
 * | `parseBlocks.cache.ts`    | its raw/block arrays + trailing-block record |
 * | `incrementalLex.ts`       | block -> token tree, open-fence fast path    |
 * | `incrementalLex.cache.ts` | its per-Block state + trim bounds            |
 * | `incrementalLex.merge.ts` | list/table tail splicing + token reuse       |
 * | `incompleteMarkdown.ts`   | the streaming-tail completers                |
 */
export { lex, lexFootnoteDefinitions } from './lexer';
export type { Extension, GenericToken, StreamdownToken } from './lexer';
export { Lexer } from './engine';
export type { LexerOptions, Token, Tokens, TokensList } from './engine';
export type {
  MathToken,
  AlertToken,
  FootnoteToken,
  SubSupToken,
  BrToken,
  HrToken,
  AlignToken,
  CitationToken,
  MdxToken,
  TableToken,
  THead,
  TBody,
  TFoot,
  THeadRow,
  TRow,
  TH,
  TD
} from './lexer';
export {
  createProvenAppend,
  createMaterializedProvenAppend,
  matchesProvenAppend
} from './provenAppend';
export type { ProvenAppend } from './provenAppend';
export { parseBlocks } from './parseBlocks';
export {
  createParseBlocksCache,
  updateParseBlockStringMaterialization
} from './parseBlocks.cache';
export type {
  ParseBlocksCache,
  ParseBlocksLexPath,
  ParseBlocksLexObserver
} from './parseBlocks.cache';
export { incrementalLex } from './incrementalLex';
export { createIncrementalLexCache } from './incrementalLex.cache';
export type {
  IncrementalLexCache,
  IncrementalLexObserver,
  IncrementalLexPath
} from './incrementalLex.cache';
