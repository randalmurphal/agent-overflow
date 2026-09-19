// Forge-hosted attachments referenced by PR/MR bodies and review comments.
//
// GitHub and GitLab serve uploaded media from URLs only a logged-in browser
// can read: `/uploads/<32 hex>/shot.png` on GitLab, and on GitHub either a
// bare `https://github.com/user-attachments/assets/<uuid>` line (how the
// forge embeds both videos and images) or a signed
// `private-user-images.githubusercontent.com` redirect. A webview holds no
// forge cookie, so rendering those srcs directly gives a broken image on a
// public repo and nothing at all on a private one.
//
// So the bytes come the way every other computer-owned byte does: the
// backend fetches them with the user's `gh` / `glab` login on the computer
// that owns the PR (`FetchForgeAttachment`) and serves them once through a
// ticketed URL. This module is the PURE half of that — no transport, no
// store, no bindings — so the parser extension, the renderer, the click
// delegate and the copy serializer all read one set of rules:
//
//   - SHAPE DETECTION per forge, on the raw href as written.
//   - our nonce-gated `agent-overflow:forge?…` href, which is what the
//     parser extension rewrites a claimed token to and what the render and
//     click paths recognize by prefix.
//   - the browser URL, used as a hover title and as the fallback the user
//     can always fall through to.
//
// Fetching lives in `forgeAttachmentCache.ts`; the click ladder lives in
// `forgeAttachmentActions.ts`.

import type { BackendKey } from '../transport/backendKey';
import type { Forge, PRRef } from './prReference';
import { MARKDOWN_HREF_NONCE } from './markdownHrefNonce';

/** Where a rendered body's forge attachments are fetched from. */
export interface ForgeAttachmentSource {
  pr: PRRef;
  /** The computer that owns the PR; every RPC is pinned to it. */
  backend: BackendKey;
  /** The PR/MR web URL (`review.prDetail.url`), or '' when unknown. */
  webBase: string;
}

// ---------------------------------------------------------------------------
// Shape detection
// ---------------------------------------------------------------------------

// WHATWG URL parsing strips leading and trailing C0 controls and spaces
// before reading a scheme; the checks below see the same string the render
// layer's `parseUrl` would.
const C0_OR_SPACE = /^[\u0000-\u0020]+|[\u0000-\u0020]+$/g;

const GITHUB_HOST = 'github.com';
// The signed redirect target GitHub hands a logged-in browser. Its plain
// sibling `user-images.githubusercontent.com` is PUBLIC and stays a direct
// <img>: claiming it would spend an RPC on bytes the page can fetch itself.
const GITHUB_PRIVATE_IMAGE_HOST = 'private-user-images.githubusercontent.com';

// `/user-attachments/assets/<id>` and `/user-attachments/files/<id>/<name>`.
const GITHUB_USER_ATTACHMENTS = /^\/user-attachments\/(?:assets|files)\/([^/]+)(?:\/([^/]+))?$/;
// `/<owner>/<repo>/assets/<userid>/<id>` and `/<owner>/<repo>/files/<id>/<name>`.
const GITHUB_REPO_ASSET = /^\/[^/]+\/[^/]+\/assets\/[^/]+\/([^/]+)$/;
const GITHUB_REPO_FILE = /^\/[^/]+\/[^/]+\/files\/[^/]+\/([^/]+)$/;

// GitLab writes every upload as `/uploads/<32 lowercase hex>/<name>`, either
// relative to the project or absolute. The hex segment is the secret.
const GITLAB_UPLOAD_TAIL = /\/uploads\/[0-9a-f]{32}\/([^/]+)$/;
// The project-id spelling GitLab itself renders in comment bodies.
const GITLAB_PROJECT_PREFIX = /^\/-\/project\/\d+$/;

interface ForgeHrefShape {
  /** The attachment's display name: the `<name>` segment, else the id. */
  name: string;
  /** Absolute when the href named a host, else null. */
  absolute: URL | null;
  /** The reference's path, query and fragment removed. */
  path: string;
  /** Everything before `/uploads/…`, for a GitLab reference. */
  prefix: string;
}

function trimHref(href: string): string {
  return href.replace(C0_OR_SPACE, '');
}

