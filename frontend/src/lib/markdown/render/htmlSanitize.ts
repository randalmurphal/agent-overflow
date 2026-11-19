/**
 * The `renderHtml` policy for surfaces that opt into embedded forge HTML
 * (see `parser/extensions/embeddedHtml.ts` for the split of duties).
 *
 * Everything the structural extensions did not claim lands here: raw
 * HTML tables and badge rows, wrapper `<p align>` / `<div>` runs, HTML
 * comments, and whatever else a forge comment carries. The contract:
 *
 *  - The output is built from the surface's `policy`
 *    (parser/extensions/htmlPolicy.ts), the table the inline tokenizer
 *    reads too: kept elements with the attributes it lists, and text. As
 *    a forge sanitizer does, any other element contributes its children
 *    (`<big>`, `<font>`, form controls), except removed ones, which
 *    contribute nothing (a script's or style's source is never shown).
 *    Comments render nothing.
 *  - BLOCK tokens are complete runs (the engine's html block rule
 *    consumes to a blank line), so they parse into a balanced tree.
 *  - INLINE tag tokens are single tags the inline extension did not pair
 *    (it claims every element pair it renders), so only an allowlisted
 *    void element (`<hr>`, `<img>` without a src) shows; a start or end
 *    tag shows nothing.
 *  - The policy's `tagFilter` is GFM's disallowed raw HTML rule, which
 *    GitHub applies: `TAG_FILTERED` tags show as their source text.
 *  - An `href` or `src` is decided as a Markdown link's or image's
 *    destination is (`decideDestination` with the surface's
 *    `destinations`), then admitted by the renderers' own rules
 *    (`classifyLinkHref`, `transformMediaUrl`) with the surface's
 *    prefixes, and written as the URL parser read it: no value reaches an
 *    anchor or img attribute that the browser could resolve against the
 *    page. An image it refuses is the renderers' unavailable chip. With
 *    `anchors`, a `#name` href is a same-document link
 *    (`data-md-fragment`), the one other href written, and an anchor name
 *    a `data-md-anchor`, never an `id`.
 *
 * Two host hooks keep a surface's rules in the surface: `destinations`
 * (below) and `text`, which turns a text run into inline tokens
 * (references, emoji). What a hook returns is data this file renders
 * under the same allowlist and URL policy, never markup.
 *
 * The output string is injected via `{@html}` by `Element.svelte`'s html
 * branch, inside `.markdown-body`, whose cascade already styles tables,
 * details and summaries.
 *
 * An html region (`parser/extensions/htmlRegion.ts`) is sanitized the way a
 * forge sanitizes a document: its HTML strings joined around one placeholder
 * per Markdown island, parsed and sanitized once, so the browser's parser
 * balances wrappers that open in one block and close in another.
 *
 * Media a host fetches itself (forge-hosted attachments, whose bytes this
 * page cannot read directly) is claimed by a destination hook, which
 * rewrites the src to an app href the image prefixes admit; the element is
 * then emitted WITHOUT a `src`, carrying the claim in a data attribute for
 * the host to hydrate. The claim is never a URL this file fetches.
 */

import type { MarkedToken, Token, Tokens } from '../parser/engine';
import { codespanClass } from './codespan';
import { imageSizeOf } from './imageSize';
import { imageSrcFor, pictureSources, type ColorScheme } from '../parser/extensions/themedImages';
import { ANY_FORGE_HTML_POLICY, imageAlign, ruleAttributes, type HtmlPolicy } from '../parser/extensions/htmlPolicy';
import { videoSrcs } from '../parser/extensions/embeddedHtml';
import { canonicalHref, decideDestination, type DestinationHook } from '../parser/engine/destination';
import {
	classifyLinkHref,
	transformMediaUrl,
	UNAVAILABLE_IMAGE_CLASS,
	unavailableImageText,
	unavailableImageTitle
} from './elements/url';

/** Without the surface's prefixes: absolute http(s) only. */
const HTTP_ONLY: string[] = ['http://', 'https://'];

