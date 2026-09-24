// One fetch per (computer, PR, href), shared by every mount of that body.
//
// A PR description is rendered in at least two places at once — the
// collapsed header and the expanded one, a conversation thread and its
// comments list, two panes on the same PR — and each mount would otherwise
// spend its own RPC, its own ticket and its own copy of the bytes. So the
// promise is the cache entry: the second mount awaits the first one's.
//
// Bytes, not entries, are the real bound: one 40 MiB screen recording
// outweighs two hundred avatars. The cache evicts on both, and an evicted
// entry's object URL is revoked, because a blob URL pins its data for as
// long as it lives. An entry a mounted host is still displaying is
// RETAINED and skipped by eviction: revoking underneath a playing <video>
// stops it mid-frame.
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
import { prKey, prReferenceWire, type PRRef } from './prReference';

export type ForgeAttachmentKind = 'image' | 'video' | 'audio' | 'file';

export interface ResolvedForgeAttachment {
  /** An object URL, or a `data:` URL for SVG (see below). */
  url: string;
  mimeType: string;
  kind: ForgeAttachmentKind;
  sizeBytes: number;
  filename: string;
  /**
   * The bytes `url` was made from. A copy reads them here: the page's CSP
   * (connect-src 'self') refuses a fetch of a blob: or data: URL. For an
   * object URL this is the same data the URL pins, not a second copy.
   */
  blob: Blob;
}

export interface ForgeAttachmentHandle {
  value: Promise<ResolvedForgeAttachment>;
  /** Releases this holder's retention; safe to call more than once. */
  release: () => void;
}

/** Total decoded bytes the cache may pin at once. */
const MAX_CACHED_BYTES = 64 * 1024 * 1024;
/** Entries the cache may hold at once, whatever they weigh. */
const MAX_CACHED_ENTRIES = 256;

interface CacheEntry {
  value: Promise<ResolvedForgeAttachment>;
  /** Object URL to revoke on eviction; '' for a data URL or before settle. */
  objectURL: string;
  bytes: number;
  retained: number;
}

const entries = new Map<string, CacheEntry>();
let cachedBytes = 0;

/** Cache key: one entry per computer, PR and raw href. */
export function forgeAttachmentCacheKey(
  backend: BackendKey,
  pr: PRRef,
  href: string,
): string {
  return `${backend} ${prKey(pr)} ${href}`;
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
  const key = forgeAttachmentCacheKey(backend, pr, href);
  let entry = entries.get(key);
  if (entry) {
    // Re-insert so the Map's iteration order stays least-recently-used first.
    entries.delete(key);
    entries.set(key, entry);
  } else {
    const created: CacheEntry = {
      value: undefined as unknown as Promise<ResolvedForgeAttachment>,
      objectURL: '',
      bytes: 0,
      retained: 0,
    };
    created.value = loadForgeAttachment(backend, pr, href).then(
      (resolved) => {
        // The entry may already have been dropped by a reset between the
        // request and its reply; only account for one still in the map.
        if (entries.get(key) === created) {
          created.objectURL = resolved.url.startsWith('blob:') ? resolved.url : '';
          // A data URL is a second, base64 copy beside the Blob.
          created.bytes = Math.max(0, resolved.sizeBytes)
            + (resolved.url.startsWith('data:') ? resolved.url.length : 0);
          cachedBytes += created.bytes;
          evict();
        } else if (resolved.url.startsWith('blob:')) {
          revoke(resolved.url);
        }
        return resolved;
      },
      (err: unknown) => {
        // A failure is not a memoized answer: the next mount, or the retry
        // the user triggers by reopening the thread, must fetch again.
        if (entries.get(key) === created) entries.delete(key);
        throw err;
      },
    );
    entries.set(key, created);
    entry = created;
  }
  const held = entry;
  held.retained += 1;
  let released = false;
  return {
    value: held.value,
    release() {
      if (released) return;
      released = true;
      held.retained = Math.max(0, held.retained - 1);
    },
  };
}

/** Drop everything, revoking every object URL the cache still owns. */
export function __resetForgeAttachmentCacheForTest(): void {
  for (const entry of entries.values()) {
    if (entry.objectURL) revoke(entry.objectURL);
    // Swallow a rejection nobody is awaiting any more.
    void entry.value.catch(() => {});
  }
  entries.clear();
  cachedBytes = 0;
}

function evict(): void {
  for (const [key, entry] of entries) {
    if (cachedBytes <= MAX_CACHED_BYTES && entries.size <= MAX_CACHED_ENTRIES) return;
    if (entry.retained > 0) continue;
    entries.delete(key);
    cachedBytes -= entry.bytes;
    if (entry.objectURL) revoke(entry.objectURL);
  }
}

function revoke(url: string): void {
  if (typeof URL.revokeObjectURL === 'function') URL.revokeObjectURL(url);
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

/**
 * SVG becomes a `data:` URL; everything else an object URL.
 *
 * A blob URL inherits the APP's origin, so an SVG navigated to (a
 * middle-click, a "view image") would execute its script against this page.
 * A data URL is an opaque origin, which is the whole difference. Raster
 * images, video and audio have no script surface and keep the object URL,
 * which is what lets a 40 MiB recording stream instead of being inlined as
 * a base64 string a third larger than the file.
 */
async function objectOrDataUrl(blob: Blob, mimeType: string): Promise<string> {
  const svg = /^image\/svg\+xml\b/i.test(mimeType);
  if (!svg && typeof URL.createObjectURL === 'function') {
    return URL.createObjectURL(blob);
  }
  const bytes = new Uint8Array(await blob.arrayBuffer());
  return `data:${mimeType};base64,${bytesToBase64(bytes)}`;
}

function bytesToBase64(bytes: Uint8Array): string {
  // Chunked so a large buffer cannot blow the argument limit of `apply`.
  let binary = '';
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}
