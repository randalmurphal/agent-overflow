/**
 * Opt-in embedded-HTML support for forge-authored markdown (PR/MR
 * descriptions, review-thread comments, bot reports).
 *
 * Deliberately NOT in the default extension sets: agent chat keeps
 * `renderHtml={false}` and renders no HTML at all. A surface that opts in
 * registers `EMBEDDED_HTML_EXTENSIONS` and passes
 * `render/htmlSanitize.ts#sanitizeEmbeddedHtmlToken` as `renderHtml`, so
 * every html token has exactly one of three fates:
 *
 *  - a STRUCTURAL token rendered by our own elements — `<details>` /
 *    `<summary>` become container tokens whose children are ordinary
 *    markdown tokens (the content inside a forge dropdown is markdown),
 *    and the safe inline pairs map onto the native inline tokens
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
import { Lexer } from '../engine';
import type { Token, Tokens } from '../engine';
import { decodeHtmlEntities, markedHtmlEntities } from './htmlEntities';
import { htmlBlockStart, markedHtmlRegion } from './htmlRegion';
import { attributeOf, readTag, scanTags, type HtmlTag } from './htmlTags';
import { pictureSources, type ColorSchemeSources } from './themedImages';
import {
	bindBlockInside,
	bindInlineInside,
	boundBlockRun,
	closeOf,
	htmlBodyTokens,
	inlineRun,
	isMeasuringBlocks,
	isMeasuringInline,
	measureBlockRun,
	measuredHtmlBlock,
	pairOf,
	recordInlineTag
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

// GitLab's collapsible sections write a Markdown summary apart from its
// tags by a blank line.
const MARKDOWN_SUMMARY = /^[ \t]*\r?\n[ \t]*\r?\n/;

/** Whether inline `text` holds a tag the inline tokenizer does not render, read from its tokens. */
const holdsOtherHtml = (lexer: Lexer, text: string): boolean => {
	if (!text.includes('<')) return false;
	const probe = new Lexer(lexer.options);
	probe.footnotes = { refs: new Map(), footnotes: new Map() };
	probe.tokens.links = lexer.tokens.links;
	const other = (tokens: Token[]): boolean =>
		tokens.some((token) => {
			if (token.type === 'html') {
				const tag = readTag(token.raw, 0);
				return tag !== null && !INLINE_CLAIMED.has(tag.name);
			}
			const children = (token as { tokens?: Token[] }).tokens;
			return Array.isArray(children) && other(children);
		});
	return other(probe.inlineTokens(text, []));
};

/**
 * A summary's content: a Markdown summary's blocks (a lone paragraph's
 * inline tokens); inline Markdown; or, when it holds block HTML
 * (`<h3>Title</h3>`), one HTML fragment the sanitizer renders whole, as a
 * forge does. A tag inside a code span is text.
 */
const summaryContent = (lexer: Lexer, inner: string, text: string): Token[] => {
	if (MARKDOWN_SUMMARY.test(inner)) {
		const blocks = lexer.blockTokens(text, []).filter(kept);
		return blocks.length === 1 && blocks[0].type === 'paragraph' ? (blocks[0] as Tokens.Paragraph).tokens : blocks;
	}
	if (holdsOtherHtml(lexer, text)) return [{ type: 'html', block: true, pre: false, raw: text, text }];
	// Deferred inline lexing on the ACTIVE lexer, like the paragraph
	// tokenizer: the array fills when lex() drains.
	return lexer.inline(text, []);
};

/**
 * A `<details>` run, from its start tag through the `</details>` that
 * closes it, as one token whose body is Markdown. The end tag is the one
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
				const inner = src.slice(summary.end, summaryClose.start);
				const text = inner.trim();
				children.push({
					type: 'detailsSummary',
					raw: src.slice(open.end, summaryClose.end),
					text,
					tokens: summaryContent(this.lexer, inner, text)
				} as DetailsSummaryToken);
				bodyStart = summaryClose.end;
			}
		}
		const body: Token[] = [];
		bindBlockInside(this.lexer, body, run, src, close.start, false);
		for (const child of htmlBodyTokens(this.lexer, src.slice(bodyStart, close.start), body)) {
			if (kept(child)) children.push(child);
		}
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
 * A safe inline element with no native markdown token: it renders as
 * itself around its inline children, with no authored attribute but its
 * `title` (an `<abbr>`'s expansion, a tooltip). A `<span>` is here so its
 * content is a child list: the engine appends later text to a bare `text`
 * token. A `<kbd>` holds Markdown and nests, as on the forges.
 */
export type HtmlInlineToken = {
	type: 'htmlInline';
	raw: string;
	tag: HtmlInlineTag;
	title: string | null;
	text: string;
	tokens: Token[];
};

