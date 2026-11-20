// The href a repository image claim carries (forgeRepositoryFiles.ts) and
// its reading by the hosts (RepositoryImageHost.svelte): what the image
// fact answered, as data a nonce-gated `agent-overflow:repo-image?…` href
// holds, so the host paints the box at its final size from the first frame.
// Pure; the bytes are repositoryImages.ts's.

import type { BackendKey } from '../transport/backendKey';
import { MARKDOWN_HREF_NONCE } from './markdownHrefNonce';
import type { PRRef } from './prReference';

/** Prefix of every repository image href this app mints (see markdownHrefNonce.ts). */
export const REPOSITORY_IMAGE_HREF_PREFIX = `agent-overflow:repo-image?nonce=${MARKDOWN_HREF_NONCE}&`;

/** What the image fact answered: shown, a Git LFS pointer, not there, or still being asked. */
export type RepositoryImageState = 'found' | 'lfs' | 'absent' | 'pending';

export interface RepositoryImageClaim {
  state: RepositoryImageState;
  /** The computer that owns the PR. */
  backend: BackendKey;
  pr: PRRef;
  commit: string;
  path: string;
  /** The blob id; '' unless found. */
  oid: string;
  /** Intrinsic pixel size; 0 when the format gives none (svg) or unknown. */
  width: number;
  height: number;
  /** The file's page on the forge; '' when unknown. */
  browserUrl: string;
}

export function buildRepositoryImageHref(claim: RepositoryImageClaim): string {
  const params = new URLSearchParams();
  params.set('s', claim.state);
  params.set('backend', claim.backend);
  params.set('forge', claim.pr.forge);
  params.set('host', claim.pr.host);
  params.set('ns', claim.pr.namespace);
  params.set('repo', claim.pr.repo);
  params.set('n', String(claim.pr.number));
  params.set('commit', claim.commit);
  params.set('path', claim.path);
  if (claim.oid) params.set('oid', claim.oid);
  if (claim.width > 0 && claim.height > 0) {
    params.set('w', String(claim.width));
    params.set('h', String(claim.height));
  }
  if (claim.browserUrl) params.set('web', claim.browserUrl);
  return `${REPOSITORY_IMAGE_HREF_PREFIX}${params.toString()}`;
}

const STATES: ReadonlySet<string> = new Set(['found', 'lfs', 'absent', 'pending']);
const OID = /^[0-9a-f]{40}(?:[0-9a-f]{24})?$/;

const dimension = (value: string | null): number => {
  const parsed = Number(value ?? '');
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : 0;
};

export function parseRepositoryImageHref(href: string | null | undefined): RepositoryImageClaim | null {
  if (!href || !href.startsWith(REPOSITORY_IMAGE_HREF_PREFIX)) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  const get = (name: string) => url.searchParams.get(name) ?? '';
  const state = get('s');
  const forge = get('forge');
  const number = Number(get('n'));
  const oid = get('oid');
  if (!STATES.has(state) || (forge !== 'github' && forge !== 'gitlab')) return null;
  if (!Number.isSafeInteger(number) || number <= 0 || !get('repo') || !get('host') || !get('path')) return null;
  if (state === 'found' && !OID.test(oid)) return null;
  const width = dimension(url.searchParams.get('w'));
  const height = dimension(url.searchParams.get('h'));
  return {
    state: state as RepositoryImageState,
    backend: get('backend'),
    pr: { forge, host: get('host'), namespace: get('ns'), repo: get('repo'), number },
    commit: get('commit'),
    path: get('path'),
    oid,
    width: width > 0 && height > 0 ? width : 0,
    height: width > 0 && height > 0 ? height : 0,
    browserUrl: get('web'),
  };
}
