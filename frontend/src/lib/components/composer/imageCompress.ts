// Client-side recompression for oversized image attachments.
//
// Pasted screenshots from HiDPI displays routinely land as 10–30 MB PNGs;
// hard-rejecting them ("limit is 10 MB") makes the user open an editor to
// do what the composer can do itself. When a file is over the upload
// limit, `compressImageToFit` re-encodes it — dimension-capped, walking a
// quality ladder and then a scale ladder — and the upload proceeds with
// the smaller file. Files already under the limit are never touched, so
// in-budget images keep their original bytes and format.
//
// The decision helpers (`shouldCompressImage`, `fitWithin`,
// `compressedImageName`) are pure and unit-tested; only
// `compressImageToFit` touches browser APIs (createImageBitmap, canvas).

import { classifyAttachment } from './attachmentHelpers';

/**
 * Sources beyond this are rejected outright rather than decoded: a
 * decode allocates width×height×4 bytes, and a corrupt or adversarial
 * multi-hundred-MB file should fail fast instead of stalling the tab.
 */
export const MAX_COMPRESS_SOURCE_BYTES = 50 * 1024 * 1024;

/**
 * Longest-edge cap for re-encoded images. Providers downscale to about
 * this size anyway (Claude's vision limit is 1568px on the long edge),
 * so pixels past it only cost upload bytes.
 */
export const MAX_COMPRESS_DIMENSION = 2048;

/** Quality ladder tried at full (dimension-capped) size, best first. */
const QUALITY_STEPS = [0.92, 0.85, 0.78, 0.68];

/**
 * Scale ladder tried at the lowest quality once the quality ladder is
 * exhausted — for extreme inputs where quality alone can't fit.
 */
const FALLBACK_SCALE_STEPS = [0.75, 0.55];

/**
 * True when an over-limit file is worth a compression attempt: an upload
 * that classifies as an IMAGE, within the decode-safety ceiling. A `file`
 * is never re-encoded — it travels by path, byte-identical. Decode failures
 * on something that only looked like an image are caught by the caller.
 */
export function shouldCompressImage(file: File, maxBytes: number): boolean {
  if (file.size <= maxBytes || file.size > MAX_COMPRESS_SOURCE_BYTES) return false;
  return classifyAttachment(file.type, file.name) === 'image';
}

/**
 * Scale (width, height) down to fit maxDim on the longest edge,
 * preserving aspect ratio. Never upscales; never returns below 1px.
 */
export function fitWithin(
  width: number,
  height: number,
  maxDim: number,
): { width: number; height: number } {
  const longest = Math.max(width, height);
  if (longest <= maxDim) return { width, height };
  const scale = maxDim / longest;
  return {
    width: Math.max(1, Math.round(width * scale)),
    height: Math.max(1, Math.round(height * scale)),
  };
}

/** Swap the filename extension to match the re-encoded format. */
export function compressedImageName(name: string, mimeType: string): string {
  const stem = name.replace(/\.[^.]+$/, '') || name;
  return stem + (mimeType === 'image/webp' ? '.webp' : '.jpg');
}

// WebP encodes markedly smaller than JPEG at the same visual quality
// and keeps alpha; probed once because canvas encoders that don't
// support a format silently hand back PNG instead of erroring.
let webpEncodeSupport: boolean | null = null;

async function supportsWebPEncode(): Promise<boolean> {
  if (webpEncodeSupport !== null) return webpEncodeSupport;
  const canvas = document.createElement('canvas');
  canvas.width = 1;
  canvas.height = 1;
  try {
    const blob = await encodeCanvas(canvas, 'image/webp', 0.8);
    webpEncodeSupport = blob.type === 'image/webp';
  } catch {
    webpEncodeSupport = false;
  }
  return webpEncodeSupport;
}

function drawToCanvas(
  bitmap: ImageBitmap,
  width: number,
  height: number,
  matte: boolean,
): HTMLCanvasElement {
  const canvas = document.createElement('canvas');
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext('2d');
  if (!ctx) throw new Error('canvas 2d context unavailable');
  if (matte) {
    // JPEG has no alpha channel and canvas composites transparency onto
    // black; matte transparent sources onto white so a screenshot with a
    // transparent background doesn't come out as a black rectangle.
    ctx.fillStyle = '#ffffff';
    ctx.fillRect(0, 0, width, height);
  }
  ctx.drawImage(bitmap, 0, 0, width, height);
  return canvas;
}

function encodeCanvas(canvas: HTMLCanvasElement, type: string, quality: number): Promise<Blob> {
  return new Promise((resolve, reject) => {
    canvas.toBlob(
      (blob) => (blob ? resolve(blob) : reject(new Error(`canvas ${type} encode failed`))),
      type,
      quality,
    );
  });
}

/**
 * Re-encode an oversized image to fit maxBytes. Returns the replacement
 * File, or null when even the smallest ladder step is still too large.
 * Throws when the source can't be decoded or the canvas can't encode —
 * callers treat that the same as null and fall back to the size
 * rejection. Animated GIFs are flattened to their first frame; that
 * still beats a hard rejection, since providers read a single frame
 * regardless.
 */
export async function compressImageToFit(file: File, maxBytes: number): Promise<File | null> {
  const bitmap = await createImageBitmap(file);
  try {
    const format = (await supportsWebPEncode()) ? 'image/webp' : 'image/jpeg';
    const matte = format === 'image/jpeg';
    const base = fitWithin(bitmap.width, bitmap.height, MAX_COMPRESS_DIMENSION);
    const name = compressedImageName(file.name, format);

    const canvas = drawToCanvas(bitmap, base.width, base.height, matte);
    for (const quality of QUALITY_STEPS) {
      const blob = await encodeCanvas(canvas, format, quality);
      if (blob.size <= maxBytes) {
        return new File([blob], name, { type: blob.type });
      }
    }

    const lastQuality = QUALITY_STEPS[QUALITY_STEPS.length - 1]!;
    for (const scale of FALLBACK_SCALE_STEPS) {
      const scaled = drawToCanvas(
        bitmap,
        Math.max(1, Math.round(base.width * scale)),
        Math.max(1, Math.round(base.height * scale)),
        matte,
      );
      const blob = await encodeCanvas(scaled, format, lastQuality);
      if (blob.size <= maxBytes) {
        return new File([blob], name, { type: blob.type });
      }
    }
    return null;
  } finally {
    bitmap.close();
  }
}