export const HTML_INLINE_TAGS = [
	'ins', 'mark', 'u', 'small', 'abbr', 'q', 'var', 'cite', 'dfn', 'span', 'kbd', 'ruby', 'rt', 'rp'
] as const;
export type HtmlInlineTag = (typeof HTML_INLINE_TAGS)[number];
const isHtmlInlineTag = (tag: string): tag is HtmlInlineTag =>
	(HTML_INLINE_TAGS as readonly string[]).includes(tag);

/**
 * Elements whose content does not show (render/htmlSanitize.ts `REMOVED`):
 * a pair is one inline html token for the sanitizer, so a script's source
 * is not lexed as Markdown.
 */
const INLINE_REMOVED: ReadonlySet<string> = new Set(['script', 'style', 'svg', 'iframe']);

/** The elements paired with their end tag. */
const INLINE_PAIRED: ReadonlySet<string> = new Set([
	...Object.keys(INLINE_PAIR_TARGETS),
	...HTML_INLINE_TAGS,
	...INLINE_REMOVED,
	'a'
]);
/** Every tag name the inline tokenizer claims. */
const INLINE_CLAIMED: ReadonlySet<string> = new Set([...INLINE_PAIRED, 'picture', 'img']);

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
 * The image token an `<img>` becomes. `width`, `height`, `align` and
 * `hspace` are the authored attributes; `render/imageSize.ts` decides what
 * they paint as. A `<picture>` adds the src each color scheme's source
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
const imageToken = (raw: string, img: Element | null | undefined): EmbeddedImageToken | undefined => {
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
		align: img.getAttribute('align'),
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
const pictureToken = (src: string, open: HtmlTag): EmbeddedImageToken | undefined => {
	let close: HtmlTag | undefined;
	scanTags(src.slice(open.end), (tag) => {
		if (tag.name !== 'picture') return;
		if (tag.closing) close = tag;
		return false;
	});
	if (!close) return undefined;
	const raw = src.slice(0, open.end + close.end);
	const picture = parseInert(raw).querySelector('picture');
	const image = imageToken(raw, picture?.querySelector('img'));
	if (!image || !picture) return image;
	const sources = pictureSources(picture);
	return sources.dark === undefined && sources.light === undefined ? image : { ...image, sources };
};

export const markedEmbeddedInlineHtml: Extension = {
	name: 'embeddedInlineHtml',
	level: 'inline',
	tokenizer(src, tokens) {
		if (src.charCodeAt(0) !== 60 /* < */) return undefined;
		const tag = readTag(src, 0);
		if (!tag || !INLINE_CLAIMED.has(tag.name)) return undefined;
		if (tag.name === 'img') return tag.closing ? undefined : imageToken(src.slice(0, tag.end), elementOf(src, tag));
		if (tag.name === 'picture') return tag.closing ? undefined : pictureToken(src, tag);
		if (isMeasuringInline(this.lexer)) {
			recordInlineTag(this.lexer, src, tokens as Token[], tag);
			return undefined;
		}
		const nesting = inlineNesting.get(this.lexer) ?? 0;
		if (tag.closing || nesting >= MAX_ELEMENT_NESTING) return undefined;
		const run = inlineRun(this.lexer, src, tokens as Token[], INLINE_PAIRED);
		const removed = INLINE_REMOVED.has(tag.name);
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
				if (!nested) return { type: 'link', raw, href, title: anchor?.getAttribute('title') ?? null, text: inner, tokens: children };
			}
		}
		// An anchor with no `href` (a named jump target), one inside a link,
		// or one around a link, is not a link: its content renders as plain
		// text, which a bare `<span>` is.
		const element = tag.name === 'a' ? 'span' : tag.name;
		if (isHtmlInlineTag(element)) {
			return {
				type: 'htmlInline',
				raw,
				tag: element,
				title: elementOf(src, tag)?.getAttribute('title') ?? null,
				text: inner,
				tokens: content()
			} satisfies HtmlInlineToken;
		}

		const target = INLINE_PAIR_TARGETS[element];
		if (target === 'codespan') {
			// The element's text, not markdown: `<code>Vec&lt;T&gt;</code>`.
			return { type: 'codespan', raw, text: decodeHtmlEntities(inner) };
		}
		return { type: target, raw, text: inner, tokens: content() };
	}
};

/**
 * The registration set a forge surface passes as `extensions`. A `<details>`
 * run keeps its structural token; an HTML wrapper around Markdown, its own
 * or in a details body, is a region (htmlRegion.ts).
 */
export const EMBEDDED_HTML_EXTENSIONS: Extension[] = [
	markedDetails,
	markedHtmlRegion,
	markedEmbeddedInlineHtml,
	markedHtmlEntities
];
