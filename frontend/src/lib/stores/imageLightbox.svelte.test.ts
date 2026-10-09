import { flushSync } from 'svelte';
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest';

import type { ExpandedImagePreview } from '../utils/attachmentPreview.svelte';
import { closeImageLightbox, imageLightbox, openImageLightbox } from './imageLightbox.svelte';

function preview(id: string): ExpandedImagePreview & { dispose: Mock<() => void> } {
  return {
    images: [{
      id,
      filename: `${id}.png`,
      mimeType: 'image/png',
      url: `blob:${id}`,
      width: 0,
      height: 0,
      originalBytes: 10,
      menuTag: {},
    }],
    index: 0,
    dispose: vi.fn(),
  };
}

afterEach(() => {
  closeImageLightbox();
});

describe('imageLightbox', () => {
  it('opens a preview and reports it reactively', () => {
    const seen: (ExpandedImagePreview | null)[] = [];
    const stop = $effect.root(() => {
      $effect(() => {
        seen.push(imageLightbox());
      });
    });
    flushSync();
    const first = preview('a');
    openImageLightbox(first);
    flushSync();
    closeImageLightbox();
    flushSync();
    stop();
    expect(seen).toEqual([null, first, null]);
  });

  it('disposes a still-open preview when another opens over it', () => {
    const first = preview('a');
    const second = preview('b');
    openImageLightbox(first);
    openImageLightbox(second);
    expect(first.dispose).toHaveBeenCalledTimes(1);
    expect(second.dispose).not.toHaveBeenCalled();
    expect(imageLightbox()).toBe(second);
  });

  it('does not dispose the preview it keeps when the same one opens again', () => {
    const first = preview('a');
    openImageLightbox(first);
    openImageLightbox(first);
    expect(first.dispose).not.toHaveBeenCalled();
    expect(imageLightbox()).toBe(first);
  });

  it('disposes on close, and a second close disposes nothing', () => {
    const first = preview('a');
    openImageLightbox(first);
    closeImageLightbox();
    expect(first.dispose).toHaveBeenCalledTimes(1);
    expect(imageLightbox()).toBeNull();
    closeImageLightbox();
    expect(first.dispose).toHaveBeenCalledTimes(1);
    expect(imageLightbox()).toBeNull();
  });

  it('opens again after a close', () => {
    const first = preview('a');
    const second = preview('b');
    openImageLightbox(first);
    closeImageLightbox();
    openImageLightbox(second);
    expect(imageLightbox()).toBe(second);
    expect(first.dispose).toHaveBeenCalledTimes(1);
    expect(second.dispose).not.toHaveBeenCalled();
  });

  it('accepts a preview without dispose', () => {
    const { dispose: _dispose, ...bare } = preview('a');
    openImageLightbox(bare);
    openImageLightbox(preview('b'));
    closeImageLightbox();
    expect(imageLightbox()).toBeNull();
  });

  it('leaves the next state in place when a dispose throws', () => {
    const first = preview('a');
    first.dispose.mockImplementation(() => {
      throw new Error('release failed');
    });
    const second = preview('b');
    openImageLightbox(first);
    expect(() => openImageLightbox(second)).toThrow('release failed');
    expect(imageLightbox()).toBe(second);
    closeImageLightbox();
    expect(second.dispose).toHaveBeenCalledTimes(1);
    expect(imageLightbox()).toBeNull();
  });
});
