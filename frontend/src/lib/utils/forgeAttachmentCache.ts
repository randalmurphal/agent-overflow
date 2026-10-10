// One fetch per (computer, PR, href, tier), shared by every mount of that
// body through the media blob cache (mediaBlobCache.ts owns retention, the
// byte budget and revocation).
//
// The tier (utils/imageTiers.ts) is the display width a host asked for. The
// backend honors it only for an image; any other kind is served whole at
// every tier, which is why a save or a copy asks at the tier the page last
// used (`acquirePaintedForgeAttachment`) rather than a tier of its own.
//
// A ticket is spent by the first request that presents it, and the backend
// caches the fetched bytes only briefly. A 404 on the GET therefore means
// "mint again", not "gone": one retry through a fresh RPC, and a second
// failure is the error the user sees.

import { FetchForgeAttachment } from '../stores/bindings';
import { fetchTicketedBytes, TransferUnavailableError } from '../transport/attachmentTransfer';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { lastImageTier } from './imageTiers';
import {
  acquireMediaBlob,
  objectOrDataUrl,
  type ImageSize,
  type MediaBytes,
  type MediaHandle,
} from './mediaBlobCache';
import { prKey, prReferenceWire, type PRRef } from './prReference';

export type ForgeAttachmentKind = 'image' | 'video' | 'audio' | 'file';

export interface ResolvedForgeAttachment extends MediaBytes, ImageSize {
  kind: ForgeAttachmentKind;
  /** The original attachment's byte count, whichever bytes `blob` holds. */
  sizeBytes: number;
  filename: string;
  /** The original image's pixel size; 0 when unknown and for other kinds. */
  originalWidth: number;
  originalHeight: number;
  originalBytes: number;
  /** False when `blob` is the attachment's own bytes. */
  derived: boolean;
}

export type ForgeAttachmentHandle = MediaHandle<ResolvedForgeAttachment>;

/**
 * One attachment's identity: computer, PR and raw href. The tier is not
 * part of it, so the tier memo and the lightbox key on the attachment, not
 * on one of its sizes.
 */
export function forgeAttachmentCacheKey(
  backend: BackendKey,
  pr: PRRef,
  href: string,
): string {
  return JSON.stringify(['forge', backend, prKey(pr), href]);
}

/**
 * The shared resolution for one attachment at `tier` (a ladder width in
 * device pixels, or 0 for the original), retained until `release()`.
 *
 * Every caller must release: a host in its effect cleanup, the download
 * action in a `finally`. A retained entry is never evicted, so a leaked
 * retention is a leaked blob URL.
 */
export function acquireForgeAttachment(
  backend: BackendKey,
  pr: PRRef,
  href: string,
  tier: number,
): ForgeAttachmentHandle {
  return acquireMediaBlob(
    JSON.stringify(['forge', backend, prKey(pr), href, tier]),
    () => loadForgeAttachment(backend, pr, href, tier),
  );
}

/**
 * The resolution the page already holds or is fetching for this
 * attachment, at the tier it last asked for, or the original when nothing
 * on the page has asked. A file or video is the same bytes at every tier,
 * so a save reuses them instead of fetching them twice; an image may come
 * back as a derivative (`derived`), which a caller wanting the original
 * replaces through `fetchForgeAttachmentBytes`.
 */
export function acquirePaintedForgeAttachment(
  backend: BackendKey,
  pr: PRRef,
  href: string,
): ForgeAttachmentHandle {
  const tier = lastImageTier(forgeAttachmentCacheKey(backend, pr, href)) ?? 0;
  return acquireForgeAttachment(backend, pr, href, tier);
}

/**
 * The attachment's own bytes at `maxWidth` 0, else, for an image, the
 * widest variant at most `maxWidth` device pixels (what a full-size view
 * decodes on this device, `imageTiers.ts#fullSizeMaxWidth`), typed by what
 * the backend classified. Not cached: the caller (the lightbox, a copy, a
 * download) holds the Blob for as long as it needs it.
 */
export async function fetchForgeAttachmentBytes(
  backend: BackendKey,
  pr: PRRef,
  href: string,
  maxWidth: number,
  signal?: AbortSignal,
): Promise<Blob> {
  const { blob } = await mintAndFetch(backend, pr, href, maxWidth, signal);
  return blob;
}

async function loadForgeAttachment(
  backend: BackendKey,
  pr: PRRef,
  href: string,
  tier: number,
): Promise<ResolvedForgeAttachment> {
  const { meta, mimeType, blob } = await mintAndFetch(backend, pr, href, tier);
  return {
    url: await objectOrDataUrl(blob, mimeType),
    mimeType,
    kind: normalizeKind(meta.kind),
    sizeBytes: meta.sizeBytes || blob.size,
    filename: meta.filename,
    blob,
    width: meta.width,
    height: meta.height,
    originalWidth: meta.originalWidth,
    originalHeight: meta.originalHeight,
    originalBytes: meta.sizeBytes || blob.size,
    derived: meta.derived,
  };
}

/**
 * The body, read ONCE. The ticket is spent by the first request, so the URL
 * can never be handed to `<img src>` or `<video src>`: a browser issues
 * range requests for media and the second one would 404.
 */
async function mintAndFetch(
  backend: BackendKey,
  pr: PRRef,
  href: string,
  maxWidth: number,
  signal?: AbortSignal,
) {
  const wire = prReferenceWire(pr);
  const mint = () => withBackendTarget(backend, () => FetchForgeAttachment(wire, href, maxWidth));
  let meta = await mint();
  let bytes: Blob;
  try {
    bytes = await fetchTicketedBytes(backend, meta.url, signal);
  } catch (err) {
    if (!(err instanceof TransferUnavailableError)) throw err;
    // Spent ticket, or the backend's byte cache evicted the entry. Mint once
    // more; a second 404 is a real failure and surfaces as one.
    signal?.throwIfAborted();
    meta = await mint();
    try {
      bytes = await fetchTicketedBytes(backend, meta.url, signal);
    } catch (again) {
      // The shared message says "image"; a forge attachment may be a video
      // or a file.
      if (again instanceof TransferUnavailableError) {
        throw new TransferUnavailableError('This attachment transfer is no longer available. Try again.');
      }
      throw again;
    }
  }
  const mimeType = meta.mimeType || bytes.type || 'application/octet-stream';
  // Typed by what the backend classified, so a consumer of `blob` reads the
  // same type `url` was made with.
  const blob = bytes.type === mimeType ? bytes : new Blob([bytes], { type: mimeType });
  return { meta, mimeType, blob };
}

function normalizeKind(kind: string): ForgeAttachmentKind {
  return kind === 'image' || kind === 'video' || kind === 'audio' ? kind : 'file';
}
