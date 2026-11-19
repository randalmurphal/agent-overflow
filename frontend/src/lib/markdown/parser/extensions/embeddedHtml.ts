/**
 * Opt-in embedded-HTML support for forge-authored markdown (PR/MR
 * descriptions, review-thread comments, bot reports).
 *
 * Deliberately NOT in the default extension sets: agent chat keeps
 * `renderHtml={false}` and renders no HTML at all. A surface that opts in
 * registers the extensions of `embeddedHtmlFor(forge)` and passes
 * `render/htmlSanitize.ts#sanitizeEmbeddedHtmlToken` as `renderHtml` with
 * its `policy` (htmlPolicy.ts), the one element table both paths read, so
 * every html token has exactly one of three fates:
 *
 *  - a STRUCTURAL token rendered by our own elements — `<details>` /
 *    `<summary>` become container tokens whose children are the HTML on
 *    their HTML block's lines and the Markdown after it, and the safe
 *    inline pairs map onto the native inline tokens
 *    (`<b>` → strong, `<a href>` → link, `<img>` and a `<picture>`'s
 *    fallback → image), which is what routes their hrefs/srcs through the
 *    existing render-layer URL policy; a safe inline tag with no native
 *    token (`<ins>`, `<mark>`, `<kbd>`, …) is an `htmlInline` token, and
 *    an unclosed one runs to the end of its inline run;
 *  - a SANITIZED fragment (raw HTML tables, badge rows, a stray inline
 *    tag) via the sanitizer, which keeps what a forge keeps: an element
 *    off its allowlist shows its children, and a script, style or svg
 *    nothing.
 *
 * Tags and attributes are read by `htmlTags.ts`; where an element closes
 * comes from one pairing per run (`htmlRuns.ts`).
 */
import type { Extension } from '../lexer';
import type { Lexer, Token, Tokens } from '../engine';
import { decodeHtmlEntities, markedHtmlEntities } from './htmlEntities';
import { htmlBlockStart, htmlFragment, markedHtmlRegion } from './htmlRegion';
import { attributeOf, readTag, scanTags, type HtmlTag } from './htmlTags';
import { ANY_FORGE_HTML_POLICY, htmlPolicyOf, imageAlign, ruleAttributes, type HtmlPolicy } from './htmlPolicy';
import type { DocumentForge } from '../documentScope';
import { pictureSources, type ColorSchemeSources } from './themedImages';
import {
	bindBlockInside,
	bindInlineInside,
	boundBlockRun,
	closeOf,
	htmlBlockEnd,
	htmlBodyTokens,
	inlineRun,
	isMeasuringBlocks,
	isMeasuringInline,
	measureBlockRun,
	measuredHtmlBlock,
	pairOf,
	recordInlineTag,
	type BlockRun
} from './htmlRuns';

export type DetailsToken = {
	type: 'details';
	raw: string;
	/** The authored `open` attribute: render expanded. */
	open: boolean;
	/** `[detailsSummary?, ...body block tokens]`. */
	tokens: Token[];
};

export type DetailsSummaryToken = {
	type: 'detailsSummary';
	raw: string;
	text: string;
	/** Inline tokens, or the block tokens of a summary holding several blocks. */
	tokens: Token[];
};

// A token the render list keeps (mirrors lexer.ts#isKeptType; restated
// here to avoid an import cycle through the barrel).
const kept = (token: Token): boolean => token.type !== 'space' && token.type !== 'footnote';

/**
 * A claimed element lexes its content by recursion. Past this depth (block
 * nesting for a details run, element nesting inline) a tag is left to the
 * engine's html rule, so one comment cannot exhaust the stack.
 */
export const MAX_ELEMENT_NESTING = 64;
const inlineNesting = new WeakMap<Lexer, number>();

/**
 * The content of a claimed element, `src` from `start` to `end`. The rest
 * of the HTML block holding `start` is HTML, as CommonMark reads an HTML
 * block to its blank line: Markdown syntax there is text, entities and
 * inline tags are HTML. Markdown resumes after it, as a region's does
 * (htmlRegion.ts). With nothing but whitespace on those lines, the content
 * is the blocks the rest lexes to.
 */
