// The bytes of an image a PR/MR body names as a file of the PR's
// repository (repositoryFiles.ts), claimed by the parse with the href of
// repositoryImageHref.ts and loaded only once its host is mounted.
//
// Bytes come by blob id from the computer that owns the PR
// (`GetGitBlobImage`, the ticketed byte route, the display tiers of
// imageTiers.ts), through the media blob cache (mediaBlobCache.ts owns
// retention, the 64 MiB / 256 entry budget and revocation). The cache key
// is the computer, the repository and the blob id, plus the tier: two
// bodies, two PRs of one repository and a new head that keeps the file
// share one fetch and one blob. The blob is read in the checkout the PR's
// facts are read in (stores/forgeFacts.svelte.ts); any checkout of the
// repository holds the same object.

import { GetGitBlobImage } from '../stores/bindings';
import { forgeFactsWorkspace } from '../stores/forgeFacts.svelte';
import { fetchTicketedBytes, TransferUnavailableError } from '../transport/attachmentTransfer';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { WorkspaceRef } from '../types/git';
import { lastImageTier } from './imageTiers';
import type { MarkdownImageSource, MarkdownImageVariant } from './markdownImageSource';
import { acquireMediaBlob, objectOrDataUrl, paintedSize, type MediaHandle, type MediaSizeSink } from './mediaBlobCache';
import { pathBasename } from './pathDisplay';
import { prKey, repoKey, type PRRef } from './prReference';
import { composeWorkspaceKey } from './workspaceKey';
import type { RepositoryImageClaim } from './repositoryImageHref';

/** One repository image's identity: computer, repository and blob, without the tier. */
export function repositoryImageCacheKey(backend: BackendKey, pr: PRRef, oid: string): string {
  return JSON.stringify(['repo', backend, repoKey(pr), oid]);
}

/**
 * The shared resolution for the blob at `tier` (a ladder width in device
 * pixels, or 0 for the file itself), retained until `release()`.
 */
export function acquireRepositoryImage(claim: RepositoryImageClaim, tier: number): MediaHandle<MarkdownImageVariant> {
  return acquireMediaBlob(
    JSON.stringify(['repo', claim.backend, repoKey(claim.pr), claim.oid, tier]),
    (sized) => loadRepositoryImage(claim, tier, sized),
  );
}

/**
 * The blob at the tier the page last painted it at, or the original when
 * nothing on the page has asked: a copy reuses those bytes when they are
 * not a display-size derivative (`derived`).
 */
export function acquirePaintedRepositoryImage(claim: RepositoryImageClaim): MediaHandle<MarkdownImageVariant> {
  return acquireRepositoryImage(claim, lastImageTier(repositoryImageCacheKey(claim.backend, claim.pr, claim.oid)) ?? 0);
}

/**
 * The blob's own bytes at `maxWidth` 0, else the widest variant at most
 * `maxWidth` device pixels. Not cached: the caller (the lightbox, a copy, a
 * download) holds the Blob for as long as it needs it.
 */
export async function fetchRepositoryImageBytes(claim: RepositoryImageClaim, maxWidth: number, signal?: AbortSignal): Promise<Blob> {
  return (await mintAndFetch(claim, maxWidth, signal)).blob;
}

/** The checkout the PR's facts are read in, which holds the blob. */
export function repositoryImageWorkspace(claim: RepositoryImageClaim): WorkspaceRef {
  const workspace = forgeFactsWorkspace(composeWorkspaceKey(claim.backend, prKey(claim.pr)));
  if (!workspace) throw new Error('No checkout of this repository to read the image from');
  return workspace;
}

async function loadRepositoryImage(claim: RepositoryImageClaim, tier: number, sized: MediaSizeSink): Promise<MarkdownImageVariant> {
  const { result, blob } = await mintAndFetch(claim, tier, undefined, sized);
  return {
    url: await objectOrDataUrl(blob, result.mimeType),
    mimeType: result.mimeType,
    blob,
    width: result.width,
    height: result.height,
    originalWidth: result.originalWidth,
    originalHeight: result.originalHeight,
    originalBytes: result.originalBytes,
    derived: result.derived,
    ...(result.fluid === true ? { fluid: true } : {}),
  };
}

// A ticket is spent by the first request that presents it, so a 404 means
// "mint again": one retry, and a second 404 is the error the user sees.
async function mintAndFetch(claim: RepositoryImageClaim, maxWidth: number, signal?: AbortSignal, sized?: MediaSizeSink) {
  const workspace = repositoryImageWorkspace(claim);
  const mint = () => withBackendTarget(claim.backend, () => GetGitBlobImage(workspace, claim.oid, maxWidth));
  let result = await mint();
  sized?.(paintedSize(result));
  let bytes: Blob;
  try {
    bytes = await fetchTicketedBytes(claim.backend, result.url, 'Could not load image', signal);
  } catch (err) {
    if (!(err instanceof TransferUnavailableError)) throw err;
    signal?.throwIfAborted();
    result = await mint();
    bytes = await fetchTicketedBytes(claim.backend, result.url, 'Could not load image', signal);
  }
  return { result, blob: new Blob([bytes], { type: result.mimeType }) };
}

/** A found repository image, as `MarkdownImage` paints one, carrying the image menu's tag (`repositoryImageMenuTag`). */
export function repositoryMarkdownImageSource(claim: RepositoryImageClaim, menuTag: Record<string, string>): MarkdownImageSource {
  return {
    key: repositoryImageCacheKey(claim.backend, claim.pr, claim.oid),
    filename: pathBasename(claim.path) || claim.path,
    acquire: (tier) => acquireRepositoryImage(claim, tier),
    original: (signal, maxWidth) => fetchRepositoryImageBytes(claim, maxWidth, signal),
    menuTag,
  };
}
