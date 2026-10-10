// One fetch per (computer, workspace, path, tier) for the `![x](/abs/path.png)`
// an agent writes, shared by every mount of that markdown: the thread and
// the side chat forked from it, and a remount of the row after a scroll
// away and back (mediaBlobCache.ts owns retention, the byte budget and
// revocation).
//
// The bytes come from the thread's computer: GetLocalImage, pinned by the
// caller, validates the file and mints a single-use ticket, and the bytes
// cross on the ticketed byte route rather than inside an RPC frame. A path
// names a different file on every machine, so the computer is part of the
// key and the route is never left to the focused pane.
//
// The tier (utils/imageTiers.ts) is the display width the timeline asked
// for, so each tier is its own entry. The original, which the lightbox,
// copy and save want, is fetched by `fetchLocalImageBytes` and never
// cached here: the timeline's budget is for what is on screen.

import { GetLocalImage } from '../stores/bindings';
import { fetchTicketedBytes, TransferUnavailableError } from '../transport/attachmentTransfer';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import {
  acquireMediaBlob,
  objectOrDataUrl,
  type ImageSize,
  type MediaBytes,
  type MediaHandle,
} from './mediaBlobCache';

export interface LocalImage extends MediaBytes, ImageSize {
  /** The file's pixel size; 0 when the backend cannot read its header. */
  originalWidth: number;
  originalHeight: number;
  originalBytes: number;
  /** False when the bytes are the file itself. */
  derived: boolean;
}

export type LocalImageHandle = MediaHandle<LocalImage>;

/**
 * One local image's identity: computer, workspace and path. The tier is not
 * part of it, so the tier memo and the lightbox key on the image, not on
 * one of its sizes.
 */
export function localImageCacheKey(
  backend: BackendKey,
  path: string,
  workspacePath: string,
): string {
  return JSON.stringify(['local', backend, workspacePath, path]);
}

/**
 * The shared resolution for one local image at `tier` (a ladder width in
 * device pixels, or 0 for the file itself), retained until `release()`.
 * A host releases in its effect cleanup; a retained entry is never
 * evicted, so a leaked retention is a leaked blob URL.
 */
export function acquireLocalImage(
  backend: BackendKey,
  path: string,
  workspacePath: string,
  tier: number,
): LocalImageHandle {
  return acquireMediaBlob(
    JSON.stringify(['local', backend, workspacePath, path, tier]),
    () => loadLocalImage(backend, path, workspacePath, tier),
  );
}

/**
 * The file's own bytes at `maxWidth` 0, else the widest variant at most
 * `maxWidth` device pixels (what a full-size view decodes on this device,
 * `imageTiers.ts#fullSizeMaxWidth`), typed by what GetLocalImage
 * classified. Not cached: the caller (the lightbox, a copy, a download)
 * holds the Blob for as long as it needs it.
 */
export async function fetchLocalImageBytes(
  backend: BackendKey,
  path: string,
  workspacePath: string,
  maxWidth: number,
  signal?: AbortSignal,
): Promise<Blob> {
  const { blob } = await mintAndFetch(backend, path, workspacePath, maxWidth, signal);
  return blob;
}

async function loadLocalImage(
  backend: BackendKey,
  path: string,
  workspacePath: string,
  tier: number,
): Promise<LocalImage> {
  const { result, blob } = await mintAndFetch(backend, path, workspacePath, tier);
  return {
    url: await objectOrDataUrl(blob, result.mimeType),
    mimeType: result.mimeType,
    blob,
    width: result.width,
    height: result.height,
    originalWidth: result.originalWidth,
    originalHeight: result.originalHeight,
    originalBytes: result.originalBytes,
    derived: result.derived,
  };
}

// A ticket is spent by the first request that presents it and expires
// soon after it is minted, so a 404 means "mint again", not "gone": one
// retry through a fresh RPC, and a second 404 is the error the user sees.
async function mintAndFetch(
  backend: BackendKey,
  path: string,
  workspacePath: string,
  maxWidth: number,
  signal?: AbortSignal,
) {
  const mint = () => withBackendTarget(backend, () => GetLocalImage(path, workspacePath, maxWidth));
  let result = await mint();
  let bytes: Blob;
  try {
    bytes = await fetchTicketedBytes(backend, result.url, signal);
  } catch (err) {
    if (!(err instanceof TransferUnavailableError)) throw err;
    signal?.throwIfAborted();
    result = await mint();
    bytes = await fetchTicketedBytes(backend, result.url, signal);
  }
  // The RPC's classification is the type; the route's body may arrive
  // untyped.
  return { result, blob: new Blob([bytes], { type: result.mimeType }) };
}

const FAILURE_PREFIX = 'load local image: ';
const MAX_REASON_LENGTH = 48;

/**
 * The short phrase a failure chip shows beside the image's alt text: the
 * reason GetLocalImage names between its prefix and the cause
 * (`load local image: file not found: /path`), or the first clause of any
 * other message (a transport refusal, a scope error), bounded so the chip
 * stays a chip. The whole message belongs in the chip's tooltip.
 */
export function localImageFailureReason(message: string): string {
  const rest = message.startsWith(FAILURE_PREFIX) ? message.slice(FAILURE_PREFIX.length) : message;
  const clause = rest.indexOf(': ');
  const reason = (clause > 0 ? rest.slice(0, clause) : rest).trim();
  if (reason.length <= MAX_REASON_LENGTH) return reason;
  return `${reason.slice(0, MAX_REASON_LENGTH - 1).trimEnd()}…`;
}