const elementContent = (lexer: Lexer, run: BlockRun, src: string, start: number, end: number): Token[] => {
	const first = Math.min(htmlBlockEnd(run, src, start) ?? start, end);
	if (/\S/.test(src.slice(start, first))) {
		if (first < end) return [htmlFragment(lexer, run, src, start, first, end)];
		const html = src.slice(start, end);
		return [{ type: 'html', block: true, pre: false, raw: html, text: html } satisfies Tokens.HTML];
	}
	const body: Token[] = [];
	bindBlockInside(lexer, body, run, src, end, false);
	return htmlBodyTokens(lexer, src.slice(start, end), body).filter(kept);
};

/**
 * A `<details>` run, from its start tag through the `</details>` that
 * closes it, as one token whose summary and body are element content
 * (`elementContent`). The end tag is the one
 * the run's pairing of HTML blocks gives (htmlRuns.ts), so one in a fence,
 * a comment, a script or a paragraph does not end it.
 */
export const markedDetails: Extension = {
	name: 'details',
	level: 'block',
	// The document→blocks splitter must consume a whole details run as ONE
	// block: the dropdown's body spans blank lines, and a split at the
	// first one would strand the closing tag in a later block.
	applyInBlockParsing: true,
	reopensSealedBlocks: true,
	tokenizer(src, tokens) {
		const at = htmlBlockStart(src);
		if (at === -1) return undefined;
		if (isMeasuringBlocks(this.lexer)) return measuredHtmlBlock(this.lexer, src);
		const open = readTag(src, at);
		if (!open || open.closing || open.name !== 'details' || this.lexer.blockDepth > MAX_ELEMENT_NESTING) return undefined;
		const run = boundBlockRun(this.lexer, src, tokens as Token[]) ?? measureBlockRun(this.lexer, src, tokens as Token[]);
		const close = closeOf(run, src, at);
		// Unclosed (a truncated comment): claim nothing. A region or the
		// engine's html rule takes it, and the sanitizer degrades it.
		if (!close) return undefined;
		// The rest of the end tag's line, so no html token is minted for
		// trailing whitespace.
		const trailing = /^[ \t]*(?:\r?\n|$)/.exec(src.slice(close.end));
		const raw = src.slice(0, close.end + (trailing ? trailing[0].length : 0));

		const children: Token[] = [];
		let bodyStart = open.end;
		// The summary is the first element; whitespace and comments before
		// it show nothing.
		let summaryAt = open.end;
		for (;;) {
			while (summaryAt < close.start && /\s/.test(src[summaryAt])) summaryAt++;
			if (!src.startsWith('<!--', summaryAt)) break;
			const commentEnd = src.indexOf('-->', summaryAt + 4);
			if (commentEnd === -1 || commentEnd + 3 > close.start) break;
			summaryAt = commentEnd + 3;
		}
		const summary = summaryAt < close.start ? readTag(src, summaryAt) : null;
		if (summary && !summary.closing && summary.name === 'summary') {
			const summaryClose = closeOf(run, src, summaryAt);
			if (summaryClose && summaryClose.end <= close.start) {
				const content = elementContent(this.lexer, run, src, summary.end, summaryClose.start);
				children.push({
					type: 'detailsSummary',
					raw: src.slice(open.end, summaryClose.end),
					text: src.slice(summary.end, summaryClose.start).trim(),
					// A lone paragraph (GitLab's Markdown summary, set apart by
					// blank lines) is its inline tokens.
					tokens: content.length === 1 && content[0].type === 'paragraph' ? (content[0] as Tokens.Paragraph).tokens : content
				} as DetailsSummaryToken);
				bodyStart = summaryClose.end;
			}
		}
		children.push(...elementContent(this.lexer, run, src, bodyStart, close.start));
		return { type: 'details', raw, open: attributeOf(open, 'open') !== null, tokens: children } as DetailsToken;
	}
};

// ---------------------------------------------------------------------------
// Inline safe-tag pairs
// ---------------------------------------------------------------------------

