import { describe, expect, it, vi } from 'vitest';
import { lex } from '../lexer';
import { Lexer } from './Lexer';
import { imageAlt } from '../../render/imageAlt';
import type { Extension, GenericToken } from '../lexer';
import { EMBEDDED_HTML_EXTENSIONS } from '../extensions/embeddedHtml';
import { markedHtmlEntities } from '../extensions/htmlEntities';

const inlineOf = (source: string, extensions: Extension[] = []): GenericToken[] =>
	(lex(source, extensions) as GenericToken[])[0].tokens as GenericToken[];

const types = (tokens: GenericToken[]): unknown[] =>
	tokens.map((token) => (token.tokens && token.type !== 'text' ? [token.type, types(token.tokens)] : token.type));

describe('link state', () => {
	it('stays in the link after an image in its text', () => {
		// A bare URL is an autolink only outside a link.
		const [link, ...after] = inlineOf(
			'[![alt](https://example.invalid/i.png) https://example.invalid/in](https://example.invalid/out) https://example.invalid/after'
		);
		expect(types([link])).toEqual([['link', [['image', ['text']], 'text']]]);
		expect(after.map((token) => token.type)).toEqual(['text', 'link']);
	});

	it('builds no link in link text, whoever would build it: the innermost link wins', () => {
		const deep = (tokens: GenericToken[]): GenericToken[] =>
			tokens.flatMap((token) => [token, ...deep((token.tokens as GenericToken[] | undefined) ?? [])]);
		const links = (source: string, extensions: Extension[] = []) =>
			deep(inlineOf(source, extensions)).filter((token) => token.type === 'link').map((token) => token.href);
		const source = '[ref]: /ref\n\n';
		expect(links('[a [b](/b) c](/out)')).toEqual(['/b']);
		expect(links('[a [b [c](/c) d](/b) e](/out)')).toEqual(['/c']);
		expect(links(`[a [b][ref] c](/out)\n\n${source}`)).toEqual(['/ref']);
		expect(links(`[a [ref] c](/out)\n\n${source}`)).toEqual(['/ref']);
		// An autolink in link text is text; CommonMark's innermost rule
		// counts bracket links only.
		expect(links('[a <https://example.invalid/b> c](/out)')).toEqual(['/out']);
		// A bracket pair naming no definition is text, not a link.
		expect(links('[a [b] c](/out)')).toEqual(['/out']);
		// An HTML anchor around a Markdown link gives way to it, as a forge's
		// HTML parser closes the outer anchor; one inside link text is a
		// span, and a stray end tag in a label does not end the label.
		expect(links('a <a href="/out">[b](/b)</a> z', EMBEDDED_HTML_EXTENSIONS)).toEqual(['/b']);
		expect(links('a <a href="/out">b <em>c</em></a> z', EMBEDDED_HTML_EXTENSIONS)).toEqual(['/out']);
		expect(links('[a <a href="/b">b</a> c](/out)', EMBEDDED_HTML_EXTENSIONS)).toEqual(['/out']);
		expect(links('[a </a> <a href="/b">b</a> c](/out)', EMBEDDED_HTML_EXTENSIONS)).toEqual(['/out']);
	});

	it('shows the outer brackets and destination of a link around a link as text', () => {
		const source = '[outer [inner](/i) text](/o) after';
		const tokens = inlineOf(source);
		expect(tokens.map((token) => token.raw).join('')).toBe(source);
		expect(tokens.map((token) => [token.type, token.raw])).toEqual([
			['text', '[outer '],
			['link', '[inner](/i)'],
			['text', ' text](/o) after']
		]);
		// The anchor's own text, around a raw anchor.
		const [span] = inlineOf('<a href="/out">x [md](/in) y</a>', EMBEDDED_HTML_EXTENSIONS);
		expect(types([span])).toEqual([['htmlInline', ['text', ['link', ['text']], 'text']]]);
	});

	it('lexes an image label for its links, and a link there unlinks the link around the image', () => {
		const [image] = inlineOf('![alt with [link](/x)](/y.png)');
		expect(image.type).toBe('image');
		expect(imageAlt(image as never)).toBe('alt with link');
		const tokens = inlineOf('[![a [b](/b)](/i.png)](/o)');
		expect(tokens.map((token) => token.type)).toEqual(['text', 'image', 'text']);
		expect(tokens.map((token) => token.raw).join('')).toBe('[![a [b](/b)](/i.png)](/o)');
	});

	it('lexes link text once unless the link turns out not to be one', () => {
		const lexed = vi.spyOn(Lexer.prototype, 'inlineTokens');
		try {
			inlineOf('[plain **text**](/u) and [a [b](/b) c](/o)');
			const calls = lexed.mock.calls.map(([src]) => src);
			expect(calls.filter((src) => src === 'plain **text**')).toHaveLength(1);
			// The outer text once as the would-be link's, the inner once as a link.
			expect(calls.filter((src) => src === 'a [b](/b) c')).toHaveLength(1);
			expect(calls.filter((src) => src === 'b')).toHaveLength(1);
		} finally {
			lexed.mockRestore();
		}
	});

	it('keeps Markdown links between raw anchor tags, which are text in chat', () => {
		const tokens = inlineOf('<a href="x"> [b](https://example.invalid/b) and [c](https://example.invalid/c)');
		expect(tokens.filter((token) => token.type === 'link').map((token) => token.href)).toEqual([
			'https://example.invalid/b',
			'https://example.invalid/c'
		]);
	});

	it('leaves the link when it ends', () => {
		const tokens = inlineOf('[a](https://example.invalid/a) https://example.invalid/b');
		expect(tokens.map((token) => token.type)).toEqual(['link', 'text', 'link']);
	});
});

describe('the linkText option', () => {
	// An extension that builds its token from the engine's link rule, as the
	// app's attachment and path extensions do.
	const claimsImages: Extension = {
		name: 'claimsImages',
		level: 'inline',
		tokenizer(src) {
			if (!src.startsWith('![')) return undefined;
			const image = this.lexer.tokenizer.link(src);
			return image && { ...image, type: 'claimed' };
		}
	};

	it('resolves a destination for every tokenizer that uses the engine rule', () => {
		const source =
			'![a](https://example.invalid/i.png?a=1&amp;b=2 "A &amp; B") [go](https://example.invalid/?c=1&amp;d=2)';
		const [claimed, , link] = inlineOf(source, [claimsImages, markedHtmlEntities]);
		expect(claimed).toMatchObject({ type: 'claimed', href: 'https://example.invalid/i.png?a=1&b=2', title: 'A & B' });
		expect(link).toMatchObject({ type: 'link', href: 'https://example.invalid/?c=1&d=2' });
	});

	it('is the escape-only default without an extension that sets it', () => {
		const [claimed, , link] = inlineOf(
			'![a](https://example.invalid/i.png?a=1&amp;b=\\*) [go](https://example.invalid/?c=1&amp;d=2)',
			[claimsImages]
		);
		expect(claimed.href).toBe('https://example.invalid/i.png?a=1&amp;b=*');
		expect(link.href).toBe('https://example.invalid/?c=1&amp;d=2');
	});
});
