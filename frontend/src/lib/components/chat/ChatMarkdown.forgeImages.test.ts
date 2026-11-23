import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import { resetAppearanceForTest, setAppearance } from '../../stores/appearance.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Images in forge content as the forge shows them, on both renderer paths
// (a settled body is the compact static path, which hands images and HTML
// to components; a streaming one is the component tree): GitHub's
// theme-specific images answer the app's theme, GitLab plays an image of a
// video or audio file, an alt is its label's plain text, and `align`
// places an image. Agent chat keeps its own behavior.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};
const ASSET = 'https://github.com/user-attachments/assets/0d0d0d0d-1111-2222-3333-444455556666';

const THEMED = [
  'Inline <picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/inline-dark.png"><img alt="inline" src="https://example.invalid/inline.png"></picture> logo.',
  '',
  '<p align="center"><picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/block-dark.png 1x, https://example.invalid/block-dark@2x.png 2x"><source media="(prefers-color-scheme: light)" srcset="https://example.invalid/block-light.png"><img alt="block" src="https://example.invalid/block.png" width="300"></picture></p>',
  '',
  '![md light](https://example.invalid/md-light.png#gh-light-mode-only) ![md dark](https://example.invalid/md-dark.png#gh-dark-mode-only)',
  '',
  '<img src="https://example.invalid/html-light.png#gh-light-mode-only" alt="html light">',
  '<img src="https://example.invalid/html-dark.png#gh-dark-mode-only" alt="html dark">',
  '',
  'last line',
].join('\n');

const srcs = (root: Element) => [...root.querySelectorAll('img')].map((img) => img.getAttribute('src')!.replace('https://example.invalid/', ''));

function mount(source: string, forgeSource: ForgeAttachmentSource | null, streaming: boolean) {
  return render(ForgeAttachmentChatHarness, { props: { source, forgeSource, embeddedHtml: true, streaming } });
}

beforeEach(() => {
  setBindingMock('SetAppearance', async () => {});
});

afterEach(() => {
  resetAppearanceForTest();
  resetBindingMocks();
});

describe('theme-specific images', () => {
  it.each([false, true])('on GitHub paint the source and the images of the app theme (streaming %s)', async (streaming) => {
    void setAppearance({ mode: 'light' });
    const { container } = mount(THEMED, GITHUB, streaming);
    await waitFor(() => expect(container.textContent).toContain('last line'));
    await waitFor(() => expect(srcs(container)).toEqual(['inline.png', 'block-light.png', 'md-light.png#gh-light-mode-only', 'html-light.png#gh-light-mode-only']));
    // The authored size is the chosen source's.
    expect(container.querySelector<HTMLImageElement>('img[src$="block-light.png"]')!.style.width).toBe('300px');
    // A hidden image still ends find's run where it stood: the Markdown
    // one and the HTML one, each line of which is its own paragraph.
    expect(container.querySelectorAll('[data-streamdown-image-hidden][data-find-skip]')).toHaveLength(2);

    void setAppearance({ mode: 'dark' });
    await waitFor(() => expect(srcs(container)).toEqual(['inline-dark.png', 'block-dark.png', 'md-dark.png#gh-dark-mode-only', 'html-dark.png#gh-dark-mode-only']));
  });

  it('switch with the theme without rebuilding the blocks around them', async () => {
    void setAppearance({ mode: 'dark' });
    const { container } = mount(THEMED, GITHUB, false);
    await waitFor(() => expect(srcs(container)).toContain('inline-dark.png'));
    const paragraph = container.querySelector('p')!;
    const text = [...paragraph.childNodes].find((node) => node.textContent === 'Inline ')!;
    const last = [...container.querySelectorAll('p')].at(-1)!;
    void setAppearance({ mode: 'light' });
    flushSync();
    await waitFor(() => expect(srcs(container)).toContain('inline.png'));
    expect(container.querySelector('p')).toBe(paragraph);
    expect(text.isConnected).toBe(true);
    expect([...container.querySelectorAll('p')].at(-1)).toBe(last);
  });

  it.each([false, true])('on GitLab paint the fallback and every image, as GitLab does (streaming %s)', async (streaming) => {
    void setAppearance({ mode: 'dark' });
    const { container } = mount(THEMED, GITLAB, streaming);
    await waitFor(() => expect(container.textContent).toContain('last line'));
    await waitFor(() =>
      expect(srcs(container)).toEqual([
        'inline.png',
        'block.png',
        'md-light.png#gh-light-mode-only',
        'md-dark.png#gh-dark-mode-only',
        'html-light.png#gh-light-mode-only',
        'html-dark.png#gh-dark-mode-only',
      ]),
    );
  });

  it('leave agent chat showing both fragment images', async () => {
    void setAppearance({ mode: 'dark' });
    const { container } = render(ChatMarkdown, { props: { source: THEMED } });
    await waitFor(() => expect(container.textContent).toContain('last line'));
    expect(srcs(container)).toEqual(['md-light.png#gh-light-mode-only', 'md-dark.png#gh-dark-mode-only']);
  });

  it.each([false, true])('load a forge-hosted source through the attachment loader (streaming %s)', async (streaming) => {
    void setAppearance({ mode: 'dark' });
    const source = `Logo <picture><source media="(prefers-color-scheme: dark)" srcset="${ASSET}"><img alt="logo" src="https://example.invalid/default.png"></picture> end`;
    const { container } = mount(source, GITHUB, streaming);
    await waitFor(() => expect(container.querySelector('[data-forge-attachment-loading]')?.textContent).toContain('0d0d0d0d'));
    expect(container.querySelector('img')).toBeNull();
    void setAppearance({ mode: 'light' });
    await waitFor(() => expect(srcs(container)).toEqual(['default.png']));
    expect(container.querySelector('[data-forge-attachment-loading]')).toBeNull();
  });

  it('paint the fallback over a source the URL policy refuses', async () => {
    void setAppearance({ mode: 'dark' });
    const source = 'Logo <picture><source media="(prefers-color-scheme: dark)" srcset="javascript:alert(1)"><img alt="logo" src="https://example.invalid/default.png"></picture> end';
    const { container } = mount(source, GITHUB, true);
    await waitFor(() => expect(srcs(container)).toEqual(['default.png']));
  });
});

