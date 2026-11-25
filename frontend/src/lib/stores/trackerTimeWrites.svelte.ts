// Every write of logged time (docs/architecture/tracker-backend.md,
// "Writes"): a new worklog, a change to one, a delete. Each goes through
// its bound method once: one write may be pending per connection and
// target (a new worklog of one ticket and day, or one worklog), a second
// press of a pending target sends nothing, and nothing retries on its
// own. A refusal for what a change to a Jira worklog's comment would lose
// offers Save anyway, the same change accepting the loss.
//
// The week changes only from what the computer answers. A write that was
// made stored its days on the computer, so the weeks holding them read
// the computer's copy again; a write refused as stale, or whose answer
// says nothing certain (unknown, not sent, lost), refreshes them from the
// tracker, so the person sees what it holds before trying again.
//
// A write's outcome is held here under its target, so a surface that
// closed while it ran does not lose it: a surface that shows a target
// holds it (`holdTimeWriteTarget`), and an outcome that settles with no
// surface holding its target becomes a toast unless it was made. An
// outcome goes when the last surface holding its target lets go.

import { SvelteMap } from 'svelte/reactivity';
import type { WorklogView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { AddTrackerWorklog, DeleteTrackerWorklog, EditTrackerWorklog } from './bindings';
import { addErrorToast, addToast } from './toast.svelte';
import { refreshTrackerWorklogDays, rereadTrackerWorklogDays } from './trackerTime.svelte';
import { refusalSentence, sendWrite, type WriteOutcome, type WriteSubject } from './trackerWriteOutcome';
import { errString } from '../utils/errors';

const WORKLOG_SUBJECT: WriteSubject = {
  look: "the day's logged time",
  shows: 'The week',
  ended: 'the week no longer shows its time',
};

/** The connection a write is made on, and whose time it is. */
export interface TimeWriteContext {
  readonly backend: string;
  readonly connection: string;
  /** The person's account ID, whose weeks show the write. */
  readonly author: string;
  /** The computer's name, for an answer the connection lost. */
  readonly computer: string;
}

export type TimeWriteKind = 'add' | 'edit' | 'delete';

/** A target's last write. */
export interface TimeWrite {
  readonly kind: TimeWriteKind;
  readonly pending: boolean;
  /** How it ended; null while pending. */
  readonly outcome: WriteOutcome | null;
  /** On a refusal for what a comment change would lose: sends the same
   * change accepting the loss. */
  readonly saveAnyway: (() => Promise<WriteOutcome | null>) | null;
}

const writes = new SvelteMap<string, TimeWrite>();
const holds = new Map<string, number>();

/** A new worklog's target: one ticket's time on one day. */
export function addTarget(issue: string, day: string): string {
  return `add:${issue}:${day}`;
}

export function worklogTarget(id: string): string {
  return `worklog:${id}`;
}

function writeKey(ctx: TimeWriteContext, target: string): string {
  return `${ctx.backend}\n${ctx.connection}\n${target}`;
}

/** The target's last write; reactive. */
export function timeWrite(ctx: TimeWriteContext, target: string): TimeWrite | null {
  return writes.get(writeKey(ctx, target)) ?? null;
}

/** Drops a settled outcome the person read. */
export function dismissTimeWrite(ctx: TimeWriteContext, target: string): void {
  const k = writeKey(ctx, target);
  if (writes.get(k)?.pending === false) writes.delete(k);
}

/** A surface shows the target's outcome until the release. */
export function holdTimeWriteTarget(ctx: TimeWriteContext, target: string): () => void {
  const k = writeKey(ctx, target);
  holds.set(k, (holds.get(k) ?? 0) + 1);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const n = (holds.get(k) ?? 1) - 1;
    if (n > 0) {
      holds.set(k, n);
      return;
    }
    holds.delete(k);
    if (writes.get(k)?.pending === false) writes.delete(k);
  };
}

/** What an outcome says, as one sentence; empty for a plain success. */
export function timeWriteText(outcome: WriteOutcome): string {
  switch (outcome.kind) {
    case 'applied':
      return outcome.note ?? '';
    case 'refused':
      return outcome.refusal.reason === 'stale'
        ? 'This entry changed on the tracker since it was read. The week was read again; look at it before you change it.'
        : refusalSentence(outcome.refusal, (field) => field);
    case 'unknown':
    case 'not_sent':
    case 'lost':
      return outcome.message;
    case 'failed':
      return errString(outcome.error);
  }
}

