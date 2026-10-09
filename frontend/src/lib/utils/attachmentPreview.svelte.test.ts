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
  loadAttachmentPreview,
  type AttachmentPreviewCache,
  type ExpandedImagePreview,
  type ImagePreviewItem,
} from './attachmentPreview.svelte';
import { attachmentImageMenuTag } from './imageMenuActions';
import type { AttachmentPreviewSource } from './userMessageMeta';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { mockAttachmentDownload, TEST_PNG_BYTES } from '../../test/mocks/attachmentTransfer';

const ATTACHMENT: AttachmentPreviewSource = {
  id: 'att-1',
  threadId: 'thread-1',
  filename: 'image.png',
  mimeType: 'image/png',
  size: 10,
  kind: 'image',
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
      url: 'blob:foreign',
      width: 0,
      height: 0,
      originalBytes: 10,
      menuTag: attachmentImageMenuTag(ATTACHMENT),
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

const SECOND: AttachmentPreviewSource = {
  id: 'att-2',
  threadId: 'thread-1',
  filename: 'second.jpg',
  mimeType: 'image/jpeg',
  size: 2048,
  kind: 'image',
};

const FILE: AttachmentPreviewSource = {
  id: 'att-3',
  threadId: 'thread-1',
  filename: 'notes.pdf',
  mimeType: 'application/pdf',
  size: 99,
  kind: 'file',
};

function mountPreviews(attachments: AttachmentPreviewSource[]) {
  let previews!: ReturnType<typeof createAttachmentPreviews>;
  const cleanup = $effect.root(() => {
    previews = createAttachmentPreviews(() => attachments);
  });
  flushSync();
  return { previews, cleanup };
}

describe('loadExpandedPreview', () => {
  it('paints the thumbnails the opener holds, without fetching or allocating', async () => {
    const download = mockAttachmentDownload();
    const { previews, cleanup } = mountPreviews([ATTACHMENT, SECOND]);
    await settled();
    flushSync();
    const tiles = [previews.previewFor('att-1')!.url, previews.previewFor('att-2')!.url];
    expect(tiles.every((url) => url !== '')).toBe(true);

    const opened = previews.loadExpandedPreview('att-2');
    expect(opened).not.toBeInstanceOf(Promise);
    expect(opened!.index).toBe(1);
    expect(opened!.images.map((image) => image.url)).toEqual(tiles);
    expect(opened!.dispose).toBeUndefined();
    expect(download).not.toHaveBeenCalled();
    cleanup();
  });

  it('opens with an empty url for a thumbnail that has not loaded', () => {
    setBindingMock('GetAttachmentThumbnail', () => new Promise(() => {}));
    const { previews, cleanup } = mountPreviews([ATTACHMENT, SECOND]);
    expect(previews.previewFor('att-1')).toBeUndefined();

    const opened = previews.loadExpandedPreview('att-1');
    expect(opened!.index).toBe(0);
    expect(opened!.images.map((image) => image.url)).toEqual(['', '']);
    cleanup();
  });

  it('describes each image by its attachment row and tags it for the image menu', async () => {
    const { previews, cleanup } = mountPreviews([ATTACHMENT, SECOND]);
    await settled();
    flushSync();

    const opened: ExpandedImagePreview = previews.loadExpandedPreview('att-1')!;
    expect(opened.images.map(({ url: _url, original: _original, ...described }) => described)).toEqual([
      {
        id: 'att-1',
        filename: 'image.png',
        mimeType: 'image/png',
        width: 0,
        height: 0,
        originalBytes: 10,
        menuTag: attachmentImageMenuTag(ATTACHMENT),
      },
      {
        id: 'att-2',
        filename: 'second.jpg',
        mimeType: 'image/jpeg',
        width: 0,
        height: 0,
        originalBytes: 2048,
        menuTag: attachmentImageMenuTag(SECOND),
      },
    ]);
    cleanup();
  });

  it("resolves each image's original bytes on demand", async () => {
    const download = mockAttachmentDownload(
      () => new Blob([TEST_PNG_BYTES], { type: 'image/png' }),
    );
    const { previews, cleanup } = mountPreviews([ATTACHMENT, SECOND]);

    const opened = previews.loadExpandedPreview('att-1')!;
    const blob = await opened.images[1].original!(new AbortController().signal);
    expect(new Uint8Array(await blob.arrayBuffer())).toEqual(TEST_PNG_BYTES);
    expect(blob.type).toBe('image/png');
    expect(download).toHaveBeenCalledExactlyOnceWith('thread-1', 'att-2');
    cleanup();
  });

  it('lists the images only, and opens on nothing that is not one', () => {
    const { previews, cleanup } = mountPreviews([ATTACHMENT, FILE, SECOND]);

    const opened = previews.loadExpandedPreview('att-2')!;
    expect(opened.images.map((image) => image.id)).toEqual(['att-1', 'att-2']);
    expect(opened.index).toBe(1);
    expect(previews.loadExpandedPreview('att-3')).toBeNull();
    expect(previews.loadExpandedPreview('missing')).toBeNull();
    cleanup();
  });
});

describe('loadAttachmentPreview', () => {
  it('paints the thumbnail and leaves the original to an explicit fetch', async () => {
    const download = mockAttachmentDownload();
    const item = await loadAttachmentPreview(ATTACHMENT);
    expect(item).toMatchObject({
      id: 'att-1',
      filename: 'image.png',
      mimeType: 'image/png',
      width: 0,
      height: 0,
      originalBytes: 10,
      menuTag: attachmentImageMenuTag(ATTACHMENT),
    });
    expect(item.url).toMatch(/^(blob:|data:image\/png;base64,)/);
    expect(download).not.toHaveBeenCalled();

    await item.original!(new AbortController().signal);
    expect(download).toHaveBeenCalledExactlyOnceWith('thread-1', 'att-1');
    if (item.url.startsWith('blob:')) URL.revokeObjectURL(item.url);
  });
});