describe('playable images', () => {
  const MEDIA = [
    '![clip](https://example.invalid/clip.mp4){width=320 height=180}',
    '',
    '![memo](https://example.invalid/memo.ogg)',
    '',
    '[![linked](https://example.invalid/linked.mp4)](https://example.invalid/page)',
    '',
    'last line',
  ].join('\n');

  it.each([false, true])('play on GitLab, and an image in a link stays an image (streaming %s)', async (streaming) => {
    const { container } = mount(MEDIA, GITLAB, streaming);
    await waitFor(() => expect(container.textContent).toContain('last line'));
    const video = container.querySelector('video')!;
    expect(video.getAttribute('src')).toBe('https://example.invalid/clip.mp4');
    expect(video.controls).toBe(true);
    expect(video.getAttribute('preload')).toBe('metadata');
    expect(video.style.aspectRatio).toBe('320 / 180');
    expect(container.querySelector('audio')!.getAttribute('src')).toBe('https://example.invalid/memo.ogg');
    expect(container.querySelector('a img')!.getAttribute('src')).toBe('https://example.invalid/linked.mp4');
    expect(container.querySelector('a video, a audio')).toBeNull();
  });

  it.each([
    ['GitHub', GITHUB],
    ['agent chat', null],
  ] as const)('stay images on %s', async (_name, forgeSource) => {
    const { container } = mount(MEDIA, forgeSource, false);
    await waitFor(() => expect(container.textContent).toContain('last line'));
    expect(container.querySelector('video, audio')).toBeNull();
    expect(srcs(container)).toContain('clip.mp4');
  });
});

describe('image alt text', () => {
  it.each([
    ['GitHub', GITHUB],
    ['GitLab', GITLAB],
  ] as const)('is the plain text of the label on %s', async (_name, forgeSource) => {
    for (const streaming of [false, true]) {
      const { container, unmount } = mount('![&copy; **alt** `x`](https://example.invalid/i.png)\n\nlast line', forgeSource, streaming);
      await waitFor(() => expect(container.querySelector('img')?.getAttribute('alt')).toBe('© alt x'));
      unmount();
    }
  });

  it('names a blocked image by its plain alt', async () => {
    const { container } = mount('![a *b*](docs/x.png)\n\nlast line', GITHUB, true);
    await waitFor(() => expect(container.querySelector('[data-streamdown-image-blocked]')?.textContent?.trim()).toBe('[Image unavailable: a b]'));
  });

  it('is plain in agent chat too', async () => {
    const { container } = render(ChatMarkdown, { props: { source: '![a *b*](https://example.invalid/i.png)' } });
    await waitFor(() => expect(container.querySelector('img')?.getAttribute('alt')).toBe('a b'));
  });
});

