import { describe, expect, it } from 'vitest';
import { EMBEDDED_HTML_EXTENSIONS, lex } from '../../index';
import { colorSchemeOnly, imageSrcFor, pictureSources } from './themedImages';

const picture = (html: string): Element => {
	const template = document.createElement('template');
	template.innerHTML = html;
	return template.content.querySelector('picture')!;
};

type Tok = { type: string; raw: string; href?: string; sources?: unknown; tokens?: Tok[] };
const inline = (source: string): Tok[] => ((lex(source, EMBEDDED_HTML_EXTENSIONS) as unknown as Tok[])[0]?.tokens ?? []) as Tok[];

describe('pictureSources', () => {
	it('reads the first candidate of each color scheme source, as GitHub keeps it', () => {
		expect(
			pictureSources(
				picture(
					'<picture>' +
						'<source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png 1x, https://example.invalid/dark@2x.png 2x">' +
						'<source media=" ( PREFERS-COLOR-SCHEME : light ) " srcset="  https://example.invalid/light.png">' +
						'<img src="https://example.invalid/default.png"></picture>'
				)
			)
		).toEqual({ dark: 'https://example.invalid/dark.png', light: 'https://example.invalid/light.png' });
	});

	it('keeps the first source for a scheme, skips one with no candidate, and reads nothing after the img', () => {
		expect(
			pictureSources(
				picture(
					'<picture>' +
						'<source media="(prefers-color-scheme: dark)" srcset="">' +
						'<source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/a.png,">' +
						'<source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/b.png">' +
						'<img src="https://example.invalid/default.png">' +
						'<source media="(prefers-color-scheme: light)" srcset="https://example.invalid/late.png"></picture>'
				)
			)
		).toEqual({ dark: 'https://example.invalid/a.png' });
	});

	it('reads no source whose media is another query, or none', () => {
		expect(
			pictureSources(
				picture(
					'<picture>' +
						'<source media="(min-width: 800px)" srcset="https://example.invalid/wide.png">' +
						'<source media="(prefers-color-scheme: dark) and (min-width: 800px)" srcset="https://example.invalid/x.png">' +
						'<source srcset="https://example.invalid/any.webp" type="image/webp">' +
						'<img src="https://example.invalid/default.png"></picture>'
				)
			)
		).toEqual({});
	});
});

describe('colorSchemeOnly', () => {
	it.each([
		['https://example.invalid/a.png#gh-dark-mode-only', 'dark'],
		['https://example.invalid/a.png#gh-light-mode-only', 'light'],
		['docs/a.png#gh-light-mode-only', 'light'],
		['https://example.invalid/a.png#gh-dark-mode-only-not', null],
		['https://example.invalid/a.png#GH-DARK-MODE-ONLY', null],
		['https://example.invalid/a.png', null]
	])('reads %s', (href, scheme) => {
		expect(colorSchemeOnly(href)).toBe(scheme);
	});
});

describe('imageSrcFor', () => {
	const sources = { dark: 'https://example.invalid/dark.png' };
	it('paints the scheme source, else the fallback', () => {
		expect(imageSrcFor({ href: 'https://example.invalid/d.png', sources }, 'dark')).toBe('https://example.invalid/dark.png');
		expect(imageSrcFor({ href: 'https://example.invalid/d.png', sources }, 'light')).toBe('https://example.invalid/d.png');
	});

	it('hides an image in the scheme its fragment or claim does not name', () => {
		const dark = 'https://example.invalid/d.png#gh-dark-mode-only';
		expect(imageSrcFor({ href: dark }, 'light')).toBeNull();
		expect(imageSrcFor({ href: dark }, 'dark')).toBe(dark);
		expect(imageSrcFor({ href: 'agent-overflow:forge?x', shownIn: 'light' }, 'dark')).toBeNull();
		expect(imageSrcFor({ href: 'agent-overflow:forge?x', shownIn: 'light' }, 'light')).toBe('agent-overflow:forge?x');
	});

	it('paints the image itself without a scheme, as GitLab does', () => {
		expect(imageSrcFor({ href: 'https://example.invalid/d.png#gh-dark-mode-only', sources }, undefined)).toBe(
			'https://example.invalid/d.png#gh-dark-mode-only'
		);
	});
});

describe('the embedded picture token', () => {
	it('carries its color scheme sources beside its fallback', () => {
		const tokens = inline(
			'Logo <picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png"><img alt="logo" src="https://example.invalid/default.png"></picture> end'
		);
		expect(tokens[1]).toMatchObject({
			type: 'image',
			href: 'https://example.invalid/default.png',
			sources: { dark: 'https://example.invalid/dark.png' }
		});
	});

	it('carries no sources for a picture without a color scheme source', () => {
		const tokens = inline('<picture><img alt="logo" src="https://example.invalid/default.png"></picture> end');
		expect(tokens[0]).not.toHaveProperty('sources');
	});
});
