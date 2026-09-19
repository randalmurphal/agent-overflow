import { beforeEach, describe, expect, it } from 'vitest';
import { sanitizeEmbeddedHtmlToken } from './htmlSanitize';

// The allowlist sanitizer behind `renderHtml` on embedded-HTML surfaces.
// Contract: sanitized fragment for complete blocks, escaped literal for
// stray inline tags, '' only for comments.

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

	it('renders a script as escaped literal text, never as an element', () => {
		const out = block('<script>alert(1)</script>');
		expect(out).not.toContain('<script');
		expect(out).toContain('&lt;script&gt;');
		expect(out).toContain('alert(1)');
	});

	it('renders unknown elements as escaped literal tags around their children', () => {
		const out = block('<blink>still <b>here</b></blink>');
		expect(out).toContain('&lt;blink&gt;');
		expect(out).toContain('<b>here</b>');
		expect(out).toContain('&lt;/blink&gt;');
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

	it('keeps absolute http(s) srcs and hrefs', () => {
		expect(block('<img src="https://img.shields.io/b.svg" alt="b">')).toBe(
			'<img src="https://img.shields.io/b.svg" alt="b">'
		);
	});

	it('renders comments as nothing, alone or padded', () => {
		expect(block('<!-- coderabbit marker -->')).toBe('');
		expect(block('  <!-- a -->\n<!-- b -->  ')).toBe('');
		expect(inline('<!-- inline marker -->')).toBe('');
	});

	it('renders a stray inline tag as escaped literal text', () => {
		expect(inline('<blink>')).toBe('&lt;blink&gt;');
		expect(inline('</b>')).toBe('&lt;/b&gt;');
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

	it('leaves an unclaimed video as the escaped literal it renders today', () => {
		const out = claimed('<video src="https://cdn.example.test/clip.mp4">fallback</video>');
		expect(out).toContain('&lt;video&gt;');
		expect(out).toContain('fallback');
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
