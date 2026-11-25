// A ticket's attachments on this page: what saving one does, the bytes of
// one, and the images its text shows in place (docs/specs/issues.md,
// "Ticket").
//
// Saving follows `fileSaveAction` with no browser URL, since the tracker
// gives an attachment none of its own: the computer that holds the ticket
// writes it into its Downloads (`SaveTrackerAttachment`) for its own
// desktop and for a webview or phone that cannot run a browser download,
// and a connected browser downloads it. That download holds the file in
// memory, so above `BROWSER_SAVE_MAX_BYTES` the page opens the ticket in
// the tracker instead, where the file saves without passing through here.
//
// The bytes come through the ticket's subscription on its computer: the
// download ticket names it, and the computer finds the attachment on the
// ticket as its subscribers were last told. An image in the ticket's text
// paints from the media cache (mediaBlobCache.ts) under the ticket and
// attachment, so a reconnect does not fetch a held image again, and every
// entry of a ticket goes when its last surface lets it go.
//
// Previews must keep true:
// - One rule admits an attachment to a preview in the app, in the text
//   and from its row: `previewsInApp` (an image up to PREVIEW_MAX_BYTES).
//   Any other file is saved, or opened in the tracker.
// - While a ticket is open, the bytes held for its images are those of
//   the images within 200 px of the viewport and of an open lightbox: an
//   image scrolled away releases its hold and keeps its box
//   (TrackerMediaChip.svelte), and the media cache evicts what nobody
//   holds past its budget.

