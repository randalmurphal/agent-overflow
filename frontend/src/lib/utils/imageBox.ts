// The box a Markdown image will paint in, known before its bytes arrive:
// the size its author gave it (render/imageSize.ts), its intrinsic pixel
// size (a repository image's fact, or the answer that mints its byte
// transfer), or both, combined as `MarkdownImage.svelte` sizes the `<img>`
// once it paints, so the bytes landing change no geometry. The image's own
// box is the original's pixel size whichever tier is painted.
//
// A fluid image has a ratio and no pixel size, and the browser sizes it
// from its container, differently in a line, a float, a table cell or a
// shrink-to-fit wrapper. Its box is no length this computes: it is a
// blank `<img>` of an svg with only the same ratio (`viewBox`), styled as
// the painted `<img>` is, which the browser lays out as it will the image
// in whatever context it sits.

import type { ImageSize as AuthoredSize } from '../markdown';
import type { ImageSize as PixelSize } from './mediaBlobCache';

export interface ReservedImageBox {
  width?: string;
  height?: string;
  aspectRatio?: string;
  /** A fluid image's ratio as a `viewBox` ("0 0 w h"): the box is an
   * `<img>` of an svg with only it, sized by `width`, `height` and
   * `aspectRatio` as the painted `<img>` is. */
  viewBox?: string;
}

/**
 * Whether an image with no pixel box (a fluid image, or one whose size
 * nothing answered) is laid out as a bare `<img>`: no authored width or
 * height sizes it, so a shrink-to-fit wrapper would size a ratio-only
 * image to nothing where a forge fills the line with it. Its wrapper then
 * carries `data-image-fill` (app.css), in `MarkdownImage` and in the
 * `MarkdownImageBox` that reserves it alike.
 */
export function fillsLine(size: AuthoredSize | undefined): boolean {
  return !size?.width && !size?.height;
}

/** The box, or null when neither size makes it knowable. */
export function reservedImageBox(size: AuthoredSize | undefined, intrinsic: PixelSize | null | undefined): ReservedImageBox | null {
  const width = intrinsic?.width ?? 0;
  const height = intrinsic?.height ?? 0;
  if (width > 0 && height > 0 && intrinsic?.fluid) {
    // As the painted `<img>`, which has no width or height attribute: an
    // authored size wins, and a height alone leaves the width to the ratio.
    return { width: size?.width ?? (size?.height ? 'auto' : undefined), height: size?.height, aspectRatio: size?.aspectRatio, viewBox: `0 0 ${width} ${height}` };
  }
  if (width > 0 && height > 0) {
    // An authored height alone takes its width from the ratio, as the
    // painted `<img>` does (MarkdownImage's `derivedWidth`).
    const derived = size?.height && !size.width ? `${(Number.parseFloat(size.height) * width) / height}px` : undefined;
    return { width: size?.width ?? derived ?? `${width}px`, height: size?.height, aspectRatio: size?.aspectRatio ?? `${width} / ${height}` };
  }
  if (size?.width && (size.aspectRatio || size.height)) return { width: size.width, height: size.height, aspectRatio: size.aspectRatio };
  return null;
}
