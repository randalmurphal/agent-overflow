// One fetch per (computer, workspace, path) for the `![x](/abs/path.png)`
// an agent writes, shared by every mount of that markdown: the thread and
// the side chat forked from it, and a remount of the row after a scroll
// away and back (mediaBlobCache.ts owns retention, the byte budget and
// revocation).
//
// The bytes come from the thread's computer through GetLocalImageData,
// pinned by the caller. A path names a different file on every machine,
// so the computer is part of the key and the route is never left to the
// focused pane.

import { GetLocalImageData } from '../stores/bindings';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { base64ToBytes } from './base64';
import {
  acquireMediaBlob,
  objectOrDataUrl,
  type ImageSize,
  type MediaBytes,
  type MediaHandle,
} from './mediaBlobCache';

export interface LocalImage extends MediaBytes, ImageSize {}

export type LocalImageHandle = MediaHandle<LocalImage>;

/** Cache key: one entry per computer, workspace and path. */
export function localImageCacheKey(
  backend: BackendKey,
  path: string,
  workspacePath: string,
): string {
  return JSON.stringify(['local', backend, workspacePath, path]);
}

/**
 * The shared resolution for one local image, retained until `release()`.
 * A host releases in its effect cleanup; a retained entry is never
 * evicted, so a leaked retention is a leaked blob URL.
 */
export function acquireLocalImage(
  backend: BackendKey,
  path: string,
  workspacePath: string,
): LocalImageHandle {
  return acquireMediaBlob(
    localImageCacheKey(backend, path, workspacePath),
    () => loadLocalImage(backend, path, workspacePath),
  );
}

async function loadLocalImage(
  backend: BackendKey,
  path: string,
  workspacePath: string,
): Promise<LocalImage> {
  const result = await withBackendTarget(backend, () => GetLocalImageData(path, workspacePath));
  const blob = new Blob([base64ToBytes(result.data)], { type: result.mimeType });
  return {
    url: await objectOrDataUrl(blob, result.mimeType),
    mimeType: result.mimeType,
    blob,
    width: result.width,
    height: result.height,
  };
}

const FAILURE_PREFIX = 'load local image: ';
const MAX_REASON_LENGTH = 48;

/**
 * The short phrase a failure chip shows beside the image's alt text: the
 * reason GetLocalImageData names between its prefix and the cause
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
