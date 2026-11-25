// Svelte context key for "this markdown is a ticket's text": the ticket
// screen sets it once for its subtree, and every `TrackerMediaChip` inside
// reads which of the ticket's attachments a file in the text names
// (stores/trackerTicketModel.ts#mediaAttachment). A surface that sets none
// (a ticket text preview, a test) shows each file as its name alone.
//
// A function rather than a value, as the forge's context is: the ticket's
// attachments change as its view is read again, and context is set once at
// mount. What it answers is built by the Issues chunk
// (utils/trackerAttachments.ts), so this module and the chip hold no
// tracker code of their own.

import type { MarkdownImageSource } from '../../../utils/markdownImageSource';

export const TRACKER_MEDIA_CONTEXT = 'agent-overflow:tracker-media';

/** A file of the ticket a file in its text names. */
export interface TrackerMediaFile {
  readonly name: string;
  /** Its size in bytes. */
  readonly size: number;
  /** The image the text shows in place; null for a file it shows as a chip. */
  readonly image: MarkdownImageSource | null;
  /** Saves the file where this page's person finds it, or opens the
   * ticket in the tracker when this page cannot save it; says how it
   * went. */
  save(): void;
  /** The Save control's label ("Save", or "Open in the tracker"). */
  readonly saveLabel: string;
}

/** What `getContext(TRACKER_MEDIA_CONTEXT)` answers: the file a name in the
 * text names, or null when no attachment, or several, have that name. */
export type TrackerMediaReader = (name: string) => TrackerMediaFile | null;
