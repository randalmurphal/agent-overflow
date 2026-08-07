import { type MarkedToken, type Token, type TokenizerStartFunction, type TokenizerThis, type Tokens, type TokensList } from 'marked';
import { type AlertToken } from './marked-alert.js';
import { type FootnoteToken } from './marked-footnotes.js';
import { type MathToken } from './marked-math.js';
import { type SubSupToken } from './marked-subsup.js';
import { type ListItemToken, type ListToken } from './marked-list.js';
import { type BrToken } from './marked-br.js';
import { type HrToken } from './marked-hr.js';
import { type TableToken, type THead, type TBody, type TFoot, type THeadRow, type TRow, type TH, type TD } from './marked-table.js';
import { type DescriptionDetailToken, type DescriptionListToken, type DescriptionTermToken, type DescriptionToken } from './marked-dl.js';
import { type AlignToken } from './marked-align.js';
import { type CitationToken } from './marked-citations.js';
import { type MdxToken } from './marked-mdx.js';
export type GenericToken = {
    type: string;
    raw: string;
    tokens?: Token[];
} & Record<string, any>;
export type Extension = {
    name: string;
    level: 'block' | 'inline';
    tokenizer: (this: TokenizerThis, src: string, tokens: Token[] | TokensList) => GenericToken | undefined;
    start?: TokenizerStartFunction;
    applyInBlockParsing?: boolean;
};
export type StreamdownToken = Exclude<MarkedToken, Tokens.List | Tokens.ListItem | Tokens.Table> | ListToken | ListItemToken | MathToken | AlertToken | FootnoteToken | SubSupToken | BrToken | HrToken | TableToken | THead | TBody | TFoot | THeadRow | TRow | TH | TD | DescriptionListToken | DescriptionToken | DescriptionDetailToken | DescriptionTermToken | AlignToken | CitationToken | MdxToken;
export type { TableToken, THead, TBody, TFoot, THeadRow, TRow, TH, TD } from './marked-table.js';
export declare const lex: (markdown: string, extensions?: Extension[]) => StreamdownToken[];
/**
 * Opaque incremental state for `parseBlocks`. Create one per Streamdown
 * instance (or per simulated stream) and pass it on every call: append-only
 * content updates then re-lex only the last couple of blocks instead of the
 * whole document. Any non-append update falls back to a full parse.
 */
export type ParseBlocksCache = {
    content: string;
    /** every block token's raw (including space/footnote tokens) in document order */
    raws: string[];
    /** parallel to raws: whether the token is part of the rendered block list */
    keep: boolean[];
    /** descent record when the last rendered block is a list or table with a sealed prefix */
    trailingBlock: {
        kind: 'list';
        rawIndex: number;
        blockStart: number;
        sealedLen: number;
    } | {
        kind: 'table';
        rawIndex: number;
        blockStart: number;
        prefixLen: number;
        lastRowStart: number;
    } | null;
};
export declare const createParseBlocksCache: () => ParseBlocksCache;
export declare const parseBlocks: (markdown: string, extensions?: Extension[], cache?: ParseBlocksCache) => string[];
/**
 * Opaque incremental state for `incrementalLex`. Create one per streamed
 * block (a Block component instance) and pass it on every call. `lastPath`
 * is a debug breadcrumb ('none' | 'full' | 'list-append' | 'table-append')
 * so tests can assert the fast path actually engaged. `links` and
 * `footnotes` carry the last full lex's reference-link table and footnote
 * maps into tail-only re-lexes.
 */
export type IncrementalLexCache = {
    src: string;
    extKey: Extension[] | null;
    tokens: StreamdownToken[] | null;
    links: Record<string, {
        href: string | null;
        title: string | null;
    }> | null;
    footnotes: unknown;
    lastPath: 'none' | 'full' | 'list-append' | 'table-append';
};
export declare const createIncrementalLexCache: () => IncrementalLexCache;
/**
 * Drop-in replacement for `lex` on streaming content: identical output, but
 * an append-only update to a document whose single block is a list or table
 * re-lexes only from the last cached item / source row, and sealed content
 * keeps its token references. `complete` (the incomplete-markdown pass)
 * runs inside so the fast path can scope it to the re-lexed slice; pass
 * null to lex verbatim.
 */
export declare const incrementalLex: (block: string, extensions: Extension[] | undefined, cache: IncrementalLexCache, complete?: ((markdown: string) => string) | null) => StreamdownToken[];
export type { MathToken, AlertToken, FootnoteToken, SubSupToken, BrToken, HrToken, AlignToken, CitationToken, MdxToken };
