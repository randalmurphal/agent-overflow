/**
 * The `renderHtml` policy for surfaces that opt into embedded forge HTML
 * (see `parser/extensions/embeddedHtml.ts` for the split of duties).
 *
 * Everything the structural extensions did not claim lands here: raw
 * HTML tables and badge rows, wrapper `<p align>` / `<div>` runs, HTML
 * comments, and whatever else a forge comment carries. The contract:
 *
 *  - The output is built from an allowlist: allowlisted elements with
 *    allowlisted attributes, and text. As a forge sanitizer does, any
 *    other element contributes its children (`<big>`, `<font>`, form
 *    controls), except `REMOVED` ones, which contribute nothing (a
 *    script's or style's source is never shown). Comments render nothing.
 *  - BLOCK tokens are complete runs (the engine's html block rule
 *    consumes to a blank line), so they parse into a balanced tree.
 *  - INLINE tag tokens are single tags the inline extension did not pair
 *    (it claims every element pair it renders), so only an allowlisted
 *    void element (`<hr>`, `<img>` without a src) shows; a start or end
 *    tag shows nothing.
 *  - `tagFilter` is GFM's disallowed raw HTML rule, which GitHub applies:
 *    `TAG_FILTERED` tags show as their source text.
 *  - `href` / `src` accept absolute http(s) only, mirroring the
 *    security boundary in AGENTS.md, and are written as the URL parser
 *    read them: no value reaches an anchor or img attribute that the
 *    browser could resolve against the page. With `anchors`, a `#name`
 *    href is a same-document link (`data-md-fragment`), the one other
 *    href written, and an anchor name a `data-md-anchor`, never an `id`.
 *
 * Three host hooks keep a surface's rules in the surface: `claimMedia`
 * (below), `href`, which resolves a relative link, and `text`, which turns
 * a text run into inline tokens (references, emoji). What a hook returns is
 * data this file renders under the same allowlist and URL policy, never
 * markup.
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
 * `claimMedia` is the app seam for media. A host that fetches certain media
 * itself (forge-hosted attachments, whose bytes this page cannot read
 * directly) returns an opaque claim for a matching `src`; the element is
 * then emitted WITHOUT that `src`, carrying the claim in a data
 * attribute for the host to hydrate. The claim is an app-owned string,
 * never a URL this file resolves, fetches or writes to an href.
 */

import type { MarkedToken, Token, Tokens } from '../parser/engine';
import { codespanClass } from './codespan';
import { imageSizeOf } from './imageSize';
import { colorSchemeOnly, pictureSources, type ColorScheme } from '../parser/extensions/themedImages';

// Allowlisted tag → allowlisted attributes, besides `GLOBAL_ATTRS`. Presence
// means the element renders as itself; any other element contributes its
// children or, when `REMOVED`, nothing. A Map, so an element named after an
// Object.prototype member is not found.
const ALLOWED_ATTRS: ReadonlyMap<string, readonly string[]> = new Map(Object.entries({
	a: ['href', 'title'],
	img: ['src', 'alt', 'title', 'width', 'height'],
	table: ['width'],
	caption: [],
	thead: [],
	tbody: [],
	tfoot: [],
	tr: [],
	td: ['align', 'colspan', 'rowspan', 'width', 'valign', 'nowrap'],
	th: ['align', 'colspan', 'rowspan', 'width', 'valign', 'nowrap'],
	p: ['align'],
	div: ['align'],
	center: [],
	span: [],
	b: [],
	strong: [],
	i: [],
	em: [],
	code: [],
	pre: [],
	sub: [],
	sup: [],
	kbd: [],
	samp: [],
	tt: [],
	del: [],
	s: [],
	strike: [],
	ins: [],
	u: [],
	mark: [],
	small: [],
	abbr: ['title'],
	q: [],
	var: [],
	cite: [],
	dfn: [],
	ruby: [],
	rt: [],
	rp: [],
	br: [],
	hr: [],
	ul: [],
	ol: ['start'],
	li: ['value'],
	blockquote: [],
	dl: [],
	dt: [],
	dd: [],
	h1: ['align'],
	h2: ['align'],
	h3: ['align'],
	h4: ['align'],
	h5: ['align'],
	h6: ['align'],
	details: ['open'],
	summary: [],
	figure: [],
	figcaption: []
}));

/**
 * The canonical form of an absolute http(s) URL, or null. Parsed with no
 * base, as the token renderers' `parseUrl` does, and emitted as parsed: the
 * authored spelling can still resolve against the page (`http:/path` on an
 * http page, a U+00A0 before the scheme), the parsed one cannot.
 */
const absoluteHttpUrl = (value: string): string | null => {
	let url: URL;
	try {
		url = new URL(value);
	} catch {
		return null;
	}
	return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : null;
};

/** Attributes both forges keep on every allowlisted element. */
const GLOBAL_ATTRS = ['title', 'dir', 'lang'] as const;

