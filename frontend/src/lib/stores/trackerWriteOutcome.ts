// What a tracker write's answer means for the person, for every screen
// that writes (docs/architecture/tracker-backend.md, "Writes"): the write
// is sent once, pinned to the computer that holds its connection, and its
// answer, or the connection dropping before it, becomes one outcome. Each
// screen keeps its own pending writes and what an outcome does to what it
// shows; the sentences differ only in what the person looks at to learn
// what became of the write (`WriteSubject`).

import type { RefusalView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { withBackendTarget } from '../transport/backends';
import { DisconnectedError } from '../transport/wsClient';

/** What became of a write, as the person reads it. */
export type WriteOutcome =
  /** Made. `note` says what the screen does not show yet, or null. */
  | { readonly kind: 'applied'; readonly note: string | null }
  /** The tracker, or the computer before sending, refused a part. */
  | { readonly kind: 'refused'; readonly refusal: RefusalView }
  /** A part was sent and not answered in full (`unknown`), or was not
   * sent; `message` says which and where to look. */
  | { readonly kind: 'unknown' | 'not_sent'; readonly message: string }
  /** The call failed: its error says why and what to do. */
  | { readonly kind: 'failed'; readonly error: unknown }
  /** The connection to the computer dropped before the answer. */
  | { readonly kind: 'lost'; readonly message: string };

/** What a screen's write sentences name. */
export interface WriteSubject {
  /** What the person looks at before trying again: "the ticket". */
  readonly look: string;
  /** What shows a made write after its next read: "The ticket". */
  readonly shows: string;
  /** What no longer follows the connection once it was turned off during
   * the write: "this ticket no longer follows it". */
  readonly ended: string;
}

export const TICKET_SUBJECT: WriteSubject = {
  look: 'the ticket',
  shows: 'The ticket',
  ended: 'this ticket no longer follows it',
};

/** A write's answer as its outcome: the first part that was not applied
 * decides; with every part applied, what the computer could not show yet
 * is its note. */
export function writeOutcome(view: WriteView, subject: WriteSubject): WriteOutcome {
  for (const part of view.parts ?? []) {
    if (part.state === 'applied') continue;
    if (part.state === 'refused' && part.refusal) return { kind: 'refused', refusal: part.refusal };
    const kind = part.state === 'not_sent' ? 'not_sent' : 'unknown';
    return { kind, message: part.failure?.message ?? `The tracker did not say what became of this change; look at ${subject.look} before you try again.` };
  }
  if (view.ended) return { kind: 'applied', note: `Saved. Its connection was turned off or removed while saving, so ${subject.ended}.` };
  const stale = view.stale?.[0];
  if (stale) return { kind: 'applied', note: `Saved. ${subject.shows} shows it after its next read: ${stale.failure.message}` };
  return { kind: 'applied', note: null };
}

/** The sentence of a write whose answer a dropped connection lost. */
export function lostWriteSentence(computer: string, subject: WriteSubject): string {
  return `${computer}: the connection dropped before the answer, so the change may or may not have been made; look at ${subject.look} before you try again.`;
}

/** A refusal as one sentence, with what the tracker said of each field,
 * named by `fieldName`. */
export function refusalSentence(refusal: RefusalView, fieldName: (id: string) => string): string {
  const fields = (refusal.fields ?? []).map((f) => `${fieldName(f.field)}: ${f.message}`);
  const lost = refusal.reason === 'loss' && refusal.lost?.length ? ` It would lose: ${refusal.lost.join(', ')}.` : '';
  const said = refusal.message || 'The tracker refused this change.';
  return fields.length > 0 ? `${said} ${fields.join('; ')}${lost}` : `${said}${lost}`;
}

/** Sends a write once on `backend`, the computer holding its connection,
 * and answers its outcome; `computer` names that computer in the sentence
 * of an answer the connection lost. A write sent other than as one call
 * (an upload's bytes) says what its own failures mean through `failure`,
 * which answers null for one it does not know. Nothing retries. */
export async function sendWrite(
  backend: string,
  computer: string,
  subject: WriteSubject,
  send: () => Promise<WriteView>,
  failure?: (err: unknown) => WriteOutcome | null,
): Promise<WriteOutcome> {
  try {
    return writeOutcome(await withBackendTarget(backend, send), subject);
  } catch (err) {
    const known = failure?.(err);
    if (known) return known;
    return err instanceof DisconnectedError
      ? { kind: 'lost', message: lostWriteSentence(computer, subject) }
      : { kind: 'failed', error: err };
  }
}
