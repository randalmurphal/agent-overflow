import { describe, expect, it, vi } from 'vitest';
import { Tokenizer } from './engine';
import { lex } from './lexer';
import type { GenericToken, StreamdownToken } from './lexer';
import { EMBEDDED_HTML_EXTENSIONS } from './extensions/embeddedHtml';
import type { DetailsToken } from './extensions/embeddedHtml';
import type { HtmlRegionToken } from './extensions/htmlRegion';
import { OpenElements } from './extensions/htmlTags';
import { parseBlocks } from './parseBlocks';

// The html region block extension: an HTML wrapper around Markdown lexes as
// one token whose HTML parts and Markdown islands stay in source order.

const lexEmbedded = (src: string) => lex(src, EMBEDDED_HTML_EXTENSIONS);
const types = (tokens: readonly { type: string }[]) => tokens.map((token) => token.type);

const regionOf = (src: string): HtmlRegionToken => {
	const tokens = lexEmbedded(src);
	const region = tokens.find((token) => token.type === 'htmlRegion');
	expect(region, `no region in: ${JSON.stringify(types(tokens))}`).toBeDefined();
	return region as HtmlRegionToken;
};

// Each part as its HTML or `[type]`.
const shape = (region: HtmlRegionToken) =>
	region.parts.map((part) => (typeof part === 'string' ? part : `[${part.type}]`));

const CODERABBIT =
	'<details>\n<summary>🧩 Analysis chain</summary><blockquote>\n\nSome **markdown** here.\n\n<details>\n<summary>Nested</summary>\n\ninner text\n\n</details>\n\n</blockquote></details>';
const TABLE = '<table>\n<tr><th>A</th><th>B</th></tr>\n<tr><td>\n\n**md in cell**\n\n</td><td><code>x</code></td></tr>\n</table>';
const CENTERED = '<div align="center">\n\n**markdown** inside div\n\n</div>';

