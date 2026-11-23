import { describe, expect, it } from 'vitest';
import { EMBEDDED_HTML_EXTENSIONS, lex, type Extension } from '../../index';
import { markedImageAttributes, readImageAttributes } from './imageAttributes';

type Tok = { type: string; raw: string; text?: string; href?: string; width?: string | null; height?: string | null; tokens?: Tok[] };

const inline = (source: string, extensions: Extension[] = [markedImageAttributes]): Tok[] =>
	((lex(source, extensions) as unknown as Tok[])[0]?.tokens ?? []) as Tok[];
const kinds = (tokens: Tok[]) => tokens.map((token) => [token.type, token.raw]);

describe('readImageAttributes', () => {
	it.each([
		['{width=100 height=100px}', '100', '100', null],
		['{width=75%}', '75%', null, null],
		['{height=20}', null, '20', null],
		['{width="30" height="40px"}', '30', '40', null],
		['{ width=30   height=40 }', '30', '40', null],
		// GitLab's float classes.
		['{.float-left width=30}', '30', null, 'left'],
		['{.float-right}', null, null, 'right'],
		// Every other entry is dropped; the list is consumed anyway.
		['{.float-center .custom}', null, null, null],
		['{width=30em height=12345 depth=3}', null, null, null],
		['{Width=30}', null, null, null],
		['{width=30 width=40}', '40', null, null]
	])('reads %s', (list, width, height, align) => {
		expect(readImageAttributes(`${list} after`)).toEqual({ raw: list, width, height, align });
	});

	it.each([
		['no list', 'width=30'],
		['an empty list', '{}'],
		['an unclosed list', '{width=30'],
		['a list across lines', '{width=30\nheight=20}'],
		['a list over 100 characters', `{width=30 ${'x'.repeat(92)}}`]
	])('reads nothing from %s', (_name, source) => {
		expect(readImageAttributes(source)).toBeUndefined();
	});

	it('ends at the first closing brace', () => {
		expect(readImageAttributes('{width=30} and {kept}')?.raw).toBe('{width=30}');
	});

	it('reads only a list the source starts with', () => {
		expect(readImageAttributes('x{width=30}')).toBeUndefined();
	});

	it('allows a 100-character list', () => {
		const list = `{width=30 ${'x'.repeat(91)}}`;
		expect(list.length).toBe(102);
		expect(readImageAttributes(list)?.width).toBe('30');
	});
});

describe('markedImageAttributes', () => {
	it('sizes the image before it and consumes exactly the list, as a token that shows nothing', () => {
		const source = '![label](https://example.com/a.png "Title Text"){width=30 height=20px} after';
		const tokens = inline(source);
		expect(kinds(tokens)).toEqual([
			['image', '![label](https://example.com/a.png "Title Text")'],
			['space', '{width=30 height=20px}'],
			['text', ' after']
		]);
		expect(tokens[0]).toMatchObject({ href: 'https://example.com/a.png', text: 'label', width: '30', height: '20' });
		expect(tokens.map((token) => token.raw).join('')).toBe(source);
	});

	it('sizes a reference image, whose definition is elsewhere', () => {
		const source = 'See ![label][pic]{width=30} here.\n\n[pic]: https://example.com/a.png';
		const tokens = inline(source);
		expect(kinds(tokens)).toEqual([
			['text', 'See '],
			['image', '![label][pic]'],
			['space', '{width=30}'],
			['text', ' here.']
		]);
		expect(tokens[1]).toMatchObject({ href: 'https://example.com/a.png', width: '30' });
	});

	it('sizes an embedded <img> and <picture>, over the size the tag wrote', () => {
		const extensions = [markedImageAttributes, ...EMBEDDED_HTML_EXTENSIONS];
		const img = inline('A <img src="https://example.com/a.png" width="80" height="20">{width=30} badge.', extensions);
		expect(kinds(img)).toEqual([
			['text', 'A '],
			['image', '<img src="https://example.com/a.png" width="80" height="20">'],
			['space', '{width=30}'],
			['text', ' badge.']
		]);
		// One side named, one kept: GitLab sets only the attributes in the list.
		expect([img[1].width, img[1].height]).toEqual(['30', '20']);
		const picture = inline('A <picture><img src="https://example.com/a.png"></picture>{height=12} badge.', extensions);
		expect(picture.map((token) => token.type)).toEqual(['text', 'image', 'space', 'text']);
		expect(picture[1].height).toBe('12');
	});

	it('consumes a list that names no size and leaves the image unsized', () => {
		const tokens = inline('![a](https://example.com/a.png){.custom}');
		expect(kinds(tokens)).toEqual([['image', '![a](https://example.com/a.png)'], ['space', '{.custom}']]);
		expect(tokens[0]).not.toHaveProperty('width');
		expect(tokens[0]).not.toHaveProperty('align');
	});

	it('floats the image before a float class, as an align', () => {
		const tokens = inline('![a](https://example.com/a.png){.float-right width=100}\nwraps');
		expect(kinds(tokens)[1]).toEqual(['space', '{.float-right width=100}']);
		expect(tokens[0]).toMatchObject({ width: '100', align: 'right' });
	});

	it('leaves a list apart from its image, after any other token, and a second list to marked', () => {
		expect(kinds(inline('![a](https://example.com/a.png) {width=30}'))).toEqual([
			['image', '![a](https://example.com/a.png)'],
			['text', ' {width=30}']
		]);
		expect(inline('![a](https://example.com/a.png) {width=30}')[0].width).toBeUndefined();
		expect(kinds(inline('*![a](https://example.com/a.png)*{width=30}')).slice(1)).toEqual([['text', '{width=30}']]);
		expect(kinds(inline('word{width=30}'))).toEqual([['text', 'word{width=30}']]);
		const twice = inline('![a](https://example.com/a.png){width=30}{height=20}');
		expect(kinds(twice)).toEqual([
			['image', '![a](https://example.com/a.png)'],
			['space', '{width=30}'],
			['text', '{height=20}']
		]);
		expect(twice[0].height).toBeUndefined();
	});

	it('shows the list as text without the extension', () => {
		const tokens = inline('![a](https://example.com/a.png){width=30}', []);
		expect(kinds(tokens)).toEqual([['image', '![a](https://example.com/a.png)'], ['text', '{width=30}']]);
	});

	it('sizes an image inside link text and stays in the link after it', () => {
		const tokens = inline('[![a](https://example.com/a.png){width=30} https://example.com/x](https://example.com/l)');
		expect(tokens.map((token) => token.type)).toEqual(['link']);
		const inner = tokens[0].tokens!;
		// Still in the link: the URL after the image is its label text, not
		// an autolink inside a link.
		expect(inner.map((token) => token.type)).toEqual(['image', 'space', 'text']);
		expect(inner[0]).toMatchObject({ raw: '![a](https://example.com/a.png)', width: '30' });
	});
});
