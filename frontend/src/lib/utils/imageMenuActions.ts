// Copy and save for the images the image menu (`ImageMenuHost.svelte`)
// opens on, and the DOM tags that let that one delegated menu find them.
//
// Three kinds of image carry the menu:
//
//   attachment  A thread's image attachment. Every surface that paints one
//               (the user message grid, the composer and editor thumbs, a
//               generated image) spreads `attachmentImageMenuTag` onto the
//               element that owns the picture; the lightbox spreads its
//               item's `menuTag`, which `attachmentPreview.svelte.ts` builds
//               with it. Both actions work on the ORIGINAL bytes from the
//               download route, never the thumbnail a tile is painted from.
//   forge       An image a PR/MR body or comment references, painted by
//               `ForgeAttachmentHost` through `forgeImageMenuTag`. The tag
//               carries the nonce-gated href the host was given, and
//               `taggedMenuImage` accepts it only through
//               `parseForgeAttachmentHref`, so text a forge wrote cannot
//               name one. Copy uses the original: the bytes the page
//               already holds when they are not a display-size derivative;
//               save is the forge attachment activation
//               (`openForgeAttachment`).
//   local       An image an agent wrote as a path, painted by
//               `StreamdownImageHost` through `localImageMenuTag`. The tag
//               carries the nonce-gated href the parse built and the
//               computer the file is read from; `taggedMenuImage` accepts
//               the href only through `parseLocalImageHref`. Copy and save
//               work on the file itself, never the timeline's display-size
//               derivative, and Copy Path and Copy Markdown give back the
//               reference as the agent wrote it.
//
// The host reads a tag back with `taggedMenuImage`, so the attribute names
// live here only.

import { SaveAttachment, SaveLocalImage } from '../stores/bindings';
import type { BackendKey } from '../transport/backendKey';
import { addErrorToast, addToast } from '../stores/toast.svelte';
import { fetchAttachmentBytes } from '../transport/attachmentTransfer';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { resolveThreadBackend } from '../transport/entityIndex';
import { hasScope } from '../transport/scopes';
import { downloadBlob } from './blobDownload';
import { copyToClipboard } from './clipboard';
import { errString } from './errors';
import { fileSaveAction, savedFileMessage, type FileSaveAction } from './fileSaveAction';
import { openForgeAttachment } from './forgeAttachmentActions';
import { acquirePaintedForgeAttachment, fetchForgeAttachmentOriginal } from './forgeAttachmentCache';
import {
  browserUrlForForgeAttachment,
  parseForgeAttachmentHref,
  type ParsedForgeAttachmentHref,
} from './forgeAttachments';
import { fetchLocalImageOriginal } from './localImageCache';
import { pathBasename } from './pathDisplay';
import { parseLocalImageHref } from './pathLinkExtension';
import { asPng, writePngToClipboard } from './pngClipboard';

export type LocalMenuImage = {
  kind: 'local';
  /** The computer the file is read from: the thread's. */
  backend: BackendKey;
  path: string;
  workspacePath: string;
  /** The reference as the agent wrote it; '' when the parse kept none. */
  sourceHref: string;
  alt: string;
};

export type ImageMenuTarget =
  | { kind: 'attachment'; threadId: string; attachmentId: string; filename: string }
  | { kind: 'forge'; attachment: ParsedForgeAttachmentHref }
  | LocalMenuImage;

const KIND = 'data-image-menu';
const THREAD = 'data-image-menu-thread';
const ID = 'data-image-menu-id';
const FILENAME = 'data-image-menu-filename';
const HREF = 'data-image-menu-href';
const BACKEND = 'data-image-menu-backend';
const ALT = 'data-image-menu-alt';

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
  return { [KIND]: 'forge', [HREF]: href };
}

/**
 * Attributes marking an element as one local image: the app href the parse
 * built (`buildLocalImageHref`), the computer it is read from and its alt
 * text.
 */
export function localImageMenuTag(image: {
  backend: BackendKey;
  href: string;
  alt: string;
}): Record<string, string> {
  return { [KIND]: 'local', [BACKEND]: image.backend, [HREF]: image.href, [ALT]: image.alt };
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
    const attachment = parseForgeAttachmentHref(element.getAttribute(HREF));
    return attachment ? { element, target: { kind: 'forge', attachment } } : null;
  }
  if (kind === 'local') {
    const local = parseLocalImageHref(element.getAttribute(HREF));
    // HOME is the empty key, so only an absent attribute is no computer.
    const backend = element.getAttribute(BACKEND);
    if (!local || backend === null) return null;
    return {
      element,
      target: {
        kind: 'local',
        backend,
        path: local.path,
        workspacePath: local.workspacePath,
        sourceHref: local.sourceHref,
        alt: element.getAttribute(ALT) ?? '',
      },
    };
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
    : target.kind === 'local'
      ? () => fetchLocalImageOriginal(target.backend, target.path, target.workspacePath)
      : () => fetchAttachmentBytes(target.threadId, target.attachmentId);
  return writePngToClipboard(async () => asPng(await original()), 'Could not copy the image');
}

