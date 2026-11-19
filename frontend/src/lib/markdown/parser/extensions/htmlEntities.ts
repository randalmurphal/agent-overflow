/**
 * HTML entity and numeric character references in text (CommonMark 0.31
 * section 2.5), for forge-authored markdown: `&copy;` renders `©`,
 * `&#35;` renders `#`, `&#x1F600;` renders `😀`. Only complete references
 * decode; an unknown name, a missing semicolon or an over-long number
 * stays literal text.
 *
 * The decoded text rides an `escape` token, which both renderers and
 * find's projection already show as its `text`. Code spans, fenced code,
 * autolinks and raw HTML consume their `&` before an inline extension
 * sees it, so references there stay as written. Link and image
 * destinations and titles, inline and in reference definitions, resolve
 * references as CommonMark does, through the engine's `linkText` option,
 * so every tokenizer that builds a link from the engine's rule agrees. An
 * autolink's URL and a bare URL stay as written.
 */
import type { Extension } from '../lexer';

// Named: an HTML5 name, the longest of which is 31 characters. Decimal:
// 1-7 digits. Hex: 1-6 digits.
const REFERENCE = /^&(?:#[xX]([0-9a-fA-F]{1,6})|#([0-9]{1,7})|([A-Za-z][A-Za-z0-9]{0,31}));/;

const REPLACEMENT = '�';

/** cmark's rule: NUL, surrogates and anything past U+10FFFF become U+FFFD. */
const fromCodePoint = (code: number): string =>
	code === 0 || (code >= 0xd800 && code <= 0xdfff) || code > 0x10ffff
		? REPLACEMENT
		: String.fromCodePoint(code);

// Successful decodes only, so the memo is bounded by the named table.
const named = new Map<string, string>();
let decoder: HTMLElement | null = null;

/**
 * The text `&name;` names, or undefined when it is not a named reference.
 * The platform's HTML parser holds the full named table, so this reads it
 * through an element of an inert document (no scripts, no loads). The
 * parser also accepts legacy prefixes without a semicolon (`&notx;` reads
 * as `&not` then `x;`), which CommonMark does not: every named
 * reference decodes to one or two code points, while an unknown name or
 * a prefix match leaves at least the unmatched tail and `;`, three or more.
 */
const decodeNamed = (name: string): string | undefined => {
	const hit = named.get(name);
	if (hit !== undefined) return hit;
	if (typeof document === 'undefined') return undefined;
	decoder ??= document.implementation.createHTMLDocument('').createElement('div');
	decoder.innerHTML = `&${name};`;
	const text = decoder.textContent ?? '';
	let codePoints = 0;
	for (const _ of text) {
		if (++codePoints > 2) return undefined;
	}
	if (codePoints === 0) return undefined;
	named.set(name, text);
	return text;
};

/** The text one matched reference names; undefined for an unknown name. */
const decodeReference = (hex: string | undefined, decimal: string | undefined, name: string): string | undefined => {
	if (hex !== undefined) return fromCodePoint(parseInt(hex, 16));
	if (decimal !== undefined) return fromCodePoint(parseInt(decimal, 10));
	return decodeNamed(name);
};

const EVERY_REFERENCE = new RegExp(REFERENCE.source.slice(1), 'g');

/**
 * `html` with every complete reference decoded: the text of an HTML
 * attribute value or of a `<code>` element, which a markdown tokenizer
 * never visits.
 */
export const decodeHtmlEntities = (html: string): string =>
	html.includes('&')
		? html.replace(EVERY_REFERENCE, (raw: string, hex?: string, decimal?: string, name?: string) =>
				decodeReference(hex, decimal, name ?? '') ?? raw
			)
		: html;

// A backslash escape (the engine's punctuation class) or a reference, in
// one left-to-right pass: `\&amp;` is `&amp;`.
const LINK_TEXT = new RegExp(`\\\\([\\p{P}\\p{S}])|${EVERY_REFERENCE.source}`, 'gu');

/** A link destination or title as written, with escapes and references resolved (CommonMark 6.3). */
const linkText = (written: string): string =>
	written.replace(LINK_TEXT, (raw: string, escaped?: string, hex?: string, decimal?: string, name?: string) =>
		escaped ?? decodeReference(hex, decimal, name ?? '') ?? raw
	);

export const markedHtmlEntities: Extension = {
	name: 'htmlEntities',
	level: 'inline',
	start(src) {
		const i = src.indexOf('&');
		return i === -1 ? undefined : i;
	},
	linkText,
	tokenizer(src) {
		if (src.charCodeAt(0) !== 38 /* & */) return undefined;
		const match = REFERENCE.exec(src);
		if (!match) return undefined;
		const [raw, hex, decimal, name] = match;
		const text = decodeReference(hex, decimal, name);
		if (text === undefined) return undefined;
		return { type: 'escape', raw, text };
	}
};