/**
 * Elements both forges drop with their content, and the ones this policy
 * drops so a script's or style's source never shows (GitHub shows them as
 * text, GitLab drops a script and shows a style's text). `iframe` is
 * GitLab's answer; GitHub's `tagFilter` shows it as text first.
 */
const REMOVED: ReadonlySet<string> = new Set(['script', 'style', 'svg', 'iframe']);

/**
 * GFM's disallowed raw HTML tags, less the `REMOVED` ones. With `tagFilter`
 * such a tag is text before the HTML is parsed, as GitHub's renderer makes
 * its `<` text. Its `>` is escaped too, which reads the same.
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

/**
 * The attributes of `element` that `allowed` and `GLOBAL_ATTRS` keep, a URL
 * in its canonical form, and with `options` its jump markers and an href
 * resolved by the `href` hook.
 */
const keptAttributes = (
	element: Element,
	allowed: readonly string[],
	options?: SanitizeEmbeddedHtmlOptions
): [name: string, value: string][] => {
	const kept: [string, string][] = [];
	for (const name of GLOBAL_ATTRS) {
		const value = allowed.includes(name) ? null : element.getAttribute(name);
		if (value !== null) kept.push([name, value]);
	}
	const listStyle = element.localName === 'ol' || element.localName === 'ul' ? listStyleOf(element) : undefined;
	if (listStyle !== undefined) kept.push(['style', `list-style-type: ${listStyle}`]);
	for (const name of allowed) {
		const authored = element.getAttribute(name);
		if (authored === null) continue;
		if (name === 'href' && options?.anchors && authored.startsWith('#')) {
			kept.push([FRAGMENT_ATTR, ''], [name, authored]);
			continue;
		}
		const resolved = name === 'href' || name === 'src' ? absoluteHttpUrl(authored) : authored;
		const value = resolved === null && name === 'href' && options?.href ? absoluteHttpUrl(options.href(authored) ?? '') : resolved;
		if (value !== null) kept.push([name, value]);
	}
	const anchor = options?.anchors ? anchorOf(element, options.anchors) : null;
	if (anchor !== null) kept.push([ANCHOR_ATTR, anchor]);
	return kept;
};
const COMMENTS_AND_SPACE = /(?:<!--[\s\S]*?(?:-->|$)|\s)+/g;

/** Attributes a claimed `<img>` keeps. `src` is deliberately absent. */
const CLAIMED_IMG_ATTRS = ['alt', 'title', 'width', 'height', 'align', 'hspace'] as const;
export const MEDIA_CLAIM_ATTR = 'data-markdown-media-claim';
export const MEDIA_KIND_ATTR = 'data-markdown-media-kind';

/**
 * Returns an app-owned claim for media the host renders itself, or null to
 * leave the element to the rules above. Called with the `src` exactly as
 * authored, before the absolute-http(s) check, so a path-relative forge
 * upload can be claimed.
 */
export type ClaimEmbeddedMedia = (
	tag: 'img' | 'video' | 'source',
	src: string
) => string | null;

