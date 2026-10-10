import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ForgeAttachmentHost from './ForgeAttachmentHost.svelte';
import { closeImageLightbox, imageLightbox } from '../../../stores/imageLightbox.svelte';
import { buildForgeAttachmentHref } from '../../../utils/forgeAttachments';
import { forgeImageMenuTag } from '../../../utils/imageMenuActions';
import {
  __reportImageBoxForTest,
  __resetImageTiersForTest,
  lastImageTier,
  rememberImageTier,
} from '../../../utils/imageTiers';
import type { PRRef } from '../../../utils/prReference';
import {
  forgeAttachmentCacheKey,
  type ResolvedForgeAttachment,
} from '../../../utils/forgeAttachmentCache';

const acquire = vi.hoisted(() => vi.fn());
const release = vi.hoisted(() => vi.fn());
const open = vi.hoisted(() => vi.fn());

vi.mock('../../../utils/forgeAttachmentCache', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../../utils/forgeAttachmentCache')>()),
  acquireForgeAttachment: (...args: unknown[]) => acquire(...args),
}));
vi.mock('../../../utils/forgeAttachmentActions', () => ({
  openForgeAttachment: (...args: unknown[]) => open(...args),
}));

const HEX = '0123456789abcdef0123456789abcdef';
const MR: PRRef = { forge: 'gitlab', namespace: 'group', repo: 'widget', number: 3 };
const WEB = 'https://gitlab.example.test/group/widget/-/merge_requests/3';

function hrefFor(raw = `/uploads/${HEX}/shot.png`, webBase = WEB): string {
  return buildForgeAttachmentHref({ href: raw, pr: MR, backend: 'gpu', webBase });
}

function token(href: string, text = '') {
  return { type: 'image' as const, raw: '', href, title: null, text, tokens: [] };
}

function attachment(value: Partial<ResolvedForgeAttachment>): ResolvedForgeAttachment {
  return {
    url: 'blob:forge-1',
    mimeType: 'image/png',
    kind: 'image',
    sizeBytes: 1024,
    filename: 'shot.png',
    blob: new Blob(),
    width: 0,
    height: 0,
    originalWidth: 0,
    originalHeight: 0,
    originalBytes: 1024,
    derived: false,
    ...value,
  };
}

/**
 * The fetch lands later, and from then on every acquire of the same bytes
 * reads them synchronously, as the media cache answers.
 */
function resolves(value: Partial<ResolvedForgeAttachment>): void {
  const resolved = attachment(value);
  let landed = false;
  const promise = Promise.resolve(resolved).then((settledValue) => {
    landed = true;
    return settledValue;
  });
  acquire.mockImplementation(() => ({
    get settled() {
      return landed ? resolved : undefined;
    },
    value: promise,
    release,
  }));
}

/** The fetch lands when the test calls the returned function. */
function landsLater(): (value: ResolvedForgeAttachment) => void {
  let settle: (value: ResolvedForgeAttachment) => void = () => {};
  let landed: ResolvedForgeAttachment | undefined;
  const value = new Promise<ResolvedForgeAttachment>((resolve) => {
    settle = (resolved) => {
      landed = resolved;
      resolve(resolved);
    };
  });
  acquire.mockImplementation(() => ({
    get settled() {
      return landed;
    },
    value,
    release,
  }));
  return (resolved) => settle(resolved);
}

const RAW = `/uploads/${HEX}/shot.png`;
const KEY = forgeAttachmentCacheKey('gpu', MR, RAW);

function tiers(): unknown[] {
  return acquire.mock.calls.map((call) => call[3]);
}

/** The cache had the bytes already: the handle answers in the same frame. */
function settled(value: Partial<ResolvedForgeAttachment>): void {
  const resolved = attachment(value);
  acquire.mockImplementation(() => ({ settled: resolved, value: Promise.resolve(resolved), release }));
}

