import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import Streamdown from './Streamdown.svelte';
import { chatMarkdownTheme } from '../../components/chat/markdown/streamdownTheme';
import { EMBEDDED_HTML_EXTENSIONS } from '../parser/extensions/embeddedHtml';
import { markedMediaImages } from '../parser/extensions/mediaImages';
import type { ColorScheme } from '../parser/extensions/themedImages';
import { sanitizeEmbeddedHtmlToken } from './htmlSanitize';

// The renderer's own image element, with no host image snippet, on the
// component and compact static paths (which hands an image to it).

const shown = async (content: string, colorScheme: ColorScheme | undefined, compactStaticHtml: boolean) => {
	const view = render(Streamdown, {
		props: {
			theme: chatMarkdownTheme,
			content,
			extensions: [markedMediaImages, ...EMBEDDED_HTML_EXTENSIONS],
			allowedImagePrefixes: ['https://'],
			colorScheme,
			compactStaticHtml
		}
	});
	await tick();
	const root = view.container;
	const out = {
		images: [...root.querySelectorAll('img')].map((img) => [img.getAttribute('src'), img.getAttribute('alt')]),
		media: [...root.querySelectorAll('video, audio')].map((element) => [element.localName, element.getAttribute('src')]),
		hidden: root.querySelectorAll('[data-streamdown-image-hidden]').length,
		float: root.querySelector<HTMLElement>('[data-streamdown-image]')?.style.float ?? ''
	};
	view.unmount();
	return out;
};

const PICTURE =
	'A <picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png"><img alt="logo" src="https://example.invalid/default.png" align="right"></picture> ![x *y*](https://example.invalid/only.png#gh-light-mode-only)';

describe.each([false, true])('the image element (compact static %s)', (compactStaticHtml) => {
	it('paints the dark source and hides a light-only image in dark', async () => {
		expect(await shown(PICTURE, 'dark', compactStaticHtml)).toEqual({
			images: [['https://example.invalid/dark.png', 'logo']],
			media: [],
			hidden: 1,
			float: 'right'
		});
	});

	it('paints the fallback and the light-only image in light', async () => {
		expect((await shown(PICTURE, 'light', compactStaticHtml)).images).toEqual([
			['https://example.invalid/default.png', 'logo'],
			['https://example.invalid/only.png#gh-light-mode-only', 'x y']
		]);
	});

	it('paints every image its own src without a scheme', async () => {
		expect((await shown(PICTURE, undefined, compactStaticHtml)).images).toHaveLength(2);
	});

	it('plays a playable image', async () => {
		expect(
			(await shown('![v](https://example.invalid/v.webm) ![a](https://example.invalid/a.wav)', undefined, compactStaticHtml)).media
		).toEqual([
			['video', 'https://example.invalid/v.webm'],
			['audio', 'https://example.invalid/a.wav']
		]);
	});
});

// One decision for both paths (themedImages.ts#imageSrcFor): a source for
// the reader's scheme paints; the fallback's `#gh-*-mode-only` gates only
// the fallback.
describe('a themed picture paints one src on the token and sanitizer paths', () => {
	const DARK = '<source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png">';
	const LIGHT = '<source media="(prefers-color-scheme: light)" srcset="https://example.invalid/light.png">';
	const REFUSED = '<source media="(prefers-color-scheme: dark)" srcset="javascript:alert(1)">';
	const img = (fragment = '') => `<img alt="logo" src="https://example.invalid/fallback.png${fragment}">`;
	const CASES: [string, string, string | null, string | null][] = [
		['a dark source over a light-only fallback', DARK + img('#gh-light-mode-only'), 'dark.png', 'fallback.png#gh-light-mode-only'],
		['a dark source over a dark-only fallback', DARK + img('#gh-dark-mode-only'), 'dark.png', null],
		['a refused source over a light-only fallback', REFUSED + img('#gh-light-mode-only'), null, 'fallback.png#gh-light-mode-only'],
		['a source per scheme', LIGHT + DARK + img(), 'dark.png', 'light.png']
	];
	const url = (name: string | null) => (name === null ? [] : [`https://example.invalid/${name}`]);

	it.each(CASES)('%s', async (_name, inner, dark, light) => {
		const picture = `<picture>${inner}</picture>`;
		for (const [scheme, expected] of [['dark', dark], ['light', light]] as const) {
			const sanitized = document.createElement('div');
			sanitized.innerHTML = sanitizeEmbeddedHtmlToken({ raw: picture, block: true }, { colorScheme: () => scheme });
			expect([...sanitized.querySelectorAll('img')].map((each) => each.getAttribute('src')), `sanitizer, ${scheme}`).toEqual(url(expected));
			for (const compactStaticHtml of [false, true]) {
				const token = await shown(`A ${picture} end`, scheme, compactStaticHtml);
				expect(token.images.map(([src]) => src), `token, ${scheme}`).toEqual(url(expected));
				expect(token.hidden).toBe(expected === null ? 1 : 0);
			}
		}
	});
});