/**
 * Where an `<a>` with `href` goes: its destination decided and admitted
 * as a Markdown link's, written as the URL parser read it (the authored
 * spelling can still resolve against the page, `http:/path` on an http
 * page, the parsed one cannot), with the title a host's claim gives it.
 */
const linkTarget = (
	href: string,
	title: string | null,
	options: SanitizeEmbeddedHtmlOptions | undefined
): { href: string; title: string | null } | null => {
	const token = decideDestination({ type: 'link', raw: '', href, title, text: '', tokens: [] }, options?.destinations, { inLink: false });
	const decided = classifyLinkHref(token.href, options?.allowedLinkPrefixes ?? HTTP_ONLY);
	return decided.kind === 'anchor' ? { href: decided.href, title: token.title ?? null } : null;
};

/** What an image or video `src` paints: its URL, a host's claim, or the unavailable chip. */
type MediaTarget = { kind: 'src'; src: string } | { kind: 'claim'; claim: string } | { kind: 'unavailable'; title: string };

/**
 * `src` decided as a Markdown image's destination and admitted by the
 * renderers' media rule. A host hook that changed it claims it.
 */
const mediaTarget = (src: string, options: SanitizeEmbeddedHtmlOptions | undefined, inLink: boolean): MediaTarget => {
	const token = decideDestination({ type: 'image', raw: '', href: src, title: null, text: '', tokens: [] }, options?.destinations, { inLink });
	const url = transformMediaUrl(token.href, options?.allowedImagePrefixes ?? HTTP_ONLY);
	if (url !== null && token.href !== canonicalHref(src)) return { kind: 'claim', claim: url };
	if (url !== null) return { kind: 'src', src: url };
	return { kind: 'unavailable', title: unavailableImageTitle(token.href) };
};

/**
 * GFM's disallowed raw HTML tags. With the policy's `tagFilter` such a tag
 * is text before the HTML is parsed, as GitHub's renderer makes its `<`
 * text. Its `>` is escaped too, which reads the same.
 */
const TAG_FILTERED = /<(\/?(?:title|textarea|xmp|iframe|noembed|noframes|plaintext)(?=[\s/>])[^>]*)(>?)/gi;
const filterTags = (raw: string): string =>
	raw.replace(TAG_FILTERED, (_tag, inside: string, end: string) => `&lt;${inside}${end === '' ? '' : '&gt;'}`);

/**
 * A list's `type` as the marker style it names. Written as a style, since
 * the Markdown cascade's list styles outrank the attribute.
 */
const OL_TYPES: ReadonlyMap<string, string> = new Map([
	['1', 'decimal'], ['a', 'lower-alpha'], ['A', 'upper-alpha'], ['i', 'lower-roman'], ['I', 'upper-roman']
]);
const UL_TYPES: ReadonlySet<string> = new Set(['disc', 'circle', 'square']);
const listStyleOf = (element: Element): string | undefined => {
	const type = element.getAttribute('type');
	if (type === null) return undefined;
	if (element.localName === 'ol') return OL_TYPES.get(type);
	return UL_TYPES.has(type.toLowerCase()) ? type.toLowerCase() : undefined;
};

const FRAGMENT_ATTR = 'data-md-fragment';
const ANCHOR_ATTR = 'data-md-anchor';

/** The jump target `element` names under `anchors`: an `<a name>`, else its `id`. */
const anchorOf = (element: Element, anchors: { readonly ids: boolean }): string | null =>
	(element.localName === 'a' ? element.getAttribute('name') : null) || (anchors.ids ? element.getAttribute('id') : null) || null;

/** The decided `href` or `src` of an element, and the title a claim gives it. */
interface DecidedUrls {
	href?: string | null;
	src?: string | null;
	title?: string | null;
}

/**
 * The attributes of `element` its policy rule keeps, with `urls` its
 * decided `href` and `src` (dropped when absent), and with `options` its
 * jump markers.
 */
