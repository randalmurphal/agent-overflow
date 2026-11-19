import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import Streamdown from './Streamdown.svelte';
import { chatMarkdownTheme } from '../../components/chat/markdown/streamdownTheme';
import { EMBEDDED_HTML_EXTENSIONS } from '../parser/extensions/embeddedHtml';
import { markedMediaImages } from '../parser/extensions/mediaImages';
import type { ColorScheme } from '../parser/extensions/themedImages';

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
