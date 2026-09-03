import { describe, expect, it } from 'vitest';
import { lex } from './lexer';
import { EMBEDDED_HTML_EXTENSIONS } from './extensions/embeddedHtml';
import type { DetailsSummaryToken, DetailsToken } from './extensions/embeddedHtml';
import { parseBlocks } from './parseBlocks';
import type { GenericToken } from './lexer';

// The opt-in embedded-HTML extensions (forge comment surfaces). Lexing
// behavior only — the sanitizer half lives in render/htmlSanitize.test.ts.

const lexEmbedded = (src: string) => lex(src, EMBEDDED_HTML_EXTENSIONS);

const detailsOf = (src: string): DetailsToken => {
	const tokens = lexEmbedded(src);
	const details = tokens.find((token) => token.type === 'details');
	expect(details, `no details token in: ${JSON.stringify(tokens.map((t) => t.type))}`).toBeDefined();
	return details as DetailsToken;
};

describe('markedDetails', () => {
	it('folds an entire details run, across blank lines, into one token', () => {
		const src = '<details>\n<summary>More</summary>\n\nSome **bold** prose.\n\n- a\n- b\n\n</details>\n';
		const tokens = lexEmbedded(src);
		expect(tokens).toHaveLength(1);
		const details = tokens[0] as DetailsToken;
		expect(details.type).toBe('details');
		expect(details.raw).toBe(src);
		expect(details.open).toBe(false);
		const summary = details.tokens[0] as DetailsSummaryToken;
		expect(summary.type).toBe('detailsSummary');
		expect(summary.text).toBe('More');
		const childTypes = details.tokens.slice(1).map((token) => token.type);
		expect(childTypes).toEqual(['paragraph', 'list']);
	});

	it('keeps the whole run one block at the document splitter too', () => {
		const src = '<details>\n<summary>s</summary>\n\nbody paragraph\n\n</details>\n\nafter';
		const blocks = parseBlocks(src, EMBEDDED_HTML_EXTENSIONS);
		expect(blocks[0]).toContain('</details>');
		expect(blocks[blocks.length - 1]).toBe('after');
	});

	it('honors the authored open attribute in any spelling', () => {
		expect(detailsOf('<details open>\n<summary>x</summary>\nb\n</details>').open).toBe(true);
		expect(detailsOf('<details open="">\nb\n</details>').open).toBe(true);
		expect(detailsOf('<details class="c" open>\nb\n</details>').open).toBe(true);
		expect(detailsOf('<details class="open-question">\nb\n</details>').open).toBe(false);
	});

	it('supports nesting: the inner dropdown is a child details token', () => {
		const details = detailsOf(
			'<details>\n<summary>outer</summary>\n\n<details>\n<summary>inner</summary>\n\ninner body\n\n</details>\n\n</details>'
		);
		const inner = details.tokens.find((token) => token.type === 'details') as DetailsToken;
		expect(inner).toBeDefined();
		expect((inner.tokens[0] as DetailsSummaryToken).text).toBe('inner');
	});

	it('ignores a </details> quoted inside a fenced code block', () => {
		const details = detailsOf(
			'<details>\n<summary>code</summary>\n\n```html\n</details>\n```\n\ntail prose\n\n</details>'
		);
		expect(details.raw).toMatch(/tail prose[\s\S]*<\/details>$/);
		const fence = details.tokens.find((token) => token.type === 'code') as GenericToken;
		expect(fence?.text).toBe('</details>');
	});

	it('claims nothing for an unclosed details (the sanitizer degrades it)', () => {
		const tokens = lexEmbedded('<details>\n<summary>truncated</summary>\n\nbody');
		expect(tokens.some((token) => token.type === 'details')).toBe(false);
		expect(tokens[0].type).toBe('html');
	});

	it('lexes a summary containing safe inline html', () => {
		const details = detailsOf('<details>\n<summary><b>3 issues</b> found</summary>\nbody\n</details>');
		const summary = details.tokens[0] as DetailsSummaryToken;
		expect(summary.tokens.map((token) => (token as GenericToken).type)).toContain('strong');
	});

	it('renders a details with no summary as body-only children', () => {
		const details = detailsOf('<details>\n\njust a body\n\n</details>');
		expect(details.tokens.every((token) => token.type !== 'detailsSummary')).toBe(true);
		expect(details.tokens[0].type).toBe('paragraph');
	});
});

describe('markedEmbeddedInlineHtml', () => {
	const inlineOf = (src: string): GenericToken[] => {
		const [paragraph] = lexEmbedded(src);
		return ((paragraph as GenericToken).tokens ?? []) as GenericToken[];
	};

	it.each([
		['<b>bold</b>', 'strong'],
		['<strong>bold</strong>', 'strong'],
		['<i>it</i>', 'em'],
		['<em>it</em>', 'em'],
		['<del>gone</del>', 'del'],
		['<s>gone</s>', 'del'],
		['<sub>2</sub>', 'sub'],
		['<sup>2</sup>', 'sup'],
		['<code>x</code>', 'codespan'],
		['<kbd>Ctrl</kbd>', 'codespan'],
	])('maps %s onto a native %s token', (src, type) => {
		const tokens = inlineOf(`before ${src} after`);
		expect(tokens.map((token) => token.type)).toContain(type);
	});

	it('recurses into pair content so nesting and markdown both work', () => {
		const tokens = inlineOf('x <b>has <i>both</i> and *stars*</b> y');
		const strong = tokens.find((token) => token.type === 'strong') as GenericToken;
		const innerTypes = (strong.tokens as GenericToken[]).map((token) => token.type);
		expect(innerTypes).toContain('em');
	});

	it('turns <img> into an image token carrying src and alt', () => {
		const tokens = inlineOf('badge <img src="https://x/b.svg" alt="CI"> row');
		const image = tokens.find((token) => token.type === 'image') as GenericToken;
		expect(image.href).toBe('https://x/b.svg');
		expect(image.text).toBe('CI');
	});

	it('turns an <a href> pair into a link token', () => {
		const tokens = inlineOf('see <a href="https://example.com" title="t">the docs</a>.');
		const link = tokens.find((token) => token.type === 'link') as GenericToken;
		expect(link.href).toBe('https://example.com');
		expect(link.title).toBe('t');
		expect((link.tokens as GenericToken[])[0].text).toBe('the docs');
	});

	it('leaves an unpaired safe tag to the engine html rule', () => {
		const tokens = inlineOf('a <b>never closed');
		expect(tokens.some((token) => token.type === 'strong')).toBe(false);
		expect(tokens.some((token) => token.type === 'html')).toBe(true);
	});

	it('leaves unknown tags to the engine html rule', () => {
		const tokens = inlineOf('a <blink>x</blink> b');
		expect(tokens.some((token) => token.type === 'html')).toBe(true);
	});

	it('claims nothing without the extensions registered', () => {
		const [paragraph] = lex('a <b>bold</b> c');
		const tokens = ((paragraph as GenericToken).tokens ?? []) as GenericToken[];
		expect(tokens.some((token) => token.type === 'strong')).toBe(false);
	});
});