import type { AttachmentView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import type { TrackerMediaFile, TrackerMediaReader } from '../components/chat/markdown/trackerMediaContext';
import { SaveTrackerAttachment } from '../stores/bindings';
import { addErrorToast, addToast } from '../stores/toast.svelte';
import { onTrackerTicketClosed, trackerTicketSubscription, type TrackerTicketRef } from '../stores/trackerTicket.svelte';
import { mediaAttachment, previewsInApp } from '../stores/trackerTicketModel';
import { fetchTrackerAttachmentBytes } from '../transport/attachmentTransfer';
import { withBackendTarget } from '../transport/backends';
import { downloadBlob } from './blobDownload';
import { errString } from './errors';
import { handleExternalURL, safeExternalURL } from './externalLinks';
import { fileSaveAction, savedFileMessage } from './fileSaveAction';
import type { MarkdownImageSource, MarkdownImageVariant } from './markdownImageSource';
import { acquireMediaBlob, discardMediaBlobs, objectOrDataUrl } from './mediaBlobCache';

/** The largest attachment a connected browser saves through this page; a
 * larger one opens the ticket in the tracker. */
export const BROWSER_SAVE_MAX_BYTES = 256 * 1024 * 1024;

/** The ticket an attachment belongs to, as the screen holds it. */
export interface TrackerAttachmentTicket {
  /** trackerTicketKey of `ref`. */
  readonly key: string;
  readonly ref: TrackerTicketRef;
  /** The ticket's page in the tracker; '' when it has none. */
  readonly url: string;
}

/** Where Save puts the attachment: the ticket's computer writes it, this
 * browser downloads it, or the ticket opens in the tracker. */
export type TrackerAttachmentSave = 'computer' | 'browser' | 'tracker';

export function trackerAttachmentSave(ticket: TrackerAttachmentTicket, attachment: Pick<AttachmentView, 'size'>): TrackerAttachmentSave {
  if (fileSaveAction(ticket.ref.backend, null) !== 'download') return 'computer';
  return attachment.size > BROWSER_SAVE_MAX_BYTES ? 'tracker' : 'browser';
}

/** The Save control's label for `trackerAttachmentSave`. */
export function trackerAttachmentSaveLabel(save: TrackerAttachmentSave): string {
  return save === 'tracker' ? 'Open in the tracker' : 'Save';
}

function subscriptionOf(ticket: TrackerAttachmentTicket): string {
  const id = trackerTicketSubscription(ticket.key);
  if (!id) throw new Error('The ticket is not followed on its computer right now; try again once it shows.');
  return id;
}

/** Saves the attachment where this page's person finds it, and says how
 * it went in a toast. */
export async function saveTrackerAttachment(ticket: TrackerAttachmentTicket, attachment: AttachmentView): Promise<void> {
  const save = trackerAttachmentSave(ticket, attachment);
  try {
    if (save === 'tracker') {
      await openTicketInTracker(ticket, attachment.name);
      return;
    }
    const action = fileSaveAction(ticket.ref.backend, null);
    if (save === 'computer' && (action === 'save-here' || action === 'save-there')) {
      const id = subscriptionOf(ticket);
      const path = await withBackendTarget(ticket.ref.backend, () => SaveTrackerAttachment(id, attachment.id));
      addToast('success', savedFileMessage(action, ticket.ref.backend, path));
      return;
    }
    downloadBlob(await fetchTrackerAttachment(ticket, attachment), attachment.name);
  } catch (err) {
    addErrorToast(`Could not save ${attachment.name}: ${errString(err)}`, err);
  }
}

async function openTicketInTracker(ticket: TrackerAttachmentTicket, name: string): Promise<void> {
  const url = safeExternalURL(ticket.url);
  if (!url || !(await handleExternalURL(url))) {
    addToast('error', `${name} is larger than this page saves, and the ticket has no web address to save it from.`);
  }
}

/** The attachment's bytes, typed as the ticket names it. */
export async function fetchTrackerAttachment(ticket: TrackerAttachmentTicket, attachment: AttachmentView, signal?: AbortSignal): Promise<Blob> {
  const blob = await fetchTrackerAttachmentBytes(ticket.ref.backend, subscriptionOf(ticket), attachment.id, signal);
  // The route sends a save's untyped bytes; the type is the ticket's word.
  // A slice retypes the same bytes without copying them.
  return attachment.mime_type ? blob.slice(0, blob.size, attachment.mime_type) : blob;
}

/** The prefix of every media cache key of a ticket's images. */
function mediaKeyPrefix(ticketKey: string): string {
  return JSON.stringify(['tracker', ticketKey]).slice(0, -1) + ',';
}

function mediaKey(ticketKey: string, attachment: string): string {
  return JSON.stringify(['tracker', ticketKey, attachment]);
}

onTrackerTicketClosed((key) => discardMediaBlobs(mediaKeyPrefix(key)));

/** An image of the ticket as `MarkdownImage` paints it: the original at
 * every tier, since the tracker route serves nothing else. */
export function trackerMarkdownImageSource(ticket: TrackerAttachmentTicket, attachment: AttachmentView): MarkdownImageSource {
  const key = mediaKey(ticket.key, attachment.id);
  const load = async (): Promise<MarkdownImageVariant> => {
    const blob = await fetchTrackerAttachment(ticket, attachment);
    const mimeType = blob.type || attachment.mime_type || 'application/octet-stream';
    return {
      url: await objectOrDataUrl(blob, mimeType), mimeType, blob,
      width: 0, height: 0, originalWidth: 0, originalHeight: 0, originalBytes: blob.size, derived: false,
    };
  };
  return {
    key,
    filename: attachment.name,
    acquire: () => acquireMediaBlob(key, load),
    original: (signal) => fetchTrackerAttachment(ticket, attachment, signal),
    menuTag: {},
  };
}

/** What a ticket's text reads of its files (TRACKER_MEDIA_CONTEXT):
 * `current` answers the ticket and its attachments as the screen shows
 * them now, or null before its view. */
export function trackerMediaReader(current: () => { ticket: TrackerAttachmentTicket; attachments: readonly AttachmentView[] } | null): TrackerMediaReader {
  return (name) => {
    const now = current();
    if (!now) return null;
    const attachment = mediaAttachment(now.attachments, name);
    if (!attachment) return null;
    const save = trackerAttachmentSave(now.ticket, attachment);
    const file: TrackerMediaFile = {
      name: attachment.name,
      size: attachment.size,
      image: previewsInApp(attachment) ? trackerMarkdownImageSource(now.ticket, attachment) : null,
      save: () => {
        void saveTrackerAttachment(now.ticket, attachment);
      },
      saveLabel: trackerAttachmentSaveLabel(save),
    };
    return file;
  };
}
