import { beforeEach, describe, expect, it } from 'vitest';
import { SHORT_CODESPAN_CLASS, SHORT_CODESPAN_MAX_LENGTH } from './codespan';
import { htmlRegionNodes, parseHtmlRegion, sanitizeEmbeddedHtmlToken } from './htmlSanitize';

// The allowlist sanitizer behind `renderHtml` on embedded-HTML surfaces.
// Contract: sanitized fragment for complete blocks, built from allowlisted
// elements, allowlisted attributes and text; an element off the allowlist
// contributes its children, or nothing when removed with its content. A
// stray inline tag shows only an allowlisted void element.

const block = (raw: string) => sanitizeEmbeddedHtmlToken({ raw, block: true });
const inline = (raw: string) => sanitizeEmbeddedHtmlToken({ raw, block: false });

describe('sanitizeEmbeddedHtmlToken', () => {
	it('keeps an html table with its structural attributes', () => {
		const out = block('<table><tr><th colspan="2" align="center">h</th></tr><tr><td>a</td><td>b</td></tr></table>');
		expect(out).toContain('<table>');
		expect(out).toContain('<th align="center" colspan="2">h</th>');
		expect(out).toContain('<td>b</td>');
	});

	it('strips event handlers and unknown attributes from kept elements', () => {
		const out = block('<p onclick="alert(1)" data-x="1" align="center">hi</p>');
		expect(out).toBe('<p align="center">hi</p>');
	});

	it.each([
		['<script>alert(1)</script>after', 'script'],
		['<style>p { display: none }</style>after', 'style'],
		['<p>x<svg><text>label</text></svg>after</p>', 'svg'],
		['<iframe src="https://example.invalid/f">fallback</iframe>after', 'iframe']
	])('removes %s with its content', (raw, tag) => {
		const out = block(raw);
		expect(out).not.toContain(`<${tag}`);
		expect(out).not.toContain(`&lt;${tag}`);
		expect(out).toMatch(/^(<p>x)?after(<\/p>)?$/);
	});

	it('renders an element off the allowlist as its children, as a forge strips it', () => {
		expect(block('<blink>still <b>here</b></blink>')).toBe('still <b>here</b>');
		expect(block('<p><big>big</big> <font color="red">font</font> <marquee>m</marquee> <time>t</time></p>')).toBe(
			'<p>big font m t</p>'
		);
		expect(block('<h7>h7</h7><h8>h8</h8>')).toBe('h7h8');
		expect(block('<form action="https://example.invalid/"><input type="text" value="typed"> box<button>Go</button><select><option>opt</option></select><textarea>area</textarea></form>')).toBe(
			' boxGooptarea'
		);
		expect(block('<math><mi>x</mi><mo>=</mo><mn>1</mn></math>')).toBe('x=1');
		expect(block('<noscript>ns</noscript> <template><p>tpl</p></template> <slot>sl</slot>')).toBe('ns <p>tpl</p> sl');
		expect(block('<object data="https://example.invalid/x"></object><embed src="https://example.invalid/x"><wbr><link rel="x"><colgroup><col></colgroup>')).toBe('');
	});

	it('shows a tag-filtered tag as its source text, as GitHub does', () => {
		const filtered = (raw: string, isBlock = true) => sanitizeEmbeddedHtmlToken({ raw, block: isBlock }, { tagFilter: true });
		expect(filtered('<textarea>area</textarea>')).toBe('&lt;textarea&gt;area&lt;/textarea&gt;');
		expect(filtered('<TITLE>t</TITLE> <xmp>x</xmp>')).toBe('&lt;TITLE&gt;t&lt;/TITLE&gt; &lt;xmp&gt;x&lt;/xmp&gt;');
		expect(filtered('<iframe src="https://example.invalid/f"></iframe>')).toBe(
			'&lt;iframe src="https://example.invalid/f"&gt;&lt;/iframe&gt;'
		);
		expect(filtered('<textarea>', false)).toBe('&lt;textarea&gt;');
		// Still removed: a script's or style's source never shows.
		expect(filtered('<script>alert(1)</script><style>p{}</style>')).toBe('');
		// A longer name is not the tag.
		expect(filtered('<textareax>t</textareax>')).toBe('t');
		// Without the filter (GitLab) the tag is stripped.
		expect(block('<textarea>area</textarea>')).toBe('area');
	});

	it('drops a javascript: href but keeps the anchor text', () => {
		const out = block('<p><a href="javascript:alert(1)">x</a></p>');
		expect(out).toContain('<a>x</a>');
		expect(out).not.toContain('javascript');
	});

	it('never emits a path-relative or protocol-relative src/href', () => {
		expect(block('<img src="/design/x.png">')).toBe('<img>');
		expect(block('<img src="//evil.example/x.png">')).toBe('<img>');
		expect(block('<p><a href="/etc/passwd">x</a></p>')).toBe('<p><a>x</a></p>');
	});

	it('reads a URL as the browser does, so no value resolves against the page', () => {
		// U+00A0 is whitespace to String.prototype.trim and a path character
		// to the URL parser: the value is relative, whatever follows it.
		for (const lead of ['&#160;', '\u00a0', '\ufeff', '\u2028', '&nbsp;', 'x']) {
			const url = `${lead}https://example.invalid/../../../x.svg`;
			expect(block(`<img src="${url}">`)).toBe('<img>');
			expect(block(`<picture><img src="${url}"></picture>`)).toBe('<img>');
			expect(block(`<p><a href="${url}">x</a></p>`)).toBe('<p><a>x</a></p>');
		}
		expect(block('<p><a href="https://">x</a></p>')).toBe('<p><a>x</a></p>');
	});

	it('writes a URL as parsed, so a spelling that is relative on a same-scheme page is not', () => {
		// `http:/path` and `http:path` name a host to a parser with no base
		// and a path to one whose base is an http page.
		for (const [authored, canonical] of [
			['http:/forge-audit/path', 'http://forge-audit/path'],
			['https:forge-audit/path', 'https://forge-audit/path'],
			['HTTPS://Example.Invalid', 'https://example.invalid/'],
			[' https://example.invalid/a b ', 'https://example.invalid/a%20b']
		]) {
			expect(new URL(canonical, 'http://app.invalid/pane/').href).toBe(canonical);
			expect(block(`<p><a href="${authored}">x</a><img src="${authored}"></p>`)).toBe(
				`<p><a href="${canonical}">x</a><img src="${canonical}"></p>`
			);
			const nodes = htmlRegionNodes(parseHtmlRegion([`<a href="${authored}">`, { type: 'paragraph' }, '</a>'], true));
			expect(nodes).toMatchObject([{ kind: 'element', tag: 'a', attrs: { href: canonical } }]);
		}
	});

	it('checks a URL on an element that wraps a Markdown island the same way', () => {
		const island = { type: 'paragraph' };
		for (const [href, kept] of [
			['\u00a0https://example.invalid/../x', false],
			['/etc/passwd', false],
			['javascript:alert(1)', false],
			['https://example.invalid/x', true]
		] as const) {
			// `true` keeps authored attributes, so only this check stands.
			const nodes = htmlRegionNodes(parseHtmlRegion([`<a href="${href}" title="t">`, island, '</a>'], true));
			expect(nodes).toEqual([
				{ kind: 'element', tag: 'a', attrs: kept ? { href, title: 't' } : { title: 't' }, children: [{ kind: 'island', index: 0 }] }
			]);
		}
	});

	it('renders an element named after an Object.prototype member as its children', () => {
		for (const tag of ['constructor', 'tostring', 'valueof', 'hasownproperty', '__proto__']) {
			expect(block(`<${tag}>in <b>bold</b></${tag}>`)).toBe('in <b>bold</b>');
			const nodes = htmlRegionNodes(parseHtmlRegion([`<${tag}>`, { type: 'paragraph' }, `</${tag}>`], true));
			expect(nodes).toEqual([{ kind: 'island', index: 0 }]);
		}
	});

	it('keeps absolute http(s) srcs and hrefs', () => {
		expect(block('<img src="https://img.shields.io/b.svg" alt="b">')).toBe(
			'<img src="https://img.shields.io/b.svg" alt="b">'
		);
	});

	it('marks short inline code like the token renderers, and no other code', () => {
		const short = 'x'.repeat(SHORT_CODESPAN_MAX_LENGTH);
		const long = 'x'.repeat(SHORT_CODESPAN_MAX_LENGTH + 1);
		const out = block(
			`<table><tr><td><code class="mine">${short}</code></td><td><code>${long}</code></td></tr></table>` +
				`<pre><code>${short}</code></pre>`
		);
		expect(out).toContain(`<td><code class="${SHORT_CODESPAN_CLASS}">${short}</code></td>`);
		expect(out).toContain(`<td><code>${long}</code></td>`);
		expect(out).toContain(`<pre><code>${short}</code></pre>`);
	});

	it('renders comments as nothing, alone or padded', () => {
		expect(block('<!-- coderabbit marker -->')).toBe('');
		expect(block('  <!-- a -->\n<!-- b -->  ')).toBe('');
		expect(inline('<!-- inline marker -->')).toBe('');
	});

	it('renders a stray inline tag as nothing, and a void element as itself', () => {
		for (const tag of ['<blink>', '</b>', '<b>', '<p style="x">', '<details>', '<input type="checkbox" checked>', '<!DOCTYPE html>']) {
			expect(inline(tag), tag).toBe('');
		}
		expect(inline('<hr class="x">')).toBe('<hr>');
		expect(inline('<script>alert(1)</script>')).toBe('');
	});

	it('keeps a details block usable, open attribute included', () => {
		const out = block('<details open><summary>s</summary><p>b</p></details>');
		expect(out).toContain('<details open=""');
		expect(out).toContain('<summary>s</summary>');
	});

	it('escapes text content it carries through', () => {
		const out = block('<p>a &lt; b &amp; c</p>');
		expect(out).toBe('<p>a &lt; b &amp; c</p>');
	});

	it('renders a picture as its fallback img, the sources dropped', () => {
		const out = block(
			'<picture>\n<source media="(prefers-color-scheme: dark)" srcset="https://x/dark.png">\n' +
				'<source srcset="https://x/light.png">\n<img alt="Shows a thing" src="https://x/default.png">\n</picture>'
		);
		expect(out).toBe('<img src="https://x/default.png" alt="Shows a thing">');
		expect(block('<p align="center"><a href="https://x/go"><picture><source srcset="https://x/d.png"><img src="https://x/l.png"></picture></a></p>')).toBe(
			'<p align="center"><a href="https://x/go"><img src="https://x/l.png"></a></p>'
		);
	});

	it('renders a picture with no img as nothing', () => {
		expect(block('<p>a<picture><source srcset="https://x/d.png"></picture>b</p>')).toBe('<p>ab</p>');
	});

	it('still refuses an unsafe src inside a picture', () => {
		expect(block('<picture><img src="javascript:alert(1)" onerror="x()" alt="a"></picture>')).toBe('<img alt="a">');
	});

	it('keeps the align of a heading', () => {
		expect(block('<h3 align="center" id="x">Title</h3>')).toBe('<h3 align="center">Title</h3>');
	});

	it.each(['mark', 'small', 'q', 'var', 'cite', 'dfn', 'ins', 'u'])('keeps <%s> as itself, without attributes', (tag) => {
		expect(block(`<p><${tag} class="x">t</${tag}></p>`)).toBe(`<p><${tag}>t</${tag}></p>`);
	});

	it('keeps an abbr title', () => {
		expect(block('<p><abbr title="Merge Request" style="x">MR</abbr></p>')).toBe('<p><abbr title="Merge Request">MR</abbr></p>');
	});

	it('keeps title, dir and lang on any kept element, and never style or a handler', () => {
		expect(block('<p title="tip" dir="rtl" lang="ar" style="color:red" onclick="x()">t</p>')).toBe(
			'<p title="tip" dir="rtl" lang="ar">t</p>'
		);
		expect(block('<span title="tip">s</span>')).toBe('<span title="tip">s</span>');
	});

	it('keeps the list and table attributes the forges keep', () => {
		expect(block('<ol start="3" type="a" reversed><li value="7">x</li></ol>')).toBe(
			'<ol style="list-style-type: lower-alpha" start="3"><li value="7">x</li></ol>'
		);
		expect(block('<ol type="I"><li>x</li></ol><ol type="q"><li>y</li></ol>')).toBe(
			'<ol style="list-style-type: upper-roman"><li>x</li></ol><ol><li>y</li></ol>'
		);
		expect(block('<ul type="SQUARE"><li>x</li></ul><ul type="a"><li>y</li></ul>')).toBe(
			'<ul style="list-style-type: square"><li>x</li></ul><ul><li>y</li></ul>'
		);
		expect(block('<table width="100%" border="1"><tr><td width="20%" valign="top" nowrap>a</td><th width="5" valign="bottom">b</th></tr></table>')).toBe(
			'<table width="100%"><tbody><tr><td width="20%" valign="top" nowrap="">a</td><th width="5" valign="bottom">b</th></tr></tbody></table>'
		);
	});

	it('keeps a ruby annotation', () => {
		expect(block('<p><ruby>漢<rp>(</rp><rt>kan</rt><rp>)</rp></ruby></p>')).toBe('<p><ruby>漢<rp>(</rp><rt>kan</rt><rp>)</rp></ruby></p>');
	});

	it('paints an authored img size as a style a stylesheet cannot unset', () => {
		expect(block('<p><img src="https://x/a.png" width="120" height="40"></p>')).toBe(
			'<p><img src="https://x/a.png" width="120" height="40" style="width: 120px; aspect-ratio: 120 / 40;"></p>'
		);
		expect(block('<p><img src="https://x/a.png" width="50%"></p>')).toBe(
			'<p><img src="https://x/a.png" width="50%" style="width: 50%;"></p>'
		);
		// Not a size: no style, and never a way to write one.
		expect(block('<p><img src="https://x/a.png" width="1px;position:fixed" height="expression(1)"></p>')).not.toContain('style');
	});
});

