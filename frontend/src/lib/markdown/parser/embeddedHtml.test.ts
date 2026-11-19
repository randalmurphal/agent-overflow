import { describe, expect, it, vi } from 'vitest';
import { Tokenizer } from './engine';
import { lex } from './lexer';
import { EMBEDDED_HTML_EXTENSIONS, MAX_ELEMENT_NESTING } from './extensions/embeddedHtml';
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

	// CommonMark: an HTML block runs to a blank line, and its lines are HTML.
	const html = (raw: string) => ({ type: 'html', block: true, pre: false, raw, text: raw });

	it.each([
		['inline html', '<b>3 issues</b> found &amp; more'],
		['block html', '<h3>Big <b>title</b></h3>'],
		['Markdown syntax', '<ins>new</ins> and **bold** `code` <picture><img src="https://x/a.png"></picture>'],
		['a tag in a code span', 'See `<div>` and **bold**']
	])('hands a summary on its HTML block lines holding %s to the sanitizer whole', (_, inner) => {
		for (const src of [
			`<details>\n<summary>${inner}</summary>\n\nbody\n\n</details>`,
			`<details><summary>${inner}</summary>\n\nbody\n\n</details>`,
			`<details>\n\n<summary>${inner}</summary>\n\nbody\n\n</details>`
		]) {
			const details = detailsOf(src);
			expect((details.tokens[0] as DetailsSummaryToken).tokens).toEqual([html(inner)]);
			expect(details.tokens.slice(1).map((token) => token.type)).toEqual(['paragraph']);
		}
	});

	it.each([
		['Markdown syntax', 'body with **stars** and `code`'],
		['snake_case', 'a snake_case_name here'],
		['emphasis stars', '2 * 3 * 4']
	])('hands a body line on its HTML block lines holding %s to the sanitizer', (_, line) => {
		const details = detailsOf(`<details>\n<summary>Tight</summary>\n${line}\n</details>`);
		expect(details.tokens.slice(1)).toEqual([html(`\n${line}\n`)]);
	});

	it('reads where the HTML block ends with CRLF line endings', () => {
		// The lexer reads CRLF as LF.
		expect(detailsOf('<details>\r\n<summary>S</summary>\r\nraw **x**\r\n</details>').tokens.slice(1)).toEqual([html('\nraw **x**\n')]);
		const markdown = detailsOf('<details>\r\n<summary>S</summary>\r\n\r\nmarkdown **x**\r\n\r\n</details>');
		expect(markdown.tokens.slice(1).map((token) => token.type)).toEqual(['paragraph']);
	});

	it('resumes Markdown after the blank line that ends the HTML block', () => {
		const details = detailsOf('<details>\n<summary>S</summary>\nraw **one**\n\nmarkdown **two**\n</details>');
		const [region] = details.tokens.slice(1) as unknown as { type: string; parts: (string | { type: string })[] }[];
		expect(region.type).toBe('htmlRegion');
		expect(region.parts.map((part) => (typeof part === 'string' ? part : part.type))).toEqual(['\nraw **one**\n\n', 'paragraph']);
	});

	it('renders a details with no summary as body-only children', () => {
		const details = detailsOf('<details>\n\njust a body\n\n</details>');
		expect(details.tokens.every((token) => token.type !== 'detailsSummary')).toBe(true);
		expect(details.tokens[0].type).toBe('paragraph');
	});

	it.each([
		['a comment in its first HTML block', '\n<!-- </details> -->'],
		['a comment block', '\n\n<!-- </details> -->'],
		['raw text', '\n<script>const end = "</details>";</script>'],
		['a fence whose info string is no close', '\n\n~~~text\n~~~ not a close\n</details>\n~~~'],
		['a paragraph', '\n\nprose names </details> inline'],
		['a code span', '\n\nsee `</details>` here']
	])('does not end at a </details> in %s', (_, inert) => {
		const src = `<details>\n<summary>Label</summary>${inert}\n\n**inside**\n\n</details>\n\nafter`;
		const tokens = lexEmbedded(src);
		expect(tokens.map((token) => token.type)).toEqual(['details', 'paragraph']);
		const details = tokens[0] as DetailsToken;
		expect(details.raw).toBe(src.slice(0, -'after'.length));
		expect(JSON.stringify(details.tokens)).toContain('"type":"strong"');
	});

	it('takes the summary after whitespace and comments, which show nothing', () => {
		for (const lead of ['<!-- rationale -->\n', '\n<!-- a --> <!-- b -->\n  ', '<!-- <summary>not this</summary> -->']) {
			const details = detailsOf(`<details>${lead}<summary>needle label</summary>\n\nbody\n\n</details>`);
			expect(details.tokens.map((token) => token.type)).toEqual(['detailsSummary', 'paragraph']);
			expect((details.tokens[0] as { text: string }).text).toBe('needle label');
		}
		// An unterminated comment hides the summary, as it does in a browser.
		const hidden = detailsOf('<details><!-- open\n<summary>label</summary>\n\nbody\n\n</details>');
		expect(hidden.tokens.every((token) => token.type !== 'detailsSummary')).toBe(true);
	});

	it('opens on an HTML block indented up to three spaces', () => {
		const details = detailsOf('   <details>\n<summary>s</summary>\n\nbody\n\n   </details>');
		expect(details.tokens.map((token) => token.type)).toEqual(['detailsSummary', 'paragraph']);
	});

	it('claims a details that opens on the line its predecessor closes on', () => {
		const tokens = lexEmbedded('<details>\n<summary>one</summary>\n\nfirst\n\n</details>\n<details>\n<summary>two</summary>\n\nsecond\n\n</details>');
		expect(tokens.map((token) => token.type)).toEqual(['details', 'details']);
		expect(((tokens[1] as DetailsToken).tokens[0] as DetailsSummaryToken).text).toBe('two');
	});

	it('ends where the document splitter ends it, with CRLF line endings', () => {
		const run = '<details>\r\n<summary>s</summary>\r\n\r\n```\r\n</details>\r\n```\r\n\r\nbody\r\n\r\n</details>\r\n';
		const blocks = parseBlocks(`${run}\r\nafter`, EMBEDDED_HTML_EXTENSIONS);
		expect(blocks).toEqual([run, '\r\nafter']);
		expect(lexEmbedded(blocks[0]).map((token) => token.type)).toEqual(['details']);
		expect(lexEmbedded(`${run}\r\nafter`).map((token) => token.type)).toEqual(['details', 'paragraph']);
	});

	it('renders a summary set apart by blank lines as Markdown, as GitLab documents it', () => {
		const details = detailsOf('<details>\n<summary>\n\nSee `<div>` and **bold**.\n\n</summary>\n\nbody\n\n</details>');
		const summary = details.tokens[0] as DetailsSummaryToken;
		expect(summary.text).toBe('See `<div>` and **bold**.');
		expect(summary.tokens.map((token) => token.type)).toEqual(['text', 'codespan', 'text', 'strong', 'text']);
		expect(details.tokens.slice(1).map((token) => token.type)).toEqual(['paragraph']);
	});

	describe('scanning work', () => {
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

		it.each([
			['unclosed', (n: number) => '<details>\n\n'.repeat(n)],
			['nested', (n: number) => `${'<details>\n\n'.repeat(n)}ok\n\n${'</details>\n\n'.repeat(n)}`]
		])('is linear in the runs for %s ones', (_, shape) => {
			for (const n of [400, 1800]) {
				expect(htmlCalls(shape(n)), `n=${n}`).toBeLessThanOrEqual(10 * n);
			}
		});

		it('leaves a details nested past the bound to a region', () => {
			const depth = MAX_ELEMENT_NESTING + 6;
			const src = `${'<details>\n\n'.repeat(depth)}ok\n\n${'</details>\n\n'.repeat(depth)}`;
			let token = lexEmbedded(src)[0] as GenericToken;
			let claimed = 0;
			while (token.type === 'details') {
				claimed++;
				token = token.tokens[0];
			}
			expect(claimed).toBe(MAX_ELEMENT_NESTING);
			expect(token.type).toBe('htmlRegion');
			expect(token.raw).toBe(`${'<details>\n\n'.repeat(6)}ok\n\n${'</details>\n\n'.repeat(6)}`);
		});
	});
});

