// The box a Markdown image will paint in, known before its bytes arrive:
// the size its author gave it (render/imageSize.ts), its intrinsic pixel
// size (a repository image's fact, or the answer that mints its byte
// transfer), or both, combined as `MarkdownImage.svelte` sizes the `<img>`
// once it paints, so the bytes landing change no geometry. The image's own
// box is the original's pixel size whichever tier is painted.

import type { ImageSize as AuthoredSize } from '../markdown';
import type { ImageSize as PixelSize } from './mediaBlobCache';

export interface ReservedImageBox {
  width?: string;
  height?: string;
  aspectRatio?: string;
}

/** The box, or null when neither size makes it knowable. */
export function reservedImageBox(size: AuthoredSize | undefined, intrinsic: PixelSize | null | undefined): ReservedImageBox | null {
  const width = intrinsic?.width ?? 0;
  const height = intrinsic?.height ?? 0;
  if (width > 0 && height > 0) {
    // An authored height alone takes its width from the ratio, as the
    // painted `<img>` does (MarkdownImage's `derivedWidth`).
    const derived = size?.height && !size.width ? `${(Number.parseFloat(size.height) * width) / height}px` : undefined;
    return { width: size?.width ?? derived ?? `${width}px`, height: size?.height, aspectRatio: size?.aspectRatio ?? `${width} / ${height}` };
  }
  if (size?.width && (size.aspectRatio || size.height)) return { width: size.width, height: size.height, aspectRatio: size.aspectRatio };
  return null;
}