// Tag → the native inline token type it maps to. `codespan` targets take
// their inner source as literal text; the rest recurse into inline
// markdown so nesting (`<b><i>…</i></b>`, links inside bold) works.
const INLINE_PAIR_TARGETS: Record<string, 'strong' | 'em' | 'del' | 'codespan' | 'sub' | 'sup'> = {
	b: 'strong',
	strong: 'strong',
	i: 'em',
	em: 'em',
	del: 'del',
	s: 'del',
	strike: 'del',
	code: 'codespan',
	tt: 'codespan',
	samp: 'codespan',
	sub: 'sub',
	sup: 'sup'
};

/**
 * A kept inline element with no native markdown token, or one whose native
 * token cannot carry the attributes the policy keeps on it: it renders as
 * itself around its inline children, with those attributes (`<abbr
 * title>`, `<span dir lang>`, `<bdo dir>`). A `<span>` is here so its
 * content is a child list: the engine appends later text to a bare `text`
 * token. A `<kbd>` holds Markdown and nests, as on the forges.
 */
export type HtmlInlineToken = {
	type: 'htmlInline';
	raw: string;
	tag: HtmlInlineTag;
	/** The attributes the policy keeps, as authored. */
	attrs: Readonly<Record<string, string>>;
	text: string;
	tokens: Token[];
};

export const HTML_INLINE_TAGS = [
	'ins', 'mark', 'small', 'abbr', 'q', 'var', 'cite', 'dfn', 'span', 'kbd', 'ruby', 'rt', 'rp', 'time', 'bdo',
	'b', 'strong', 'i', 'em', 'del', 's', 'strike', 'sub', 'sup'
] as const;
export type HtmlInlineTag = (typeof HTML_INLINE_TAGS)[number];
const isHtmlInlineTag = (tag: string): tag is HtmlInlineTag =>
	(HTML_INLINE_TAGS as readonly string[]).includes(tag);

/** The element pairs a policy's inline tokenizer claims: the ones it keeps
 * that have an inline form, and the ones it removes with their content (one
 * inline html token for the sanitizer, so a script's source is not lexed as
 * Markdown). */
interface InlineClaims {
	readonly paired: ReadonlySet<string>;
	/** `paired`, and the image forms. */
	readonly claimed: ReadonlySet<string>;
}
const INLINE_FORMS: readonly string[] = [...Object.keys(INLINE_PAIR_TARGETS), ...HTML_INLINE_TAGS, 'a'];
const inlineClaims = (policy: HtmlPolicy): InlineClaims => {
	const paired = new Set<string>();
	for (const [name, rule] of policy.elements) {
		if (rule === 'remove' || INLINE_FORMS.includes(name)) paired.add(name);
	}
	return { paired, claimed: new Set([...paired, 'picture', 'img']) };
};

let inert: Document | undefined;
/**
 * `html` as the HTML parser reads it, in a document where nothing loads or
 * runs. The sanitizer parses a block the same way, so an attribute's value
 * (`href="?a=1&amp;b=2"` names `&`) and a `<picture>`'s fallback are one
 * answer inline and in a block.
 */
const parseInert = (html: string): DocumentFragment => {
	inert ??= document.implementation.createHTMLDocument('');
	const template = inert.createElement('template');
	template.innerHTML = html;
	return template.content;
};

/** The element a start tag opens, for its attributes. */
const elementOf = (src: string, tag: HtmlTag): Element | null => parseInert(src.slice(tag.start, tag.end)).firstElementChild;

/**
 * The image token an `<img>` becomes. `width`, `height` and `hspace` are
 * the authored attributes and `align` its placement under the surface's
 * policy (`imageAlign`); `render/imageSize.ts` decides what they paint as. A `<picture>` adds the src each color scheme's source
 * offers (themedImages.ts).
 */
export type EmbeddedImageToken = Tokens.Image & {
	width: string | null;
	height: string | null;
	align?: string | null;
	hspace?: string | null;
	sources?: ColorSchemeSources;
};

/** An `<img>` element as the image token of `raw`; nothing without a `src`. */
const imageToken = (policy: HtmlPolicy, raw: string, img: Element | null | undefined): EmbeddedImageToken | undefined => {
	const href = img?.getAttribute('src');
	if (!img || href === null || href === undefined) return undefined;
	return {
		type: 'image',
		raw,
		href,
		title: img.getAttribute('title'),
		text: img.getAttribute('alt') ?? '',
		tokens: [],
		width: img.getAttribute('width'),
		height: img.getAttribute('height'),
		align: imageAlign(policy, img),
		hspace: img.getAttribute('hspace')
	};
};

