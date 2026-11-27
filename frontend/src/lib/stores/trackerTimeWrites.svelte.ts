// Every write of logged time (docs/architecture/tracker-backend.md,
// "Writes"): a new worklog, a change to one, a delete. Each goes through
// its bound method once: one write may be pending per connection and
// target (a new worklog of one ticket and day, or one worklog), a second
// press of a pending target sends nothing, and nothing retries on its
// own. A refusal for what a change to a Jira worklog's comment would lose
// offers Save anyway, the same change accepting the loss, under
// `trackerWriteOutcome.ts#LossConsent`: the form that holds the draft
// revises its target on every change of it and when it closes
// (`reviseTimeDraft`).
//
// What a write does to the weeks is the weeks' store's
// (trackerTime.svelte.ts, which loads with the Time screen and takes the
// writes here, `takeWorklogWrites`), so a surface that only logs time
// does not load them: a write that was made asks for the computer's copy
// of its days again; one refused as stale, or whose answer says nothing
// certain (unknown, not sent, lost), asks for its days from the tracker,
// and a stale refusal says how that read ended. A write that was made
// while its days could not be read after it ("Saved. The week shows it
// after its next read") is a receipt on the weeks that show its days,
// or a toast when none does.
//
// A write's outcome is held here under its target, so a surface that
// closed while it ran does not lose it: a surface that shows a target
// holds it (`holdTimeWriteTarget`), and an outcome that settles with no
// surface holding its target becomes a toast unless it was made. An
// outcome goes when the last surface holding its target lets go.
//
// The changes of an agent's draft go through here too, under the same
// rules: a change of one entry (its ticket, hours or reason, or a
// question left out) and its removal under the entry (`entryTarget`), an
// approval or a discard of several under what it covers (`draftTarget`:
// a day, the week shown, a call's card). Nothing of them goes to the
// tracker but an approval (`PostTrackerTimeEntries`), whose sentence, in
// the tracker's words, is held as its outcome until the surface lets go.
// What the week and the cards show after one is the computer's
// `tracker:time_entries_updated`, which they read again on.

