// The cache-backed preview factory. The one contract worth pinning is
// blob-lifecycle authority: when a cache is provided, the cache owner
// (the pane) decides which blob URLs are alive, and the factory's local
// copy is only ever a mirror. A local handle the owner has disposed —
// the row-UI prune revokes the blob and drops the cache entry while the
// component is unmounted — must be reloaded, not served (the served URL
// would be a revoked blob: dead <img>, the "image.png placeholder"
// symptom from the 2026-08-22 agent-pane incident).
import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  createAttachmentPreviews,
  type AttachmentPreviewCache,
  type ImagePreviewItem,
} from './attachmentPreview.svelte';
import type { AttachmentPreviewSource } from './userMessageMeta';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const ATTACHMENT: AttachmentPreviewSource = {
  id: 'att-1',
  threadId: 'thread-1',
  filename: 'image.png',
  mimeType: 'image/png',
  size: 10,
};

function mapCache(): AttachmentPreviewCache & { store: Map<string, ImagePreviewItem> } {
  const store = new Map<string, ImagePreviewItem>();
  return {
    store,
    get: (id) => store.get(id),
    set: (id, preview) => {
      store.set(id, preview);
    },
  };
}

async function settled(): Promise<void> {
  // Two microtask hops: the binding promise, then the .then that writes
  // `previews` back.
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
}

let loads = 0;

beforeEach(() => {
  resetBindingMocks();
  loads = 0;
  setBindingMock('GetAttachmentThumbnail', async () => {
    loads += 1;
    return { data: 'iVBORw0KGgo=', mimeType: 'image/png' };
  });
});

afterEach(() => {
  resetBindingMocks();
});

describe('createAttachmentPreviews with a cache', () => {
  it('reloads when the cache owner disposed the seeded entry', async () => {
    const cache = mapCache();

    // First mount loads and writes through to the cache.
    const cleanupFirst = $effect.root(() => {
      createAttachmentPreviews(() => [ATTACHMENT], { cache });
    });
    flushSync();
    await settled();
    expect(loads).toBe(1);
    expect(cache.store.has('att-1')).toBe(true);
    cleanupFirst();

    // The owner disposes the blob between mounts (row-UI prune).
    cache.store.clear();

    // A remount that seeded from a stale snapshot must reload rather
    // than serve the dead handle.
    let previewUrl = '';
    const cleanupSecond = $effect.root(() => {
      const previews = createAttachmentPreviews(() => [ATTACHMENT], { cache });
      $effect(() => {
        previewUrl = previews.previewFor('att-1')?.url ?? '';
      });
    });
    flushSync();
    await settled();
    flushSync();
    expect(loads).toBe(2);
    expect(cache.store.has('att-1')).toBe(true);
    expect(previewUrl).toBe(cache.store.get('att-1')!.url);
    cleanupSecond();
  });

  it('adopts an entry another factory instance loaded after the seed', async () => {
    const cache = mapCache();
    const foreign: ImagePreviewItem = {
      id: 'att-1',
      filename: 'image.png',
      mimeType: 'image/png',
      size: 10,
      url: 'blob:foreign',
    };

    let previewUrl = '';
    const cleanup = $effect.root(() => {
      // Seeded empty; the cache gains an entry before the load effect's
      // recheck. The factory must adopt it, not fetch a duplicate blob.
      const previews = createAttachmentPreviews(() => [ATTACHMENT], { cache });
      cache.store.set('att-1', foreign);
      $effect(() => {
        previewUrl = previews.previewFor('att-1')?.url ?? '';
      });
    });
    flushSync();
    await settled();
    flushSync();
    expect(loads).toBe(0);
    expect(previewUrl).toBe('blob:foreign');
    cleanup();
  });
});
