import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ForgeAttachmentHost from './ForgeAttachmentHost.svelte';
import { buildForgeAttachmentHref } from '../../../utils/forgeAttachments';
import type { PRRef } from '../../../utils/prReference';
import type { ResolvedForgeAttachment } from '../../../utils/forgeAttachmentCache';

const acquire = vi.hoisted(() => vi.fn());
const release = vi.hoisted(() => vi.fn());
const open = vi.hoisted(() => vi.fn());

vi.mock('../../../utils/forgeAttachmentCache', () => ({
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

function resolves(value: Partial<ResolvedForgeAttachment>): void {
  acquire.mockImplementation(() => ({
    value: Promise.resolve({
      url: 'blob:forge-1',
      mimeType: 'image/png',
      kind: 'image',
      sizeBytes: 1024,
      filename: 'shot.png',
      ...value,
    } as ResolvedForgeAttachment),
    release,
  }));
}

describe('<ForgeAttachmentHost>', () => {
  beforeEach(() => {
    acquire.mockReset();
    release.mockReset();
    open.mockReset();
  });
  afterEach(() => vi.restoreAllMocks());

  it('renders an image, keeps the original href for copy, and releases on unmount', async () => {
    resolves({});
    const href = hrefFor();
    const { container, unmount } = render(ForgeAttachmentHost, {
      props: { token: token(href, 'a shot') },
    });

    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    const img = container.querySelector('img')!;
    expect(img.getAttribute('src')).toBe('blob:forge-1');
    expect(img.getAttribute('alt')).toBe('a shot');
    expect(img.getAttribute('data-markdown-image-src')).toBe(`/uploads/${HEX}/shot.png`);
    expect(img.getAttribute('loading')).toBe('lazy');
    expect(acquire).toHaveBeenCalledWith('gpu', MR, `/uploads/${HEX}/shot.png`);

    unmount();
    expect(release).toHaveBeenCalledTimes(1);
  });

  it('renders a player for video and a player for audio, by what the bytes were', async () => {
    resolves({ kind: 'video', mimeType: 'video/mp4', filename: 'clip.mp4' });
    const { container, unmount } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    await waitFor(() => expect(container.querySelector('video')).not.toBeNull());
    const video = container.querySelector('video')!;
    expect(video.hasAttribute('controls')).toBe(true);
    expect(video.getAttribute('preload')).toBe('metadata');
    unmount();

    resolves({ kind: 'audio', mimeType: 'audio/mpeg', filename: 'note.mp3' });
    const audioRender = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    await waitFor(() => expect(audioRender.container.querySelector('audio')).not.toBeNull());
    expect(audioRender.container.querySelector('audio')!.hasAttribute('controls')).toBe(true);
  });

  it('never paints a file kind, and its chip runs the same action as a link click', async () => {
    resolves({ kind: 'file', mimeType: 'application/octet-stream', filename: 'report.pdf', sizeBytes: 2048 });
    const { container } = render(ForgeAttachmentHost, {
      props: { token: token(hrefFor(`/uploads/${HEX}/report.pdf`)) },
    });

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
    let settle: (value: ResolvedForgeAttachment) => void = () => {};
    acquire.mockImplementation(() => ({
      value: new Promise<ResolvedForgeAttachment>((resolve) => { settle = resolve; }),
      release,
    }));
    const { container } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    expect(container.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    settle({ url: 'blob:x', mimeType: 'image/png', kind: 'image', sizeBytes: 1, filename: 'a.png', blob: new Blob() });
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
    await waitFor(() => expect(container.querySelector('[data-forge-attachment-error]')).not.toBeNull());
    expect(container.querySelector('a[data-forge-attachment-fallback]')).toBeNull();
  });

  it('reports a decode failure instead of leaving a blank gap', async () => {
    resolves({});
    const { container } = render(ForgeAttachmentHost, { props: { token: token(hrefFor()) } });
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());

    container.querySelector('img')!.dispatchEvent(new Event('error'));

    await waitFor(() => expect(container.querySelector('[data-forge-attachment-error]')).not.toBeNull());
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('[data-forge-attachment-error]')?.getAttribute('title'))
      .toContain('decode');
  });
});
