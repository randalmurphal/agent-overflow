// What `MarkdownImage.svelte` paints from: one image, offered at any display
// tier, plus its original for the lightbox and the menu tag that names it.
// The two hosts build one each: a local image an agent wrote
// (`StreamdownImageHost`) and a forge image in a PR body
// (`ForgeAttachmentHost`).

import type { BackendKey } from '../transport/backendKey';
import {
  acquireForgeAttachment,
  fetchForgeAttachmentOriginal,
  forgeAttachmentCacheKey,
} from './forgeAttachmentCache';
import { forgeAttachmentName, type ParsedForgeAttachmentHref } from './forgeAttachments';
import { acquireLocalImage, fetchLocalImageOriginal, localImageCacheKey } from './localImageCache';
import type { ImageSize, MediaBytes, MediaHandle } from './mediaBlobCache';
import { pathBasename } from './pathDisplay';

export interface MarkdownImageVariant extends MediaBytes, ImageSize {
  originalWidth: number;
  originalHeight: number;
  originalBytes: number;
  derived: boolean;
}

export interface MarkdownImageSource {
  /** Cache identity without the tier; the tier memo and lightbox id key on it. */
  key: string;
  filename: string;
  acquire(tier: number): MediaHandle<MarkdownImageVariant>;
  original(signal: AbortSignal): Promise<Blob>;
  menuTag: Record<string, string>;
}

/** A local image on `backend`, the thread's computer. */
export function localMarkdownImageSource(
  backend: BackendKey,
  image: { path: string; workspacePath: string },
  menuTag: Record<string, string>,
): MarkdownImageSource {
  const { path, workspacePath } = image;
  return {
    key: localImageCacheKey(backend, path, workspacePath),
    filename: pathBasename(path) || path,
    acquire: (tier) => acquireLocalImage(backend, path, workspacePath, tier),
    original: (signal) => fetchLocalImageOriginal(backend, path, workspacePath, signal),
    menuTag,
  };
}

/** A forge image, resolved on the computer that owns the PR. */
export function forgeMarkdownImageSource(
  attachment: ParsedForgeAttachmentHref,
  menuTag: Record<string, string>,
): MarkdownImageSource {
  const { backend, pr, href } = attachment;
  return {
    key: forgeAttachmentCacheKey(backend, pr, href),
    filename: forgeAttachmentName(pr.forge, href) || 'attachment',
    acquire: (tier) => acquireForgeAttachment(backend, pr, href, tier),
    original: (signal) => fetchForgeAttachmentOriginal(backend, pr, href, signal),
    menuTag,
  };
}
