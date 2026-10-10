import { untrack } from 'svelte';
import { GetAttachmentThumbnail } from '../stores/bindings';
import { fetchAttachmentBytes } from '../transport/attachmentTransfer';
import {
  parseUserMessageAttachments,
  type AttachmentPreviewSource,
} from './userMessageMeta';
import { base64ToBytes } from './base64';
import { attachmentImageMenuTag } from './imageMenuActions';
import { imageAttachments } from '../types/attachment';

export {
  parseUserMessageAttachments,
  type AttachmentPreviewSource,
};

export interface ImagePreviewItem {
  id: string;
  filename: string;
  mimeType: string;
  /** What the lightbox paints first: the thumbnail or the timeline's derivative. '' when nothing is loaded yet. */
  url: string;
  /** Original pixel size when known, else 0. */
  width: number;
  height: number;
  /** The original's byte count, for the loading line; 0 when unknown. */
  originalBytes: number;
  /**
   * Fetches the bytes for the full-size view: the original at `maxWidth`
   * 0, else the widest variant at most `maxWidth` device pixels
   * (`utils/imageTiers.ts#fullSizeMaxWidth`). Absent when `url` already is
   * the original. A thread attachment ignores the width: its bytes are the
   * composer's compressed upload, bounded already.
   */
  original?: (signal: AbortSignal, maxWidth: number) => Promise<Blob>;
  /** Attributes the image menu reads for this image (`utils/imageMenuActions.ts`). */
  menuTag: Record<string, string>;
}

export interface ExpandedImagePreview {
  images: ImagePreviewItem[];
  index: number;
  /**
   * Releases what the opener allocated for this lightbox lifetime. The
   * lightbox store (`stores/imageLightbox.svelte.ts`) calls it once, when
   * the preview is replaced or closed.
   */
  dispose?: () => void;
}

/**
 * Loads the small inline-grid thumbnail for an attachment. Hits the Go
 * `GetAttachmentThumbnail` binding, which generates the thumb on first
 * request and caches the bytes on the attachments row in SQLite. The
 * returned `ImagePreviewItem.url` is a blob: URL of the thumbnail bytes,
 * NOT the full image; `original` fetches that.
 *
 * Still an RPC (base64 in a WS frame) while the original moved to HTTP.
 * A thumb is ~10-30 KB, which is not the large body the move was about,
 * and a grid rendering a dozen tiles would otherwise pay a ticket round
 * trip for each one.
 */
export async function loadAttachmentPreview(attachment: AttachmentPreviewSource): Promise<ImagePreviewItem> {
  const result = await GetAttachmentThumbnail(attachment.threadId, attachment.id);
  return attachmentImageItem(
    attachment,
    imagePreviewUrl(result.mimeType, result.data),
    result.mimeType,
    result.width,
    result.height,
  );
}

/**
 * One attachment image, painted from `url` until its original is fetched.
 * `width` and `height` are the original's, as GetAttachmentThumbnail reads
 * them from its header, so the lightbox opens at the box the original fills.
 */
function attachmentImageItem(
  attachment: AttachmentPreviewSource,
  url: string,
  mimeType: string,
  width: number,
  height: number,
): ImagePreviewItem {
  return {
    id: attachment.id,
    filename: attachment.filename,
    mimeType,
    url,
    width,
    height,
    originalBytes: attachment.size,
    // Over HTTP, admitted by a single-use ticket, so opening a 10 MiB
    // screenshot does not stall the live event stream behind it.
    original: (signal) => fetchAttachmentBytes(attachment.threadId, attachment.id, signal),
    menuTag: attachmentImageMenuTag(attachment),
  };
}

function imagePreviewUrl(mimeType: string, base64Data: string): string {
  if (typeof URL.createObjectURL !== 'function') {
    return `data:${mimeType};base64,${base64Data}`;
  }
  return URL.createObjectURL(new Blob([base64ToBytes(base64Data)], { type: mimeType }));
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
 * In chat surfaces, the pane owns the cache so blob URLs survive
 * windowing remount and only get revoked on thread switch or pane disposal.
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
    for (const attachment of imageAttachments(getAttachments())) {
      const cached = options.cache.get(attachment.id);
      if (cached) initialSeed[attachment.id] = cached;
    }
  }
  let previews = $state<Record<string, ImagePreviewItem>>(initialSeed);
  const loading = new Map<string, Promise<ImagePreviewItem | null>>();
  let loadGeneration = 0;

  function loadPreview(attachment: AttachmentPreviewSource, generation: number): Promise<ImagePreviewItem | null> {
    const existing = untrack(() => previews[attachment.id]);
    if (options.cache) {
      // The cache owner is the blob-lifecycle authority. Re-check it even
      // when we hold a local copy: another factory instance may have
      // loaded the attachment after our seed (adopt its entry), and the
      // owner may have DISPOSED ours since (a row-UI prune revokes the
      // blob and drops the cache entry, leaving `existing` a dead
      // handle — fall through and reload instead of returning it).
      const cached = options.cache.get(attachment.id);
      if (cached) {
        // Compare by blob URL, not object identity: `existing` comes back
        // through the $state proxy, so it is never `===` the raw cache
        // entry even when it mirrors it.
        if (existing?.url !== cached.url) {
          previews = { ...previews, [attachment.id]: cached };
        }
        return Promise.resolve(cached);
      }
    } else if (existing) {
      return Promise.resolve(existing);
    }
    const pending = loading.get(attachment.id);
    if (pending) return pending;

    const load = loadAttachmentPreview(attachment)
      .then((preview) => {
        if (generation !== loadGeneration) {
          // Stale request — always revoke. Even when a cache is provided
          // the blob never made it into the cache, so the cache owner
          // can't dispose it; not revoking here orphans the blob URL.
          revokePreview(preview);
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
    // Files never get bytes: `GetAttachmentThumbnail` errors for one, so a
    // request here would be a guaranteed console error per file per mount.
    const attachments = imageAttachments(getAttachments());
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

  // Teardown-only effect (reads nothing, so it never re-runs): when a
  // cache is provided the pane owns the blob lifecycle; do not revoke
  // here or remount-and-back would render broken thumbnails. An effect
  // rather than onDestroy so the factory works under any effect root,
  // not only component init.
  $effect(() => {
    return () => {
      if (options.cache) return;
      for (const preview of Object.values(untrack(() => previews))) {
        revokePreview(preview);
      }
    };
  });

  return {
    previewFor(id: string): ImagePreviewItem | undefined {
      return previews[id];
    },

    /**
     * The lightbox preview for the message's images, opened at `selectedId`,
     * or null when that id is not one of them. Each item paints the
     * thumbnail this factory already holds (`''` for one still loading)
     * and fetches its original through `original`. Nothing is awaited and
     * nothing is allocated, so there is no `dispose`: the thumbnails stay
     * owned by this factory or its cache, and the lightbox owns the
     * original URLs it creates.
     */
    loadExpandedPreview(selectedId: string): ExpandedImagePreview | null {
      // Siblings in the lightbox are the message's IMAGES; a file is not
      // one, and has no expand affordance to reach this from.
      const images = imageAttachments(getAttachments()).map((attachment) => {
        const loaded = previews[attachment.id];
        return attachmentImageItem(attachment, loaded?.url ?? '', attachment.mimeType, loaded?.width ?? 0, loaded?.height ?? 0);
      });
      return buildExpandedImagePreview(images, selectedId);
    },
  };
}
