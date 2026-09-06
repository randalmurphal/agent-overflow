// Pure half of custom-sprite loading: manifest parsing and frame-
// geometry validation for files dropped into <configDir>/spinners/.
// The backend lists `<id>.png` + `<id>.json` pairs as opaque bytes
// (internal/spinner mirrors internal/theme: Go never parses a
// manifest); everything that can be wrong with one is decided here so
// the store and the tests share a single definition. A bad file costs
// exactly that sprite and produces a warning — never the feature.

import type { SpinnerSprite } from './catalog';

export interface CustomManifest {
  frames: number;
  frameMs: number;
  label?: string;
}

/** Sane bounds, mirrored in the SPINNERS.md the backend seeds. */
export const MIN_FRAMES = 1;
export const MAX_FRAMES = 240;
export const MIN_FRAME_MS = 20;
export const MAX_FRAME_MS = 2000;
const MAX_LABEL_CHARS = 64;
export const MAX_SPRITE_PIXELS = 4_194_304;
export const MAX_LIBRARY_PIXELS = 8_388_608;

/** Inspect the fixed PNG header before asking the browser to allocate a decoded
 * image. Compressed-byte limits alone do not bound image memory. */
export function pngDimensions(png: string): { width: number; height: number } | null {
  try {
    const header = atob(png.slice(0, 44));
    if (header.length < 33 || header.slice(0, 8) !== '\x89PNG\r\n\x1a\n' || header.slice(12, 16) !== 'IHDR') return null;
    const uint32 = (at: number): number => (
      header.charCodeAt(at) * 0x1000000 + header.charCodeAt(at + 1) * 0x10000
      + header.charCodeAt(at + 2) * 0x100 + header.charCodeAt(at + 3)
    );
    if (uint32(8) !== 13) return null;
    const width = uint32(16), height = uint32(20);
    if (width < 1 || height < 1 || width > 32_768 || height > 4_096 || width * height > MAX_SPRITE_PIXELS) return null;
    return { width, height };
  } catch { return null; }
}

/**
 * Parse one sidecar manifest. Returns the manifest or a human-readable
 * reason the sprite is skipped (surfaced in Settings beside the pool).
 */
export function parseCustomManifest(id: string, raw: string): CustomManifest | string {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return `${id}.json: not valid JSON`;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return `${id}.json: expected an object like {"frames": 8, "frameMs": 100}`;
  }
  const record = parsed as Record<string, unknown>;
  const frames = record['frames'];
  if (!Number.isInteger(frames) || (frames as number) < MIN_FRAMES || (frames as number) > MAX_FRAMES) {
    return `${id}.json: "frames" must be an integer between ${MIN_FRAMES} and ${MAX_FRAMES}`;
  }
  const frameMs = record['frameMs'];
  if (!Number.isInteger(frameMs) || (frameMs as number) < MIN_FRAME_MS || (frameMs as number) > MAX_FRAME_MS) {
    return `${id}.json: "frameMs" must be an integer between ${MIN_FRAME_MS} and ${MAX_FRAME_MS}`;
  }
  const label = record['label'];
  const manifest: CustomManifest = { frames: frames as number, frameMs: frameMs as number };
  if (typeof label === 'string' && label.trim() !== '') {
    manifest.label = label.trim().slice(0, MAX_LABEL_CHARS);
  }
  return manifest;
}

/**
 * Combine a parsed manifest with the strip's decoded pixel size into a
 * SpinnerSprite, or a warning when the geometry cannot work (a width
 * that does not divide by the frame count would render a smearing
 * cycle, never a clean one).
 */
export function buildCustomSprite(
  id: string,
  manifest: CustomManifest,
  src: string,
  width: number,
  height: number,
): SpinnerSprite | string {
  if (width <= 0 || height <= 0) {
    return `${id}.png: could not decode the image`;
  }
  if (width > 32_768 || height > 4_096 || width * height > MAX_SPRITE_PIXELS) {
    return `${id}.png: image dimensions exceed the animation memory limit`;
  }
  if (width % manifest.frames !== 0) {
    return `${id}.png: width ${width}px does not divide into ${manifest.frames} equal frames`;
  }
  return {
    id,
    label: manifest.label ?? id,
    src,
    frames: manifest.frames,
    frameMs: manifest.frameMs,
    frameWidth: width / manifest.frames,
    frameHeight: height,
    custom: true,
  };
}