async function write(
  ctx: TimeWriteContext,
  target: string,
  kind: TimeWriteKind,
  label: string,
  days: readonly string[],
  send: (acceptLoss: boolean) => Promise<WriteView>,
  acceptLoss: boolean,
): Promise<WriteOutcome | null> {
  const k = writeKey(ctx, target);
  if (writes.get(k)?.pending) return null;
  writes.set(k, { kind, pending: true, outcome: null, saveAnyway: null });
  const outcome = await sendWrite(ctx.backend, ctx.computer, WORKLOG_SUBJECT, () => send(acceptLoss));
  switch (outcome.kind) {
    case 'applied':
      rereadTrackerWorklogDays(ctx.backend, ctx.connection, ctx.author, days);
      break;
    case 'refused':
      if (outcome.refusal.reason === 'stale') refreshTrackerWorklogDays(ctx.backend, ctx.connection, ctx.author, days);
      break;
    case 'unknown':
    case 'not_sent':
    case 'lost':
      refreshTrackerWorklogDays(ctx.backend, ctx.connection, ctx.author, days);
      break;
    case 'failed':
      break;
  }
  if (!holds.has(k)) {
    writes.delete(k);
    toastUnheld(label, outcome);
    return outcome;
  }
  if (outcome.kind === 'applied' && outcome.note === null) {
    writes.delete(k);
    return outcome;
  }
  const loss = outcome.kind === 'refused' && outcome.refusal.reason === 'loss' && !acceptLoss;
  writes.set(k, { kind, pending: false, outcome, saveAnyway: loss ? () => write(ctx, target, kind, label, days, send, true) : null });
  return outcome;
}

/** A write that settled with no surface showing it: said in a toast
 * unless it was made. */
function toastUnheld(label: string, outcome: WriteOutcome): void {
  if (outcome.kind === 'applied') return;
  if (outcome.kind === 'failed') {
    addErrorToast(`${label} was not confirmed`, outcome.error);
    return;
  }
  addToast('error', `${label}: ${timeWriteText(outcome)}`);
}

/** A new worklog to log. */
export interface NewWorklog {
  readonly issue: string;
  /** The ticket's key, for a toast; empty when the tracker did not name it. */
  readonly key: string;
  readonly day: string;
  readonly spentSeconds: number;
  readonly description: string;
}

/** Logs time on a ticket (AddTrackerWorklog). */
export function addWorklog(ctx: TimeWriteContext, w: NewWorklog): Promise<WriteOutcome | null> {
  return write(ctx, addTarget(w.issue, w.day), 'add', `The time logged on ${w.key || 'a ticket'}`, [w.day], () =>
    AddTrackerWorklog(ctx.connection, { issue: w.issue, day: w.day, spent_seconds: w.spentSeconds, description: w.description || undefined }), false);
}

/** The members of a worklog a change sets; each one absent stays. */
export interface WorklogChange {
  readonly day?: string;
  readonly spentSeconds?: number;
  readonly description?: string;
}

/** Changes a worklog as read (EditTrackerWorklog). */
export function editWorklog(ctx: TimeWriteContext, from: WorklogView, change: WorklogChange): Promise<WriteOutcome | null> {
  const days = change.day && change.day !== from.day ? [from.day, change.day] : [from.day];
  return write(ctx, worklogTarget(from.id), 'edit', `The change to the time on ${from.key || 'a ticket'}`, days, (acceptLoss) =>
    EditTrackerWorklog(ctx.connection, {
      from,
      day: change.day,
      spent_seconds: change.spentSeconds,
      description: change.description,
      accept_loss: acceptLoss || undefined,
    }), false);
}

/** Deletes a worklog as read (DeleteTrackerWorklog). */
export function deleteWorklog(ctx: TimeWriteContext, from: WorklogView): Promise<WriteOutcome | null> {
  return write(ctx, worklogTarget(from.id), 'delete', `The delete of the time on ${from.key || 'a ticket'}`, [from.day], () =>
    DeleteTrackerWorklog(ctx.connection, { from }), false);
}

export function resetTrackerTimeWritesForTest(): void {
  writes.clear();
  holds.clear();
}