// The app seam. A host that fetches certain media itself marks it here and
// hydrates the marker later; everything the hook declines keeps the exact
// output the cases above assert.
describe('sanitizeEmbeddedHtmlToken with a claimMedia hook', () => {
	const calls: Array<[string, string]> = [];
	const claimUploads = (tag: 'img' | 'video' | 'source', src: string): string | null => {
		calls.push([tag, src]);
		return src.includes('/uploads/') ? `claim(${src})` : null;
	};
	const claimed = (raw: string) =>
		sanitizeEmbeddedHtmlToken({ raw, block: true }, { claimMedia: claimUploads });

	beforeEach(() => {
		calls.length = 0;
	});

	it('claims a path-relative img the plain sanitizer would have stripped', () => {
		expect(claimed('<img src="/uploads/abc/a.png" alt="a shot" width="200">')).toBe(
			'<img data-markdown-media-claim="claim(/uploads/abc/a.png)" alt="a shot" width="200">'
		);
		expect(calls).toEqual([['img', '/uploads/abc/a.png']]);
	});

	it('keeps an unclaimed img exactly as the plain sanitizer renders it', () => {
		expect(claimed('<img src="https://img.shields.io/b.svg" alt="b">')).toBe(
			'<img src="https://img.shields.io/b.svg" alt="b">'
		);
		expect(claimed('<img src="/design/x.png">')).toBe('<img>');
	});

	it('claims media inside the wrappers a forge actually writes', () => {
		expect(claimed('<p align="center"><img src="/uploads/abc/a.png"></p>')).toBe(
			'<p align="center"><img data-markdown-media-claim="claim(/uploads/abc/a.png)"></p>'
		);
		expect(
			claimed('<table><tr><td><img src="/uploads/abc/a.png"></td></tr></table>')
		).toContain('<td><img data-markdown-media-claim="claim(/uploads/abc/a.png)"></td>');
		expect(
			claimed('<a href="https://example.test/p"><img src="/uploads/abc/a.png"></a>')
		).toBe(
			'<a href="https://example.test/p"><img data-markdown-media-claim="claim(/uploads/abc/a.png)"></a>'
		);
	});

	it('replaces a claimed video with a placeholder and drops its children', () => {
		expect(claimed('<video src="/uploads/abc/clip.mp4" controls>fallback</video>')).toBe(
			'<span data-markdown-media-claim="claim(/uploads/abc/clip.mp4)" data-markdown-media-kind="video"></span>'
		);
	});

	it('reads a claimed video source element when the video has no src', () => {
		expect(
			claimed('<video controls><source src="/uploads/abc/clip.mp4" type="video/mp4"></video>')
		).toBe(
			'<span data-markdown-media-claim="claim(/uploads/abc/clip.mp4)" data-markdown-media-kind="video"></span>'
		);
		expect(calls).toEqual([['source', '/uploads/abc/clip.mp4']]);
	});

	it('leaves an unclaimed video to the policy for an element off the allowlist', () => {
		const out = claimed('<video src="https://cdn.example.test/clip.mp4">fallback</video>');
		expect(out).toBe('fallback');
		expect(out).not.toContain('data-markdown-media-claim');
	});

	it('never lets a claim reach an href or src attribute', () => {
		const out = sanitizeEmbeddedHtmlToken(
			{ raw: '<img src="/uploads/abc/a.png">', block: true },
			{ claimMedia: () => 'javascript:alert(1)' }
		);
		expect(out).toBe('<img data-markdown-media-claim="javascript:alert(1)">');
		expect(out).not.toContain('src=');
	});

	it('changes nothing when no hook is passed', () => {
		expect(sanitizeEmbeddedHtmlToken({ raw: '<img src="/uploads/abc/a.png">', block: true })).toBe(
			'<img>'
		);
	});
});
