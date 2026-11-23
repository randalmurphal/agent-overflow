import { describe, expect, it, vi } from 'vitest';
import { Lexer } from '../engine';
import { getLexOptions, lex } from '../lexer';
import type { GenericToken } from '../lexer';
import { markedHtmlEntities } from './htmlEntities';

const lexEntities = (src: string) => lex(src, [markedHtmlEntities]) as GenericToken[];

const inlineOf = (src: string): GenericToken[] => {
	const [paragraph] = lexEntities(src);
	expect(paragraph.type).toBe('paragraph');
	return paragraph.tokens as GenericToken[];
};

// The text a paragraph of text, escapes and code spans renders.
const shown = (tokens: GenericToken[]): string => tokens.map((token) => token.text).join('');

describe('markedHtmlEntities', () => {
	it('decodes named, decimal and hex references to the characters the forges render', () => {
		const tokens = inlineOf('A&nbsp;B &lt;tag&gt; &amp; &copy; &#35; &#x1F600; &#X22; &ngE; &CounterClockwiseContourIntegral;');
		expect(shown(tokens)).toBe('A B <tag> & © # 😀 " ≧̸ ∳');
		const escapes = tokens.filter((token) => token.type === 'escape');
		expect(escapes.map((token) => token.raw)).toEqual([
			'&nbsp;', '&lt;', '&gt;', '&amp;', '&copy;', '&#35;', '&#x1F600;', '&#X22;', '&ngE;', '&CounterClockwiseContourIntegral;'
		]);
	});

	it('replaces NUL, surrogates and out-of-range code points with U+FFFD', () => {
		expect(shown(inlineOf('&#0; &#xD800; &#xDFFF; &#x110000; &#9999999;'))).toBe('� � � � �');
	});

	it('leaves incomplete, unknown and over-long references literal', () => {
		const src = '&nbsp &x; &#; &#x; &#87654321; &#x1234567; &#abcdef0; &ThisIsNotDefined; &hi?; &copy &notanentity; &notin &amp';
		const tokens = inlineOf(src);
		expect(shown(tokens)).toBe(src);
		expect(tokens.some((token) => token.type === 'escape')).toBe(false);
	});

	it('does not read a legacy prefix out of a longer unknown name', () => {
		// The HTML parser alone would read `&not` here and leave `anentity;`.
		expect(shown(inlineOf('a &notanentity; b'))).toBe('a &notanentity; b');
		expect(shown(inlineOf('a &ampx; b'))).toBe('a &ampx; b');
		// The full name still decodes, before and after a miss is seen.
		expect(shown(inlineOf('&notin; &not; &notinva;'))).toBe('∉ ¬ ∉');
	});

	it('leaves a code span untouched', () => {
		const tokens = inlineOf('x `&amp; &#35;` y &amp;');
		const code = tokens.find((token) => token.type === 'codespan');
		expect(code?.text).toBe('&amp; &#35;');
		expect(shown(tokens)).toBe('x &amp; &#35; y &');
	});

	it('leaves fenced and indented code untouched', () => {
		const [fence] = lexEntities('```\n&amp; &#35; &copy;\n```');
		expect(fence.type).toBe('code');
		expect(fence.text).toBe('&amp; &#35; &copy;');
		const [indented] = lexEntities('    &amp; &copy;');
		expect(indented.type).toBe('code');
		expect(indented.text).toBe('&amp; &copy;');
	});

	it('decodes link text and leaves an escaped ampersand alone', () => {
		const [link] = inlineOf('[a &amp; b](https://example.com/?x=1&amp;y=2)');
		expect(link.type).toBe('link');
		expect(shown(link.tokens)).toBe('a & b');
		expect(shown(inlineOf('\\&amp;'))).toBe('&amp;');
	});

	it('decodes references in link and image destinations and titles', () => {
		const [link] = inlineOf('[go](https://example.invalid/?a=1&amp;b=2 "A &amp; B")');
		expect(link).toMatchObject({ type: 'link', href: 'https://example.invalid/?a=1&b=2', title: 'A & B' });
		expect(new URL(link.href).searchParams.get('b')).toBe('2');
		const [image] = inlineOf('![alt](<https://example.invalid/a&#32;b.png>)');
		expect(image).toMatchObject({ type: 'image', href: 'https://example.invalid/a b.png', title: null });
		// Balanced parentheses stay the engine's; an escape keeps its `&` literal.
		const [parens] = inlineOf('[p](https://example.invalid/(x&amp;y)?q=\\&amp;)');
		expect(parens.href).toBe('https://example.invalid/(x&y)?q=&amp;');
	});

	it('lexes a link once, and leaves one without references to the engine', () => {
		const spy = vi.spyOn(Lexer.prototype, 'inlineTokens');
		try {
			inlineOf('[**a**](https://example.invalid/?a&amp;b)');
			// The paragraph, the link text, the strong's text.
			expect(spy).toHaveBeenCalledTimes(3);
		} finally {
			spy.mockRestore();
		}
		const [plain] = inlineOf('[a](https://example.invalid/?a=1&b=2)');
		const [engine] = (lex('[a](https://example.invalid/?a=1&b=2)') as GenericToken[])[0].tokens as GenericToken[];
		expect(plain).toEqual(engine);
	});

	it('decodes references in a reference definition, which every use of it links to', () => {
		const src = '[go][ref] and [again][Ref]\n\n[ref]: https://example.invalid/?a=1&amp;b=2 "A &amp; B"';
		const tokens = lex(src, [markedHtmlEntities]) as GenericToken[];
		const links = (tokens[0].tokens as GenericToken[]).filter((token) => token.type === 'link');
		expect(links.map((token) => [token.href, token.title])).toEqual([
			['https://example.invalid/?a=1&b=2', 'A & B'],
			['https://example.invalid/?a=1&b=2', 'A & B']
		]);
		expect(tokens.reduce((sum, token) => sum + token.raw.length, 0)).toBe(src.replace(/\n\n/, '').length);
	});

	it('leaves an autolink and a bare URL as written', () => {
		const tokens = inlineOf('<https://example.invalid/?a&amp;b> and https://example.invalid/?c&amp;d');
		expect(tokens.filter((token) => token.type === 'link').map((token) => token.href)).toEqual([
			'https://example.invalid/?a&amp;b',
			'https://example.invalid/?c&amp;d'
		]);
	});

	it('decodes inside emphasis, headings, list items and table cells', () => {
		const [strong] = inlineOf('**a &amp; b**');
		expect(strong.type).toBe('strong');
		expect(shown(strong.tokens)).toBe('a & b');
		const [heading] = lexEntities('# A &copy; B');
		expect(shown(heading.tokens)).toBe('A © B');
		const [table] = lexEntities('| h &amp; |\n| --- |\n| c &lt; |');
		expect(JSON.stringify(table)).toContain('"text":"&"');
		expect(JSON.stringify(table)).toContain('"text":"<"');
		const [list] = lexEntities('- item &gt; one');
		expect(JSON.stringify(list)).toContain('"text":">"');
	});

	it('keeps raw lengths summing to the source', () => {
		const src = 'one &amp; two &#35; &bogus; `&lt;` &copy\n\nnext &gt; para &#x1F600;';
		// The unfiltered token list: lex() drops blank-line space tokens.
		const blocks = new Lexer(getLexOptions([markedHtmlEntities])).lex(src) as GenericToken[];
		expect(blocks.reduce((sum, token) => sum + token.raw.length, 0)).toBe(src.length);
		for (const block of blocks.filter((token) => token.type === 'paragraph')) {
			const inline = block.tokens as GenericToken[];
			expect(inline.map((token) => token.raw).join('')).toBe(block.text);
		}
	});

	it('changes nothing for a lexer without the extension', () => {
		const [paragraph] = lex('A &amp; B') as GenericToken[];
		expect(paragraph.tokens.map((token: GenericToken) => token.type)).toEqual(['text']);
		expect(paragraph.text).toBe('A &amp; B');
		// Agent chat keeps a link destination as written.
		const tokens = lex('[go](https://example.invalid/?a=1&amp;b=2) [r][r]\n\n[r]: https://example.invalid/?c&amp;d') as GenericToken[];
		expect((tokens[0].tokens as GenericToken[]).filter((token) => token.type === 'link').map((token) => token.href)).toEqual([
			'https://example.invalid/?a=1&amp;b=2',
			'https://example.invalid/?c&amp;d'
		]);
	});
});
