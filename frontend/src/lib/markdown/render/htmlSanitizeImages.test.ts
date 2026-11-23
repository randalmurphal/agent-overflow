import { describe, expect, it, vi } from 'vitest';
import { MEDIA_CLAIM_ATTR, sanitizeEmbeddedHtmlToken, type SanitizeEmbeddedHtmlOptions } from './htmlSanitize';
import type { ColorScheme } from '../parser/extensions/themedImages';
import type { DestinationHook } from '../parser/engine/destination';

// Theme-specific images and image placement in an HTML block.

const sanitized = (raw: string, options?: SanitizeEmbeddedHtmlOptions): HTMLElement => {
	const out = document.createElement('div');
	out.innerHTML = sanitizeEmbeddedHtmlToken({ raw, block: true }, options);
	return out;
};
const srcs = (root: Element) => [...root.querySelectorAll('img')].map((img) => img.getAttribute('src'));
const scheme = (value: ColorScheme) => ({ colorScheme: () => value });
/** A host that claims `/uploads/` images, as the forge attachment hook does. */
const claiming = (hook: DestinationHook = (token) => (token.type === 'image' && token.href.startsWith('/uploads/') ? { ...token, href: `claim:${token.href}` } : undefined)) =>
	({ destinations: [hook], allowedImagePrefixes: ['https://', 'claim:'] }) satisfies SanitizeEmbeddedHtmlOptions;

const PICTURE =
	'<p align="center"><picture>' +
	'<source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png 1x, https://example.invalid/dark@2x.png 2x">' +
	'<source media="(prefers-color-scheme: light)" srcset="https://example.invalid/light.png">' +
	'<img alt="logo" src="https://example.invalid/default.png" width="300"></picture></p>';

describe('a picture in an HTML block', () => {
	it("paints the source of the reader's color scheme, at the fallback's size", () => {
		const dark = sanitized(PICTURE, scheme('dark'));
		expect(srcs(dark)).toEqual(['https://example.invalid/dark.png']);
		expect(dark.querySelector('img')).toMatchObject({ alt: 'logo' });
		expect(dark.querySelector('img')!.style.width).toBe('300px');
		expect(srcs(sanitized(PICTURE, scheme('light')))).toEqual(['https://example.invalid/light.png']);
	});

	it('paints the fallback without a color scheme', () => {
		expect(srcs(sanitized(PICTURE))).toEqual(['https://example.invalid/default.png']);
	});

	it('paints the fallback for a scheme no source names', () => {
		const html = '<picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png"><img src="https://example.invalid/default.png"></picture>';
		expect(srcs(sanitized(html, scheme('light')))).toEqual(['https://example.invalid/default.png']);
	});

	it.each([
		['a relative source', 'docs/dark.png'],
		['a script source', 'javascript:alert(1)']
	])('paints the fallback over %s the img would not load', (_name, src) => {
		const html = `<picture><source media="(prefers-color-scheme: dark)" srcset="${src}"><img src="https://example.invalid/default.png"></picture>`;
		expect(srcs(sanitized(html, scheme('dark')))).toEqual(['https://example.invalid/default.png']);
	});

	it('paints a scheme-relative source as the https URL it names', () => {
		const html = '<picture><source media="(prefers-color-scheme: dark)" srcset="//example.invalid/dark.png"><img src="https://example.invalid/default.png"></picture>';
		expect(srcs(sanitized(html, scheme('dark')))).toEqual(['https://example.invalid/dark.png']);
	});

	it('claims a forge-hosted source as it claims a fallback', () => {
		const html = '<picture><source media="(prefers-color-scheme: dark)" srcset="/uploads/dark.png"><img alt="logo" src="https://example.invalid/default.png"></picture>';
		const root = sanitized(html, { ...claiming(), colorScheme: () => 'dark' });
		const img = root.querySelector('img')!;
		expect(img.getAttribute(MEDIA_CLAIM_ATTR)).toBe('claim:/uploads/dark.png');
		expect(img.hasAttribute('src')).toBe(false);
		expect(sanitized(html, { ...claiming(), colorScheme: () => 'light' }).querySelector('img')!.getAttribute('src')).toBe(
			'https://example.invalid/default.png'
		);
	});
});

describe('a #gh-*-mode-only image in an HTML block', () => {
	const HTML =
		'<img src="https://example.invalid/light.png#gh-light-mode-only" alt="light">\n' +
		'<img src="https://example.invalid/dark.png#gh-dark-mode-only" alt="dark">\n' +
		'<a href="https://example.invalid/"><img src="https://example.invalid/plain.png"></a>';

	it("shows only in the scheme it names, and only where the reader's scheme is known", () => {
		expect(srcs(sanitized(HTML, scheme('light')))).toEqual([
			'https://example.invalid/light.png#gh-light-mode-only',
			'https://example.invalid/plain.png'
		]);
		expect(srcs(sanitized(HTML, scheme('dark')))).toEqual([
			'https://example.invalid/dark.png#gh-dark-mode-only',
			'https://example.invalid/plain.png'
		]);
		expect(srcs(sanitized(HTML))).toHaveLength(3);
	});

	it('is hidden before it is claimed', () => {
		const hook = vi.fn<DestinationHook>(() => undefined);
		sanitized('<img src="/uploads/dark.png#gh-dark-mode-only">', { ...claiming(hook), colorScheme: () => 'light' });
		expect(hook).not.toHaveBeenCalled();
	});
});

describe('the color scheme read', () => {
	it('happens only for HTML holding a theme-specific image', () => {
		const colorScheme = vi.fn((): ColorScheme => 'dark');
		sanitized('<p align="center"><img src="https://example.invalid/a.png"><picture><img src="https://example.invalid/b.png"></picture></p>', {
			colorScheme
		});
		expect(colorScheme).not.toHaveBeenCalled();
		sanitized(PICTURE, { colorScheme });
		expect(colorScheme).toHaveBeenCalledTimes(1);
	});
});

describe('image placement', () => {
	it('floats an aligned image, places it in its line, and spaces it by hspace', () => {
		const root = sanitized(
			'<p><img src="https://example.invalid/a.png" align="right" hspace="10">' +
				'<img src="https://example.invalid/b.png" align="MIDDLE">' +
				'<img src="https://example.invalid/c.png" align="center" hspace="1em"></p>'
		);
		const [right, middle, other] = root.querySelectorAll('img');
		expect([right.style.float, right.style.marginInline]).toEqual(['right', '10px']);
		expect([middle.style.float, middle.style.verticalAlign]).toEqual(['', 'middle']);
		expect(other.getAttribute('style')).toBeNull();
		// The attribute itself is not kept: the style is the one decision.
		expect(right.hasAttribute('align')).toBe(false);
	});

	it('keeps the placement on a claimed image for its host', () => {
		const root = sanitized('<p><img src="/uploads/a.png" align="left" hspace="4"></p>', claiming());
		const img = root.querySelector('img')!;
		expect([img.getAttribute('align'), img.getAttribute('hspace')]).toEqual(['left', '4']);
	});
});