/**
 * A `<picture>` as the image of its fallback `<img>`: the first `<img>`
 * element inside it, which is the one the sanitizer's `querySelector('img')`
 * takes from the parsed fragment, with the src each color scheme's
 * `<source>` offers. A renderer given the reader's scheme paints that src,
 * as GitHub does; without one, the fallback, as GitLab does. A picture
 * holding another `<picture>` tag is not read.
 */
const pictureToken = (policy: HtmlPolicy, src: string, open: HtmlTag): EmbeddedImageToken | undefined => {
	let close: HtmlTag | undefined;
	scanTags(src.slice(open.end), (tag) => {
		if (tag.name !== 'picture') return;
		if (tag.closing) close = tag;
		return false;
	});
	if (!close) return undefined;
	const raw = src.slice(0, open.end + close.end);
	const picture = parseInert(raw).querySelector('picture');
	const image = imageToken(policy, raw, picture?.querySelector('img'));
	if (!image || !picture) return image;
	const sources = pictureSources(picture);
	return sources.dark === undefined && sources.light === undefined ? image : { ...image, sources };
};

/** The attributes `policy` keeps on the element `tag` opens, an `<a>`'s
 * `href` aside (its link token holds that). */
const inlineAttributes = (policy: HtmlPolicy, src: string, tag: HtmlTag): Record<string, string> => {
	const rule = policy.elements.get(tag.name);
	if (tag.attrs.length === 0 || rule === undefined || rule === 'remove') return {};
	const element = elementOf(src, tag);
	return element ? Object.fromEntries(ruleAttributes(element, rule.filter((name) => name !== 'href'))) : {};
};

/** The inline tokenizer that pairs the elements `policy` keeps or removes. */
const embeddedInlineHtml = (policy: HtmlPolicy): Extension => {
	const { paired, claimed } = inlineClaims(policy);
	return {
		name: 'embeddedInlineHtml',
		level: 'inline',
		tokenizer(src, tokens) {
			if (src.charCodeAt(0) !== 60 /* < */) return undefined;
			const tag = readTag(src, 0);
			if (!tag || !claimed.has(tag.name)) return undefined;
			// An image's destination is decided as a Markdown image's is.
			const decided = (image: EmbeddedImageToken | undefined) => image && this.lexer.tokenizer.destination(image);
			if (tag.name === 'img') return tag.closing ? undefined : decided(imageToken(policy, src.slice(0, tag.end), elementOf(src, tag)));
			if (tag.name === 'picture') return tag.closing ? undefined : decided(pictureToken(policy, src, tag));
			if (isMeasuringInline(this.lexer)) {
				recordInlineTag(this.lexer, src, tokens as Token[], tag);
				return undefined;
			}
			const nesting = inlineNesting.get(this.lexer) ?? 0;
			if (tag.closing || nesting >= MAX_ELEMENT_NESTING) return undefined;
			const run = inlineRun(this.lexer, src, tokens as Token[], paired);
			const removed = policy.elements.get(tag.name) === 'remove';
			// Unclosed, an element runs to the end of its inline run, as a
			// forge's HTML parser closes it at its paragraph's end. An unclosed
			// removed element is a stray tag the engine's tag rule takes, so the
			// text after it still shows.
			const close = pairOf(run, src) ?? (removed ? undefined : { start: src.length, end: src.length });
			if (!close) return undefined;
			const raw = src.slice(0, close.end);
			if (removed) {
				return { type: 'html', raw, text: raw, block: false, inLink: this.lexer.state.inLink, inRawBlock: false } satisfies Tokens.Tag;
			}
			const inner = src.slice(tag.end, close.start);
			const content = (): Token[] => {
				const out: Token[] = [];
				bindInlineInside(out, run, src, close.start);
				inlineNesting.set(this.lexer, nesting + 1);
				try {
					return this.lexer.inlineTokens(inner, out);
				} finally {
					inlineNesting.set(this.lexer, nesting);
				}
			};

			if (tag.name === 'a') {
				const anchor = elementOf(src, tag);
				const href = anchor?.getAttribute('href') ?? null;
				const state = this.lexer.state;
				if (href !== null && !state.inLink && !state.inLinkText) {
					// Its content is link text, as the engine's own link's is,
					// and a Markdown link in it is the link instead, as a forge's
					// HTML parser closes an anchor where a nested one opens.
					const linkInText = state.linkInText;
					state.inLink = true;
					state.inLinkText = true;
					state.linkInText = false;
					let children: Token[];
					let nested: boolean;
					try {
						children = content();
					} finally {
						nested = state.linkInText;
						state.inLink = false;
						state.inLinkText = false;
						state.linkInText = linkInText;
					}
					if (!nested) return this.lexer.tokenizer.destination<Tokens.Link>({ type: 'link', raw, href, title: anchor?.getAttribute('title') ?? null, text: inner, tokens: children });
				}
			}
			// An anchor with no `href` (a named jump target), one inside a link,
			// or one around a link, is not a link: its content renders as plain
			// text, which a bare `<span>` is.
			const element = tag.name === 'a' ? 'span' : tag.name;
			const attrs = inlineAttributes(policy, src, tag);
			const target = INLINE_PAIR_TARGETS[element];
			if (target === 'codespan') {
				// The element's text, not markdown: `<code>Vec&lt;T&gt;</code>`.
				return { type: 'codespan', raw, text: decodeHtmlEntities(inner) };
			}
			// A native token carries no attribute: an element with one it keeps
			// renders as itself.
			if (target !== undefined && Object.keys(attrs).length === 0) return { type: target, raw, text: inner, tokens: content() };
			if (!isHtmlInlineTag(element)) return undefined;
			return { type: 'htmlInline', raw, tag: element, attrs, text: inner, tokens: content() } satisfies HtmlInlineToken;
		}
	};
};

