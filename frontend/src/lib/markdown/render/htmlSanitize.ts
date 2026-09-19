/**
 * The `renderHtml` policy for surfaces that opt into embedded forge HTML
 * (see `parser/extensions/embeddedHtml.ts` for the split of duties).
 *
 * Everything the structural extensions did not claim lands here: raw
 * HTML tables and badge rows, wrapper `<p align>` / `<div>` runs, HTML
 * comments, and whatever else a forge comment carries. The contract:
 *
 *  - BLOCK tokens are complete runs (the engine's html block rule
 *    consumes to a blank line), so they parse into a balanced tree:
 *    allowlisted elements survive with allowlisted attributes only,
 *    unknown elements render as escaped literal tags around their
 *    processed children, and comments render as nothing.
 *  - INLINE tag tokens are single unpaired tags by the time they reach
 *    this function (the inline extension claims every safe pair), so a
 *    non-comment renders as escaped literal text — visible, inert.
 *  - `href` / `src` accept absolute http(s) only, mirroring the
 *    security boundary in AGENTS.md: a path-relative or scheme-bearing
 *    URL never reaches an anchor or img attribute.
 *
 * The output string is injected via `{@html}` by `Element.svelte`'s html
 * branch, inside `.markdown-body`, whose cascade already styles tables,
 * details and summaries.
 *
 * `claimMedia` is the one app seam. A host that fetches certain media
 * itself (forge-hosted attachments, whose bytes this page cannot read
 * directly) returns an opaque claim for a matching `src`; the element is
 * then emitted WITHOUT that `src`, carrying the claim in a data
 * attribute for the host to hydrate. The claim is an app-owned string,
 * never a URL this file resolves, fetches or writes to an href.
 */

// Allowlisted tag → allowlisted attributes. Presence means the element
// renders as itself; anything else renders as escaped literal tags.
const ALLOWED_ATTRS: Record<string, readonly string[]> = {
	a: ['href', 'title'],
	img: ['src', 'alt', 'title', 'width', 'height'],
	table: [],
	caption: [],
	thead: [],
	tbody: [],
	tfoot: [],
	tr: [],
	td: ['align', 'colspan', 'rowspan'],
	th: ['align', 'colspan', 'rowspan'],
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
	br: [],
	hr: [],
	ul: [],
	ol: [],
	li: [],
	blockquote: [],
	dl: [],
	dt: [],
	dd: [],
	h1: [],
	h2: [],
	h3: [],
	h4: [],
	h5: [],
	h6: [],
	details: ['open'],
	summary: [],
	figure: [],
	figcaption: []
};

const SAFE_URL = /^https?:\/\//i;
const COMMENTS_AND_SPACE = /(?:<!--[\s\S]*?(?:-->|$)|\s)+/g;

/** Attributes a claimed `<img>` keeps. `src` is deliberately absent. */
const CLAIMED_IMG_ATTRS = ['alt', 'title', 'width', 'height'] as const;
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
}

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

const sanitizeInto = (
	source: Node,
	parent: Element,
	claimMedia?: ClaimEmbeddedMedia
): void => {
	for (const child of source.childNodes) {
		if (child.nodeType === Node.TEXT_NODE) {
			parent.append(child.textContent ?? '');
			continue;
		}
		if (child.nodeType !== Node.ELEMENT_NODE) continue; // comments, PIs
		const element = child as Element;
		const tag = element.tagName.toLowerCase();
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
		const allowedAttrs = ALLOWED_ATTRS[tag];
		if (allowedAttrs === undefined) {
			// Unknown element: literal tags, processed children. A
			// script/style's "children" are its raw text, so this shows
			// the code as text rather than swallowing it.
			parent.append(`<${tag}>`);
			sanitizeInto(element, parent, claimMedia);
			parent.append(`</${tag}>`);
			continue;
		}
		const clean = parent.ownerDocument.createElement(tag);
		for (const name of allowedAttrs) {
			const value = element.getAttribute(name);
			if (value === null) continue;
			if ((name === 'href' || name === 'src') && !SAFE_URL.test(value.trim())) continue;
			clean.setAttribute(name, value);
		}
		parent.append(clean);
		sanitizeInto(element, clean, claimMedia);
	}
};

/**
 * `renderHtml` implementation. Returns sanitized HTML for block tokens,
 * escaped literal text for stray inline tags, and '' for comments.
 */
export const sanitizeEmbeddedHtmlToken = (
	token: { raw: string; block?: boolean },
	options?: SanitizeEmbeddedHtmlOptions
): string => {
	const raw = token.raw;
	if (raw.replace(COMMENTS_AND_SPACE, '') === '') return '';
	if (!token.block) return escapeHtml(raw);
	const doc = new DOMParser().parseFromString(raw, 'text/html');
	const out = doc.createElement('div');
	sanitizeInto(doc.body, out, options?.claimMedia);
	return out.innerHTML;
};
