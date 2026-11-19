// In real Chromium: an attachment or local image inside an author's link
// leaves that link the one interactive target. Whatever the bytes resolve
// to, nothing inside the anchor is an anchor, a button or a player, the
// attachment still says what it is, and a click on the picture follows the
// link instead of opening the lightbox.

import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '../../../app.css';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import { closeImageLightbox, imageLightbox } from '../../stores/imageLightbox.svelte';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { __resetImageTiersForTest } from '../../utils/imageTiers';
import { __resetMediaBlobCacheForTest } from '../../utils/mediaBlobCache';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';
import { mockLocalImage } from '../../../test/mocks/attachmentTransfer';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'octo', repo: 'widget', number: 7 },
  backend: 'gpu',
  webBase: 'https://github.com/octo/widget/pull/7',
};
const ASSET = 'https://github.com/user-attachments/assets/4f0b0b1e-1111-2222-3333-444455556666';
const DESTINATION = 'https://example.com/destination';
const svg = (width: number, height: number) =>
  `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}"><rect width="${width}" height="${height}" fill="green"/></svg>`;

type Outcome = 'loading' | 'image' | 'failed' | 'video' | 'audio' | 'file';

// What the forge answers for every attachment on the page.
function stage(outcome: Outcome): void {
  if (outcome === 'loading') {
    setBindingMock('FetchForgeAttachment', () => new Promise(() => {}));
    return;
  }
  if (outcome === 'failed') {
    setBindingMock('FetchForgeAttachment', async () => {
      throw new Error('forge refused the attachment');
    });
    return;
  }
  const body = outcome === 'image' ? svg(80, 20) : 'media bytes';
  const mimeType = { image: 'image/svg+xml', video: 'video/mp4', audio: 'audio/mpeg', file: 'application/pdf' }[outcome];
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
    kind: outcome,
    sizeBytes: body.length,
    filename: `badge.${outcome === 'image' ? 'svg' : outcome === 'file' ? 'pdf' : outcome === 'video' ? 'mp4' : 'mp3'}`,
    ...(outcome === 'image' ? { width: 80, height: 20, originalWidth: 80, originalHeight: 20, derived: false } : {}),
  }));
}

// What the attachment shows in each outcome, inside the link.
const SHOWN: Record<Outcome, string> = {
  loading: '[data-forge-attachment-loading]',
  image: '[data-streamdown-image] img[src^="data:"]',
  failed: '[data-forge-attachment-error]',
  video: '[data-forge-attachment-linked="video"]',
  audio: '[data-forge-attachment-linked="audio"]',
  file: '[data-forge-attachment-linked="file"]',
};

const CONTROLS = 'a, button, video, audio, [role="button"], [tabindex]';

let host: HTMLElement;
let followed: number;

beforeEach(() => {
  document.body.innerHTML = '';
  host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  followed = 0;
  // The app's link delegate is not mounted here: count a followed link and
  // keep the page from navigating.
  host.addEventListener(
    'click',
    (event) => {
      if (!(event.target instanceof Element) || !event.target.closest(`a[href="${DESTINATION}"]`)) return;
      event.preventDefault();
      followed += 1;
    },
    true,
  );
});

afterEach(() => {
  closeImageLightbox();
  __resetMediaBlobCacheForTest();
  __resetImageTiersForTest();
  resetBindingMocks();
  vi.unstubAllGlobals();
});

async function clickFollowsLink(image: Element): Promise<void> {
  (image as HTMLElement).click();
  await Promise.resolve();
  expect(followed).toBe(1);
  expect(imageLightbox()).toBeNull();
}

const SHAPES: [string, string][] = [
  ['a markdown image in a markdown link', `[![badge](${ASSET})](${DESTINATION})`],
  ['an <img> in a markdown link', `See [<img src="${ASSET}" alt="badge">](${DESTINATION}) here.`],
  ['an <img> in an <a> in a sentence', `See <a href="${DESTINATION}"><img src="${ASSET}" alt="badge"></a> here.`],
  ['an <img> in an <a> inside an HTML wrapper', `<p align="center"><a href="${DESTINATION}"><img src="${ASSET}" alt="badge"></a></p>`],
];
const OUTCOMES: Outcome[] = ['loading', 'image', 'failed', 'video', 'audio', 'file'];
const PATHS: [string, boolean][] = [
  ['the compact static path', false],
  ['the component path', true],
];

describe.each(PATHS)('a forge attachment inside an author link, on %s', (_path, streaming) => {
  describe.each(SHAPES)('%s', (_shape, source) => {
    it.each(OUTCOMES)('renders no control of its own when it is %s', async (outcome) => {
      stage(outcome);
      render(ForgeAttachmentChatHarness, { target: host, props: { source, forgeSource: GITHUB, streaming } });
      const anchor = await waitFor(() => {
        const found = host.querySelector(`a[href="${DESTINATION}"]`);
        expect(found?.querySelector(SHOWN[outcome])).toBeTruthy();
        return found!;
      });
      expect(Array.from(anchor.querySelectorAll(CONTROLS)).map((each) => each.outerHTML)).toEqual([]);
      // Nothing escapes the anchor either: one link, and nothing the page
      // can operate besides it.
      expect(host.querySelectorAll('a')).toHaveLength(1);
      expect(host.querySelectorAll('button, video, audio, [role="button"]')).toHaveLength(0);
      if (outcome === 'image') await clickFollowsLink(anchor.querySelector(SHOWN.image)!);
      else if (outcome === 'failed') expect(anchor.textContent).toContain('Attachment unavailable');
      else if (outcome !== 'loading') expect(anchor.textContent).toContain(`badge.${outcome === 'file' ? 'pdf' : outcome === 'video' ? 'mp4' : 'mp3'}`);
    });
  });
});

describe('a local image inside an author link, in agent chat', () => {
  it('leaves the link the one control', async () => {
    mockLocalImage(() => ({ blob: new Blob([svg(80, 20)], { type: 'image/svg+xml' }), width: 80, height: 20 }));
    render(ChatMarkdown, {
      target: host,
      props: { source: `[![shot](shots/a.png)](${DESTINATION})`, pathRefs: EMPTY_PATH_REFS, workspacePath: '/workspace' },
    });
    const anchor = await waitFor(() => {
      const found = host.querySelector(`a[href="${DESTINATION}"]`);
      expect(found?.querySelector('[data-streamdown-image] img')).toBeTruthy();
      return found!;
    });
    expect(Array.from(anchor.querySelectorAll(CONTROLS)).map((each) => each.outerHTML)).toEqual([]);
    await clickFollowsLink(anchor.querySelector('img')!);
  });

  it('keeps its own control outside a link', async () => {
    mockLocalImage(() => ({ blob: new Blob([svg(80, 20)], { type: 'image/svg+xml' }), width: 80, height: 20 }));
    render(ChatMarkdown, {
      target: host,
      props: { source: '![shot](shots/a.png)', pathRefs: EMPTY_PATH_REFS, workspacePath: '/workspace' },
    });
    const picture = await waitFor(() => {
      const found = host.querySelector('[data-streamdown-image][role="button"]');
      expect(found?.querySelector('img')).toBeTruthy();
      return found!;
    });
    (picture.querySelector('img') as HTMLElement).click();
    expect(imageLightbox()).not.toBeNull();
  });
});