describe('the embedded inline tokenizer', () => {
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

	it.each(['ins', 'mark', 'small', 'abbr', 'q', 'var', 'cite', 'dfn', 'kbd', 'ruby', 'rt', 'rp', 'time', 'bdo'])(
		'renders <%s> as itself around its inline content',
		(tag) => {
			const tokens = inlineOf(`before <${tag}>has *stars*</${tag}> after`);
			const element = tokens.find((token) => token.type === 'htmlInline') as GenericToken;
			expect(element.tag).toBe(tag);
			expect((element.tokens as GenericToken[]).map((token) => token.type)).toContain('em');
			expect(tokens.some((token) => token.type === 'html')).toBe(false);
		}
	);

	it('keeps the attributes the HTML policy lists and no other', () => {
		const tokens = inlineOf('the <abbr title="Merge Request" onclick="x()">MR</abbr> is open');
		const abbr = tokens.find((token) => token.type === 'htmlInline') as GenericToken;
		expect(abbr.attrs).toEqual({ title: 'Merge Request' });
		expect(Object.keys(abbr).sort()).toEqual(['attrs', 'raw', 'tag', 'text', 'tokens', 'type']);
		const span = inlineOf('a <span dir="rtl" lang="ar" style="x">b</span>').find((token) => token.type === 'htmlInline') as GenericToken;
		expect(span.attrs).toEqual({ dir: 'rtl', lang: 'ar' });
		const plain = inlineOf('a <mark>b</mark>').find((token) => token.type === 'htmlInline') as GenericToken;
		expect(plain.attrs).toEqual({});
	});

	it('keeps a native element as its token unless it carries an attribute the policy keeps', () => {
		expect(inlineOf('a <b class="x">b</b>').map((token) => token.type)).toEqual(['text', 'strong']);
		const bold = inlineOf('a <b dir="rtl">b</b>').find((token) => token.type === 'htmlInline') as GenericToken;
		expect(bold).toMatchObject({ tag: 'b', attrs: { dir: 'rtl' } });
		// A code span carries none, so the policy keeps none on `<code>`.
		expect(inlineOf('a <code dir="rtl">c</code>').map((token) => token.type)).toEqual(['text', 'codespan']);
	});

	it('leaves an element the policy strips to the engine, which shows its text', () => {
		const tokens = inlineOf('a <u>under *line*</u> b');
		expect(tokens.some((token) => token.type === 'htmlInline')).toBe(false);
		expect(tokens.map((token) => token.type)).toContain('em');
	});

	it('nests <kbd> inside <kbd>, as the forges draw a key combination', () => {
		const [, outer] = inlineOf('Press <kbd><kbd>Ctrl</kbd>+<kbd>P</kbd></kbd> now');
		expect(outer).toMatchObject({ type: 'htmlInline', tag: 'kbd' });
		const inner = outer.tokens as GenericToken[];
		expect(inner.map((token) => [token.type, token.tag ?? token.text])).toEqual([
			['htmlInline', 'kbd'],
			['text', '+'],
			['htmlInline', 'kbd']
		]);
	});

	it.each([
		['a <span style="color:red">styled *text*</span> b', 'styled'],
		['a <a name="top">anchor *text*</a> b', 'anchor'],
	])('renders %s as a bare span, the text after it its own token', (src, word) => {
		const tokens = inlineOf(src);
		expect(tokens.map((token) => token.type)).toEqual(['text', 'htmlInline', 'text']);
		const [, holder, after] = tokens;
		expect(holder.tag).toBe('span');
		expect(holder.attrs).toEqual({});
		expect(holder.text).toContain(word);
		expect((holder.tokens as GenericToken[]).map((token) => token.type)).toContain('em');
		expect(after.text).toBe(' b');
	});

	it('turns a <picture> into the image its fallback <img> names', () => {
		const picture =
			'<picture><source media="(prefers-color-scheme: dark)" srcset="https://x/dark.svg">' +
			'<source media="(prefers-color-scheme: light)" srcset="https://x/light.svg">' +
			'<img src="https://x/fallback.svg" alt="Explain Change"></picture>';
		const tokens = inlineOf(`<a href="https://example.com/go">${picture}</a>`);
		const link = tokens.find((token) => token.type === 'link') as GenericToken;
		const inner = link.tokens as GenericToken[];
		expect(inner.map((token) => token.type)).toEqual(['image']);
		expect(inner[0].href).toBe('https://x/fallback.svg');
		expect(inner[0].text).toBe('Explain Change');
		expect(inner[0].raw).toBe(picture);
	});

	it('reads a <picture> written across lines', () => {
		const tokens = inlineOf('see <picture>\n  <source srcset="https://x/d.png">\n  <img src="https://x/l.png" alt="logo">\n</picture> here');
		expect(tokens.map((token) => token.type)).toEqual(['text', 'image', 'text']);
	});

	it('claims nothing for a <picture> with no <img>', () => {
		const tokens = inlineOf('a <picture><source srcset="https://x/d.png"></picture> b');
		expect(tokens.some((token) => token.type === 'image')).toBe(false);
		expect(tokens.some((token) => token.type === 'html')).toBe(true);
	});

	it('carries the authored size of an <img>, alone or in a <picture>', () => {
		const [image] = inlineOf('x <img src="https://x/a.png" width="120" height="40">').filter((token) => token.type === 'image');
		expect([image.width, image.height]).toEqual(['120', '40']);
		const [inPicture] = inlineOf('x <picture><img src="https://x/a.png" width="50%"></picture>').filter((token) => token.type === 'image');
		expect([inPicture.width, inPicture.height]).toEqual(['50%', null]);
	});

	it('decodes entity references in attribute values and <code> text', () => {
		const link = inlineOf('<a href="https://example.com/?a=1&amp;b=2" title="A &amp; B">go</a>').find((token) => token.type === 'link') as GenericToken;
		expect(link.href).toBe('https://example.com/?a=1&b=2');
		expect(link.title).toBe('A & B');
		const code = inlineOf('a <code>Vec&lt;T&gt;</code> b').find((token) => token.type === 'codespan') as GenericToken;
		expect(code.text).toBe('Vec<T>');
	});

	it('decodes entity references in text, and not in a markdown code span', () => {
		const tokens = inlineOf('A&nbsp;B &lt;tag&gt; and `&amp;`');
		expect(tokens.filter((token) => token.type === 'escape').map((token) => token.text)).toEqual(['\u00a0', '<', '>']);
		expect(tokens.find((token) => token.type === 'codespan')?.text).toBe('&amp;');
	});

	it('runs an unclosed element to the end of its inline run, as a forge closes it', () => {
		const tokens = inlineOf('a <b>never *closed*');
		expect(tokens.map((token) => token.type)).toEqual(['text', 'strong']);
		const [, strong] = tokens;
		expect(strong.raw).toBe('<b>never *closed*');
		expect((strong.tokens as GenericToken[]).map((token) => token.type)).toEqual(['text', 'em']);
		// Inside another element, to the end of that element's content.
		const [, em, after] = inlineOf('x <i>one <b>two</i> three');
		expect(em.raw).toBe('<i>one <b>two</i>');
		expect((em.tokens as GenericToken[]).map((token) => token.type)).toEqual(['text', 'strong']);
		expect(after.text).toBe(' three');
	});

	it('leaves a stray end tag to the engine html rule', () => {
		const tokens = inlineOf('a stray </i> tag');
		expect(tokens.map((token) => token.type)).toEqual(['text', 'html', 'text']);
	});

	it.each(['script', 'style', 'svg', 'iframe'])('claims a <%s> pair with its content as one html token', (tag) => {
		const pair = `<${tag}>**not** markdown</${tag}>`;
		const tokens = inlineOf(`a ${pair} b`);
		expect(tokens.map((token) => [token.type, token.raw])).toEqual([
			['text', 'a '],
			['html', pair],
			['text', ' b']
		]);
	});

	it('leaves an unclosed <script> to the engine html rule, the text after it shown', () => {
		const tokens = inlineOf('a <script>b *c*');
		expect(tokens.map((token) => token.type)).toEqual(['text', 'html', 'text', 'em']);
	});

	it('leaves unknown tags to the engine html rule', () => {
		const tokens = inlineOf('a <blink>x</blink> b');
		expect(tokens.some((token) => token.type === 'html')).toBe(true);
	});

	it.each([
		['a comment', '<!-- <img src="https://example.invalid/wrong.png"> -->'],
		['a longer tag name', '<img-x src="https://example.invalid/wrong.png"></img-x>'],
		['a quoted attribute value', '<source srcset="x" title="<img src=https://example.invalid/wrong.png>">']
	])('takes the <img> the sanitizer takes from a <picture> with %s before it', (_, before) => {
		const picture = `<picture>${before}<img src="https://example.invalid/right.png"></picture>`;
		const [image] = inlineOf(`a ${picture} b`).filter((token) => token.type === 'image');
		const parsed = new DOMParser().parseFromString(picture, 'text/html').querySelector('picture img');
		expect(image.href).toBe(parsed?.getAttribute('src'));
		expect(image.href).toBe('https://example.invalid/right.png');
		expect(image.raw).toBe(picture);
	});

	it('reads an attribute by name, not from inside another one', () => {
		const [image] = inlineOf('x <img title="text src=https://example.invalid/wrong.png text" src="https://example.invalid/right.png"> y').filter(
			(token) => token.type === 'image'
		);
		expect(image.href).toBe('https://example.invalid/right.png');
		expect(image.title).toBe('text src=https://example.invalid/wrong.png text');
	});

	it('pairs nested elements of the same name', () => {
		const tokens = inlineOf('a <span>one <span>two</span> three</span> b');
		expect(tokens.map((token) => token.type)).toEqual(['text', 'htmlInline', 'text']);
		const outer = tokens[1];
		expect(outer.raw).toBe('<span>one <span>two</span> three</span>');
		expect((outer.tokens as GenericToken[]).map((token) => [token.type, token.raw])).toEqual([
			['text', 'one '],
			['htmlInline', '<span>two</span>'],
			['text', ' three']
		]);
		expect(tokens[2].text).toBe(' b');
	});

	it('does not pair with an end tag inside a code span', () => {
		const tokens = inlineOf('a <b>see `</b>` here</b> c');
		const strong = tokens.find((token) => token.type === 'strong') as GenericToken;
		expect(strong.raw).toBe('<b>see `</b>` here</b>');
		expect((strong.tokens as GenericToken[]).map((token) => token.type)).toEqual(['text', 'codespan', 'text']);
	});

	it('makes an <a href> inside a link plain text, as links do not nest', () => {
		const [, outer, tail] = inlineOf(
			'lead [outer <span><a href="https://example.invalid/inner">inner</a></span> end](https://example.invalid/outer) tail'
		);
		expect(outer.type).toBe('link');
		expect(JSON.stringify(outer.tokens)).not.toContain('"type":"link"');
		const span = (outer.tokens as GenericToken[])[1];
		expect(span.type).toBe('htmlInline');
		expect((span.tokens as GenericToken[])[0]).toMatchObject({ type: 'htmlInline', tag: 'span' });
		expect((outer.tokens as GenericToken[])[2].text).toBe(' end');
		expect(tail.text).toBe(' tail');
		// Its own content is inside a link: a bare URL there is text.
		const [link] = inlineOf('<a href="https://example.invalid/a">see https://example.invalid/b</a>');
		expect((link.tokens as GenericToken[]).map((token) => token.type)).toEqual(['text']);
	});

	describe('pairing work', () => {
		const tagCalls = (src: string): number => {
			const spy = vi.spyOn(Tokenizer.prototype, 'tag');
			try {
				lexEmbedded(src);
				return spy.mock.calls.length;
			} finally {
				spy.mockRestore();
			}
		};

		it('is linear in the tags of a run', () => {
			for (const n of [2000, 8000]) {
				expect(tagCalls(`a ${'<span>x '.repeat(n)}`), `n=${n}`).toBeLessThanOrEqual(5 * n);
			}
		});

		it('leaves an element nested past the bound to the engine', () => {
			const depth = MAX_ELEMENT_NESTING + 6;
			let token = inlineOf(`a ${'<b>'.repeat(depth)}x${'</b>'.repeat(depth)}`)[1];
			let claimed = 0;
			while (token.type === 'strong') {
				claimed++;
				token = token.tokens[0];
			}
			expect(claimed).toBe(MAX_ELEMENT_NESTING);
			expect(token).toMatchObject({ type: 'html', raw: '<b>' });
			expect(() => lexEmbedded(`${'<b>'.repeat(5000)}x${'</b>'.repeat(5000)}`)).not.toThrow();
		});
	});

	it('claims nothing without the extensions registered', () => {
		const [paragraph] = lex('a <b>bold</b> c');
		const tokens = ((paragraph as GenericToken).tokens ?? []) as GenericToken[];
		expect(tokens.some((token) => token.type === 'strong')).toBe(false);
	});
});
