import { describe, expect, it } from 'vitest';
import { lex } from './lexer';
import type { GenericToken } from './lexer';
import { EMBEDDED_HTML_EXTENSIONS } from './extensions/embeddedHtml';
import { sanitizeEmbeddedHtmlToken } from '../render/htmlSanitize';

// An image written as inline HTML becomes a token; the same HTML inside a
// block goes through the sanitizer. Both must read it as the HTML parser
// does, which only a real one can show.

const inlineImage = (html: string): GenericToken | undefined => {
	const [paragraph] = lex(`lead ${html} tail`, EMBEDDED_HTML_EXTENSIONS) as GenericToken[];
	return (paragraph.tokens as GenericToken[]).find((token) => token.type === 'image');
};

const blockImage = (html: string): HTMLImageElement | null => {
	const host = document.implementation.createHTMLDocument('').createElement('div');
	host.innerHTML = sanitizeEmbeddedHtmlToken({ raw: `<div>${html}</div>`, block: true });
	return host.querySelector('img');
};

const CASES: [name: string, html: string][] = [
	[
		'a picture whose first img is inside an inert template',
		'<picture><template><img src="https://example.invalid/wrong.png"></template><img src="https://example.invalid/right.png" alt="right"></picture>'
	],
	[
		'a picture whose first img is inside a comment',
		'<picture><!-- <img src="https://example.invalid/wrong.png"> --><img src="https://example.invalid/right.png" alt="right"></picture>'
	],
	[
		'a picture with sources before its img',
		'<picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png"><img src="https://example.invalid/right.png" alt="right"></picture>'
	],
	['legacy and numeric references in attributes', '<img src="https://example.invalid/right.png?a=1&amp;b=2&copy=3" alt="&#x80; &copy" title="badge &amp title">'],
	['an attribute name inside another value', '<img title="x src=https://example.invalid/wrong.png" src="https://example.invalid/right.png" alt="right" width="40" height="20">']
];

describe('an inline HTML image and the same image in a block', () => {
	it.each(CASES)('agree on %s', (_, html) => {
		const token = inlineImage(html);
		const element = blockImage(html);
		expect(token, 'inline token').toBeDefined();
		expect(element, 'block element').not.toBeNull();
		expect({
			src: new URL(token!.href).href,
			alt: token!.text,
			title: token!.title,
			width: token!.width,
			height: token!.height
		}).toEqual({
			src: element!.getAttribute('src'),
			alt: element!.getAttribute('alt') ?? '',
			title: element!.getAttribute('title'),
			width: element!.getAttribute('width'),
			height: element!.getAttribute('height')
		});
	});

	it('reads the values the HTML parser does', () => {
		expect(inlineImage(CASES[0][1])?.href).toBe('https://example.invalid/right.png');
		const legacy = inlineImage(CASES[3][1]);
		expect(legacy?.text).toBe('€ ©');
		expect(legacy?.title).toBe('badge & title');
		expect(legacy?.href).toBe('https://example.invalid/right.png?a=1&b=2&copy=3');
	});

	it('claims no picture without an img, or without its end tag', () => {
		expect(inlineImage('<picture><source srcset="https://example.invalid/a.png"></picture>')).toBeUndefined();
		expect(inlineImage('<picture><img src="https://example.invalid/a.png">')?.raw).toBe('<img src="https://example.invalid/a.png">');
	});
});