describe('<ForgeAttachmentHost>', () => {
  beforeEach(() => {
    acquire.mockReset();
    release.mockReset();
    open.mockReset();
  });
  afterEach(() => {
    closeImageLightbox();
    __resetImageTiersForTest();
    vi.restoreAllMocks();
  });

  it('renders an image, keeps the original href for copy, and releases on unmount', async () => {
    resolves({});
    const href = hrefFor();
    const { container, unmount } = render(ForgeAttachmentHost, {
      props: { token: token(href, 'a shot') },
    });
    __reportImageBoxForTest(800);

    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    const img = container.querySelector('img')!;
    expect(img.getAttribute('src')).toBe('blob:forge-1');
    expect(img.getAttribute('alt')).toBe('a shot');
    expect(img.getAttribute('data-markdown-image-src')).toBe(RAW);
    expect(img.getAttribute('title')).toBe(`https://gitlab.example.test/group/widget/uploads/${HEX}/shot.png`);
    expect(img.getAttribute('data-image-menu')).toBe('forge');
    expect(img.getAttribute('data-image-menu-href')).toBe(href);
    // In-memory bytes are never lazy: the box is painted the frame it mounts.
    expect(img.hasAttribute('loading')).toBe(false);
    expect(acquire).toHaveBeenCalledWith('gpu', MR, RAW, 1080);

    // One claim, the host's, handed to the picture; both release it on
    // unmount and the second release is a no-op.
    expect(acquire).toHaveBeenCalledTimes(1);
    unmount();
    expect(release).toHaveBeenCalled();
  });

  it("releases its claim on the first tier once the picture upgrades, so nothing stays behind the sharper one", async () => {
    resolves({ width: 720, height: 360, originalWidth: 3000, originalHeight: 1500, derived: true });
    const { container } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    __reportImageBoxForTest(700);
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    expect(tiers()).toEqual([720]);
    expect(release).not.toHaveBeenCalled();

    __reportImageBoxForTest(1400);
    await waitFor(() => expect(tiers()).toEqual([720, 1440]));
    expect(release).toHaveBeenCalledTimes(1);
  });

  it('measures before its first fetch, and that fetch is the one the picture paints', async () => {
    resolves({ width: 720, height: 360, originalWidth: 3000, originalHeight: 1500, derived: true });
    const { container } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    expect(container.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    expect(acquire).not.toHaveBeenCalled();

    __reportImageBoxForTest(700);
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    // The one claim is on the tier the first fetch asked for.
    expect(tiers()).toEqual([720]);
    expect(container.querySelector('[data-streamdown-image-loading]')).toBeNull();
    const img = container.querySelector('img')!;
    expect(img.getAttribute('width')).toBe('3000');
    expect(img.getAttribute('height')).toBe('1500');
  });

  it('hands the picture the tier it holds, whatever another pane asked for since', async () => {
    const settle = landsLater();
    const { container } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    __reportImageBoxForTest(700);
    await tick();
    rememberImageTier(KEY, 1440);
    settle(attachment({ width: 720, height: 360, originalWidth: 3000, originalHeight: 1500, derived: true }));
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    expect(tiers()).toEqual([720]);
  });

  it('skips the measurement when the page already asked for this attachment', () => {
    rememberImageTier(KEY, 1440);
    resolves({});
    render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    expect(tiers()).toEqual([1440]);
  });

  it('opens the lightbox on the forge image and fetches the original only behind a derivative', async () => {
    resolves({ width: 720, height: 360, originalWidth: 3000, originalHeight: 1500, originalBytes: 4096, derived: true });
    const href = hrefFor();
    const { container } = render(ForgeAttachmentHost, { props: { token: token(href, 'a shot') } });
    __reportImageBoxForTest(700);
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());

    await fireEvent.click(container.querySelector('img')!);
    const item = imageLightbox()?.images[0];
    expect(item).toMatchObject({
      id: KEY,
      filename: 'shot.png',
      url: 'blob:forge-1',
      width: 3000,
      height: 1500,
      originalBytes: 4096,
      menuTag: forgeImageMenuTag(href),
    });
    expect(item?.original).toBeTypeOf('function');
  });

  it('paints a cached attachment in the same frame it mounts, at its declared size', () => {
    rememberImageTier(KEY, 1080);
    settled({ width: 640, height: 480 });
    const { container } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    expect(container.querySelector('[data-forge-attachment-loading]')).toBeNull();
    const img = container.querySelector('img')!;
    expect(img.getAttribute('src')).toBe('blob:forge-1');
    expect(img.getAttribute('width')).toBe('640');
    expect(img.getAttribute('height')).toBe('480');
  });

  it('renders a player for video and a player for audio, by what the bytes were', async () => {
    resolves({ kind: 'video', mimeType: 'video/mp4', filename: 'clip.mp4' });
    const { container, unmount } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    __reportImageBoxForTest(800);
    await waitFor(() => expect(container.querySelector('video')).not.toBeNull());
    const video = container.querySelector('video')!;
    expect(video.hasAttribute('controls')).toBe(true);
    expect(video.getAttribute('preload')).toBe('metadata');
    unmount();

    resolves({ kind: 'audio', mimeType: 'audio/mpeg', filename: 'note.mp3' });
    const audioRender = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    __reportImageBoxForTest(800);
    await waitFor(() => expect(audioRender.container.querySelector('audio')).not.toBeNull());
    expect(audioRender.container.querySelector('audio')!.hasAttribute('controls')).toBe(true);
  });

  it('never paints a file kind, and its chip runs the same action as a link click', async () => {
    resolves({ kind: 'file', mimeType: 'application/octet-stream', filename: 'report.pdf', sizeBytes: 2048 });
    const { container } = render(ForgeAttachmentHost, {
      props: { token: token(hrefFor(`/uploads/${HEX}/report.pdf`)) },
    });
    __reportImageBoxForTest(800);

    await waitFor(() => expect(container.querySelector('[data-forge-attachment-file]')).not.toBeNull());
    const chip = container.querySelector('button[data-forge-attachment-file]')!;
    expect(chip.textContent).toContain('report.pdf');
    expect(chip.textContent).toContain('2.0 KB');
    expect(container.querySelector('img')).toBeNull();

    chip.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(open).toHaveBeenCalledWith(
      expect.objectContaining({ href: `/uploads/${HEX}/report.pdf`, backend: 'gpu', pr: MR }),
    );
  });

  it('shows a loading state before the bytes land', async () => {
    const settle = landsLater();
    const { container } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    __reportImageBoxForTest(800);
    await tick();
    expect(container.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    // Remembered at the request, before anything lands, so a save or a
    // second pane shares it.
    expect(lastImageTier(KEY)).toBe(1080);
    // The first width decides the request; a later one does not restart it.
    __reportImageBoxForTest(1600);
    await tick();
    expect(tiers()).toEqual([1080]);
    settle(attachment({ url: 'blob:x', sizeBytes: 1, filename: 'a.png' }));
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
  });

  it('names the failure and still offers the forge, so the reader is never stuck', async () => {
    acquire.mockImplementation(() => ({
      value: Promise.reject(new Error("GitLab CLI (`glab`) is not authenticated.")),
      release,
    }));
    const { container } = render(ForgeAttachmentHost, {
      props: { token: token(hrefFor()) },
    });
    __reportImageBoxForTest(800);

    await waitFor(() => expect(container.querySelector('[data-forge-attachment-error]')).not.toBeNull());
    expect(container.textContent).toContain('[Attachment unavailable: shot.png]');
    expect(container.querySelector('[data-forge-attachment-error]')?.getAttribute('title'))
      .toContain('not authenticated');
    const fallback = container.querySelector('a[data-forge-attachment-fallback]')!;
    expect(fallback.getAttribute('href'))
      .toBe(`https://gitlab.example.test/group/widget/uploads/${HEX}/shot.png`);
    expect(fallback.textContent).toContain('GitLab');
  });

  it('offers no fallback link when the merge request web URL is unknown', async () => {
    acquire.mockImplementation(() => ({ value: Promise.reject(new Error('nope')), release }));
    const { container } = render(ForgeAttachmentHost, {
      props: { token: token(hrefFor(`/uploads/${HEX}/shot.png`, '')) },
    });
    __reportImageBoxForTest(800);
    await waitFor(() => expect(container.querySelector('[data-forge-attachment-error]')).not.toBeNull());
    expect(container.querySelector('a[data-forge-attachment-fallback]')).toBeNull();
  });

  it('reports a decode failure instead of leaving a blank gap', async () => {
    resolves({});
    const { container } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    __reportImageBoxForTest(800);
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());

    container.querySelector('img')!.dispatchEvent(new Event('error'));

    await waitFor(() => expect(container.querySelector('[data-forge-attachment-error]')).not.toBeNull());
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('[data-forge-attachment-error]')?.getAttribute('title'))
      .toContain('decode');
  });
});