export interface SanitizeEmbeddedHtmlOptions {
	claimMedia?: ClaimEmbeddedMedia;
	/**
	 * The reader's color scheme, which picks a `<picture>`'s source and
	 * hides a `#gh-*-mode-only` image in the other scheme
	 * (parser/extensions/themedImages.ts). Read only for HTML holding one,
	 * so a caller in a reactive scope re-renders just those blocks when it
	 * changes. Absent: every image is its own src.
	 */
	colorScheme?: () => ColorScheme;
	/** GitHub's GFM tag filter: `TAG_FILTERED` tags show as source text. */
	tagFilter?: boolean;
	/**
	 * An `<a>`'s href the policy refuses as written (a relative path) as
	 * the URL the surface sends it to, or null to drop it. The answer
	 * passes the same policy.
	 */
	href?: (authored: string) => string | null;
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
const appendTokenNodes = (tokens: readonly Token[], parent: Node, doc: Document): void => {
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
				if (absoluteHttpUrl(token.href) === null) {
					appendTokenNodes(token.tokens, parent, doc);
					break;
				}
				const anchor = doc.createElement('a');
				anchor.setAttribute('href', token.href);
				if (token.title) anchor.setAttribute('title', token.title);
				appendTokenNodes(token.tokens, anchor, doc);
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

/** A `<video>`'s source: its own attribute, else its first `<source src>`. */
const videoClaim = (element: Element, claimMedia: ClaimEmbeddedMedia): string | null => {
	const own = element.getAttribute('src');
	if (own !== null) {
		const claimed = claimMedia('video', own);
		if (claimed !== null) return claimed;
	}
	const source = element.querySelector('source[src]');
	const src = source?.getAttribute('src');
	return src === null || src === undefined ? null : claimMedia('source', src);
};

const escapeHtml = (text: string): string =>
	text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

/** Marks an island placeholder in a region's HTML: `${nonce}:${index}`. */
const ISLAND_ATTR = 'data-markdown-island';

/** Whether the img branch below would paint `src`: claimed, or absolute http(s). */
const paintsSrc = (src: string, claimMedia: ClaimEmbeddedMedia | undefined): boolean =>
	(claimMedia?.('img', src) ?? null) !== null || absoluteHttpUrl(src) !== null;

const sanitizeInto = (
	source: Node,
	parent: Element,
	options: SanitizeEmbeddedHtmlOptions | undefined,
	islands: string | undefined,
	scope: TextScope
): void => {
	const claimMedia = options?.claimMedia;
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
			appendTokenNodes(tokens, nodes, parent.ownerDocument);
			sanitizeInto(nodes, parent, options, islands, 'literal');
			continue;
		}
		if (child.nodeType !== Node.ELEMENT_NODE) continue; // comments, PIs
		const element = child as Element;
		const tag = element.tagName.toLowerCase();
		// A placeholder carries this call's nonce, which authored HTML cannot
		// know; any other `data-markdown-island` is stripped below.
		if (islands !== undefined && tag === 'div' && element.getAttribute(ISLAND_ATTR)?.startsWith(`${islands}:`)) {
			const clean = parent.ownerDocument.createElement('div');
			clean.setAttribute(ISLAND_ATTR, element.getAttribute(ISLAND_ATTR)!);
			parent.append(clean);
			continue;
		}
		if (tag === 'img' && colorScheme) {
			const only = colorSchemeOnly(element.getAttribute('src') ?? '');
			if (only !== null && only !== colorScheme()) continue;
		}
		if (claimMedia && (tag === 'img' || tag === 'video')) {
			const src = tag === 'img' ? element.getAttribute('src') : null;
			const claim =
				tag === 'img'
					? src === null
						? null
						: claimMedia('img', src)
					: videoClaim(element, claimMedia);
			if (claim !== null) {
				if (tag === 'img') {
					// Same element the unclaimed path emits for a src this
					// file refuses, so an un-hydrated claim degrades to
					// today's output rather than to a broken URL.
					// The host reads the kept `width`/`height` for the size.
					const clean = parent.ownerDocument.createElement('img');
					clean.setAttribute(MEDIA_CLAIM_ATTR, claim);
					for (const name of CLAIMED_IMG_ATTRS) {
						const value = element.getAttribute(name);
						if (value !== null) clean.setAttribute(name, value);
					}
					parent.append(clean);
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
		}
		if (tag === 'picture') {
			// The fallback `<img>`, painting the source the reader's scheme
			// selects when its src is one the img would paint (see the inline
			// tokenizer's `picture` branch); the `<source>` elements are dropped.
			const img = element.querySelector('img');
			if (img) {
				const chosen = img.cloneNode(true) as Element;
				const sources = colorScheme ? pictureSources(element) : {};
				const candidate =
					colorScheme && (sources.dark ?? sources.light) !== undefined ? sources[colorScheme()] : undefined;
				if (candidate !== undefined && paintsSrc(candidate, claimMedia)) chosen.setAttribute('src', candidate);
				const only = element.ownerDocument.createDocumentFragment();
				only.append(chosen);
				sanitizeInto(only, parent, options, islands, scope);
			}
			continue;
		}
		const allowedAttrs = ALLOWED_ATTRS.get(tag);
		if (allowedAttrs === undefined) {
			// A template's children are its content fragment.
			if (!REMOVED.has(tag)) sanitizeInto(tag === 'template' ? (element as HTMLTemplateElement).content : element, parent, options, islands, scope);
			continue;
		}
		const clean = parent.ownerDocument.createElement(tag);
		for (const [name, value] of keptAttributes(element, allowedAttrs, options)) clean.setAttribute(name, value);
		if (tag === 'img') {
			// No wrapper: a percentage is the image's own width.
			const size = imageSizeOf(
				{
					width: element.getAttribute('width'),
					height: element.getAttribute('height'),
					align: element.getAttribute('align'),
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
		parent.append(clean);
		sanitizeInto(element, clean, options, islands, scopeIn(tag, scope));
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
	const html = options?.tagFilter ? filterTags(raw) : raw;
	const doc = new DOMParser().parseFromString(html, 'text/html');
	const out = doc.createElement('div');
	// Only a block's text is asked: an inline tag token's is its own source.
	sanitizeInto(doc.body, out, options, token.islands, token.block ? 'linkable' : 'literal');
	// A start tag alone is an empty element, which shows nothing.
	if (!token.block && out.textContent === '' && out.querySelector('br, hr, img') === null) return '';
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
	root.append(...doc.body.childNodes);
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

export const htmlRegionNodes = ({ root, islands }: ParsedHtmlRegion): HtmlRegionNode[] => {
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
			const allowed = ALLOWED_ATTRS.get(tag);
			// Only `renderHtml === true` keeps an unlisted element; its
			// children render in its place.
			if (allowed === undefined) {
				out.push(...nodesOf(element));
				continue;
			}
			const attrs = Object.fromEntries(keptAttributes(element, allowed));
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
