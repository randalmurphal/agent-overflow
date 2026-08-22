import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import StreamdownImageHost from './StreamdownImageHost.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { buildLocalImageHref } from '../../../utils/pathLinkExtension';

describe('<StreamdownImageHost>', () => {
  it('loads a guarded local image through the backend and revokes its blob URL', async () => {
    const createObjectURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:local-image');
    const revokeObjectURL = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    const getLocalImage = setBindingMock('GetLocalImageData', async () => ({
      data: 'iVBORw0KGgo=',
      mimeType: 'image/png',
    }));
    const { container, unmount } = render(StreamdownImageHost, {
      props: {
        token: {
          type: 'image',
          raw: '![diagram](file:///workspace/diagram.png)',
          href: buildLocalImageHref('/workspace/diagram.png', '/workspace'),
          title: null,
          text: 'diagram',
          tokens: [],
        },
      },
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
    const { container } = render(StreamdownImageHost, {
      props: {
        token: {
          type: 'image',
          raw: '![diagram](file:///workspace/diagram.svg)',
          href: buildLocalImageHref('/workspace/diagram.svg', '/workspace'),
          title: null,
          text: 'diagram',
          tokens: [],
        },
      },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-image-error]')).not.toBeNull();
    });
    expect(container.textContent).toContain('[Image unavailable: diagram]');
    expect(container.querySelector('[data-streamdown-image-error]')?.getAttribute('title')).toContain(
      'not a supported image',
    );
  });
});
