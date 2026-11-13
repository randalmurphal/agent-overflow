// A write that failed after the surface that made it went: the review
// pane's comment writes and its title and description edits share this
// one rule. What a failure reads as, and whether it may have landed
// anyway; and the one toast that says so, held until dismissed, with the
// text that was sent to copy. Nothing is kept for a later pane.

import { addErrorToast, showToastText } from './toast.svelte';
import { isTransportClassError } from './transportStatus.svelte';
import { TransportError } from '../transport/wsClient';
import { copyToClipboard } from '../utils/clipboard';

/** A write's text, for the toast when it fails after its surface is gone. */
export interface LostWriteText {
  readonly body: string;
  /** Who and where: `Your comment on src/a.ts:12`. */
  readonly subject: string;
  /** What did not happen: `was not sent` (a comment), `was not saved`
   * (an edit of the PR). */
  readonly notDone?: string;
}

function sentence(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  return 'The forge did not take this change.';
}

// A write whose answer never arrived (the connection dropped, or the call
// timed out) may have gone through, as one the forge did not confirm may.
const ANSWER_LOST = 'The connection dropped before the answer arrived, so this may have gone through. Check the pull request before sending it again.';

/** What a failed write reads as, and whether it may have landed anyway. */
export function writeFailure(err: unknown): { unconfirmed: boolean; sentence: string } {
  if (isTransportClassError(err)) return { unconfirmed: true, sentence: ANSWER_LOST };
  return { unconfirmed: err instanceof TransportError && err.code === 'forge_unconfirmed', sentence: sentence(err) };
}

// An unconfirmed write may be on the PR, so it never reads as not done;
// its sentence says what to check. When the clipboard refuses, the toast
// shows the text itself instead.
export function toastLostWrite(text: LostWriteText, err: unknown): void {
  const failure = writeFailure(err);
  const message = `${text.subject} ${failure.unconfirmed ? 'was not confirmed' : (text.notDone ?? 'was not sent')}. ${failure.sentence}`;
  const id = addErrorToast(message, err, 0, { label: 'Copy text', keep: true, run: () => {
    void copyToClipboard(text.body).then((copied) => {
      if (!copied) showToastText(id, `${message} The text could not be copied, so it is below.`, text.body);
    });
  } });
}
