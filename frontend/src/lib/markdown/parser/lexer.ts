/**
 * The lexer seam: marked's rule overrides, the extension registries, the
 * cached options objects, and the two lex entry points everything else in
 * this tree builds on.
 *
 * Importing this module APPLIES the rule overrides below. Every Lexer in the
 * tree is constructed from `getLexOptions`/`getBlockOptions`, so any code
 * path that lexes has already evaluated this module.
 */
import { Lexer } from 'marked';
import type { MarkedOptions, MarkedToken, Token, TokenizerStartFunction, TokenizerThis, Tokens, TokensList } from 'marked';
import type { AlertToken } from './extensions/alert';
import { footnoteLexer } from './extensions/footnotes';
import type { Footnote, FootnoteMaps, FootnoteToken } from './extensions/footnotes';
import type { MathToken } from './extensions/math';
import type { SubSupToken } from './extensions/subsup';
import type { ListItemToken, ListToken } from './extensions/list';
import type { BrToken } from './extensions/br';
import type { HrToken } from './extensions/hr';
import type { TableToken, THead, TBody, TFoot, THeadRow, TRow, TH, TD } from './extensions/table';
import type { DescriptionDetailToken, DescriptionListToken, DescriptionTermToken, DescriptionToken } from './extensions/dl';
import type { AlignToken } from './extensions/align';
import type { CitationToken } from './extensions/citations';
import type { MdxToken } from './extensions/mdx';
import { markedAlert } from './extensions/alert';
import { markedFootnote, markedFootnoteBlock } from './extensions/footnotes';
import { markedMath } from './extensions/math';
import { markedSub, markedSup } from './extensions/subsup';
import { markedList, markedListBlock } from './extensions/list';
import { markedBr } from './extensions/br';
import { markedHr } from './extensions/hr';
import { markedTable } from './extensions/table';
import { markedTableBlock } from './extensions/tableSource';
import { markedDl, markedDlBlock } from './extensions/dl';
import { markedAlign, markedAlignBlock } from './extensions/align';
import { markedCitations } from './extensions/citations';
import { markedMdx, markedMdxBlock } from './extensions/mdx';
import { markedBlockquoteBlock } from './extensions/blockquoteSource';
// marked 16's GFM del regex treats ~text~ (single tilde) as strikethrough,
// producing false positives on approximate values like ~240MB. Require ~~.
const _fixedDel = /^(~~)(?=[^\s~])((?:\\[\s\S]|[^\\])*?(?:\\[\s\S]|[^\s~\\]))\1(?=[^~]|$)/;
Lexer.rules.inline.gfm.del = Lexer.rules.inline.breaks.del = _fixedDel;
// Disable marked's mailto autolinking. Agent prose commonly uses
// labels like `composer@0.7s`; marked sees those as email addresses,
// Streamdown rejects the resulting `mailto:` URL, then renders a
// visible `[blocked]` marker. HTTP/HTTPS/FTP and `www.` autolinks
// still go through the original rules.
const _emailUrlAlternative = '|^[A-Za-z0-9._+-]+(@)[a-zA-Z0-9-_]+(?:\\.[a-zA-Z0-9-_]*[a-zA-Z0-9])+(?![-_])';
const _emailAutolinkAlternative = "|[a-zA-Z0-9.!#$%&'*+/=?_`{|}~-]+(@)[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+(?![-_])";
const _removeRegexAlternative = (re: RegExp, alternative: string): RegExp => new RegExp(re.source.replace(alternative, ''), re.flags);
Lexer.rules.inline.gfm.url = _removeRegexAlternative(Lexer.rules.inline.gfm.url, _emailUrlAlternative);
Lexer.rules.inline.breaks.url = _removeRegexAlternative(Lexer.rules.inline.breaks.url, _emailUrlAlternative);
Lexer.rules.inline.normal.autolink = _removeRegexAlternative(Lexer.rules.inline.normal.autolink, _emailAutolinkAlternative);
Lexer.rules.inline.gfm.autolink = _removeRegexAlternative(Lexer.rules.inline.gfm.autolink, _emailAutolinkAlternative);
Lexer.rules.inline.breaks.autolink = _removeRegexAlternative(Lexer.rules.inline.breaks.autolink, _emailAutolinkAlternative);
Lexer.rules.inline.pedantic.autolink = _removeRegexAlternative(Lexer.rules.inline.pedantic.autolink, _emailAutolinkAlternative);
// marked's GFM inline `text` rule begins with `[`~]+` — a COMBINED run of
// backticks AND tildes — so a `~` fused to a backtick (e.g. ~`code`) gets
// swallowed into a literal text run and the code span never opens (it can
// also mis-pair onto a later backtick, producing a stray pill mid-line).
// Split that leading class into homogeneous runs so a backtick is never
// consumed as part of a tilde run. Mirrors the _fixedDel override above.
const _fixText = (re: RegExp): RegExp => new RegExp(re.source.replace('[`~]+|[^`~]', '`+|~+|[^`~]'), re.flags);
Lexer.rules.inline.gfm.text = _fixText(Lexer.rules.inline.gfm.text);
Lexer.rules.inline.breaks.text = _fixText(Lexer.rules.inline.breaks.text);
// Default plugin sets, in registration order. Hoisted so the options object
// (and the regexes/closures inside each plugin) is built once, not per chunk.
// The tokenizers are stateless at creation time — per-document state lives on
// the Lexer instance (e.g. footnotes maps, reference-link defs), so a fresh
// Lexer per call keeps documents isolated while the options are shared.
const DEFAULT_LEX_EXTENSIONS: Extension[] = [
    markedHr,
    markedTable,
    ...markedFootnote(),
    markedAlert,
    ...markedMath,
    markedSub,
    markedSup,
    markedList,
    markedBr,
    markedDl,
    markedAlign,
    markedCitations,
    markedMdx
];
const DEFAULT_BLOCK_EXTENSIONS: Extension[] = [
    markedHr,
    markedFootnoteBlock,
    markedBlockquoteBlock,
    markedListBlock,
    markedDlBlock,
    markedTableBlock,
    markedAlignBlock,
    markedMdxBlock
];
const parseExtensions = (...extensions: Extension[]): MarkedOptions => {
    const options: LexOptions = {
        gfm: true,
        extensions: {
            block: [],
            inline: [],
            childTokens: {},
            renderers: {},
            startBlock: [],
            startInline: []
        }
    };
    extensions.forEach(({ level, name, tokenizer, ...rest }) => {
        if ('start' in rest && rest.start) {
            if (level === 'block') {
                options.extensions.startBlock.push(rest.start);
            }
            else {
                options.extensions.startInline.push(rest.start);
            }
        }
        if (tokenizer) {
            if (level === 'block') {
                options.extensions.block.push(tokenizer);
            }
            else {
                options.extensions.inline.push(tokenizer);
            }
        }
    });
    return options;
};
// Options objects are stateless and reusable across Lexer instances; cache them
// per user-extension array (props are referentially stable across chunks) so the
// hot path skips rebuilding ~20 tokenizer registrations on every streamed chunk.
const DEFAULT_LEX_OPTIONS = parseExtensions(...DEFAULT_LEX_EXTENSIONS);
export const DEFAULT_BLOCK_OPTIONS = parseExtensions(...DEFAULT_BLOCK_EXTENSIONS);
const lexOptionsCache = new WeakMap<Extension[], MarkedOptions>();
const blockOptionsCache = new WeakMap<Extension[], MarkedOptions>();
const blockExtensionsCache = new WeakMap<Extension[], Extension[] | null>();
export const blockExtensionsOf = (extensions: Extension[]): Extension[] | null => {
    if (extensions.length === 0)
        return null;
    const cached = blockExtensionsCache.get(extensions);
    if (cached !== undefined)
        return cached;
    let blockExtensions: Extension[] | null = null;
    for (const extension of extensions) {
        if (extension.level !== 'block' || !extension.applyInBlockParsing)
            continue;
        blockExtensions ??= [];
        blockExtensions.push(extension);
    }
    const result = blockExtensions?.length === extensions.length
        ? extensions
        : blockExtensions;
    blockExtensionsCache.set(extensions, result);
    return result;
};
export const sameExtensionSequence = (left: Extension[] | null, right: Extension[] | null): boolean => {
    if (left === right)
        return true;
    if (!left || !right || left.length !== right.length)
        return false;
    for (let index = 0; index < left.length; index++) {
        if (left[index] !== right[index])
            return false;
    }
    return true;
};
export const getLexOptions = (extensions: Extension[]): MarkedOptions => {
    if (extensions.length === 0)
        return DEFAULT_LEX_OPTIONS;
    let options = lexOptionsCache.get(extensions);
    if (!options) {
        options = parseExtensions(...DEFAULT_LEX_EXTENSIONS, ...extensions);
        lexOptionsCache.set(extensions, options);
    }
    return options;
};
export const getBlockOptions = (extensions: Extension[]): MarkedOptions => {
    const blockExtensions = blockExtensionsOf(extensions);
    if (!blockExtensions)
        return DEFAULT_BLOCK_OPTIONS;
    let options = blockOptionsCache.get(blockExtensions);
    if (!options) {
        options = parseExtensions(...DEFAULT_BLOCK_EXTENSIONS, ...blockExtensions);
        blockOptionsCache.set(blockExtensions, options);
    }
    return options;
};
export const lex = (markdown: string, extensions: Extension[] = []): StreamdownToken[] => {
    return new Lexer(getLexOptions(extensions))
        .lex(markdown)
        .filter((token) => token.type !== 'space' && token.type !== 'footnote') as StreamdownToken[];
};
// Divergence 29: `[^label]` → its `[^label]: body` definition, for a whole
// document. The render path cannot answer this. A definition is always its
// own block, parseBlocks splits blocks apart, and each Block lexes its
// string in its own Lexer (see incrementalLex's cross-BLOCK note), so the
// tokenizer's `ref.content` back-reference is the empty placeholder for
// every ref whose definition lives in another block — which is every real
// document, since definitions conventionally sit at the end. The definition
// tokens are also filtered out of every render list, so the body reaches no
// DOM either. One Lexer over the whole source answers it with the real
// grammar (fenced code, list nesting and blockquotes included) instead of a
// second `[^x]:` regex somewhere in the app. Returns null when the document
// declares no footnotes at all. Callers own the memo: this is a full lex,
// paid lazily on the gesture that needs it, never during render.
export const lexFootnoteDefinitions = (
    markdown: string,
    extensions: Extension[] = []
): Map<string, Footnote> | null => {
    const lexer = footnoteLexer(new Lexer(getLexOptions(extensions)));
    lexer.lex(markdown);
    return lexer.hasFootnotes ? lexer.footnotes.footnotes : null;
};
// A token the render list keeps: marked's blank-line `space` tokens and
// footnote definitions own source bytes but produce no rendered block.
export const isKeptType = (type: string): boolean => type !== 'space' && type !== 'footnote';
// lex(), but the Lexer is observable: seeded with a prior links table /
// footnote maps before the run, captured after. The public lex() stays a
// pure function; the incremental paths need the per-document state to
// carry sealed-item context into a tail-only re-lex.
export const lexCapture = (
    markdown: string,
    extensions: Extension[],
    seedLinks: LinkTable | null,
    seedFootnotes: FootnoteMaps | null
): LexCapture => {
    const lexer = footnoteLexer(new Lexer(getLexOptions(extensions)));
    if (seedLinks)
        Object.assign(lexer.tokens.links, seedLinks);
    if (seedFootnotes) {
        lexer.footnotes = seedFootnotes;
        lexer.hasFootnotes = true;
    }
    const tokens = lexer.lex(markdown).filter((token) => isKeptType(token.type)) as StreamdownToken[];
    return {
        tokens,
        links: lexer.tokens.links,
        // In component context the footnote extension uses the shared
        // Streamdown maps and never touches the lexer — null here keeps
        // the cache out of the way and the context authoritative.
        footnotes: lexer.hasFootnotes ? lexer.footnotes : null
    };
};

