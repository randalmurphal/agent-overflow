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

/**
 * Loss consent: Save anyway, a write sent again accepting what a rich
 * text change would lose. What must hold, for every write a loss can
 * refuse, on every screen:
 * - Save anyway is offered only for the draft that was sent and refused,
 *   as it stands then;
 * - any change to that draft, its replacement, or Cancel withdraws the
 *   offer, so what is sent next is checked again;
 * - a draft takes no change while its write is out: every input of it is
 *   disabled while its target's write is pending, so what a write sends
 *   is what its editor shows.
 * A draft can still go while its write is out (its editor closed, which
 * keeps the write), and a new editor holds a new draft. So a store takes
 * the draft's revision when it sends (`sent`) and offers Save anyway only
 * when the target still holds that revision once the refusal lands
 * (`offers`); every change of a draft, Cancel and a closed editor among
 * them, is `revise`, which gives it a new revision and withdraws the
 * offer through `withdraw`, the store's.
 */
export class LossConsent {
  readonly #revisions = new Map<string, number>();
  #next = 1;
  readonly #withdraw: (key: string) => void;

  constructor(withdraw: (key: string) => void) {
    this.#withdraw = withdraw;
  }

  /** The draft of `key` changed, was replaced, or went. */
  revise(key: string): void {
    this.#revisions.set(key, this.#next++);
    this.#withdraw(key);
  }

  /** The draft's revision as a write of it is sent. */
  sent(key: string): number {
    return this.#revisions.get(key) ?? 0;
  }

  /** Whether `outcome` of a write of `key` sent at revision `sent`
   * offers Save anyway. */
  offers(key: string, sent: number, outcome: WriteOutcome, acceptedLoss: boolean): boolean {
    return outcome.kind === 'refused' && outcome.refusal.reason === 'loss' && !acceptedLoss && this.sent(key) === sent;
  }

  clear(): void {
    this.#revisions.clear();
  }
}

/** Sends a write once on `backend`, the computer holding its connection,
 * and answers its outcome; `computer` names that computer in the sentence
 * of an answer the connection lost. Nothing retries. */
export async function sendWrite(
  backend: string,
  computer: string,
  subject: WriteSubject,
  send: () => Promise<WriteView>,
): Promise<WriteOutcome> {
  try {
    return writeOutcome(await withBackendTarget(backend, send), subject);
  } catch (err) {
    return err instanceof DisconnectedError
      ? { kind: 'lost', message: lostWriteSentence(computer, subject) }
      : { kind: 'failed', error: err };
  }
}