const keptAttributes = (
	element: Element,
	allowed: readonly string[],
	options: SanitizeEmbeddedHtmlOptions | undefined,
	urls: DecidedUrls
): [name: string, value: string][] => {
	const kept: [string, string][] = [];
	const listStyle = element.localName === 'ol' || element.localName === 'ul' ? listStyleOf(element) : undefined;
	if (listStyle !== undefined) kept.push(['style', `list-style-type: ${listStyle}`]);
	for (const [name, authored] of ruleAttributes(element, allowed)) {
		if (name === 'href' && options?.anchors && authored.startsWith('#')) {
			kept.push([FRAGMENT_ATTR, ''], [name, authored]);
			continue;
		}
		const value = name === 'href' || name === 'src' ? urls[name] : name === 'title' ? (urls.title ?? authored) : authored;
		if (value !== null && value !== undefined) kept.push([name, value]);
	}
	if (urls.title && allowed.includes('title') && !kept.some(([name]) => name === 'title')) kept.push(['title', urls.title]);
	const anchor = options?.anchors ? anchorOf(element, options.anchors) : null;
	if (anchor !== null) kept.push([ANCHOR_ATTR, anchor]);
	return kept;
};
const COMMENTS_AND_SPACE = /(?:<!--[\s\S]*?(?:-->|$)|\s)+/g;

/** Attributes a claimed `<img>` keeps. `src` is deliberately absent. */
const CLAIMED_IMG_ATTRS = ['alt', 'title', 'width', 'height', 'align', 'hspace'] as const;
export const MEDIA_CLAIM_ATTR = 'data-markdown-media-claim';
export const MEDIA_KIND_ATTR = 'data-markdown-media-kind';

export interface SanitizeEmbeddedHtmlOptions {
	/**
	 * The surface's destination hooks (`destinationsOf` its extensions):
	 * every `href` and `src` is decided by them as the same destination in
	 * Markdown is. A hook that changes an image's src claims the element.
	 */
	destinations?: readonly DestinationHook[];
	/** The renderers' link and image prefixes (`transformUrl`). Absent: absolute http(s) only. */
	allowedLinkPrefixes?: string[];
	allowedImagePrefixes?: string[];
	/**
	 * The reader's color scheme, which picks a `<picture>`'s source and
	 * hides a `#gh-*-mode-only` image in the other scheme
	 * (parser/extensions/themedImages.ts). Read only for HTML holding one,
	 * so a caller in a reactive scope re-renders just those blocks when it
	 * changes. Absent: every image is its own src.
	 */
	colorScheme?: () => ColorScheme;
	/** What the surface's forge keeps (parser/extensions/htmlPolicy.ts); the
	 * inline tokenizer of the same surface reads the same one. Absent:
	 * `ANY_FORGE_HTML_POLICY`. */
	policy?: HtmlPolicy;
	/**
	 * Same-document jumps as the Markdown renderers draw them on a forge
	 * (parser/documentScope.ts): `<a href="#x">` is a `data-md-fragment`
	 * link and an `<a name>`, with `ids` any element's `id` too, a
	 * `data-md-anchor` target. Absent, both are dropped.
	 */
	anchors?: { readonly ids: boolean };
	/**
	 * A text run of a block as the inline tokens to show in its place, or
	 * null for the text itself. Not asked inside `code`, `pre`, `tt` or
	 * `samp`, whose text is literal; `linkable` is false inside an `<a>`
	 * or a `<kbd>`. The tokens render as the elements they name (a text or
	 * escape's text, a code span, a link, an image), read by this policy as
	 * authored HTML is, so a link it refuses shows its text; any other
	 * token shows its `raw`.
	 */
	text?: (text: string, linkable: boolean) => readonly Token[] | null;
}

/** Whether a text run may become tokens, and whether they may link. */
type TextScope = 'linkable' | 'unlinkable' | 'literal';
/** Elements whose text is literal, as a Markdown code span's is. */
const LITERAL_TEXT: ReadonlySet<string> = new Set(['code', 'pre', 'tt', 'samp']);
const scopeIn = (tag: string, scope: TextScope): TextScope =>
	scope === 'literal' || LITERAL_TEXT.has(tag) ? 'literal' : tag === 'a' || tag === 'kbd' ? 'unlinkable' : scope;