/** The reference Copy Path puts on the clipboard: as the agent wrote it. */
export function localImagePathText(image: LocalMenuImage): string {
  return image.sourceHref || image.path;
}

/**
 * The image reference Copy Markdown puts on the clipboard, as CommonMark
 * that parses back to the same image: brackets and backslashes in the alt
 * are escaped, and a destination with whitespace, parentheses or angle
 * brackets is written in angle brackets, where only `<` and `>` need
 * escaping. A plain path is copied as the agent wrote it.
 */
export function localImageMarkdownText(image: LocalMenuImage): string {
  const alt = image.alt.replace(/[\\[\]]/g, (char) => `\\${char}`);
  return `![${alt}](${markdownDestination(localImagePathText(image))})`;
}

function markdownDestination(destination: string): string {
  if (destination !== '' && !/[\s()<>\u0000-\u001f]/.test(destination)) return destination;
  return `<${destination.replace(/[<>]/g, (char) => `\\${char}`)}>`;
}

/**
 * Copy Path and Copy Markdown. The clipboard write is reached before
 * anything is awaited, inside the click; the outcome is a toast, and a
 * refused write is recorded by `copyToClipboard`.
 */
export function copyLocalImagePath(image: LocalMenuImage): Promise<void> {
  return copyText(localImagePathText(image), 'Path');
}

export function copyLocalImageMarkdown(image: LocalMenuImage): Promise<void> {
  return copyText(localImageMarkdownText(image), 'Markdown');
}

async function copyText(text: string, what: 'Path' | 'Markdown'): Promise<void> {
  if (await copyToClipboard(text)) {
    addToast('success', `${what} copied`);
  } else {
    addToast('error', `Could not copy the ${what.toLowerCase()}`);
  }
}

// The forge image's original bytes: the ones the page painted it from when
// those are the original, under a claim of this copy's own so the cache
// cannot evict them mid-read, else a fetch of the original behind the
// display-size derivative.
async function heldForgeImage(attachment: ParsedForgeAttachmentHref): Promise<Blob> {
  const handle = acquirePaintedForgeAttachment(attachment.backend, attachment.pr, attachment.href);
  try {
    const resolved = await handle.value;
    if (resolved.kind !== 'image') throw new Error('this attachment is not an image');
    if (!resolved.derived) return resolved.blob;
  } finally {
    handle.release();
  }
  return await fetchForgeAttachmentOriginal(attachment.backend, attachment.pr, attachment.href);
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
  if (target.kind === 'local') {
    return { backend: target.backend, action: fileSaveAction(target.backend, null) };
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
 * git:operate, `SaveLocalImage` is files:read); a browser download reads
 * bytes this page can already read, and opening the forge's own page needs
 * no grant. An owner this page cannot resolve answers true, so the click
 * reports the real error.
 */
export function canSaveMenuImage(target: ImageMenuTarget): boolean {
  const decided = saveAction(target);
  if (!decided) return true;
  const { backend, action } = decided;
  if (action === 'download' || action === 'open-externally') return true;
  return hasScope(SAVE_SCOPE[target.kind], backend);
}

const SAVE_SCOPE = {
  attachment: 'attachments:write',
  forge: 'git:operate',
  local: 'files:read',
} as const;

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
 * Save the image where this page's user will find it. A forge image goes
 * through the forge attachment activation, which the file chip and link
 * clicks share. A thread attachment or a local image follows
 * `fileSaveAction` with no browser URL: written into the owning computer's
 * Downloads folder by the backend (this desktop, or a webview or phone that
 * cannot run a browser download), or an ordinary browser download of the
 * original. The outcome, success or failure, is a toast.
 */
export async function saveMenuImage(target: ImageMenuTarget): Promise<void> {
  if (target.kind === 'forge') {
    await openForgeAttachment(target.attachment);
    return;
  }
  if (target.kind === 'local') {
    await saveLocalImage(target);
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
    addErrorToast(errString(err), err);
  }
}

// A local image's owning computer is the thread's: SaveLocalImage copies
// the file into its Downloads, or a connected browser downloads the file's
// own bytes under its own name.
async function saveLocalImage(image: LocalMenuImage): Promise<void> {
  try {
    const action = fileSaveAction(image.backend, null);
    if (action === 'save-here' || action === 'save-there') {
      const path = await withBackendTarget(image.backend, () => SaveLocalImage(image.path, image.workspacePath));
      addToast('success', savedFileMessage(action, image.backend, path));
      return;
    }
    const blob = await fetchLocalImageOriginal(image.backend, image.path, image.workspacePath);
    downloadBlob(blob, downloadName(pathBasename(image.path), blob.type));
  } catch (err) {
    addErrorToast(errString(err), err);
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