/**
 * marked's own generic-token contract, restated locally: an extension may hang
 * any field it likes on a token, so `Record<string, any>` is the declared
 * shape rather than a widening added here.
 */
export type GenericToken = {
    type: string;
    raw: string;
} & Record<string, any>;
export type Extension = {
    name: string;
    level: 'block' | 'inline';
    tokenizer: (this: TokenizerThis, src: string, tokens: Token[] | TokensList) => GenericToken | undefined;
    start?: TokenizerStartFunction;
    applyInBlockParsing?: boolean;
};
export type StreamdownToken = Exclude<MarkedToken, Tokens.List | Tokens.ListItem | Tokens.Table> | ListToken | ListItemToken | MathToken | AlertToken | FootnoteToken | SubSupToken | BrToken | HrToken | TableToken | THead | TBody | TFoot | THeadRow | TRow | TH | TD | DescriptionListToken | DescriptionToken | DescriptionDetailToken | DescriptionTermToken | AlignToken | CitationToken | MdxToken;
export type { TableToken, THead, TBody, TFoot, THeadRow, TRow, TH, TD } from './extensions/table';

/** marked's per-Lexer reference-link table. */
export type LinkTable = TokensList['links'];

/** What `lexCapture` reports back from an observable lex run. */
export type LexCapture = {
	tokens: StreamdownToken[];
	links: LinkTable;
	footnotes: FootnoteMaps | null;
};

/** The options object `parseExtensions` builds, with its arrays non-optional. */
type LexOptions = MarkedOptions & {
	extensions: {
		block: NonNullable<NonNullable<MarkedOptions['extensions']>['block']>;
		inline: NonNullable<NonNullable<MarkedOptions['extensions']>['inline']>;
		childTokens: NonNullable<MarkedOptions['extensions']>['childTokens'];
		renderers: NonNullable<MarkedOptions['extensions']>['renderers'];
		startBlock: NonNullable<NonNullable<MarkedOptions['extensions']>['startBlock']>;
		startInline: NonNullable<NonNullable<MarkedOptions['extensions']>['startInline']>;
	};
};
export type { MathToken, AlertToken, FootnoteToken, SubSupToken, BrToken, HrToken, AlignToken, CitationToken, MdxToken };