/** Appends `tokens` to `parent` as the nodes they name, for the policy to read as authored HTML. */
const appendTokenNodes = (tokens: readonly Token[], parent: Node, doc: Document, options: SanitizeEmbeddedHtmlOptions | undefined): void => {
	for (const each of tokens) {
		const token = each as MarkedToken;
		switch (token.type) {
			case 'text':
			case 'escape':
				parent.appendChild(doc.createTextNode(token.text));
				break;
			case 'codespan': {
				const code = doc.createElement('code');
				code.textContent = token.text;
				parent.appendChild(code);
				break;
			}
			case 'link': {
				// A link this policy refuses is its text, not a bare `<a>`.
				if (linkTarget(token.href, token.title ?? null, options) === null) {
					appendTokenNodes(token.tokens, parent, doc, options);
					break;
				}
				const anchor = doc.createElement('a');
				anchor.setAttribute('href', token.href);
				if (token.title) anchor.setAttribute('title', token.title);
				appendTokenNodes(token.tokens, anchor, doc, options);
				parent.appendChild(anchor);
				break;
			}
			case 'image': {
				const img = doc.createElement('img');
				img.setAttribute('src', token.href);
				img.setAttribute('alt', token.text);
				if (token.title) img.setAttribute('title', token.title);
				for (const name of ['width', 'height'] as const) {
					const value = (token as { width?: unknown; height?: unknown })[name];
					if (typeof value === 'string') img.setAttribute(name, value);
				}
				parent.appendChild(img);
				break;
			}
			default:
				parent.appendChild(doc.createTextNode(token.raw));
		}
	}
};

/** A host's claim on a `<video>`'s first claimed src (`videoSrcs`, as inline); null for none. */
const videoClaim = (element: Element, options: SanitizeEmbeddedHtmlOptions | undefined, inLink: boolean): string | null => {
	for (const src of videoSrcs(element)) {
		const target = mediaTarget(src, options, inLink);
		if (target.kind === 'claim') return target.claim;
	}
	return null;
};

const escapeHtml = (text: string): string =>
	text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

/** Marks an island placeholder in a region's HTML: `${nonce}:${index}`. */
const ISLAND_ATTR = 'data-markdown-island';

/** Whether the img branch below would paint `src`: claimed, or admitted. */
const paintsSrc = (src: string, options: SanitizeEmbeddedHtmlOptions | undefined): boolean =>
	mediaTarget(src, options, false).kind !== 'unavailable';

/** An `<a>`'s decided href and the title a claim gives it. */
const anchorUrls = (element: Element, options: SanitizeEmbeddedHtmlOptions | undefined): DecidedUrls => {
	const href = element.getAttribute('href');
	const target = href === null ? null : linkTarget(href, element.getAttribute('title'), options);
	return { href: target?.href ?? null, title: target?.title ?? null };
};

/** Inside an `<a href>` of the HTML being sanitized. */
const inAnchor = (element: Element): boolean => element.parentElement?.closest('a[href]') != null;

/**
 * Where `parent` sits under the policy's `imageParagraphs`: the top of a
 * block, the top with a stripped element's children in its place, or
 * anywhere else (and every place without the rule).
 */
type Placement = 'top' | 'stripped' | 'nested';