function decodeSegment(raw: string): string {
  if (!raw.includes('%')) return raw;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

/** Parse an absolute http(s) URL, or null for anything else. */
function absoluteHttpUrl(trimmed: string): URL | null {
  if (!/^https?:\/\//i.test(trimmed)) return null;
  try {
    return new URL(trimmed);
  } catch {
    return null;
  }
}

function githubShape(trimmed: string): ForgeHrefShape | null {
  const url = absoluteHttpUrl(trimmed);
  if (!url) return null;
  const host = url.hostname.toLowerCase();
  if (host === GITHUB_PRIVATE_IMAGE_HOST) {
    // Any path: the signature in the query is the whole reference, and
    // GitHub reserves the right to reshape the path in front of it.
    const last = url.pathname.split('/').filter(Boolean).pop() ?? 'attachment';
    return { name: decodeSegment(last), absolute: url, path: url.pathname, prefix: '' };
  }
  if (host !== GITHUB_HOST) return null;
  const path = url.pathname;
  const userAttachment = GITHUB_USER_ATTACHMENTS.exec(path);
  if (userAttachment) {
    return {
      name: decodeSegment(userAttachment[2] ?? userAttachment[1]),
      absolute: url,
      path,
      prefix: '',
    };
  }
  const repoAsset = GITHUB_REPO_ASSET.exec(path) ?? GITHUB_REPO_FILE.exec(path);
  if (repoAsset) return { name: decodeSegment(repoAsset[1]), absolute: url, path, prefix: '' };
  return null;
}

function gitlabShape(trimmed: string): ForgeHrefShape | null {
  const url = absoluteHttpUrl(trimmed);
  const path = url ? url.pathname : pathOnlyHref(trimmed);
  if (path === null) return null;
  const tail = GITLAB_UPLOAD_TAIL.exec(path);
  if (!tail) return null;
  const prefix = path.slice(0, tail.index);
  if (url) {
    // `<host>/<ns…>/<repo>/uploads/…` or `<host>/-/project/<id>/uploads/…`.
    const segments = prefix.split('/').filter(Boolean);
    if (!GITLAB_PROJECT_PREFIX.test(prefix) && segments.length < 2) return null;
  } else if (prefix !== '' && !GITLAB_PROJECT_PREFIX.test(prefix)) {
    // Relative: `/uploads/…` or `/-/project/<id>/uploads/…` and nothing else.
    return null;
  }
  return { name: decodeSegment(tail[1]), absolute: url, path, prefix };
}

/**
 * A root-relative href's path, or null when the href is not one. A query or
 * fragment is not part of an upload reference. `//host/x` is a
 * protocol-relative URL, not a path.
 */
function pathOnlyHref(trimmed: string): string | null {
  if (!trimmed.startsWith('/') || trimmed.startsWith('//')) return null;
  const cut = trimmed.search(/[#?]/);
  return cut === -1 ? trimmed : trimmed.slice(0, cut);
}

function forgeShape(forge: Forge, href: string): ForgeHrefShape | null {
  if (typeof href !== 'string') return null;
  const trimmed = trimHref(href);
  if (trimmed === '') return null;
  return forge === 'github' ? githubShape(trimmed) : gitlabShape(trimmed);
}

/** Whether this forge would serve `href` as an attachment the CLI can fetch. */
export function isForgeAttachmentHref(forge: Forge, href: string): boolean {
  return forgeShape(forge, href) !== null;
}

/** The attachment's display name: the `<name>` segment, else the trailing id. */
export function forgeAttachmentName(forge: Forge, href: string): string {
  return forgeShape(forge, href)?.name ?? '';
}

/**
 * The URL a browser would open for this attachment, or null when it cannot
 * be derived. Used as the hover title, as the phone shell's external open,
 * and as the fallback link beside a failed fetch so the user is never stuck.
 *
 * GitHub references are absolute as written, normalized to https. A GitLab
 * upload is usually relative to the project, so the PR/MR web URL supplies
 * the origin; with no web URL there is nothing honest to point at.
 */
export function browserUrlForForgeAttachment(
  forge: Forge,
  href: string,
  webBase: string,
  pr: PRRef,
): string | null {
  const shape = forgeShape(forge, href);
  if (!shape) return null;
  if (shape.absolute) {
    if (forge === 'github') {
      const url = new URL(shape.absolute.href);
      url.protocol = 'https:';
      return url.href;
    }
    return shape.absolute.href;
  }
  const origin = originOf(webBase);
  if (origin === null) return null;
  // `/-/project/<id>/uploads/…` is already project-addressed; a bare
  // `/uploads/…` is relative to this MR's project.
  if (shape.prefix !== '') return `${origin}${shape.path}`;
  return `${origin}/${pr.namespace}/${pr.repo}${shape.path}`;
}

function originOf(webBase: string): string | null {
  if (webBase === '') return null;
  try {
    return new URL(webBase).origin;
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Our nonce-gated href
// ---------------------------------------------------------------------------

/**
 * Prefix of every forge-attachment href this app mints. Carries the shared
 * per-page-load nonce for the same reason path links do: PR bodies and
 * review comments are third-party text, and without the nonce any of it
 * could write an `agent-overflow:forge?…` link naming another computer and
 * another repository, and have it pass the renderer's URL gate.
 */
export const FORGE_ATTACHMENT_HREF_PREFIX = `agent-overflow:forge?nonce=${MARKDOWN_HREF_NONCE}&`;

export interface ParsedForgeAttachmentHref {
  /** The raw href exactly as the markdown wrote it; Go parses it again. */
  href: string;
  pr: PRRef;
  backend: BackendKey;
  webBase: string;
}

export function buildForgeAttachmentHref(
  args: { href: string } & ForgeAttachmentSource,
): string {
  const params = new URLSearchParams();
  params.set('href', args.href);
  params.set('forge', args.pr.forge);
  params.set('ns', args.pr.namespace);
  params.set('repo', args.pr.repo);
  params.set('n', String(args.pr.number));
  params.set('backend', args.backend);
  if (args.webBase) params.set('web', args.webBase);
  return `${FORGE_ATTACHMENT_HREF_PREFIX}${params.toString()}`;
}

export function parseForgeAttachmentHref(
  href: string | null | undefined,
): ParsedForgeAttachmentHref | null {
  if (!href || !href.startsWith(FORGE_ATTACHMENT_HREF_PREFIX)) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  const raw = url.searchParams.get('href');
  const forge = url.searchParams.get('forge');
  const repo = url.searchParams.get('repo');
  const number = Number(url.searchParams.get('n') ?? '');
  if (!raw || !repo) return null;
  if (forge !== 'github' && forge !== 'gitlab') return null;
  if (!Number.isSafeInteger(number) || number <= 0) return null;
  return {
    href: raw,
    pr: { forge, namespace: url.searchParams.get('ns') ?? '', repo, number },
    backend: url.searchParams.get('backend') ?? '',
    webBase: url.searchParams.get('web') ?? '',
  };
}