describe('markedHtmlRegion', () => {
	it('keeps a blockquote wrapper inside a details body around its Markdown', () => {
		const tokens = lexEmbedded(CODERABBIT);
		expect(types(tokens)).toEqual(['details']);
		const details = tokens[0] as DetailsToken;
		expect(types(details.tokens)).toEqual(['detailsSummary', 'htmlRegion']);
		const region = details.tokens[1] as HtmlRegionToken;
		expect(shape(region)).toEqual(['<blockquote>\n\n', '[paragraph]', '[details]', '</blockquote>']);
		expect(types(region.tokens)).toEqual(['paragraph', 'details']);
		expect(region.raw).toMatch(/^<blockquote>[\s\S]*<\/blockquote>$/);
		expect(CODERABBIT).toContain(region.raw);
	});

	it('keeps Markdown in a table cell inside the table', () => {
		const tokens = lexEmbedded(TABLE);
		expect(types(tokens)).toEqual(['htmlRegion']);
		const region = tokens[0] as HtmlRegionToken;
		expect(region.raw).toBe(TABLE);
		expect(shape(region)).toEqual([
			'<table>\n<tr><th>A</th><th>B</th></tr>\n<tr><td>\n\n',
			'[paragraph]',
			'</td><td><code>x</code></td></tr>\n</table>'
		]);
		// The island's inline content fills when the lex drains.
		expect(types((region.tokens[0] as GenericToken).tokens)).toEqual(['strong']);
	});

	it('keeps Markdown inside a centered div', () => {
		const region = regionOf(CENTERED);
		expect(region.raw).toBe(CENTERED);
		expect(shape(region)).toEqual(['<div align="center">\n\n', '[paragraph]', '</div>']);
	});

	it('consumes exactly its source, so block raws sum to the document', () => {
		for (const tail of ['', '\n', '\n\nafter **this**', '\nstill html\n\nafter']) {
			const src = `${CENTERED}${tail}`;
			const region = lexEmbedded(src)[0] as HtmlRegionToken;
			expect(region.type).toBe('htmlRegion');
			expect(src.startsWith(region.raw)).toBe(true);
			const blocks = parseBlocks(src, EMBEDDED_HTML_EXTENSIONS);
			expect(blocks.join('')).toBe(src);
			expect(blocks[0]).toBe(region.raw);
		}
	});

	it('is one block at the document splitter', () => {
		for (const src of [CODERABBIT, TABLE, CENTERED]) {
			const blocks = parseBlocks(`before\n\n${src}\n\nafter`, EMBEDDED_HTML_EXTENSIONS);
			expect(blocks.map((block) => block.trim())).toEqual(['before', src, 'after']);
		}
	});

	it('leaves a balanced html block to the engine', () => {
		for (const src of [
			'<div align="center">\n<img src="https://example.com/a.png">\n</div>\n\nafter',
			'<table><tr><td>cell</td></tr></table>',
			'<div>\n```\n</div>\n\n[a](https://example.com/q) w',
			'<p align="center">\n<img src="https://example.com/a.png">\n\nnot wrapped',
			'<div/>\n\nnot wrapped\n\n</div>'
		]) {
			const tokens = lexEmbedded(src);
			expect(types(tokens), src).not.toContain('htmlRegion');
			expect(tokens[0].type, src).toBe('html');
		}
	});

	it('claims nothing for a wrapper that never closes', () => {
		const tokens = lexEmbedded('<div align="center">\n\n**markdown**\n\nno close');
		expect(types(tokens)).toEqual(['html', 'paragraph', 'paragraph']);
	});

	it('does not count tags in Markdown prose, code spans or fences', () => {
		// The only real close is the last block.
		const src = '<div>\n\nprose `</div>` and </div> inline\n\n```html\n</div>\n```\n\n    </div> indented\n\n</div>\n\nafter';
		const tokens = lexEmbedded(src);
		expect(types(tokens)).toEqual(['htmlRegion', 'paragraph']);
		const region = tokens[0] as HtmlRegionToken;
		expect(shape(region)).toEqual(['<div>\n\n', '[paragraph]', '[code]', '[code]', '</div>\n\n']);
		expect((region.tokens[1] as GenericToken).text).toBe('</div>');
	});

	it('ignores a close tag quoted in a fence, and an unknown or stray tag', () => {
		const region = regionOf('<div>\n<section>\n\n```\n</div>\n```\n\n</td></span>\n</div>');
		expect(shape(region)).toEqual(['<div>\n<section>\n\n', '[code]', '</td></span>\n</div>']);
	});

	it('reads no tag inside a comment or a quoted attribute value', () => {
		const region = regionOf('<div title="a>b</div>"><!-- </div> -->\n\n**wrapped**\n\n</div>');
		expect(shape(region)).toEqual(['<div title="a>b</div>"><!-- </div> -->\n\n', '[paragraph]', '</div>']);
	});

	it('flattens a nested region into its parent', () => {
		const src = '<table><tr><td>\n\n**a**\n\n</td><td>\n\n_b_\n\n</td></tr></table>';
		const tokens = lexEmbedded(src);
		expect(types(tokens)).toEqual(['htmlRegion']);
		const region = tokens[0] as HtmlRegionToken;
		expect(shape(region)).toEqual(['<table><tr><td>\n\n', '[paragraph]', '</td><td>\n\n', '[paragraph]', '</td></tr></table>']);
		expect(region.tokens.some((token) => token.type === 'htmlRegion')).toBe(false);
	});

	it('nests a wrapper inside a wrapper', () => {
		const region = regionOf('<div align="center">\n\n<blockquote>\n\nquoted\n\n</blockquote>\n\nafter quote\n\n</div>');
		expect(shape(region)).toEqual([
			'<div align="center">\n\n<blockquote>\n\n',
			'[paragraph]',
			'</blockquote>\n\n',
			'[paragraph]',
			'</div>'
		]);
	});

	it('keeps an island that is a details run, and lexes islands with every extension', () => {
		const region = regionOf('<div>\n\n<details><summary>s</summary>\n\nbody\n\n</details>\n\n> [!NOTE]\n> alert\n\n</div>');
		expect(types(region.tokens)).toEqual(['details', 'alert']);
	});

	it('is claimed inside a list item and a blockquote', () => {
		const list = lexEmbedded('- item\n\n  <div>\n\n  **in item**\n\n  </div>\n\n- next')[0] as GenericToken;
		const item = list.tokens[0] as GenericToken;
		const inItem = (item.tokens as StreamdownToken[]).find((token) => token.type === 'htmlRegion') as HtmlRegionToken;
		expect(inItem).toBeDefined();
		expect(types(inItem.tokens).length).toBe(1);

		const quote = lexEmbedded('> <div>\n>\n> **quoted**\n>\n> </div>')[0] as GenericToken;
		expect(quote.type).toBe('blockquote');
		const inQuote = (quote.tokens as StreamdownToken[])[0] as HtmlRegionToken;
		expect(inQuote.type).toBe('htmlRegion');
		expect(shape(inQuote)).toEqual(['<div>\n\n', '[paragraph]', '</div>']);
	});

	it('drops tokens that render nothing from its islands', () => {
		const region = regionOf('<div>\n\n[ref]: https://example.com/r\n\n[x][ref]\n\n</div>');
		expect(types(region.tokens)).toEqual(['paragraph']);
	});

	it('is not registered for agent chat', () => {
		expect(types(lex(CENTERED))).toEqual(['html', 'paragraph', 'html']);
	});

	it('opens and closes on HTML blocks indented up to three spaces, as the block grammar reads them', () => {
		const src = '  <div>\n\n**inside**\n\n   </div>';
		const region = regionOf(src);
		expect(region.raw).toBe(src);
		expect(shape(region)).toEqual(['  <div>\n\n', '[paragraph]', '   </div>']);
		// Four spaces is indented code, not an HTML block.
		expect(types(lexEmbedded('    <div>\n\n**inside**\n\n</div>'))).toEqual(['code', 'paragraph', 'html']);
	});

	it('reads a fence as the block grammar does: an info string does not close it', () => {
		const src = '<div>\n\n~~~text\n~~~ not a close\n</div>\n~~~\n\n**inside**\n\n</div>';
		const tokens = lexEmbedded(src);
		expect(types(tokens)).toEqual(['htmlRegion']);
		const region = tokens[0] as HtmlRegionToken;
		expect(shape(region)).toEqual(['<div>\n\n', '[code]', '[paragraph]', '</div>']);
		expect((region.tokens[0] as GenericToken).text).toBe('~~~ not a close\n</div>');
	});

	it('reads no tag in raw text', () => {
		for (const raw of ['<script>const value = "</div>";</script>', '<style>/* </div> */</style>', '<textarea></div></textarea>']) {
			const src = `<div>\n${raw}\n\n**inside**\n\n</div>`;
			const region = regionOf(src);
			expect(region.raw, raw).toBe(src);
			expect(shape(region), raw).toEqual([`<div>\n${raw}\n\n`, '[paragraph]', '</div>']);
		}
	});

	it('counts no tag inside a list item at its own level', () => {
		// The indented `</div>` is the item's, so it closes nothing here.
		const src = '<div>\n\n- item\n\n  </div>\n\nstill inside\n\n</div>\n\nafter';
		const tokens = lexEmbedded(src);
		expect(types(tokens)).toEqual(['htmlRegion', 'paragraph']);
		expect(types((tokens[0] as HtmlRegionToken).tokens)).toEqual(['list', 'paragraph']);
	});

	it('ends where the document splitter ends it, with CRLF line endings', () => {
		const src = '<div>\r\n\r\n**inside**\r\n\r\n</div>\r\n\r\nafter';
		// The splitter reads the source as written; each block is lexed with
		// its line endings normalized, as `Lexer.lex` does.
		const blocks = parseBlocks(src, EMBEDDED_HTML_EXTENSIONS);
		expect(blocks).toEqual(['<div>\r\n\r\n**inside**\r\n\r\n</div>\r\n\r\n', 'after']);
		const tokens = lexEmbedded(src);
		expect(types(tokens)).toEqual(['htmlRegion', 'paragraph']);
		expect((tokens[0] as HtmlRegionToken).raw).toBe(blocks[0].replace(/\r\n/g, '\n'));
		expect(types(lexEmbedded(blocks[0]))).toEqual(['htmlRegion']);
	});

	it('claims a closed wrapper after one that never closes', () => {
		const tokens = lexEmbedded('<div>\n\nno close\n\n<table><tr><td>\n\n**cell**\n\n</td></tr></table>\n\ntail');
		expect(types(tokens)).toEqual(['html', 'paragraph', 'htmlRegion', 'paragraph']);
	});

	describe('scanning work', () => {
		// The engine's html tokenizer runs once per candidate HTML block in
		// the measuring pass and once in the real lex: linear in the
		// document whatever its nesting.
		const htmlCalls = (src: string): number => {
			const spy = vi.spyOn(Tokenizer.prototype, 'html');
			try {
				lexEmbedded(src);
				parseBlocks(src, EMBEDDED_HTML_EXTENSIONS);
				return spy.mock.calls.length;
			} finally {
				spy.mockRestore();
			}
		};
		const unclosed = (n: number) => '<div>\n\n'.repeat(n);
		const nested = (n: number) => `${'<div>\n\n'.repeat(n)}ok\n\n${'</div>\n\n'.repeat(n)}`;

		it.each([
			['unclosed', unclosed],
			['nested', nested]
		])('is linear in the wrappers for %s ones', (_, shape) => {
			for (const n of [400, 1800]) {
				const blocks = shape(n).split('\n\n').length;
				expect(htmlCalls(shape(n)), `n=${n}`).toBeLessThanOrEqual(5 * blocks);
			}
		});

		it('pairs end tags that close nothing without walking the open elements', () => {
			// n unmatched end tags over n open elements: each is refused by
			// name, and each open element is unwound once at most.
			const n = 4000;
			const walked = (run: () => void): number => {
				const spy = vi.spyOn(OpenElements.prototype, 'close');
				try {
					run();
					return [...new Set(spy.mock.contexts as OpenElements<unknown>[])].reduce((sum, stack) => sum + stack.walked, 0);
				} finally {
					spy.mockRestore();
				}
			};
			const src = `${'<div>'.repeat(n)}${'</table>'.repeat(n)}\n\nbody\n\n${'</div>'.repeat(n)}\n`;
			let tokens: StreamdownToken[] = [];
			// Twice n: the opener check and the pairing pass each unwind once.
			expect(walked(() => (tokens = lexEmbedded(src)))).toBeLessThanOrEqual(4 * n);
			expect(types(tokens)).toEqual(['htmlRegion']);
			expect((tokens[0] as HtmlRegionToken).raw).toBe(src);
		});

		it('unwinds the elements an end tag closes implicitly, once each', () => {
			const open = new OpenElements<number>();
			for (const [at, name] of ['div', 'table', 'tr', 'td', 'div'].entries()) open.push(name, at);
			expect(open.close('center')).toBeUndefined();
			expect([open.height, open.walked]).toEqual([5, 0]);
			expect(open.close('table')).toBe(1);
			expect([open.height, open.walked]).toEqual([1, 4]);
			expect(open.close('tr')).toBeUndefined();
			expect(open.close('div')).toBe(0);
			expect(open.close('div')).toBeUndefined();
			expect([open.height, open.walked]).toEqual([0, 5]);
		});

		it('keeps an unclosed wrapper the engine html tokens it has today', () => {
			const tokens = lexEmbedded(unclosed(1800));
			expect(tokens).toHaveLength(1800);
			expect(new Set(types(tokens))).toEqual(new Set(['html']));
		});

		it('claims a deeply nested wrapper as one region', () => {
			const src = nested(1800);
			const tokens = lexEmbedded(src);
			expect(types(tokens)).toEqual(['htmlRegion']);
			const region = tokens[0] as HtmlRegionToken;
			expect(region.raw).toBe(src);
			expect(shape(region)).toEqual(['<div>\n\n'.repeat(1800), '[paragraph]', '</div>\n\n'.repeat(1800)]);
		});
	});
});