const sanitizeInto = (
	source: Node,
	parent: Element,
	options: SanitizeEmbeddedHtmlOptions | undefined,
	islands: string | undefined,
	scope: TextScope,
	placement: Placement
): void => {
	const colorScheme = options?.colorScheme;
	for (const child of source.childNodes) {
		if (child.nodeType === Node.TEXT_NODE) {
			const text = child.textContent ?? '';
			const tokens = scope === 'literal' ? null : (options?.text?.(text, scope === 'linkable') ?? null);
			if (tokens === null) {
				parent.append(text);
				continue;
			}
			// Read as authored HTML, whose text is not asked again.
			const nodes = parent.ownerDocument.createDocumentFragment();
			appendTokenNodes(tokens, nodes, parent.ownerDocument, options);
			sanitizeInto(nodes, parent, options, islands, 'literal', 'nested');
			continue;
		}
		if (child.nodeType !== Node.ELEMENT_NODE) continue; // comments, PIs
		let element = child as Element;
		let tag = element.tagName.toLowerCase();
		// A placeholder carries this call's nonce, which authored HTML cannot
		// know; any other `data-markdown-island` is stripped below.
		if (islands !== undefined && tag === 'div' && element.getAttribute(ISLAND_ATTR)?.startsWith(`${islands}:`)) {
			const clean = parent.ownerDocument.createElement('div');
			clean.setAttribute(ISLAND_ATTR, element.getAttribute(ISLAND_ATTR)!);
			parent.append(clean);
			continue;
		}
		// A `<picture>` is its fallback `<img>`, as the inline tokenizer reads
		// it; its `<source>` elements only offer a src.
		const picture = tag === 'picture' ? element : null;
		if (picture) {
			const img = picture.querySelector('img');
			if (!img) continue;
			element = img;
			tag = 'img';
		}
		// An img's src as a Markdown image's: the one the reader's scheme
		// paints (themedImages.ts), then painted, claimed or the unavailable
		// chip. An img with no src is a void element.
		const authored = tag === 'img' ? element.getAttribute('src') : null;
		const src =
			authored === null || !colorScheme
				? authored
				: imageSrcFor({ href: authored, sources: picture ? pictureSources(picture) : undefined }, colorScheme, (candidate) =>
						paintsSrc(candidate, options)
					);
		if (authored !== null && src === null) continue;
		const media = src === null ? undefined : mediaTarget(src, options, inAnchor(element));
		// The policy's `imageParagraphs`: an image its stripped wrappers left
		// at the top of a block is a paragraph of its own. GitHub keeps a
		// `<picture>`, so the image in one is not at the top.
		const host =
			tag === 'img' && !picture && placement === 'stripped' ? parent.appendChild(parent.ownerDocument.createElement('p')) : parent;
		if (media?.kind === 'unavailable') {
			const chip = parent.ownerDocument.createElement('span');
			chip.setAttribute('data-streamdown-image-blocked', '');
			chip.setAttribute('data-find-skip', '');
			chip.setAttribute('class', UNAVAILABLE_IMAGE_CLASS);
			chip.setAttribute('title', media.title);
			chip.textContent = unavailableImageText(element.getAttribute('alt') ?? '');
			host.append(chip);
			continue;
		}
		const claim = media?.kind === 'claim' ? media.claim : tag === 'video' ? videoClaim(element, options, inAnchor(element)) : null;
		if (claim !== null) {
			if (tag === 'img') {
				// The host reads the kept `width`/`height` for the size.
				const clean = parent.ownerDocument.createElement('img');
				clean.setAttribute(MEDIA_CLAIM_ATTR, claim);
				for (const name of CLAIMED_IMG_ATTRS) {
					const value = name === 'align' ? imageAlign(options?.policy ?? ANY_FORGE_HTML_POLICY, element) : element.getAttribute(name);
					if (value !== null) clean.setAttribute(name, value);
				}
				host.append(clean);
			} else {
				// `video` is not allowlisted, so there is no element to
				// keep: the placeholder carries the claim and the host
				// decides what the bytes are. Children (sources, the
				// fallback text) belong to a player this page cannot
				// feed, so they are dropped with it.
				const clean = parent.ownerDocument.createElement('span');
				clean.setAttribute(MEDIA_CLAIM_ATTR, claim);
				clean.setAttribute(MEDIA_KIND_ATTR, 'video');
				parent.append(clean);
			}
			continue;
		}
		const allowedAttrs = (options?.policy ?? ANY_FORGE_HTML_POLICY).elements.get(tag);
		if (allowedAttrs === 'remove') continue;
		if (allowedAttrs === undefined) {
			// An HTML template's children are its content fragment; a
			// `template` in MathML is an ordinary element.
			const content = tag === 'template' ? (element as Partial<HTMLTemplateElement>).content : undefined;
			sanitizeInto(content ?? element, parent, options, islands, scope, placement === 'nested' ? 'nested' : 'stripped');
			continue;
		}
		const clean = parent.ownerDocument.createElement(tag);
		const urls: DecidedUrls =
			tag === 'a'
				? anchorUrls(element, options)
				: { src: media?.kind === 'src' ? media.src : null };
		for (const [name, value] of keptAttributes(element, allowedAttrs, options, urls)) clean.setAttribute(name, value);
		if (tag === 'img') {
			// No wrapper: a percentage is the image's own width.
			const size = imageSizeOf(
				{
					width: element.getAttribute('width'),
					height: element.getAttribute('height'),
					align: imageAlign(options?.policy ?? ANY_FORGE_HTML_POLICY, element),
					hspace: element.getAttribute('hspace')
				},
				false
			);
			if (size.width !== undefined) clean.style.width = size.width;
			if (size.height !== undefined) clean.style.height = size.height;
			if (size.aspectRatio !== undefined) clean.style.aspectRatio = size.aspectRatio;
			if (size.float !== undefined) clean.style.float = size.float;
			if (size.verticalAlign !== undefined) clean.style.verticalAlign = size.verticalAlign;
			if (size.marginInline !== undefined) clean.style.marginInline = size.marginInline;
		}
		// Inline code only: a `<pre><code>` is a block, sized by its `pre`.
		if (tag === 'code' && parent.localName !== 'pre') {
			const className = codespanClass('', element.textContent ?? '');
			if (className !== '') clean.setAttribute('class', className);
		}
		host.append(clean);
		sanitizeInto(element, clean, options, islands, scopeIn(tag, scope), 'nested');
	}
};

