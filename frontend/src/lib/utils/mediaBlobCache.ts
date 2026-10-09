// One fetch per key, shared by every mount that paints the same bytes.
//
// A body is rendered in more than one place at once (a PR description in
// its collapsed and expanded headers, a thread and the side chat forked
// from it, two panes on one PR), and each mount would otherwise spend its
// own RPC and its own copy of the bytes. So the promise is the cache entry:
// the second mount awaits the first one's, and a mount that arrives after
// the fetch settled reads `settled` and paints in the same frame. That is
// the bridge the scroll contract asks of async-short content
// (docs/architecture/frontend-scroll.md, "Async-short remount content"):
// a row that shrank to a loading placeholder and grew back by an image's
// height would clamp the viewport and glide it.
//
// Bytes, not entries, are the real bound: one 40 MiB screen recording
// outweighs two hundred avatars. The cache evicts on both, and an evicted
// entry's object URL is revoked, because a blob URL pins its data for as
// long as it lives. An entry a mounted host is still displaying is
// RETAINED and skipped by eviction: revoking underneath a playing <video>
// stops it mid-frame.
//
// A failure is not a memoized answer: the next mount, or the retry the
// user triggers by reopening the thread, fetches again.

export interface MediaBytes {
  /** An object URL, or a `data:` URL for SVG (see objectOrDataUrl). */
  url: string;
  mimeType: string;
  /**
   * The bytes `url` was made from. A copy reads them here: the page's CSP
   * (connect-src 'self') refuses a fetch of a blob: or data: URL. For an
   * object URL this is the same data the URL pins, not a second copy.
   */
  blob: Blob;
}

export interface ImageSize {
  /**
   * Pixel size an <img> renders at before its bytes decode (its `width`
   * and `height` attributes), so a remount reserves the box in the same
   * frame it mounts. From the file header when the backend could read it,
   * else what the first <img> to decode it measured (rememberDecodedSize);
   * 0 until either.
   */
  width: number;
  height: number;
}

export interface MediaHandle<V extends MediaBytes> {
  /**
   * The value once the fetch has settled, readable synchronously at acquire
   * so a remount paints without a loading frame; undefined while the fetch
   * is in flight.
   */
  readonly settled: V | undefined;
  value: Promise<V>;
  /** Releases this holder's retention; safe to call more than once. */
  release: () => void;
}

/** Total decoded bytes the cache may pin at once. */
export const MEDIA_CACHE_MAX_BYTES = 64 * 1024 * 1024;
/** Entries the cache may hold at once, whatever they weigh. */
export const MEDIA_CACHE_MAX_ENTRIES = 256;

let maxBytes = MEDIA_CACHE_MAX_BYTES;
let maxEntries = MEDIA_CACHE_MAX_ENTRIES;

interface CacheEntry {
  value: Promise<MediaBytes>;
  settled: MediaBytes | undefined;
  /** Object URL to revoke on eviction; '' for a data URL or before settle. */
  objectURL: string;
  bytes: number;
  retained: number;
}

const entries = new Map<string, CacheEntry>();
let cachedBytes = 0;

/**
 * The shared resolution for `key`, retained until `release()`. `load` runs
 * only for the first acquire of a key still in the cache.
 *
 * Every caller must release: a host in its effect cleanup, a download or
 * copy action in a `finally`. A retained entry is never evicted, so a
 * leaked retention is a leaked blob URL.
 */
export function acquireMediaBlob<V extends MediaBytes>(
  key: string,
  load: () => Promise<V>,
): MediaHandle<V> {
  let entry = entries.get(key);
  if (entry) {
    // Re-insert so the Map's iteration order stays least-recently-used first.
    entries.delete(key);
    entries.set(key, entry);
  } else {
    const created: CacheEntry = {
      value: undefined as unknown as Promise<MediaBytes>,
      settled: undefined,
      objectURL: '',
      bytes: 0,
      retained: 0,
    };
    created.value = load().then(
      (resolved) => {
        // The entry may already have been dropped by a reset between the
        // request and its reply; only account for one still in the map.
        if (entries.get(key) === created) {
          created.settled = resolved;
          created.objectURL = resolved.url.startsWith('blob:') ? resolved.url : '';
          // A data URL is a second, base64 copy beside the Blob.
          created.bytes = resolved.blob.size
            + (resolved.url.startsWith('data:') ? resolved.url.length : 0);
          cachedBytes += created.bytes;
          evict();
        } else if (resolved.url.startsWith('blob:')) {
          revoke(resolved.url);
        }
        return resolved;
      },
      (err: unknown) => {
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
    get settled() {
      return held.settled as V | undefined;
    },
    value: held.value as Promise<V>,
    release() {
      if (released) return;
      released = true;
      held.retained = Math.max(0, held.retained - 1);
    },
  };
}

/**
 * Records the size an <img> decoded for a value whose header the backend
 * could not read, so the next mount of the same bytes reserves the box.
 */
export function rememberDecodedSize(size: ImageSize, img: HTMLImageElement): void {
  if (size.width > 0 && size.height > 0) return;
  if (img.naturalWidth > 0 && img.naturalHeight > 0) {
    size.width = img.naturalWidth;
    size.height = img.naturalHeight;
  }
}

/**
 * Drop everything, revoking every object URL the cache still owns, and
 * restore the production limits.
 */
export function __resetMediaBlobCacheForTest(): void {
  for (const entry of entries.values()) {
    if (entry.objectURL) revoke(entry.objectURL);
    // Swallow a rejection nobody is awaiting any more.
    void entry.value.catch(() => {});
  }
  entries.clear();
  cachedBytes = 0;
  maxBytes = MEDIA_CACHE_MAX_BYTES;
  maxEntries = MEDIA_CACHE_MAX_ENTRIES;
}

/** Shrink the limits so eviction is reachable with small fixtures. */
export function __setMediaBlobCacheLimitsForTest(limits: { maxBytes?: number; maxEntries?: number }): void {
  maxBytes = limits.maxBytes ?? MEDIA_CACHE_MAX_BYTES;
  maxEntries = limits.maxEntries ?? MEDIA_CACHE_MAX_ENTRIES;
}

function evict(): void {
  for (const [key, entry] of entries) {
    if (cachedBytes <= maxBytes && entries.size <= maxEntries) return;
    if (entry.retained > 0) continue;
    entries.delete(key);
    cachedBytes -= entry.bytes;
    if (entry.objectURL) revoke(entry.objectURL);
  }
}

function revoke(url: string): void {
  if (typeof URL.revokeObjectURL === 'function') URL.revokeObjectURL(url);
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
export async function objectOrDataUrl(blob: Blob, mimeType: string): Promise<string> {
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
