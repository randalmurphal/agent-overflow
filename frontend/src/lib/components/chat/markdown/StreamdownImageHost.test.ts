import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import StreamdownImageHost from './StreamdownImageHost.svelte';
import { mockLocalImage, type LocalImageReply } from '../../../../test/mocks/attachmentTransfer';
import { getPinnedBackend } from '../../../transport/backends';
import { buildLocalImageHref } from '../../../utils/pathLinkExtension';
import { buildForgeAttachmentHref } from '../../../utils/forgeAttachments';
import { __resetMediaBlobCacheForTest } from '../../../utils/mediaBlobCache';

function imageToken(href: string, text = 'diagram') {
  return { type: 'image' as const, raw: `![${text}](${href})`, href, title: null, text, tokens: [] };
}

function mountImage(href: string, backend = 'gpu') {
  return render(StreamdownImageHost, { props: { token: imageToken(href), src: href, backend } });
}

function pngReply(overrides: LocalImageReply = {}): LocalImageReply {
  return { mimeType: 'image/png', width: 400, height: 300, ...overrides };
}

describe('<StreamdownImageHost>', () => {
  beforeEach(() => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:local-image');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });

  afterEach(() => {
    __resetMediaBlobCacheForTest();
    vi.restoreAllMocks();
  });

  it("loads a guarded local image from the thread's computer and reserves its box", async () => {
    let pinned: string | null = null;
    const getLocalImage = mockLocalImage(() => {
      pinned = getPinnedBackend();
      return pngReply();
    });
    const href = buildLocalImageHref('/workspace/diagram.png', '/workspace');
    const { container } = mountImage(href);
    expect(container.querySelector('[data-streamdown-image-loading]')).not.toBeNull();

    await waitFor(() => {
      expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:local-image');
    });
    expect(getLocalImage).toHaveBeenCalledWith('/workspace/diagram.png', '/workspace', 0);
    expect(pinned).toBe('gpu');
    const img = container.querySelector('img')!;
    expect(img.getAttribute('width')).toBe('400');
    expect(img.getAttribute('height')).toBe('300');
    // In-memory bytes are never lazy: the box is painted the frame it mounts.
    expect(img.hasAttribute('loading')).toBe(false);
    expect(container.querySelector('[data-streamdown-image-loading]')).toBeNull();
  });

  it('paints a second mount of the same bytes in the same frame, with no second fetch', async () => {
    const getLocalImage = mockLocalImage(() => pngReply());
    const href = buildLocalImageHref('/workspace/diagram.png', '/workspace');
    const first = mountImage(href);
    await waitFor(() => expect(first.container.querySelector('img')).not.toBeNull());

    // The side chat forked from this thread, or the row remounting after a
    // scroll away and back: no placeholder frame, no RPC.
    const second = mountImage(href);
    expect(second.container.querySelector('[data-streamdown-image-loading]')).toBeNull();
    expect(second.container.querySelector('img')?.getAttribute('src')).toBe('blob:local-image');
    expect(second.container.querySelector('img')?.getAttribute('width')).toBe('400');
    expect(getLocalImage).toHaveBeenCalledTimes(1);

    // Unmounting one holder leaves the URL live for the other; the cache
    // revokes when the entry is evicted or dropped.
    first.unmount();
    expect(URL.revokeObjectURL).not.toHaveBeenCalled();
    second.unmount();
    __resetMediaBlobCacheForTest();
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:local-image');
  });

  it('keys the bytes by computer, so the same path on another machine is its own fetch', async () => {
    const getLocalImage = mockLocalImage(() => pngReply());
    const href = buildLocalImageHref('/workspace/diagram.png', '/workspace');
    const gpu = mountImage(href, 'gpu');
    await waitFor(() => expect(gpu.container.querySelector('img')).not.toBeNull());
    const laptop = mountImage(href, 'laptop');
    await waitFor(() => expect(laptop.container.querySelector('img')).not.toBeNull());
    expect(getLocalImage).toHaveBeenCalledTimes(2);
  });

  it('remembers the size an <img> decoded when the backend could not read the header', async () => {
    mockLocalImage(() => ({ blob: new Blob(['<svg/>'], { type: 'image/svg+xml' }), width: 0, height: 0 }));
    const href = buildLocalImageHref('/workspace/diagram.svg', '/workspace');
    const first = mountImage(href);
    await waitFor(() => expect(first.container.querySelector('img')).not.toBeNull());
    const img = first.container.querySelector('img')!;
    expect(img.getAttribute('src')?.startsWith('data:image/svg+xml;base64,')).toBe(true);
    expect(img.hasAttribute('width')).toBe(false);

    Object.defineProperty(img, 'naturalWidth', { value: 120 });
    Object.defineProperty(img, 'naturalHeight', { value: 80 });
    img.dispatchEvent(new Event('load'));

    const second = mountImage(href);
    expect(second.container.querySelector('img')?.getAttribute('width')).toBe('120');
    expect(second.container.querySelector('img')?.getAttribute('height')).toBe('80');
  });

  it('names the reason in the chip and keeps the whole message as its tooltip', async () => {
    mockLocalImage(() => {
      throw new Error(
        'load local image: file not found: /workspace/diagram.png: open /workspace/diagram.png: no such file or directory',
      );
    });
    const href = buildLocalImageHref('/workspace/diagram.png', '/workspace');
    const { container } = mountImage(href);

    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-image-error]')).not.toBeNull();
    });
    expect(container.textContent).toContain('[Image unavailable: diagram (file not found)]');
    expect(container.querySelector('[data-streamdown-image-error]')?.getAttribute('title')).toContain(
      'no such file or directory',
    );
    expect(container.querySelector('[data-streamdown-image-loading]')).toBeNull();
  });

  it('renders an approved http or data:image src directly, lazy only for http', async () => {
    for (const src of ['https://example.test/x.png', 'data:image/png;base64,iVBORw0KGgo=']) {
      const { container, unmount } = mountImage(src);
      await waitFor(() => {
        expect(container.querySelector('img')?.getAttribute('src')).toBe(src);
      });
      expect(container.querySelector('img')?.getAttribute('loading')).toBe(
        src.startsWith('https:') ? 'lazy' : null,
      );
      unmount();
    }
  });

  it('reports a decode failure instead of leaving a blank gap', async () => {
    const src = 'https://example.test/not-really.png';
    const { container } = mountImage(src);
    await waitFor(() => {
      expect(container.querySelector('img')).not.toBeNull();
    });

    container.querySelector('img')!.dispatchEvent(new Event('error'));

    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-image-error]')).not.toBeNull();
    });
    expect(container.querySelector('img')).toBeNull();
    expect(container.textContent).toContain('(cannot decode)');
    expect(container.querySelector('[data-streamdown-image-error]')?.getAttribute('title')).toContain('decode');
  });

  it('hands a forge attachment to its own host rather than treating it as a scheme it cannot paint', async () => {
    // The forge host resolves through the RPC + ticket path; with no mock
    // installed it stays in its loading state, which is all this dispatch
    // assertion needs. What it must NOT do is fall into the "this surface
    // does not display agent-overflow: images" branch.
    const href = buildForgeAttachmentHref({
      href: '/uploads/0123456789abcdef0123456789abcdef/shot.png',
      pr: { forge: 'gitlab', namespace: 'group', repo: 'widget', number: 3 },
      backend: 'gpu',
      webBase: '',
    });
    const { container } = render(StreamdownImageHost, {
      props: { token: imageToken(href, 'shot'), src: href, backend: 'gpu' },
    });
    await waitFor(() => {
      expect(container.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    });
    expect(container.querySelector('[data-streamdown-image-error]')).toBeNull();
  });

  it('names a scheme it will not paint rather than dropping the image', async () => {
    const src = 'mailto:someone@example.test';
    const { container } = mountImage(src);
    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-image-error]')).not.toBeNull();
    });
    expect(container.querySelector('[data-streamdown-image-error]')?.getAttribute('title')).toContain('mailto');
  });
});
