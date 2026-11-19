// In real Chromium, under the app stylesheet: an image in review prose sits
// in its line, as a forge renders it. A badge row is one row, an authored
// size is the painted size, and chat keeps its centered picture block.

import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '../../../app.css';
import ChatMarkdown from '../chat/ChatMarkdown.svelte';
import { EMBEDDED_HTML_EXTENSIONS, Streamdown, sanitizeEmbeddedHtmlToken } from '../../markdown';
import { chatMarkdownTheme } from '../chat/markdown/streamdownTheme';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from '../chat/markdown/forgeAttachmentContext';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { __resetImageTiersForTest } from '../../utils/imageTiers';
import { __resetMediaBlobCacheForTest } from '../../utils/mediaBlobCache';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetAppearanceForTest, setAppearance } from '../../stores/appearance.svelte';

const svg = (width: number, height: number) =>
  `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}"><rect width="${width}" height="${height}" fill="green"/></svg>`;
// An 80x20 badge and a 4:1 banner, inline so the page needs no network.
const BADGE = `data:image/svg+xml;base64,${btoa(svg(80, 20))}`;
const BANNER = `data:image/svg+xml;base64,${btoa(svg(400, 100))}`;
// The same two over http(s), the only scheme the bare renderer loads:
// served by the test server under its other loopback name, since an
// authored image never loads from the page's own origin. Built at run
// time, since the bundler inlines a small asset it can see as a data: URL.
const servedOrigin = (): string => {
  const url = new URL(location.origin);
  url.hostname = url.hostname === 'localhost' ? '127.0.0.1' : 'localhost';
  return url.origin;
};
const fixture = (name: string) => `${servedOrigin()}/src/test/fixtures/images/${name}`;
const SERVED: Record<string, string> = {
  [BADGE]: fixture('badge-80x20.svg'),
  [BANNER]: fixture('banner-400x100.svg'),
};

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'octo', repo: 'widget', number: 7 },
  backend: 'gpu',
  webBase: 'https://github.com/octo/widget/pull/7',
};
const ASSET = 'https://github.com/user-attachments/assets/4f0b0b1e-1111-2222-3333-444455556666';

beforeEach(() => {
  document.body.innerHTML = '';
});

afterEach(() => {
  __resetMediaBlobCacheForTest();
  __resetImageTiersForTest();
  resetBindingMocks();
  vi.unstubAllGlobals();
});

async function mounted(source: string, className: string, images: number): Promise<HTMLImageElement[]> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  render(ChatMarkdown, { target: host, props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: className } });
  await waitFor(() => expect(host.querySelectorAll('img')).toHaveLength(images));
  const found = Array.from(host.querySelectorAll('img'));
  await Promise.all(found.map((image) => (image.src.startsWith('data:') ? image.decode() : Promise.resolve())));
  return found;
}

const box = (element: Element) => element.getBoundingClientRect();

// Serves every forge attachment as an SVG of this size: the ticket mint,
// and the transfer it names. A video is served as bytes no player decodes;
// its box is laid out before they load.
function serveForgeImage(width: number, height: number, kind: 'image' | 'video' = 'image'): void {
  const body = kind === 'image' ? svg(width, height) : 'not a video';
  const mimeType = kind === 'image' ? 'image/svg+xml' : 'video/mp4';
  const fetch = globalThis.fetch;
  vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) =>
    String(input instanceof Request ? input.url : input).includes('/attachments/forge/')
      ? Promise.resolve(new Response(body, { status: 200, headers: { 'Content-Type': mimeType } }))
      : fetch(input, init),
  );
  let ticket = 0;
  setBindingMock('FetchForgeAttachment', async () => ({
    url: `/attachments/forge/tok${++ticket}?ticket=t`,
    mimeType,
    kind,
    sizeBytes: body.length,
    filename: kind === 'image' ? 'shot.svg' : 'clip.mp4',
    ...(kind === 'image' ? { width, height, originalWidth: width, originalHeight: height, derived: false } : {}),
  }));
}

