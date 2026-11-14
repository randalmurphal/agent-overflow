// Keeps the bundled sprite strips loaded between working turns, so a turn's
// sprite paints on its first frame instead of leaving the slot empty.
//
// Every turn mounts a fresh strip whose CSS background-image names the
// strip URL. The backend serves /assets/* to the embedded webview as
// no-store (internal/transport/server.go withAssetHeaders), so Chromium
// reuses a strip only while some live reference holds its image
// resource; once the previous turn's strip is collected, the next mount
// refetches it from the backend and paints nothing until the response
// lands, which under load is seconds. A retained HTMLImageElement is that
// live reference. It holds the encoded bytes (about 1 MB for every
// built-in); decoded bitmaps stay under Chromium's own decode cache.
//
// Custom sprites are data URLs, which load without a server round trip,
// so retaining them would only duplicate bytes the spinner store already
// holds.

import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import type { SpinnerSprite } from './catalog';

const retained = new Map<string, HTMLImageElement>();

/**
 * Retain exactly the given sprites' strips: load the new ones, drop the
 * rest. An empty list releases everything. Idempotent for an unchanged
 * set.
 */
export function retainSpriteImages(sprites: readonly SpinnerSprite[]): void {
  const wanted = new Set<string>();
  for (const sprite of sprites) if (!sprite.custom) wanted.add(sprite.src);
  for (const [src, image] of retained) {
    if (wanted.has(src)) continue;
    image.onerror = null;
    retained.delete(src);
  }
  for (const src of wanted) {
    if (retained.has(src)) continue;
    const image = new Image();
    image.onerror = () => {
      // Forget the failed load so the next retain call retries it. The
      // turn's own mount still fetches the strip as it did before.
      if (retained.get(src) === image) retained.delete(src);
      reportFrontendDiagnostic('working sprite strip failed to load', src);
    };
    image.src = src;
    retained.set(src, image);
  }
}

/** Test seam: the strip URLs currently retained. */
export function __retainedSpriteSrcsForTest(): string[] {
  return [...retained.keys()];
}
