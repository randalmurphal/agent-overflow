// Copy and save for the images the image menu (`ImageMenuHost.svelte`)
// opens on, and the DOM tags that let that one delegated menu find them.
//
// Two kinds of image carry the menu:
//
//   attachment  A thread's image attachment. Every surface that paints one
//               (the user message grid, the composer and editor thumbs, a
//               generated image, the lightbox) spreads
//               `attachmentImageMenuTag` onto the element that owns the
//               picture. Both actions work on the ORIGINAL bytes from the
//               download route, never the thumbnail a tile is painted from.
//   forge       An image a PR/MR body or comment references, painted by
//               `ForgeAttachmentHost` through `forgeImageMenuTag`. The tag
//               carries the nonce-gated href the host was given, and
//               `taggedMenuImage` accepts it only through
//               `parseForgeAttachmentHref`, so text a forge wrote cannot
//               name one. Copy uses the bytes the page already holds; save
//               is the forge attachment activation (`openForgeAttachment`).
//
// The host reads a tag back with `taggedMenuImage`, so the attribute names
// live here only.

import { SaveAttachment } from '../stores/bindings';
import type { BackendKey } from '../transport/backendKey';
import { addToast } from '../stores/toast.svelte';
import { fetchAttachmentBytes } from '../transport/attachmentTransfer';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { resolveThreadBackend } from '../transport/entityIndex';
import { hasScope } from '../transport/scopes';
import { errString } from './errors';
import { fileSaveAction, savedFileMessage, type FileSaveAction } from './fileSaveAction';
import { openForgeAttachment } from './forgeAttachmentActions';
import { acquireForgeAttachment } from './forgeAttachmentCache';
import {
  browserUrlForForgeAttachment,
  parseForgeAttachmentHref,
  type ParsedForgeAttachmentHref,
} from './forgeAttachments';
import { asPng, writePngToClipboard } from './pngClipboard';

export type ImageMenuTarget =
  | { kind: 'attachment'; threadId: string; attachmentId: string; filename: string }
  | { kind: 'forge'; attachment: ParsedForgeAttachmentHref };

const KIND = 'data-image-menu';
const THREAD = 'data-image-menu-thread';
const ID = 'data-image-menu-id';
const FILENAME = 'data-image-menu-filename';
const FORGE_HREF = 'data-image-menu-href';

/** Attributes marking an element as one thread image attachment. */
export function attachmentImageMenuTag(attachment: {
  threadId: string;
  id: string;
  filename: string;
}): Record<string, string> {
  return {
    [KIND]: 'attachment',
    [ID]: attachment.id,
    [THREAD]: attachment.threadId,
    [FILENAME]: attachment.filename,
  };
}

/** Attributes marking an element as one forge image, by its app href. */
export function forgeImageMenuTag(href: string): Record<string, string> {
  return { [KIND]: 'forge', [FORGE_HREF]: href };
}

/** The image `target` belongs to, or null. */
export function taggedMenuImage(
  target: EventTarget | null,
): { element: HTMLElement; target: ImageMenuTarget } | null {
  if (!(target instanceof Element)) return null;
  const element = target.closest<HTMLElement>(`[${KIND}]`);
  if (!element) return null;
  const kind = element.getAttribute(KIND);
  if (kind === 'forge') {
    const attachment = parseForgeAttachmentHref(element.getAttribute(FORGE_HREF));
    return attachment ? { element, target: { kind: 'forge', attachment } } : null;
  }
  if (kind !== 'attachment') return null;
  const attachmentId = element.getAttribute(ID) ?? '';
  const threadId = element.getAttribute(THREAD) ?? '';
  if (!attachmentId || !threadId) return null;
  return {
    element,
    target: {
      kind: 'attachment',
      threadId,
      attachmentId,
      filename: element.getAttribute(FILENAME) ?? '',
    },
  };
}

/**
 * Put the image on the clipboard as PNG. Resolves once the clipboard holds
 * it; throws a toast-ready message otherwise. Call it directly from the
 * click: the clipboard write is reached before anything is awaited
 * (`pngClipboard.ts` note 2).
 */
export function copyMenuImage(target: ImageMenuTarget): Promise<void> {
  const original = target.kind === 'forge'
    ? () => heldForgeImage(target.attachment)
    : () => fetchAttachmentBytes(target.threadId, target.attachmentId);
  return writePngToClipboard(async () => asPng(await original()), 'Could not copy the image');
}

