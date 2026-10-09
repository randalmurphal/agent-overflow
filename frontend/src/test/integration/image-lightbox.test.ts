// The image lightbox is one app-level surface: App mounts the dialog for
// whatever preview `stores/imageLightbox.svelte.ts` holds, and the dialog's
// close goes back through the store so the preview is disposed.

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import { flush, installAnimateShim, installAppDefaults, resetAppState } from './_helpers';
import type { ExpandedImagePreview, ImagePreviewItem } from '../../lib/utils/attachmentPreview.svelte';
import {
  closeImageLightbox,
  imageLightbox,
  openImageLightbox,
} from '../../lib/stores/imageLightbox.svelte';

beforeAll(installAnimateShim);

function item(id: string): ImagePreviewItem {
  return {
    id,
    filename: `${id}.png`,
    mimeType: 'image/png',
    url: `blob:${id}`,
    width: 0,
    height: 0,
    originalBytes: 10,
    menuTag: {},
  };
}

function preview(...ids: string[]): ExpandedImagePreview & { dispose: () => void } {
  return { images: ids.map(item), index: 0, dispose: vi.fn() };
}

describe('App integration: image lightbox', () => {
  beforeEach(() => {
    resetAppState();
    installAppDefaults();
  });

  afterEach(() => {
    closeImageLightbox();
  });

  it('shows the open preview, swaps it in place, and closes through the store', async () => {
    const view = render(App);
    await flush();
    expect(view.queryByRole('dialog', { name: 'first.png' })).toBeNull();

    const first = preview('first');
    openImageLightbox(first);
    await flush();
    const dialog = view.getByRole('dialog', { name: 'first.png' });
    expect(view.getByRole('img', { name: 'first.png' }).getAttribute('src')).toBe('blob:first');

    const second = preview('second');
    openImageLightbox(second);
    await flush();
    expect(first.dispose).toHaveBeenCalledTimes(1);
    expect(view.getByRole('dialog', { name: 'second.png' })).toBe(dialog);
    expect(view.getByRole('img', { name: 'second.png' }).getAttribute('src')).toBe('blob:second');

    await fireEvent.keyDown(dialog, { key: 'Escape' });
    await waitFor(() => expect(view.queryByRole('dialog', { name: 'second.png' })).toBeNull());
    expect(imageLightbox()).toBeNull();
    expect(second.dispose).toHaveBeenCalledTimes(1);
  });
});