/**
 * `renderHtml` implementation. Returns sanitized HTML for block tokens; for
 * an inline tag, an allowlisted void element or tag-filtered text, else ''.
 */
export const sanitizeEmbeddedHtmlToken = (
	token: { raw: string; block?: boolean; islands?: string },
	options?: SanitizeEmbeddedHtmlOptions
): string => {
	const raw = token.raw;
	if (raw.replace(COMMENTS_AND_SPACE, '') === '') return '';
	const policy = options?.policy ?? ANY_FORGE_HTML_POLICY;
	const html = policy.tagFilter ? filterTags(raw) : raw;
	const doc = new DOMParser().parseFromString(html, 'text/html');
	const out = doc.createElement('div');
	// Only a block's text is asked: an inline tag token's is its own source.
	const placement = token.block && policy.imageParagraphs ? 'top' : 'nested';
	sanitizeInto(doc.body, out, options, token.islands, token.block ? 'linkable' : 'literal', placement);
	// A start tag alone is an empty element, which shows nothing.
	if (!token.block && out.textContent === '' && out.querySelector('br, hr, img, wbr') === null) return '';
	return out.innerHTML;
};

// ---------------------------------------------------------------------------
// Html regions
// ---------------------------------------------------------------------------

/**
 * A region's HTML as one html token for `renderHtml`: each island is an
 * empty `<div data-markdown-island>` placeholder carrying `islands`, the
 * nonce of this call, which the sanitizer keeps.
 */
export type HtmlRegionSource = Tokens.HTML & { islands: string };

/** `renderHtml` as the context holds it, when it is set. */
export type RenderRegionHtml = true | ((token: Tokens.HTML | Tokens.Tag) => string);

export interface ParsedHtmlRegion {
	/** A `<div>` holding the region's rendered HTML. */
	readonly root: HTMLDivElement;
	/** Each island placeholder that survived, to its index in the region's islands. */
	readonly islands: ReadonlyMap<Element, number>;
}

const nonce = (): string => {
	const bytes = new Uint8Array(12);
	crypto.getRandomValues(bytes);
	return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
};

/**
 * Renders a region's `parts` through `renderHtml` as one fragment and parses
 * the result. An island whose placeholder the HTML drops (inside a
 * `<picture>` or a claimed video) is not in `islands`.
 */
