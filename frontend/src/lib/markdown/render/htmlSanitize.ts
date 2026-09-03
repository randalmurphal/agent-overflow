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

const escapeHtml = (text: string): string =>
	text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

const sanitizeInto = (source: Node, parent: Element): void => {
	for (const child of source.childNodes) {
		if (child.nodeType === Node.TEXT_NODE) {
			parent.append(child.textContent ?? '');
			continue;
		}
		if (child.nodeType !== Node.ELEMENT_NODE) continue; // comments, PIs
		const element = child as Element;
		const tag = element.tagName.toLowerCase();
		const allowedAttrs = ALLOWED_ATTRS[tag];
		if (allowedAttrs === undefined) {
			// Unknown element: literal tags, processed children. A
			// script/style's "children" are its raw text, so this shows
			// the code as text rather than swallowing it.
			parent.append(`<${tag}>`);
			sanitizeInto(element, parent);
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
		sanitizeInto(element, clean);
	}
};

/**
 * `renderHtml` implementation. Returns sanitized HTML for block tokens,
 * escaped literal text for stray inline tags, and '' for comments.
 */
export const sanitizeEmbeddedHtmlToken = (token: { raw: string; block?: boolean }): string => {
	const raw = token.raw;
	if (raw.replace(COMMENTS_AND_SPACE, '') === '') return '';
	if (!token.block) return escapeHtml(raw);
	const doc = new DOMParser().parseFromString(raw, 'text/html');
	const out = doc.createElement('div');
	sanitizeInto(doc.body, out);
	return out.innerHTML;
};