// The bytes the page already painted the forge image from, under a claim
// of this copy's own so the cache cannot evict them mid-read.
async function heldForgeImage(attachment: ParsedForgeAttachmentHref): Promise<Blob> {
  const handle = acquireForgeAttachment(attachment.backend, attachment.pr, attachment.href);
  try {
    const resolved = await handle.value;
    if (resolved.kind !== 'image') throw new Error('this attachment is not an image');
    return resolved.blob;
  } finally {
    handle.release();
  }
}

/** What Save does for `target` on this page (`fileSaveAction`). */
function saveAction(target: ImageMenuTarget): { backend: BackendKey; action: FileSaveAction } | null {
  if (target.kind === 'forge') {
    const { attachment } = target;
    const browserUrl = browserUrlForForgeAttachment(
      attachment.pr.forge,
      attachment.href,
      attachment.webBase,
      attachment.pr,
    );
    return { backend: attachment.backend, action: fileSaveAction(attachment.backend, browserUrl) };
  }
  let backend: BackendKey;
  try {
    backend = requireEntityBackend(resolveThreadBackend(target.threadId));
  } catch {
    return null;
  }
  return { backend, action: fileSaveAction(backend, null) };
}

/**
 * Whether this page may carry out Save. A file written on the owning
 * computer needs the scope that computer's save RPC requires
 * (`SaveAttachment` is attachments:write, `SaveForgeAttachment` is
 * git:operate); a browser download reads bytes this page can already read,
 * and opening the forge's own page needs no grant. An owner this page
 * cannot resolve answers true, so the click reports the real error.
 */
export function canSaveMenuImage(target: ImageMenuTarget): boolean {
  const decided = saveAction(target);
  if (!decided) return true;
  const { backend, action } = decided;
  if (action === 'download' || action === 'open-externally') return true;
  return hasScope(target.kind === 'forge' ? 'git:operate' : 'attachments:write', backend);
}

/**
 * The Save row's label. Where Save opens the image's page on the forge
 * instead (a webview that cannot run a browser download), the row says so
 * rather than promising a file.
 */
export function saveMenuImageLabel(target: ImageMenuTarget): string {
  if (target.kind === 'forge' && saveAction(target)?.action === 'open-externally') {
    return target.attachment.pr.forge === 'gitlab' ? 'Open on GitLab' : 'Open on GitHub';
  }
  return 'Save Image';
}

/**
 * How long a download's object URL outlives the click. The anchor click
 * only starts the download; some engines read the URL later, so revoking
 * synchronously can cancel it. Bounded, so the bytes are not held for the
 * page's lifetime.
 */
export const DOWNLOAD_URL_LIFETIME_MS = 40_000;

/**
 * Save the image where this page's user will find it. A forge image goes
 * through the forge attachment activation, which the file chip and link
 * clicks share. A thread attachment follows `fileSaveAction` with no browser
 * URL: written into the owning computer's Downloads folder by the backend
 * (this desktop, or a webview or phone that cannot run a browser download),
 * or an ordinary browser download. The outcome, success or failure, is a
 * toast.
 */
export async function saveMenuImage(target: ImageMenuTarget): Promise<void> {
  if (target.kind === 'forge') {
    await openForgeAttachment(target.attachment);
    return;
  }
  try {
    const backend = requireEntityBackend(resolveThreadBackend(target.threadId));
    const action = fileSaveAction(backend, null);
    if (action === 'save-here' || action === 'save-there') {
      const path = await withBackendTarget(backend, () => SaveAttachment(target.threadId, target.attachmentId));
      addToast('success', savedFileMessage(action, backend, path));
      return;
    }
    const blob = await fetchAttachmentBytes(target.threadId, target.attachmentId);
    downloadBlob(blob, downloadName(target.filename, blob.type));
  } catch (err) {
    addToast('error', errString(err));
  }
}

const IMAGE_EXTENSIONS: Record<string, string> = {
  'image/png': '.png',
  'image/jpeg': '.jpg',
  'image/jpg': '.jpg',
  'image/webp': '.webp',
  'image/gif': '.gif',
};

/**
 * The name a browser download is saved under: the attachment's filename,
 * given the extension its bytes call for when it has none. The store
 * requires a filename, but a message row without one names the image by
 * its id (`userMessageMeta.ts`), and a name with no extension would open
 * as an unknown file.
 */
export function downloadName(filename: string, mimeType: string): string {
  const name = filename.trim() || 'image';
  if (/\.[A-Za-z0-9]{1,5}$/.test(name)) return name;
  return name + (IMAGE_EXTENSIONS[mimeType.toLowerCase()] ?? '');
}

function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  anchor.rel = 'noopener';
  anchor.style.display = 'none';
  document.body.appendChild(anchor);
  try {
    anchor.click();
  } finally {
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), DOWNLOAD_URL_LIFETIME_MS);
  }
}