const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'gpu',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};
const HEX = '0123456789abcdef0123456789abcdef';

// `source` in a GitLab MR's review prose, 320px wide.
function gitlabProse(source: string): HTMLElement {
  const host = document.createElement('div');
  host.style.width = '320px';
  document.body.appendChild(host);
  render(ChatMarkdown, {
    target: host,
    props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => GITLAB]]),
  });
  return host;
}

type ImagePath = 'direct' | 'bare renderer' | 'sanitizer' | 'forge token' | 'forge in html';

// One image at `attrs`, rendered in a review paragraph `lineWidth` wide by
// one of the paths that paint an authored size.
async function sizedImage(path: ImagePath, attrs: string, lineWidth: number, image: { src: string; width: number; height: number }): Promise<HTMLImageElement> {
  const host = document.createElement('div');
  host.style.width = `${lineWidth}px`;
  document.body.appendChild(host);
  const forge = path === 'forge token' || path === 'forge in html';
  if (forge) serveForgeImage(image.width, image.height);
  const src = forge ? ASSET : path === 'bare renderer' || path === 'sanitizer' ? SERVED[image.src] : image.src;
  const source =
    path === 'sanitizer' || path === 'forge in html'
      ? `<p><img src="${src}" ${attrs} alt="x"></p>`
      : `See <img src="${src}" ${attrs} alt="x"> here.`;
  if (path === 'bare renderer') {
    render(Streamdown, {
      target: host,
      props: { content: source, theme: chatMarkdownTheme, extensions: EMBEDDED_HTML_EXTENSIONS, renderHtml: sanitizeEmbeddedHtmlToken },
    });
  } else {
    render(ChatMarkdown, {
      target: host,
      props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
      context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => (forge ? GITHUB : null)]]),
    });
  }
  const selector = forge ? 'img[src^="data:"]' : 'img';
  await waitFor(() => expect(host.querySelector(selector)).not.toBeNull());
  const found = host.querySelector<HTMLImageElement>(selector)!;
  // A forge image's box is known in the frame it mounts, before its bytes
  // decode (MarkdownImage's box contract).
  const first = forge ? [box(found).width, box(found).height] : null;
  await found.decode();
  if (first) expect([box(found).width, box(found).height]).toEqual(first);
  // Each path must have run its own way.
  if (path === 'direct') expect(found.closest('[data-streamdown-image]')).not.toBeNull();
  if (path === 'bare renderer') expect(found.closest('[data-streamdown-image]')?.getAttribute('data-streamdown-image')).toBeTruthy();
  if (path === 'sanitizer') expect(found.parentElement?.localName).toBe('p');
  if (forge) expect(found.closest('[data-streamdown-image][role="button"]')).not.toBeNull();
  if (path === 'forge in html') expect(found.closest('[data-markdown-embedded-html]')).not.toBeNull();
  return found;
}

const PATHS: ImagePath[] = ['direct', 'bare renderer', 'sanitizer', 'forge token', 'forge in html'];