export const parseHtmlRegion = (
	parts: readonly (string | object)[],
	renderHtml: RenderRegionHtml
): ParsedHtmlRegion => {
	const id = nonce();
	let html = '';
	let count = 0;
	for (const part of parts) {
		html += typeof part === 'string' ? part : `<div ${ISLAND_ATTR}="${id}:${count++}"></div>`;
	}
	const source: HtmlRegionSource = { type: 'html', block: true, pre: false, raw: html, text: html, islands: id };
	const doc = new DOMParser().parseFromString(renderHtml === true ? html : renderHtml(source), 'text/html');
	const root = doc.createElement('div');
	// One node per call: a spread turns the child count into an argument count.
	while (doc.body.firstChild) root.append(doc.body.firstChild);
	const islands = new Map<Element, number>();
	const seen = new Set<number>();
	for (const element of root.querySelectorAll(`div[${ISLAND_ATTR}]`)) {
		// Checked again: `renderHtml === true` keeps authored attributes.
		const [owner, at] = element.getAttribute(ISLAND_ATTR)!.split(':');
		const index = Number(at);
		if (owner !== id || !Number.isInteger(index) || index < 0 || index >= count || seen.has(index)) continue;
		seen.add(index);
		islands.set(element, index);
	}
	return { root, islands };
};

/**
 * A parsed region as `HtmlRegion.svelte` renders it: the elements that hold
 * an island as elements with their allowlisted attributes, each island by
 * index, and every run of island-free nodes as one HTML string.
 */
export type HtmlRegionNode =
	| { kind: 'element'; tag: string; attrs: Record<string, string>; children: HtmlRegionNode[] }
	| { kind: 'island'; index: number }
	| { kind: 'html'; html: string };

const serialize = (node: Node): string =>
	node.nodeType === Node.ELEMENT_NODE
		? (node as Element).outerHTML
		: node.nodeType === Node.TEXT_NODE
			? escapeHtml(node.textContent ?? '')
			: '';

/**
 * `linkPrefixes` are the renderer's: an anchor holding an island keeps an
 * href they admit, as the sanitizer wrote it (`renderHtml === true` keeps
 * authored ones, so this check stands alone there).
 */
export const htmlRegionNodes = ({ root, islands }: ParsedHtmlRegion, linkPrefixes: string[]): HtmlRegionNode[] => {
	const spine = new Set<Node>();
	for (const placeholder of islands.keys()) {
		for (let node = placeholder.parentNode; node && node !== root; node = node.parentNode) spine.add(node);
	}
	const nodesOf = (parent: Node): HtmlRegionNode[] => {
		const out: HtmlRegionNode[] = [];
		let html = '';
		const flush = () => {
			if (html !== '') out.push({ kind: 'html', html });
			html = '';
		};
		for (const child of parent.childNodes) {
			const index = islands.get(child as Element);
			if (index !== undefined) {
				flush();
				out.push({ kind: 'island', index });
				continue;
			}
			if (!spine.has(child)) {
				html += serialize(child);
				continue;
			}
			flush();
			const element = child as Element;
			const tag = element.localName;
			// The most any policy keeps: this element is already sanitized.
			const rule = ANY_FORGE_HTML_POLICY.elements.get(tag);
			const allowed = rule === 'remove' ? undefined : rule;
			// Only `renderHtml === true` keeps an unlisted element; its
			// children render in its place.
			if (allowed === undefined) {
				out.push(...nodesOf(element));
				continue;
			}
			const authoredHref = element.getAttribute('href');
			const admitted = authoredHref === null ? null : classifyLinkHref(authoredHref, linkPrefixes);
			const attrs = Object.fromEntries(
				keptAttributes(element, allowed, undefined, { href: admitted?.kind === 'anchor' ? admitted.href : null, src: null })
			);
			// The sanitizer's jump markers stand, with the `#name` href of a fragment link.
			const anchor = element.getAttribute(ANCHOR_ATTR);
			if (anchor !== null) attrs[ANCHOR_ATTR] = anchor;
			const href = element.getAttribute('href');
			if (element.hasAttribute(FRAGMENT_ATTR) && href?.startsWith('#')) Object.assign(attrs, { [FRAGMENT_ATTR]: '', href });
			out.push({ kind: 'element', tag, attrs, children: nodesOf(element) });
		}
		flush();
		return out;
	};
	return nodesOf(root);
};