import { SvelteMap } from 'svelte/reactivity';
import type { TimeEntryEdit, TimeEntryView, WorklogView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { AddTrackerWorklog, DeleteTrackerWorklog, EditTrackerTimeEntry, EditTrackerWorklog, PostTrackerTimeEntries, RemoveTrackerTimeEntries } from './bindings';
import { addErrorToast, addToast } from './toast.svelte';
import { dayInSentence } from './trackerTimeModel';
import { LossConsent, refusalSentence, sendCall, sendWrite, type WriteOutcome, type WriteSubject } from './trackerWriteOutcome';
import { errString } from '../utils/errors';
import { formatTimeSpent } from '../utils/timeSpent';

const WORKLOG_SUBJECT: WriteSubject = {
  look: "the day's logged time",
  shows: 'The week',
  ended: 'the week no longer shows its time',
  // A worklog write also reads its ticket again; the week shows its days.
  reads: (resource) => resource.kind === 'worklogs',
};

/** The connection a change of an agent's draft is made on: the
 * connection is the entries', whose author the computer knows. */
export interface DraftWriteContext {
  readonly backend: string;
  readonly connection: string;
  /** The computer's name, for an answer the connection lost. */
  readonly computer: string;
}

/** The connection a write is made on, and whose time it is. */
export interface TimeWriteContext extends DraftWriteContext {
  /** The person's account ID, whose weeks show the write. */
  readonly author: string;
}

/** What a target's write is: a worklog's log, change or delete; a draft
 * entry's change or removal (a discard is a removal of several); an
 * approval. */
export type TimeWriteKind = 'add' | 'edit' | 'delete' | 'change' | 'remove' | 'post';

/** How the read a write asked of the tracker ended: the weeks holding
 * its days were read again, the read failed (`message` says why), or no
 * week showing them was read (one reads them when it shows). */
export type WeekReread = { readonly kind: 'read' } | { readonly kind: 'failed'; readonly message: string } | { readonly kind: 'later' };

/** A target's last write. */
export interface TimeWrite {
  readonly kind: TimeWriteKind;
  readonly pending: boolean;
  /** How it ended; null while pending. */
  readonly outcome: WriteOutcome | null;
  /** For a stale refusal: the read of its days from the tracker, null
   * while it is out. */
  readonly reread: WeekReread | null;
  /** On a refusal for what a comment change would lose: sends the same
   * change accepting the loss. */
  readonly saveAnyway: (() => Promise<WriteOutcome | null>) | null;
}

/** What the weeks do after a write: `refresh` reads its days from the
 * tracker and answers how that ended; `reread` reads the computer's copy
 * of them; `receipt` puts the sentence on the weeks showing them, false
 * when none does. */
export interface WorklogWeeks {
  refresh(ctx: TimeWriteContext, days: readonly string[]): Promise<WeekReread>;
  reread(ctx: TimeWriteContext, days: readonly string[]): void;
  receipt(ctx: TimeWriteContext, days: readonly string[], sentence: string): boolean;
}

let weeks: WorklogWeeks | null = null;

/** The weeks' store takes the writes when it loads. */
export function takeWorklogWrites(w: WorklogWeeks): void {
  weeks = w;
}

const writes = new SvelteMap<string, TimeWrite>();
const holds = new Map<string, number>();
const consent = new LossConsent((k) => {
  if (writes.get(k)?.saveAnyway) writes.delete(k);
});

/** A new worklog's target: one ticket's time on one day. */
export function addTarget(issue: string, day: string): string {
  return `add:${issue}:${day}`;
}

export function worklogTarget(id: string): string {
  return `worklog:${id}`;
}

function writeKey(ctx: DraftWriteContext, target: string): string {
  return `${ctx.backend}\n${ctx.connection}\n${target}`;
}

/** The target's last write; reactive. */
export function timeWrite(ctx: DraftWriteContext, target: string): TimeWrite | null {
  return writes.get(writeKey(ctx, target)) ?? null;
}

/** Drops a settled outcome the person read. */
export function dismissTimeWrite(ctx: DraftWriteContext, target: string): void {
  const k = writeKey(ctx, target);
  if (writes.get(k)?.pending === false) writes.delete(k);
}

/** The draft of the target changed, or its form closed: Save anyway is
 * no longer offered for what was sent before. */
export function reviseTimeDraft(ctx: TimeWriteContext, target: string): void {
  consent.revise(writeKey(ctx, target));
}

/** A surface shows the target's outcome until the release. */
export function holdTimeWriteTarget(ctx: DraftWriteContext, target: string): () => void {
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

const STALE = 'This entry changed on the tracker since it was read.';

/** What a settled write says, as one sentence; empty for a plain
 * success. */
export function timeWriteText(write: Pick<TimeWrite, 'outcome' | 'reread'>): string {
  const outcome = write.outcome;
  if (!outcome) return '';
  switch (outcome.kind) {
    case 'applied':
      return outcome.note ?? '';
    case 'refused':
      return outcome.refusal.reason === 'stale' ? `${STALE} ${rereadText(write.reread)}` : refusalSentence(outcome.refusal, (field) => field);
    case 'unknown':
    case 'not_sent':
    case 'lost':
      return outcome.message;
    case 'failed':
      return errString(outcome.error);
  }
}

function rereadText(reread: WeekReread | null): string {
  if (reread === null) return 'Reading the week again…';
  switch (reread.kind) {
    case 'read':
      return 'The week was read again; look at it before you change it.';
    case 'failed':
      return `The week could not be read again: ${reread.message}. Refresh it before you change the entry.`;
    case 'later':
      return 'Look at the week before you change it.';
  }
}

/** What a write did, for a receipt or a toast: "1h 30m logged on WEB-1
 * for Nov 11". */
interface WriteLabel {
  readonly done: string;
  readonly toast: string;
}

async function write(
  ctx: TimeWriteContext,
  target: string,
  kind: TimeWriteKind,
  label: WriteLabel,
  days: readonly string[],
  send: (acceptLoss: boolean) => Promise<WriteView>,
  acceptLoss: boolean,
): Promise<WriteOutcome | null> {
  const k = writeKey(ctx, target);
  if (writes.get(k)?.pending) return null;
  // The draft as it is sent: Save anyway is for it alone.
  consent.sending(k);
  writes.set(k, { kind, pending: true, outcome: null, reread: null, saveAnyway: null });
  const outcome = await sendWrite(ctx.backend, ctx.computer, WORKLOG_SUBJECT, () => send(acceptLoss));
  const offer = consent.landed(k, outcome, acceptLoss);
  const stale = outcome.kind === 'refused' && outcome.refusal.reason === 'stale';
  let reread: Promise<WeekReread> | null = null;
  switch (outcome.kind) {
    case 'applied':
      weeks?.reread(ctx, days);
      if (outcome.note !== null && !weeks?.receipt(ctx, days, `${label.done}: ${outcome.note}`)) addToast('info', `${label.done}: ${outcome.note}`);
      break;
    case 'refused':
      if (stale) reread = weeks?.refresh(ctx, days) ?? Promise.resolve({ kind: 'later' });
      break;
    case 'unknown':
    case 'not_sent':
    case 'lost':
      void weeks?.refresh(ctx, days);
      break;
    case 'failed':
      break;
  }
  // A write that was made says so on the weeks; nothing stays here.
  if (outcome.kind === 'applied') {
    writes.delete(k);
    return outcome;
  }
  if (!holds.has(k)) {
    writes.delete(k);
    toastUnheld(label.toast, outcome);
    return outcome;
  }
  const settled: TimeWrite = { kind, pending: false, outcome, reread: null, saveAnyway: offer ? () => write(ctx, target, kind, label, days, send, true) : null };
  writes.set(k, settled);
  if (reread) {
    void reread.then((ended) => {
      // Only the outcome it was read for takes it.
      if (writes.get(k) === settled) writes.set(k, { ...settled, reread: ended });
    });
  }
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
  const text = outcome.kind === 'refused' && outcome.refusal.reason === 'stale' ? `${STALE} ${rereadText({ kind: 'later' })}` : timeWriteText({ outcome, reread: null });
  addToast('error', `${label}: ${text}`);
}

function ticketName(key: string | undefined): string {
  return key || 'a ticket';
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
  const label = {
    done: `${formatTimeSpent(w.spentSeconds)} logged on ${ticketName(w.key)} for ${dayInSentence(w.day)}`,
    toast: `The time logged on ${ticketName(w.key)}`,
  };
  return write(ctx, addTarget(w.issue, w.day), 'add', label, [w.day], () =>
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
  const label = {
    done: `The change to the time on ${ticketName(from.key)} for ${dayInSentence(from.day)}`,
    toast: `The change to the time on ${ticketName(from.key)}`,
  };
  return write(ctx, worklogTarget(from.id), 'edit', label, days, (acceptLoss) =>
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
  const label = {
    done: `The delete of the time on ${ticketName(from.key)} for ${dayInSentence(from.day)}`,
    toast: `The delete of the time on ${ticketName(from.key)}`,
  };
  return write(ctx, worklogTarget(from.id), 'delete', label, [from.day], () =>
    DeleteTrackerWorklog(ctx.connection, { from }), false);
}

const DRAFT_SUBJECT: WriteSubject = {
  look: "the day's time",
  shows: 'The week',
  ended: 'the week no longer shows its time',
  reads: (resource) => resource.kind === 'worklogs',
};

/** A draft entry's target: its change and its removal. */
export function entryTarget(id: number): string {
  return `entry:${id}`;
}

/** An approval or a discard of several entries: of a day
 * (`day:<day>`), the week shown (`week:<monday>`), or a call's card
 * (`call:<thread>:<call>`). */
export function draftTarget(scope: string): string {
  return `draft:${scope}`;
}

function refs(entries: readonly TimeEntryView[]): { id: number; revision: number }[] {
  return entries.map((e) => ({ id: e.id, revision: e.revision }));
}

// One change of the draft: sent once, its outcome held under its target
// while a surface holds it, else a toast. An answer that says nothing the
// surface does not show (`done` answers null) leaves nothing held.
async function draftWrite<T>(
  ctx: DraftWriteContext,
  target: string,
  kind: TimeWriteKind,
  toast: string,
  send: () => Promise<T>,
  done: (value: T) => string | null,
): Promise<WriteOutcome | null> {
  const k = writeKey(ctx, target);
  if (writes.get(k)?.pending) return null;
  writes.set(k, { kind, pending: true, outcome: null, reread: null, saveAnyway: null });
  const sent = await sendCall(ctx.backend, ctx.computer, DRAFT_SUBJECT, send);
  const outcome: WriteOutcome = sent.ok ? { kind: 'applied', note: done(sent.value) } : sent.outcome;
  if (outcome.kind === 'applied' && outcome.note === null) {
    writes.delete(k);
    return outcome;
  }
  if (!holds.has(k)) {
    writes.delete(k);
    if (outcome.kind === 'applied') addToast('info', `${toast}: ${outcome.note}`);
    else toastUnheld(toast, outcome);
    return outcome;
  }
  writes.set(k, { kind, pending: false, outcome, reread: null, saveAnyway: null });
  return outcome;
}

function entryName(e: TimeEntryView): string {
  return e.key ? `the entry on ${e.key} for ${dayInSentence(e.day)}` : `the question for ${dayInSentence(e.day)}`;
}

/** Changes a draft entry as read: its ticket, hours or reason, or leaves
 * a question out (EditTrackerTimeEntry). Nothing is sent to the tracker. */
export function changeDraftEntry(ctx: DraftWriteContext, entry: TimeEntryView, edit: TimeEntryEdit): Promise<WriteOutcome | null> {
  return draftWrite(ctx, entryTarget(entry.id), 'change', `The change of ${entryName(entry)}`,
    () => EditTrackerTimeEntry(ctx.connection, { id: entry.id, revision: entry.revision }, edit), () => null);
}

/** Removes draft entries as read, under `target` (RemoveTrackerTimeEntries):
 * one entry, or a discard. Nothing is sent to the tracker. An entry that
 * changed or was posted since it was read stays, and the outcome says so. */
export function removeDraftEntries(ctx: DraftWriteContext, target: string, entries: readonly TimeEntryView[]): Promise<WriteOutcome | null> {
  return draftWrite(ctx, target, 'remove', entries.length === 1 ? `The removal of ${entryName(entries[0])}` : 'The discard of the draft',
    () => RemoveTrackerTimeEntries(ctx.connection, refs(entries)),
    (gone) => gone.length < entries.length
      ? `${gone.length} of ${entries.length} entries were removed; the others changed or were posted since they were read.`
      : null);
}

/** Approves draft entries as read, under `target` (PostTrackerTimeEntries):
 * a day, the week shown, a call's card, or one entry `alone`, which posts
 * it though time was logged on its ticket that day since. Its outcome is
 * the computer's sentence of what was posted and what was not, and why. */
export function postDraftEntries(ctx: DraftWriteContext, target: string, entries: readonly TimeEntryView[], alone = false): Promise<WriteOutcome | null> {
  return draftWrite(ctx, target, 'post', alone ? `Logging ${entryName(entries[0])}` : 'The approval of the draft',
    () => PostTrackerTimeEntries(ctx.connection, { entries: refs(entries), alone: alone || undefined }), (view) => view.sentence);
}

export function resetTrackerTimeWritesForTest(): void {
  writes.clear();
  holds.clear();
  consent.clear();
}