describe('images in review prose', () => {
  it('lays a badge row out as one row, from the left edge', async () => {
    const [first, second] = await mounted(`![build](${BADGE}) ![coverage](${BADGE})`, 'review-prose', 2);
    expect(box(second).top).toBe(box(first).top);
    expect(box(second).left).toBeGreaterThanOrEqual(box(first).right);
    const paragraph = first.closest('p')!;
    expect(box(first).left - box(paragraph).left).toBeLessThan(1);
    expect(box(first).width).toBe(80);
  });

  it('keeps an icon in its sentence', async () => {
    const [icon] = await mounted(`Checks <img src="${BADGE}" width="40" alt="ok"> passed for this change.`, 'review-prose', 1);
    const paragraph = icon.closest('p')!;
    // One line box: the text continues beside the icon.
    expect(box(paragraph).height).toBeLessThan(40);
    expect(box(icon).width).toBe(40);
    expect(box(icon).height).toBe(10);
  });

  it('paints an authored height the stylesheet would otherwise unset', async () => {
    const [short] = await mounted(`A <img src="${BADGE}" height="10" alt="short"> badge.`, 'review-prose', 1);
    expect(box(short).height).toBe(10);
    expect(box(short).width).toBe(40);
  });

  it('lays raw html images out in their line at their authored size', async () => {
    // A wrapper makes the run one html block: the sanitizer's images.
    const [first, second] = await mounted(
      '<p><img src="https://badges.invalid/a.svg" width="40" height="20" alt="a"> <img src="https://badges.invalid/b.svg" width="40" height="20" alt="b"></p>',
      'review-prose',
      2,
    );
    expect(box(second).top).toBe(box(first).top);
    expect([box(first).width, box(first).height]).toEqual([40, 20]);
  });

  it.each(PATHS)('resolves a percentage width against the line, through the %s path', async (path) => {
    const image = await sizedImage(path, 'width="50%"', 320, { src: BADGE, width: 80, height: 20 });
    expect([box(image).width, box(image).height]).toEqual([160, 40]);
  });

  it.each(PATHS)('keeps an authored ratio when the line narrows the width, through the %s path', async (path) => {
    const image = await sizedImage(path, 'width="400" height="100"', 200, { src: BANNER, width: 400, height: 100 });
    expect([box(image).width, box(image).height]).toEqual([200, 50]);
  });

  it.each(PATHS)('paints an authored width and height where the line has room, through the %s path', async (path) => {
    const image = await sizedImage(path, 'width="120" height="60"', 600, { src: BANNER, width: 400, height: 100 });
    expect([box(image).width, box(image).height]).toEqual([120, 60]);
  });

  it.each(PATHS)('derives the width of an authored height alone from the ratio, through the %s path', async (path) => {
    const image = await sizedImage(path, 'height="10"', 600, { src: BADGE, width: 80, height: 20 });
    expect([box(image).width, box(image).height]).toEqual([40, 10]);
  });

  it.each(PATHS)('derives the height of an authored width alone from the ratio, through the %s path', async (path) => {
    const image = await sizedImage(path, 'width="40"', 600, { src: BADGE, width: 80, height: 20 });
    expect([box(image).width, box(image).height]).toEqual([40, 10]);
  });

  it('paints a GitLab attribute list as the size and not as text', async () => {
    const host = gitlabProse(`![wide](${BADGE}){width=50%} ![small](${BADGE}){width=30}`);
    await waitFor(() => expect(host.querySelectorAll('img')).toHaveLength(2));
    const [wide, small] = Array.from(host.querySelectorAll('img'));
    await Promise.all([wide.decode(), small.decode()]);
    expect([box(wide).width, box(wide).height]).toEqual([160, 40]);
    expect([box(small).width, box(small).height]).toEqual([30, 7.5]);
    expect(host.textContent).not.toContain('width');
  });

  // GitLab applies a list to any image it directly follows, however the
  // image was written.
  it.each([
    ['an <img> in a sentence', `See <img src="${BADGE}" alt="small">{width=30} here.`, 'img'],
    ['a claimed upload <img>', `See <img src="/uploads/${HEX}/a.svg" alt="small">{width=30} here.`, 'img[src^="data:"]'],
    ['a claimed upload image', `See ![small](/uploads/${HEX}/a.svg){width=30} here.`, 'img[src^="data:"]'],
  ])('sizes %s by the GitLab attribute list after it', async (_name, source, selector) => {
    serveForgeImage(80, 20);
    const host = gitlabProse(source);
    await waitFor(() => expect(host.querySelector(selector)).not.toBeNull());
    const image = host.querySelector<HTMLImageElement>(selector)!;
    await image.decode();
    expect([box(image).width, box(image).height]).toEqual([30, 7.5]);
    expect(host.textContent).not.toContain('width');
    expect(host.textContent).toContain('See');
    expect(host.textContent).toContain('here.');
  });

  it('keeps a GitLab attribute list a space apart from its image as text', async () => {
    const host = gitlabProse(`![small](${BADGE}) {width=30}`);
    await waitFor(() => expect(host.querySelector('img')).not.toBeNull());
    const image = host.querySelector('img')!;
    await image.decode();
    expect(box(image).width).toBe(80);
    expect(host.textContent).toContain('{width=30}');
  });

  it('sizes a GitLab video by its attribute list', async () => {
    serveForgeImage(0, 0, 'video');
    const host = gitlabProse(`![clip](/uploads/${HEX}/clip.mp4){width=160 height=90}`);
    // Measured in the frame the player mounts: bytes no player decodes are
    // reported as a chip once the element gives up on them.
    const size = await waitFor(() => {
      const video = host.querySelector('video');
      expect(video).not.toBeNull();
      return [box(video!).width, box(video!).height];
    });
    expect(size).toEqual([160, 90]);
    expect(host.textContent).not.toContain('width');
  });

  it.each(PATHS)('floats an aligned image to its side of the line, through the %s path', async (path) => {
    const image = await sizedImage(path, 'width="80" align="right" hspace="6"', 400, { src: BADGE, width: 80, height: 20 });
    const line = image.closest('p')!;
    expect(box(line).right - box(image).right).toBeCloseTo(6, 0);
    // The text of the line sits beside it, not under it.
    expect(box(line).height).toBeLessThan(40);
  });

  it("floats an image by GitLab's float class", async () => {
    const host = gitlabProse(`![logo](${BADGE}){.float-right width=40}\nThis text wraps around the left side of the image.`);
    await waitFor(() => expect(host.querySelector('img')).not.toBeNull());
    const image = host.querySelector('img')!;
    await image.decode();
    expect(box(host).right - box(image).right).toBeLessThan(1);
    expect([box(image).width, box(image).height]).toEqual([40, 10]);
  });

  it("paints a GitHub picture's source for the app theme at the fallback's size, and switches with it", async () => {
    setBindingMock('SetAppearance', async () => {});
    try {
      void setAppearance({ mode: 'dark' });
      const host = document.createElement('div');
      host.style.width = '600px';
      document.body.appendChild(host);
      const picture = `<picture><source media="(prefers-color-scheme: dark)" srcset="${SERVED[BANNER]}"><img src="${SERVED[BADGE]}" width="200" alt="logo"></picture>`;
      render(ChatMarkdown, {
        target: host,
        props: { source: `Logo ${picture} inline.\n\n<p align="center">${picture}</p>`, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
        context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => GITHUB]]),
      });
      const painted = async () => {
        await waitFor(() => expect(host.querySelectorAll('img')).toHaveLength(2));
        const images = Array.from(host.querySelectorAll('img'));
        await Promise.all(images.map((image) => image.decode()));
        return images.map((image) => [image.getAttribute('src')!.split('/').pop(), box(image).width, box(image).height]);
      };
      expect(await painted()).toEqual([
        ['banner-400x100.svg', 200, 50],
        ['banner-400x100.svg', 200, 50],
      ]);
      void setAppearance({ mode: 'light' });
      await waitFor(() => expect(host.querySelector('img[src$="banner-400x100.svg"]')).toBeNull());
      expect(await painted()).toEqual([
        ['badge-80x20.svg', 200, 50],
        ['badge-80x20.svg', 200, 50],
      ]);
    } finally {
      resetAppearanceForTest();
    }
  });

  it('leaves chat its centered picture block', async () => {
    const [first, second] = await mounted(`![build](${BADGE}) ![coverage](${BADGE})`, '', 2);
    expect(box(second).top).toBeGreaterThan(box(first).bottom - 1);
    const paragraph = first.closest('p')!;
    expect(box(first).left - box(paragraph).left).toBeGreaterThan(100);
  });
});
