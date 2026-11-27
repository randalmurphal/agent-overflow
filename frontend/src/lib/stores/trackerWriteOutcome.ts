// What a tracker write's answer means for the person, for every screen
// that writes (docs/architecture/tracker-backend.md, "Writes"): the write
// is sent once, pinned to the computer that holds its connection, and its
// answer, or no answer coming, becomes one outcome. Each
// screen keeps its own pending writes and what an outcome does to what it
// shows; the sentences differ only in what the person looks at to learn
// what became of the write (`WriteSubject`).

import type { RefusalView, ResourceView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { withBackendTarget } from '../transport/backends';
import { isTransportClassError } from './transportStatus.svelte';

/** What became of a write, as the person reads it. */
export type WriteOutcome =
  /** Made. `note` says what the screen does not show yet, or null. A
   * create the tracker confirmed is made whatever became of its later
   * steps, so it is never sent again; `unplaced` is the first of those
   * that was not applied. */
  | { readonly kind: 'applied'; readonly note: string | null; readonly unplaced?: Unplaced }
  /** The tracker, or the computer before sending, refused a part. */
  | { readonly kind: 'refused'; readonly refusal: RefusalView }
  /** A part was sent and not answered in full (`unknown`), or was not
   * sent; `message` says which and where to look. */
  | { readonly kind: 'unknown' | 'not_sent'; readonly message: string }
  /** The call failed: its error says why and what to do. */
  | { readonly kind: 'failed'; readonly error: unknown }
  /** No answer came: the connection to the computer dropped, or the call
   * timed out while the computer may still hold it. */
  | { readonly kind: 'lost'; readonly message: string };

/** A later step of a confirmed create that was not applied: the new
 * ticket (`key` empty when the computer could not name it), the step
 * (`PartView.step`), and why, as a sentence. */
export interface Unplaced {
  readonly issue: string;
  readonly key: string;
  readonly step: string;
  readonly why: string;
}

/** What a screen's write sentences name. */
export interface WriteSubject {
  /** What the person looks at before trying again: "the ticket". */
  readonly look: string;
  /** What shows a made write after its next read: "The ticket". */
  readonly shows: string;
  /** What no longer follows the connection once it was turned off during
   * the write: "this ticket no longer follows it". */
  readonly ended: string;
  /** Whether the screen shows `resource`: a read after the write that
   * failed for only what it does not show says nothing there. */
  readonly reads: (resource: ResourceView) => boolean;
}

export const TICKET_SUBJECT: WriteSubject = {
  look: 'the ticket',
  shows: 'The ticket',
  ended: 'this ticket no longer follows it',
  reads: () => true,
};

/** A write's answer as its outcome: the first part that was not applied
 * decides; with every part applied, what the computer could not show yet
 * of what the screen shows is its note. */
export function writeOutcome(view: WriteView, subject: WriteSubject): WriteOutcome {
  for (const part of view.parts ?? []) {
    if (part.state === 'applied') continue;
    const unknown = `The tracker did not say what became of this change; look at ${subject.look} before you try again.`;
    if (view.created) {
      const why = part.state === 'refused' && part.refusal ? refusalSentence(part.refusal, (id) => id) : (part.failure?.message ?? unknown);
      const unplaced = { issue: view.created, key: view.created_key ?? '', step: part.step, why };
      return { kind: 'applied', note: unplacedSentence(unplaced, part.state === 'unknown'), unplaced };
    }
    if (part.state === 'refused' && part.refusal) return { kind: 'refused', refusal: part.refusal };
    const kind = part.state === 'not_sent' ? 'not_sent' : 'unknown';
    return { kind, message: part.failure?.message ?? unknown };
  }
  if (view.ended) return { kind: 'applied', note: `Saved. Its connection was turned off or removed while saving, so ${subject.ended}.` };
  if (view.not_on_board) return { kind: 'applied', note: 'Created, but this board does not show it: its filter or its columns leave the new ticket out.' };
  const stale = view.stale?.find((s) => s.resources.some(subject.reads));
  if (stale) return { kind: 'applied', note: `Saved. ${subject.shows} shows it after its next read: ${stale.failure.message}` };
  return { kind: 'applied', note: null };
}

/** What a create's step that was not applied left: the ticket stands,
 * and the step did not happen, or may not have when its answer was lost
 * (`unknown`). Only a move into the sprint follows a create. */
function unplacedSentence(u: Unplaced, unknown: boolean): string {
  const made = `Created ${u.key || 'the ticket'}`;
  return unknown ? `${made}; it may not be in its sprint: ${u.why}` : `${made}, but it was not moved into its sprint: ${u.why}`;
}

/** The sentence of a write whose answer never came. */
export function lostWriteSentence(computer: string, subject: WriteSubject): string {
  return `${computer}: no answer came back, so the change may or may not have been made; look at ${subject.look} before you try again.`;
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
 * keeps the write), and a new editor holds a new draft. So a store marks
 * each send (`sending`) and, once its outcome lands, offers Save anyway
 * only when no change of the draft came while it was out (`landed`).
 * Every change of a draft, Cancel and a closed editor among them, is
 * `revise`, which withdraws an offer already made through `withdraw`, the
 * store's. A target has an entry only while a send of it is out, so the
 * consent holds nothing for drafts that went.
 */
export class LossConsent {
  /** The targets with a send out, and whether their draft changed since. */
  readonly #out = new Map<string, boolean>();
  readonly #withdraw: (key: string) => void;

  constructor(withdraw: (key: string) => void) {
    this.#withdraw = withdraw;
  }

  /** The draft of `key` changed, was replaced, or went. */
  revise(key: string): void {
    if (this.#out.has(key)) this.#out.set(key, true);
    this.#withdraw(key);
  }

  /** A write of `key`'s draft is sent. */
  sending(key: string): void {
    this.#out.set(key, false);
  }

  /** The write of `key` landed with `outcome`: whether it offers Save
   * anyway. */
  landed(key: string, outcome: WriteOutcome, acceptedLoss: boolean): boolean {
    const kept = this.#out.get(key) === false;
    this.#out.delete(key);
    return kept && outcome.kind === 'refused' && outcome.refusal.reason === 'loss' && !acceptedLoss;
  }

  /** The targets it holds an entry for. */
  get size(): number {
    return this.#out.size;
  }

  clear(): void {
    this.#out.clear();
  }
}

/** Sends a write once on `backend`, the computer holding its connection,
 * and answers its outcome; `computer` names that computer in the sentence
 * of an answer that never came. A write sent other than as one call
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
    return sendFailure(err, computer, subject, failure);
  }
}

// What a send's failure means: the caller's reading of it, else a lost
// answer for a transport failure, else the failure itself.
function sendFailure(err: unknown, computer: string, subject: WriteSubject, failure?: (err: unknown) => WriteOutcome | null): WriteOutcome {
  const known = failure?.(err);
  if (known) return known;
  return isTransportClassError(err) ? { kind: 'lost', message: lostWriteSentence(computer, subject) } : { kind: 'failed', error: err };
}

/** Sends a call that answers something other than a write's view (an
 * agent's time draft change) once on `backend`, as `sendWrite` does: its
 * answer, or what its failure means. */
export async function sendCall<T>(
  backend: string,
  computer: string,
  subject: WriteSubject,
  send: () => Promise<T>,
  failure?: (err: unknown) => WriteOutcome | null,
): Promise<{ readonly ok: true; readonly value: T } | { readonly ok: false; readonly outcome: WriteOutcome }> {
  try {
    return { ok: true, value: await withBackendTarget(backend, send) };
  } catch (err) {
    return { ok: false, outcome: sendFailure(err, computer, subject, failure) };
  }
}
