import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import StreamdownImageHost from './StreamdownImageHost.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { buildLocalImageHref } from '../../../utils/pathLinkExtension';
import { buildForgeAttachmentHref } from '../../../utils/forgeAttachments';

function imageToken(href: string, text = 'diagram') {
  return { type: 'image' as const, raw: `![${text}](${href})`, href, title: null, text, tokens: [] };
}

describe('<StreamdownImageHost>', () => {
  it('loads a guarded local image through the backend and revokes its blob URL', async () => {
    const createObjectURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:local-image');
    const revokeObjectURL = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    const getLocalImage = setBindingMock('GetLocalImageData', async () => ({
      data: 'iVBORw0KGgo=',
      mimeType: 'image/png',
    }));
    const href = buildLocalImageHref('/workspace/diagram.png', '/workspace');
    const { container, unmount } = render(StreamdownImageHost, {
      props: { token: imageToken(href), src: href },
    });

    await waitFor(() => {
      expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:local-image');
    });
    expect(getLocalImage).toHaveBeenCalledWith('/workspace/diagram.png', '/workspace');
    expect(createObjectURL).toHaveBeenCalledTimes(1);

    unmount();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:local-image');
  });

  it('surfaces backend failures in the markdown body', async () => {
    setBindingMock('GetLocalImageData', async () => {
      throw new Error('file is not a supported image');
    });
    const href = buildLocalImageHref('/workspace/diagram.svg', '/workspace');
    const { container } = render(StreamdownImageHost, {
      props: { token: imageToken(href), src: href },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-image-error]')).not.toBeNull();
    });
    expect(container.textContent).toContain('[Image unavailable: diagram]');
    expect(container.querySelector('[data-streamdown-image-error]')?.getAttribute('title')).toContain(
      'not a supported image',
    );
  });

  it('renders an approved http or data:image src directly', async () => {
    for (const src of ['https://example.test/x.png', 'data:image/png;base64,iVBORw0KGgo=']) {
      const { container, unmount } = render(StreamdownImageHost, {
        props: { token: imageToken(src), src },
      });
      await waitFor(() => {
        expect(container.querySelector('img')?.getAttribute('src')).toBe(src);
      });
      unmount();
    }
  });

  it('reports a decode failure instead of leaving a blank gap', async () => {
    const src = 'https://example.test/not-really.png';
    const { container } = render(StreamdownImageHost, {
      props: { token: imageToken(src), src },
    });
    await waitFor(() => {
      expect(container.querySelector('img')).not.toBeNull();
    });

    container.querySelector('img')!.dispatchEvent(new Event('error'));

    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-image-error]')).not.toBeNull();
    });
    expect(container.querySelector('img')).toBeNull();
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
      props: { token: imageToken(href, 'shot'), src: href },
    });
    await waitFor(() => {
      expect(container.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    });
    expect(container.querySelector('[data-streamdown-image-error]')).toBeNull();
  });

  it('names a scheme it will not paint rather than dropping the image', async () => {
    const src = 'mailto:someone@example.test';
    const { container } = render(StreamdownImageHost, {
      props: { token: imageToken(src), src },
    });
    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-image-error]')).not.toBeNull();
    });
    expect(container.querySelector('[data-streamdown-image-error]')?.getAttribute('title')).toContain('mailto');
  });
});
