// One fetch per (computer, PR, href), shared by every mount of that body
// through the media blob cache (mediaBlobCache.ts owns retention, the byte
// budget and revocation).
//
// A ticket is spent by the first request that presents it, and the backend
// caches the fetched bytes only briefly. A 404 on the GET therefore means
// "mint again", not "gone": one retry through a fresh RPC, and a second
// failure is the error the user sees.

import { FetchForgeAttachment } from '../stores/bindings';
import { withBackendTarget } from '../transport/backends';
import { backendCredentials, backendTransferUrl } from '../transport/homeEndpoint';
import { fetchPairedComputer } from '../transport/deviceSession';
import { networkFetch } from '../transport/networkFetch';
import type { BackendKey } from '../transport/backendKey';
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
  sizeBytes: number;
  filename: string;
}

export type ForgeAttachmentHandle = MediaHandle<ResolvedForgeAttachment>;

/** Cache key: one entry per computer, PR and raw href. */
export function forgeAttachmentCacheKey(
  backend: BackendKey,
  pr: PRRef,
  href: string,
): string {
  return JSON.stringify(['forge', backend, prKey(pr), href]);
}

/**
 * The shared resolution for one attachment, retained until `release()`.
 *
 * Every caller must release: a host in its effect cleanup, the download
 * action in a `finally`. A retained entry is never evicted, so a leaked
 * retention is a leaked blob URL.
 */
export function acquireForgeAttachment(
  backend: BackendKey,
  pr: PRRef,
  href: string,
): ForgeAttachmentHandle {
  return acquireMediaBlob(
    forgeAttachmentCacheKey(backend, pr, href),
    () => loadForgeAttachment(backend, pr, href),
  );
}

async function loadForgeAttachment(
  backend: BackendKey,
  pr: PRRef,
  href: string,
): Promise<ResolvedForgeAttachment> {
  const wire = prReferenceWire(pr);
  let meta = await withBackendTarget(backend, () => FetchForgeAttachment(wire, href));
  let blob = await fetchTicketedBytes(backend, meta.url);
  if (blob === null) {
    // Spent ticket, or the backend's byte cache evicted the entry. Mint once
    // more; a second 404 is a real failure and surfaces as one.
    meta = await withBackendTarget(backend, () => FetchForgeAttachment(wire, href));
    blob = await fetchTicketedBytes(backend, meta.url);
    if (blob === null) {
      throw new Error('This attachment transfer is no longer available. Try again.');
    }
  }
  const kind = normalizeKind(meta.kind);
  const mimeType = meta.mimeType || blob.type || 'application/octet-stream';
  // Typed by what the backend classified, so a consumer of `blob` reads the
  // same type `url` was made with.
  const typed = blob.type === mimeType ? blob : new Blob([blob], { type: mimeType });
  return {
    url: await objectOrDataUrl(typed, mimeType),
    mimeType,
    kind,
    sizeBytes: meta.sizeBytes || blob.size,
    filename: meta.filename,
    blob: typed,
    width: meta.width,
    height: meta.height,
  };
}

/**
 * The whole body, read ONCE. The ticket is spent by the first request, so
 * the URL can never be handed to `<img src>` or `<video src>` — a browser
 * issues range requests for media and the second one would 404.
 *
 * Null means 404 (spent or evicted), which the caller retries. Every other
 * refusal throws with the line the route answered.
 */
async function fetchTicketedBytes(backend: BackendKey, url: string): Promise<Blob | null> {
  const response = await fetchPairedComputer(
    backend,
    networkFetch,
    backendTransferUrl(url, backend),
    { credentials: backendCredentials(backend) },
  );
  if (response.status === 404) {
    // Read the body anyway so the connection can be reused.
    await response.text().catch(() => '');
    return null;
  }
  if (!response.ok) {
    let detail = '';
    try {
      detail = (await response.text()).trim();
    } catch {
      // A body we could not read tells us nothing the status did not.
    }
    throw new Error(
      detail && detail.length <= 200
        ? `Could not load attachment: ${detail}`
        : `Could not load attachment (${response.status}).`,
    );
  }
  return await response.blob();
}

function normalizeKind(kind: string): ForgeAttachmentKind {
  return kind === 'image' || kind === 'video' || kind === 'audio' ? kind : 'file';
}
