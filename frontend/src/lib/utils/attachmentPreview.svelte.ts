import { onDestroy, untrack } from 'svelte';
import { GetAttachmentData } from '../stores/bindings';

export interface AttachmentPreviewSource {
  id: string;
  threadId: string;
  filename: string;
  mimeType: string;
  size: number;
}

export interface ImagePreviewItem {
  id: string;
  filename: string;
  mimeType: string;
  size: number;
  url: string;
}

export interface ExpandedImagePreview {
  images: ImagePreviewItem[];
  index: number;
}

export interface UserMessageMeta {
  attachments?: AttachmentPreviewSource[];
}

export function parseUserMessageAttachments(meta: string | undefined): AttachmentPreviewSource[] {
  if (!meta) return [];
  try {
    const parsed = JSON.parse(meta) as UserMessageMeta;
    return Array.isArray(parsed.attachments) ? parsed.attachments : [];
  } catch {
    return [];
  }
}

export async function loadAttachmentPreview(attachment: AttachmentPreviewSource): Promise<ImagePreviewItem> {
  const data = await GetAttachmentData(attachment.threadId, attachment.id);
  return {
    id: attachment.id,
    filename: attachment.filename,
    mimeType: attachment.mimeType,
    size: attachment.size,
    url: imagePreviewUrl(attachment.mimeType, data),
  };
}

function imagePreviewUrl(mimeType: string, base64Data: string): string {
  if (typeof URL.createObjectURL !== 'function') {
    return `data:${mimeType};base64,${base64Data}`;
  }
  const binary = atob(base64Data);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return URL.createObjectURL(new Blob([bytes], { type: mimeType }));
}

function revokePreview(preview: ImagePreviewItem | undefined): void {
  if (!preview?.url.startsWith('blob:')) return;
  URL.revokeObjectURL(preview.url);
}

export function buildExpandedImagePreview(images: ImagePreviewItem[], selectedId: string): ExpandedImagePreview | null {
  const selectedIndex = images.findIndex((image) => image.id === selectedId);
  if (selectedIndex < 0) return null;
  return { images, index: selectedIndex };
}

/**
 * External cache for attachment previews. When provided, the factory:
 *   - seeds `previews` from `cache.get(id)` on first read,
 *   - writes loaded previews back via `cache.set(id, preview)`,
 *   - delegates blob-URL revocation to the cache owner (does NOT revoke
 *     on component destroy).
 * In chat surfaces, the pane owns the cache so blob URLs survive virtua
 * remount and only get revoked on thread switch or pane disposal.
 */
export interface AttachmentPreviewCache {
  get(attachmentId: string): ImagePreviewItem | undefined;
  set(attachmentId: string, preview: ImagePreviewItem): void;
}

interface AttachmentPreviewOptions {
  shouldLoad?: () => boolean;
  cache?: AttachmentPreviewCache;
}

export function createAttachmentPreviews(
  getAttachments: () => AttachmentPreviewSource[],
  options: AttachmentPreviewOptions = {},
) {
  // Seed local state from the cache so cache hits render synchronously
  // (no flash of placeholder on remount). Attachments not in the cache
  // are populated by the load $effect below.
  const initialSeed: Record<string, ImagePreviewItem> = {};
  if (options.cache) {
    for (const attachment of getAttachments()) {
      const cached = options.cache.get(attachment.id);
      if (cached) initialSeed[attachment.id] = cached;
    }
  }
  let previews = $state<Record<string, ImagePreviewItem>>(initialSeed);
  const loading = new Map<string, Promise<ImagePreviewItem | null>>();
  let loadGeneration = 0;

  function loadPreview(attachment: AttachmentPreviewSource, generation: number): Promise<ImagePreviewItem | null> {
    const existing = untrack(() => previews[attachment.id]);
    if (existing) return Promise.resolve(existing);
    // Re-check cache here in case another factory instance loaded the
    // same attachment after our initial seed.
    if (options.cache) {
      const cached = options.cache.get(attachment.id);
      if (cached) {
        previews = { ...previews, [attachment.id]: cached };
        return Promise.resolve(cached);
      }
    }
    const pending = loading.get(attachment.id);
    if (pending) return pending;

    const load = loadAttachmentPreview(attachment)
      .then((preview) => {
        if (generation !== loadGeneration) {
          // Stale request — only revoke if we own the blob. Cache-owned
          // blobs are managed by the cache provider's dispose path.
          if (!options.cache) revokePreview(preview);
          return null;
        }
        previews = { ...previews, [attachment.id]: preview };
        options.cache?.set(attachment.id, preview);
        return preview;
      })
      .catch((err) => {
        console.error('Failed to load attachment preview:', err);
        return null;
      })
      .finally(() => {
        loading.delete(attachment.id);
      });
    loading.set(attachment.id, load);
    return load;
  }

  $effect(() => {
    const attachments = getAttachments();
    const generation = ++loadGeneration;
    const nextIds = new Set(attachments.map((attachment) => attachment.id));
    const currentPreviews = untrack(() => previews);
    // Only revoke blobs we own. Cache-owned blobs survive attachment
    // turnover (the row's attachments shouldn't change in practice; this
    // is just defensive cleanup for the local-state path).
    if (!options.cache) {
      for (const [id, preview] of Object.entries(currentPreviews)) {
        if (!nextIds.has(id)) revokePreview(preview);
      }
    }
    const retainedPreviews = Object.fromEntries(
      Object.entries(currentPreviews).filter(([id]) => nextIds.has(id)),
    );
    if (Object.keys(retainedPreviews).length !== Object.keys(currentPreviews).length) {
      previews = retainedPreviews;
    }

    if (options.shouldLoad && !options.shouldLoad()) return;

    for (const attachment of attachments) {
      void loadPreview(attachment, generation);
    }
  });

  onDestroy(() => {
    // When a cache is provided the pane owns the blob lifecycle; do not
    // revoke here or remount-and-back would render broken thumbnails.
    if (options.cache) return;
    for (const preview of Object.values(previews)) {
      revokePreview(preview);
    }
  });

  return {
    previewFor(id: string): ImagePreviewItem | undefined {
      return previews[id];
    },

    expandedPreview(selectedId: string): ExpandedImagePreview | null {
      const orderedPreviews = getAttachments()
        .map((attachment) => previews[attachment.id])
        .filter((preview): preview is ImagePreviewItem => Boolean(preview));
      return buildExpandedImagePreview(orderedPreviews, selectedId);
    },

    async loadExpandedPreview(selectedId: string): Promise<ExpandedImagePreview | null> {
      const attachments = getAttachments();
      const generation = loadGeneration;
      await Promise.all(attachments.map((attachment) => loadPreview(attachment, generation)));
      const orderedPreviews = attachments
        .map((attachment) => previews[attachment.id])
        .filter((preview): preview is ImagePreviewItem => Boolean(preview));
      return buildExpandedImagePreview(orderedPreviews, selectedId);
    },
  };
}
