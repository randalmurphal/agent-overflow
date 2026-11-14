import type { ReviewFile } from './patchStore';

// The review diff's media row: a binary file's images (or its sizes), or a
// text SVG shown rendered. Its height is reserved from the image area's
// bounded size before any byte arrives, so a load never moves the rows
// below it; only a file that turns out not to be shown as an image (not an
// image, over the cap's note, a failed read) settles once to the shorter
// binary row.

/** Images the webview renders (an SVG only ever through an <img>). */
const IMAGE_EXTENSIONS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'ico', 'svg']);

export function isImagePath(path: string): boolean {
  const dot = path.lastIndexOf('.');
  if (dot < 0 || dot < path.lastIndexOf('/')) return false;
  return IMAGE_EXTENSIONS.has(path.slice(dot + 1).toLowerCase());
}

export function isSvgPath(path: string): boolean {
  return /\.svg$/i.test(path);
}

/** The image area's height: an image scales down to fit it, never up. */
export const REVIEW_IMAGE_AREA_PX = 200;
export const REVIEW_IMAGE_CAPTION_PX = 20;
const PAD_PX = 12;
const STACK_GAP_PX = 12;
/** The binary row's one line. */
export const REVIEW_BINARY_ROW_PX = 36;

/** The sides an image row shows: one for an add or a delete (or an edit,
 * which records no old side), both for a change. */
export function imageSides(file: Pick<ReviewFile, 'kind'>, newSideOnly: boolean): ('old' | 'new')[] {
  if (file.kind === 'deleted') return newSideOnly ? [] : ['old'];
  if (file.kind === 'added' || newSideOnly) return ['new'];
  return ['old', 'new'];
}

/** The reserved height of an image row: split view puts two sides in two
 * columns, unified stacks them. */
export function imageRowHeight(sides: number, split: boolean): number {
  const stacked = split ? 1 : Math.max(1, sides);
  return PAD_PX * 2 + stacked * (REVIEW_IMAGE_AREA_PX + REVIEW_IMAGE_CAPTION_PX) + (stacked - 1) * STACK_GAP_PX;
}

/** Bytes for a caption: `812 B`, `4.2 KB`, `1.5 MB`. */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB'];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value >= 10 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`;
}
