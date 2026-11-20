import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import Streamdown from './Streamdown.svelte';
import { chatMarkdownTheme } from '../../components/chat/markdown/streamdownTheme';
import { EMBEDDED_HTML_EXTENSIONS } from '../parser/extensions/embeddedHtml';
import { sanitizeEmbeddedHtmlToken } from './htmlSanitize';

// An image's title is kept however the image was written: Markdown, an
// inline `<img>`, or an `<img>` in an HTML block.

const titleOf = async (content: string): Promise<string | null | undefined> => {
	const view = render(Streamdown, {
		props: {
			theme: chatMarkdownTheme,
			content,
			extensions: EMBEDDED_HTML_EXTENSIONS,
			renderHtml: (token) => sanitizeEmbeddedHtmlToken(token),
			allowedImagePrefixes: ['https://']
		}
	});
	await tick();
	const title = view.container.querySelector('img')?.getAttribute('title');
	view.unmount();
	return title;
};

describe('an image title', () => {
	it.each([
		['a Markdown image', 'see ![alt](https://example.invalid/a.png "badge title") here'],
		['an inline <img>', 'see <img src="https://example.invalid/a.png" alt="alt" title="badge title"> here'],
		['an <img> in an HTML block', '<p><img src="https://example.invalid/a.png" alt="alt" title="badge title"></p>']
	])('is kept on %s', async (_, content) => {
		expect(await titleOf(content)).toBe('badge title');
	});

	it('is absent when none is written', async () => {
		expect(await titleOf('see ![alt](https://example.invalid/a.png) here')).toBeNull();
	});
});