/**
 * The extensions of an embedded-HTML surface on `forge` (null: none), with
 * the policy its sanitizer runs with: one answer for both, so the inline
 * tokenizer and the sanitizer never keep different elements. Stable per
 * forge. A `<details>` run keeps its structural token; an HTML wrapper
 * around Markdown, its own or in a details body, is a region
 * (htmlRegion.ts).
 */
export const embeddedHtmlFor = (forge: DocumentForge): EmbeddedHtml => EMBEDDED_HTML[forge ?? 'any'];

export interface EmbeddedHtml {
	readonly extensions: readonly Extension[];
	readonly policy: HtmlPolicy;
}

const LINE_END = /^[ \t]*(?:\r?\n|$)/;

/**
 * A policy's `imageLines`: a lone `<img>` line at a block start is a
 * paragraph of its inline lex, so an extension that claims the image
 * (a forge upload) claims it there. Lines after it are read afresh, so
 * each image line of a run is its own paragraph. Mid-paragraph the line
 * stays paragraph text, as an HTML block cannot interrupt a paragraph.
 */
const markedImageLines: Extension = {
	name: 'htmlImageLine',
	level: 'block',
	tokenizer(src) {
		if (src.charCodeAt(0) !== 60 /* < */ || (src.charCodeAt(1) | 32) !== 105 /* i */) return undefined;
		const tag = readTag(src, 0);
		if (!tag || tag.closing || tag.name !== 'img') return undefined;
		const end = LINE_END.exec(src.slice(tag.end));
		if (!end) return undefined;
		const text = src.slice(0, tag.end);
		return { type: 'paragraph', raw: text + end[0], text, tokens: this.lexer.inline(text, []) };
	}
};

/** A surface whose extensions are built from the policy it carries. */
const embeddedHtml = (policy: HtmlPolicy): EmbeddedHtml => {
	const extensions = [markedDetails, markedHtmlRegion, embeddedInlineHtml(policy), markedHtmlEntities];
	return { extensions: policy.imageLines ? [markedImageLines, ...extensions] : extensions, policy };
};

const EMBEDDED_HTML = {
	github: embeddedHtml(htmlPolicyOf('github')),
	gitlab: embeddedHtml(htmlPolicyOf('gitlab')),
	any: embeddedHtml(ANY_FORGE_HTML_POLICY)
};

/** The extensions of an embedded-HTML surface with no forge. */
export const EMBEDDED_HTML_EXTENSIONS: Extension[] = [...EMBEDDED_HTML.any.extensions];