describe('image placement', () => {
  it.each([false, true])('floats an aligned image inline and in an HTML block (streaming %s)', async (streaming) => {
    const source = [
      'Text <img src="https://example.invalid/a.png" align="right" hspace="8" alt="a"> wraps.',
      '',
      '<img src="https://example.invalid/b.png" align="left" alt="b">',
      '',
      'last line',
    ].join('\n');
    const { container } = mount(source, GITHUB, streaming);
    await waitFor(() => expect(container.querySelectorAll('img')).toHaveLength(2));
    const [inline, block] = container.querySelectorAll('img');
    const wrapper = inline.closest<HTMLElement>('[data-streamdown-image]')!;
    expect([wrapper.style.float, wrapper.style.marginInline]).toEqual(['right', '8px']);
    // A lone image line is GitHub's paragraph; the image token's wrapper floats.
    expect(block.closest<HTMLElement>('[data-streamdown-image]')!.style.float).toBe('left');
  });

  it.each([false, true])("floats a raw HTML image by GitLab's float class, on GitLab only (streaming %s)", async (streaming) => {
    const source = [
      'Text <img src="https://example.invalid/a.png" class="glfm-float-right" alt="a"> wraps.',
      '',
      '<img src="https://example.invalid/b.png" class="glfm-float-left" align="right" alt="b">',
      '',
      '<img src="https://example.invalid/c.png" class="custom glfm-float-right" alt="c">',
      '',
      'last line',
    ].join('\n');
    // The box a placement lands on: the inline image's wrapper, else the image.
    const floats = (root: Element) =>
      [...root.querySelectorAll('img')].map((img) => (img.closest<HTMLElement>('[data-streamdown-image]') ?? img).style.float);
    const gitlab = mount(source, GITLAB, streaming).container;
    await waitFor(() => expect(gitlab.querySelectorAll('img')).toHaveLength(3));
    // The class beats `align`, as GitLab's stylesheet beats a presentational
    // attribute; a class value holding anything else is dropped whole.
    expect(floats(gitlab)).toEqual(['right', 'left', '']);
    const github = mount(source, GITHUB, streaming).container;
    await waitFor(() => expect(github.querySelectorAll('img')).toHaveLength(3));
    expect(floats(github)).toEqual(['', 'right', '']);
    for (const root of [gitlab, github]) expect(root.querySelector('[class*="glfm"]')).toBeNull();
  });

  it("floats an image by GitLab's float class", async () => {
    const { container } = mount('![a](https://example.invalid/a.png){.float-right width=100}\nwraps\n\nlast line', GITLAB, false);
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    expect(container.querySelector<HTMLElement>('[data-streamdown-image]')!.style.float).toBe('right');
    expect(container.textContent).not.toContain('float');
  });
});

describe('lone image lines', () => {
  const SOURCE = [
    '<img src="https://example.invalid/a.png" alt="a">',
    '<img src="https://example.invalid/b.png" alt="b">',
    '',
    'Text',
    '<img src="https://example.invalid/c.png" alt="c">',
    '',
    'last line',
  ].join('\n');
  // The alts of the images sharing each image's paragraph.
  const paragraphs = (root: Element) =>
    [...root.querySelectorAll('img')].map((img) => [...(img.closest('p')?.querySelectorAll('img') ?? [])].map((each) => each.alt).join());

  it.each([false, true])('stack on GitHub, each line its own paragraph, and flow on GitLab (streaming %s)', async (streaming) => {
    const github = mount(SOURCE, GITHUB, streaming).container;
    await waitFor(() => expect(github.querySelectorAll('img')).toHaveLength(3));
    // An image line after text stays in that paragraph, as an HTML block
    // cannot interrupt one.
    expect(paragraphs(github)).toEqual(['a', 'b', 'c']);
    expect(github.querySelector('img[alt="c"]')!.closest('p')!.textContent).toContain('Text');

    const gitlab = mount(SOURCE, GITLAB, streaming).container;
    await waitFor(() => expect(gitlab.querySelectorAll('img')).toHaveLength(3));
    expect(paragraphs(gitlab)).toEqual(['', '', 'c']);
  });
});
