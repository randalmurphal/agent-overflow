import { describe, expect, it } from 'vitest';
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
