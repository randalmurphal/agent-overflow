// A ticket's Work (docs/specs/issues.md, "Ticket"): its pull requests and
// branches as its computer reads them, the thread of an Ask, and a Start
// work run. Everything here is held by the ticket's key and goes when the
// ticket's last surface lets it go (`onTrackerTicketClosed`), but an Ask
// in flight: its guard is its request's from send to settle, so a second
// Ask of the ticket from any surface sends nothing meanwhile.
//
// The Work read is the ticket's computer's answer, newest read wins; it is
// read when Work shows, on the ticket's Refresh and after a Start work
// thread is made, and does not follow pull requests or branches live.
//
// A Start work run is its plan, the thread it made and each step's
// outcome. The steps run in order: the thread (StartTicketThread, which
// undoes what it made when it fails, so on its failure nothing else runs),
// then the move, then the assignment, decided from the ticket as the move
// left it (after a move whose outcome is unknown, by Try again), then
// saving the defaults. Nothing is done twice: Try again runs the steps not
// applied, and its thread call answers the Start work thread the computer
// already made for the ticket, project and branch, so a reply that never
// came never makes a second one; a tracker step is decided only from a
// read of the ticket that succeeded after the step before it, sends nothing
// when the ticket already holds what it would set, and is not sent when
// that read failed; and the write store sends nothing for a target with a
// write out.
// The move and the assignment are the write store's writes, and what
// became of each is shown by its rule (trackerTicketWrites.svelte.ts):
// under the controls of its target on a shown surface (the Status and
// Assignee fields), else in a toast. A step's outcome here (`written`) is
// what the run decides by, with the write the store kept for it; Work's
// row of the step shows that write only while it is its target's latest,
// so a later write of the target by anything else never shows there.

import { SvelteMap } from 'svelte/reactivity';
import type { FieldValueView, Setup } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { GetTicketWork, StartTicketThread, type TicketWork } from './bindings';
import { editTrackerSetup } from './trackerSetup.svelte';
import { withStartWorkDefaults, type DefaultsPlace } from './startWorkDefaults';
import { mountThreadForToolPane } from './panes.svelte';
import { revealProject } from './sidebarEntryContinuity.svelte';
import { threadRowFrameMark } from './threadRowFrames';
import { prependThreadReply } from './threads.svelte';
import { addErrorToast } from './toast.svelte';
import { onTrackerTicketClosed, refreshTrackerTicket, trackerTicketSettled, type TrackerTicketRef, type TrackerTicketState } from './trackerTicket.svelte';
import { editTicketFields, fieldTarget, moveTicket, type TicketWriteContext } from './trackerTicketWrites.svelte';
import { refusalSentence, type WriteOutcome } from './trackerWriteOutcome';
import type { TrackerRefreshNotice } from './trackerBoard.svelte';
import { hasScope } from '../transport/scopes';
import { isTransportClassError } from './transportStatus.svelte';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { withBackendTarget } from '../transport/backends';
import type { Thread } from '../types/models';
import { userFacingError } from '../utils/userFacingError';

/** The Work read of one ticket. */
export interface TicketWorkRead {
  /** The last answer; null until one arrives. */
  readonly work: TicketWork | null;
  readonly loading: boolean;
  /** The last read failed: why; null otherwise. */
  readonly error: unknown;
  /** This device may not read git on the ticket's computer. */
  readonly denied: boolean;
}

interface HeldRead extends TicketWorkRead {
  readonly request: number;
}

const reads = new SvelteMap<string, HeldRead>();
let nextRequest = 0;

/** The ticket's Work as last read; reactive. */
export function ticketWork(key: string): TicketWorkRead | null {
  return reads.get(key) ?? null;
}

/** Reads the ticket's pull requests and branches on its computer. A newer
 * read supersedes this one's answer; the last answer shows meanwhile. A
 * dropped connection keeps it and says nothing: the next read is the
 * reconnect's Refresh or the next open. */
export async function loadTicketWork(key: string, ref: TrackerTicketRef): Promise<void> {
  const held = reads.get(key);
  if (!hasScope('git:operate', ref.backend)) {
    reads.set(key, { request: ++nextRequest, work: null, loading: false, error: null, denied: true });
    return;
  }
  const request = ++nextRequest;
  reads.set(key, { request, work: held?.work ?? null, loading: true, error: null, denied: false });
  const write = (next: Omit<HeldRead, 'request' | 'denied'>) => {
    if (reads.get(key)?.request === request) reads.set(key, { request, denied: false, ...next });
  };
  try {
    const work = await withBackendTarget(ref.backend, () => GetTicketWork(ref.connection, ref.issue));
    write({ work, loading: false, error: null });
  } catch (err) {
    const work = reads.get(key)?.work ?? null;
    write({ work, loading: false, error: isPassiveConnectionFailure(err) ? null : err });
  }
}

const asking = new SvelteMap<string, true>();

/** An Ask is being made for the ticket; reactive. */
export function ticketAskPending(key: string): boolean {
  return asking.has(key);
}

/** Ask: a thread in `project` linked to the ticket, with the ticket's file
 * attached to its draft and nothing else written, opened in a pane.
 * Nothing on the ticket changes. A failure leaves nothing and says why. */
export async function askAboutTicket(t: TicketWriteContext, project: string): Promise<void> {
  if (asking.has(t.key)) return;
  asking.set(t.key, true);
  const mark = threadRowFrameMark();
  try {
    const made = await StartTicketThread(project, { connectionId: t.ref.connection, issueId: t.ref.issue, origin: 'ask' });
    await openTicketThread(prependThreadReply(made, mark));
  } catch (err) {
    addErrorToast(`Could not start a thread on ${t.label}: ${userFacingError(err)}`, err);
  } finally {
    asking.delete(t.key);
  }
}

// ---- Start work ----

/** The prefix of a ticket key ("WEB" of "WEB-1"). */
export function keyPrefix(key: string): string {
  const dash = key.lastIndexOf('-');
  return dash > 0 ? key.slice(0, dash) : key;
}

/** What Start work does, as the form showed it. */
export interface StartWorkPlan {
  /** The project the thread runs in. */
  readonly repository: string;
  /** The status to move the ticket to; null for no move. */
  readonly status: { readonly id: string; readonly name: string } | null;
  /** Assign the ticket to `me`. */
  readonly assign: boolean;
  /** The connection's verified account; empty when it is not verified. */
  readonly me: string;
  readonly branch: string;
  /** Save these as the defaults: in `project`'s setup at `place`, the
   * branch as `branchTemplate`, or the stored one kept when null. */
  readonly save: {
    readonly project: string;
    readonly place: DefaultsPlace;
    readonly branchTemplate: string | null;
  } | null;
}

export type StartWorkStep = 'thread' | 'move' | 'assign' | 'defaults';

/** Set on the outcome of a step that was a tracker write: the write store
 * shows what became of it (under its target's controls on a shown
 * surface, else in a toast), so the step's own `text` is not shown.
 * `target` is the write's, and `outcome` the one its call answered: the
 * store holds that object as long as it keeps this write's outcome. */
interface Written {
  readonly written?: { readonly target: string; readonly outcome: WriteOutcome };
}

/** What a running step is doing: reading the ticket it decides from, or
 * sending its change (a tracker write, or the defaults). */
export type StepActivity = 'reading' | 'saving' | 'making';

/** A step's outcome as the form shows it. */
export type StepOutcome =
  | { readonly kind: 'waiting' | 'skipped' }
  | { readonly kind: 'running'; readonly doing: StepActivity }
  | ({ readonly kind: 'applied'; readonly note: string | null } & Written)
  | { readonly kind: 'already'; readonly text: string }
  /** The tracker, git or the setup refused it: `text` says why. */
  | ({ readonly kind: 'refused'; readonly text: string } & Written)
  /** It was sent and its outcome is not known. */
  | ({ readonly kind: 'unknown'; readonly text: string } & Written)
  /** It was not sent, or not run: `text` says why. */
  | ({ readonly kind: 'not_sent'; readonly text: string } & Written)
  | ({ readonly kind: 'failed'; readonly text: string; readonly error: unknown } & Written);

export interface StartWorkRun {
  readonly plan: StartWorkPlan;
  /** The thread the run made; set once, never made again. */
  readonly thread: Thread | null;
  readonly steps: Readonly<Record<StartWorkStep, StepOutcome>>;
  readonly running: boolean;
}

const runs = new SvelteMap<string, StartWorkRun>();

/** The ticket's Start work run that has not ended; reactive. */
export function startWorkRun(key: string): StartWorkRun | null {
  return runs.get(key) ?? null;
}

/** Ends a run the person is done with: its thread stays. */
export function dismissStartWorkRun(key: string): void {
  if (runs.get(key)?.running === false) runs.delete(key);
}

const SETTLED: ReadonlySet<StepOutcome['kind']> = new Set(['applied', 'already', 'skipped']);

/** A step the person may try again. */
export function stepRetryable(outcome: StepOutcome): boolean {
  return !SETTLED.has(outcome.kind) && outcome.kind !== 'waiting' && outcome.kind !== 'running';
}

function setRun(key: string, run: StartWorkRun): void {
  runs.set(key, run);
}

function setStep(key: string, step: StartWorkStep, outcome: StepOutcome): void {
  const run = runs.get(key);
  if (run) setRun(key, { ...run, steps: { ...run.steps, [step]: outcome } });
}

/** The outcome as a step's of a tracker write to `target` whose call
 * answered `outcome`. */
function writeStep(t: TicketWriteContext, target: string, outcome: WriteOutcome | null, what: string): StepOutcome {
  if (outcome === null) return { kind: 'not_sent', text: `A change of ${t.label}'s ${what} is already being saved; try again once it ends.` };
  const written = { target, outcome };
  switch (outcome.kind) {
    case 'applied':
      return { kind: 'applied', note: outcome.note, written };
    case 'refused':
      return { kind: 'refused', text: refusalSentence(outcome.refusal, (id) => id), written };
    case 'unknown':
    case 'lost':
      return { kind: 'unknown', text: outcome.message, written };
    case 'not_sent':
      return { kind: 'not_sent', text: outcome.message, written };
    case 'failed':
      return { kind: 'failed', text: userFacingError(outcome.error), error: outcome.error, written };
  }
}

async function moveStep(t: TicketWriteContext, plan: StartWorkPlan, state: TrackerTicketState | null): Promise<StepOutcome> {
  const status = plan.status;
  if (!status) return { kind: 'skipped' };
  const detail = state?.detail;
  if (!detail) return { kind: 'not_sent', text: `${t.label} has not been read, so it was not moved.` };
  if (detail.card.status.id === status.id) return { kind: 'already', text: `Already in ${status.name}.` };
  const move = detail.moves.find((m) => m.to.id === status.id);
  if (!move) return { kind: 'not_sent', text: `${t.label} has no move to ${status.name} from ${detail.card.status.name || 'its status'} now.` };
  return writeStep(t, 'status', await moveTicket(t, move.id), 'status');
}

async function assignStep(t: TicketWriteContext, plan: StartWorkPlan, state: TrackerTicketState | null): Promise<StepOutcome> {
  if (!plan.assign) return { kind: 'skipped' };
  if (!plan.me) return { kind: 'not_sent', text: 'Its connection has no verified account, so who you are on the tracker is not known. Verify the connection in Settings > Issues.' };
  const detail = state?.detail;
  if (!detail) return { kind: 'not_sent', text: `${t.label} has not been read, so it was not assigned.` };
  if (detail.card.assignee?.id === plan.me) return { kind: 'already', text: 'Already yours.' };
  const value = { kind: 'user', choices: [{ id: plan.me }] } as FieldValueView;
  const target = fieldTarget('assignee');
  return writeStep(t, target, await editTicketFields(t, target, [{ field: 'assignee', value }]), 'assignee');
}

/** The setup with the Start work defaults of `plan` saved at its place:
 * the branch template kept when the plan's branch names none, and naming
 * the branch after the ticket turned on when it names one. */
function withPlanDefaults(setup: Setup, plan: StartWorkPlan): Setup {
  const save = plan.save;
  if (!save) return setup;
  const named = save.branchTemplate !== null;
  return withStartWorkDefaults(setup, save.place, save.project, {
    status: plan.status?.id ?? null,
    assign: plan.assign,
    branch: save.branchTemplate,
    nameBranch: named ? true : undefined,
    repository: plan.repository,
  });
}

async function defaultsStep(plan: StartWorkPlan): Promise<StepOutcome> {
  const save = plan.save;
  if (!save) return { kind: 'skipped' };
  try {
    const answer = await editTrackerSetup(save.project, (setup) => withPlanDefaults(setup, plan));
    if (answer.kind === 'refused') return { kind: 'refused', text: `The defaults were not saved: ${answer.sentence}.` };
    return { kind: 'applied', note: save.branchTemplate === null ? 'The branch name does not hold the ticket\'s key, so the branch pattern saved before stays.' : null };
  } catch (err) {
    return { kind: 'failed', text: userFacingError(err), error: err };
  }
}

/** Why the ticket as `state` holds it is not a read that succeeded since
 * the Refresh that answered `notice` (null when nothing refreshed); null
 * when it is. Refresh answers success for a read the tracker failed and
 * tells that failure on the ticket, so the ticket's own state decides. */
function unreadReason(notice: TrackerRefreshNotice | null, state: TrackerTicketState | null): string | null {
  if (notice) return notice.kind === 'failed' ? userFacingError(notice.error) : notice.message;
  if (!state?.detail) return 'its computer has not read it.';
  if (state.failure) return state.failure.message;
  if (state.readError) return userFacingError(state.readError);
  return null;
}

/** The steps of a run whose thread call failed. A failure of the wire
 * (`isTransportClassError`: the connection dropped, or the call timed out
 * while the computer may still hold it) leaves the thread unknown: the
 * computer may have made it, and Try again asks for it again, which
 * answers the thread it made. Any other failure is the computer's answer
 * that it made nothing. */
function threadFailed(err: unknown): StartWorkRun['steps'] {
  if (isTransportClassError(err)) {
    const waiting: StepOutcome = { kind: 'not_sent', text: 'Not run yet: the thread is not confirmed.' };
    return {
      thread: { kind: 'unknown', text: 'No answer came back, so the thread may have been made; Try again finds it if it was.' },
      move: waiting, assign: waiting, defaults: waiting,
    };
  }
  const notRun: StepOutcome = { kind: 'not_sent', text: 'Not run: the thread was not made, so nothing on the ticket changed.' };
  return { thread: { kind: 'failed', text: userFacingError(err), error: err }, move: notRun, assign: notRun, defaults: notRun };
}

/** Runs Start work for the ticket by `plan`: `read` is the ticket as its
 * surface holds it, read again before the tracker steps. */
export function startWork(t: TicketWriteContext, plan: StartWorkPlan, read: () => TrackerTicketState | null): Promise<void> {
  const held = runs.get(t.key);
  if (held?.running) return Promise.resolve();
  const waiting: StepOutcome = { kind: 'waiting' };
  setRun(t.key, { plan, thread: null, running: false, steps: { thread: waiting, move: waiting, assign: waiting, defaults: waiting } });
  return runSteps(t, read);
}

/** Runs the steps of the ticket's run that did not settle again, with the
 * thread it made. */
export function retryStartWork(t: TicketWriteContext, read: () => TrackerTicketState | null): Promise<void> {
  const run = runs.get(t.key);
  if (!run || run.running) return Promise.resolve();
  return runSteps(t, read);
}

async function runSteps(t: TicketWriteContext, read: () => TrackerTicketState | null): Promise<void> {
  const key = t.key;
  const start = runs.get(key);
  if (!start) return;
  const owed = (step: StartWorkStep) => !SETTLED.has(start.steps[step].kind);
  setRun(key, {
    ...start,
    running: true,
    steps: Object.fromEntries((Object.keys(start.steps) as StartWorkStep[]).map((s) => [s, owed(s) ? { kind: 'waiting' } : start.steps[s]])) as StartWorkRun['steps'],
  });
  const plan = start.plan;
  // The run is the store's until it ends: a closed ticket drops it.
  const mine = () => runs.get(key)?.plan === plan;
  let thread = start.thread;
  if (!thread) {
    setStep(key, 'thread', { kind: 'running', doing: 'making' });
    const mark = threadRowFrameMark();
    try {
      const made = await StartTicketThread(plan.repository, { connectionId: t.ref.connection, issueId: t.ref.issue, origin: 'start_work', branch: plan.branch });
      thread = prependThreadReply(made, mark);
    } catch (err) {
      if (!mine()) return;
      setRun(key, { ...runs.get(key)!, running: false, steps: threadFailed(err) });
      return;
    }
    if (!mine()) return;
    setRun(key, { ...runs.get(key)!, thread });
    setStep(key, 'thread', { kind: 'applied', note: null });
  }
  // The move settles before the assignment is decided, and each decision
  // rests on a read of the ticket that succeeded after the step before it:
  // the Refresh here, then the read the move's own write made. No read is
  // known to follow a move whose outcome is unknown, so nothing is decided
  // after it in this run; Try again starts from a Refresh.
  const wantMove = owed('move') && plan.status !== null;
  const wantAssign = owed('assign') && plan.assign && plan.me !== '';
  const unsent = (reason: string | null): StepOutcome | null =>
    reason === null ? null : { kind: 'not_sent', text: `${t.label} could not be read again, so this was not sent: ${reason}` };
  // Why the ticket as `read()` holds it cannot decide the next step; null
  // when it can.
  // Each tracker step reads while the read it decides from is out, and
  // saves once that read answered; the assignment waits while the move
  // runs.
  let undecided: StepOutcome | null = null;
  if (wantMove || wantAssign) {
    setStep(key, wantMove ? 'move' : 'assign', { kind: 'running', doing: 'reading' });
    const notice = await refreshTrackerTicket(key);
    await trackerTicketSettled(key);
    undecided = unsent(unreadReason(notice, read()));
  }
  if (owed('move')) {
    if (wantMove && !undecided) setStep(key, 'move', { kind: 'running', doing: 'saving' });
    const move = wantMove && undecided ? undecided : await moveStep(t, plan, read());
    if (!mine()) return;
    setStep(key, 'move', move);
    if (move.kind === 'applied') {
      if (wantAssign) setStep(key, 'assign', { kind: 'running', doing: 'reading' });
      await trackerTicketSettled(key);
      undecided = unsent(unreadReason(null, read()));
    } else if (move.kind === 'unknown') {
      undecided = { kind: 'not_sent', text: 'Not decided: the move is not confirmed. Try again reads the ticket first.' };
    }
  }
  if (owed('assign')) {
    if (wantAssign && !undecided) setStep(key, 'assign', { kind: 'running', doing: 'saving' });
    const assign = wantAssign && undecided ? undecided : await assignStep(t, plan, read());
    if (!mine()) return;
    setStep(key, 'assign', assign);
  }
  if (owed('defaults')) {
    setStep(key, 'defaults', { kind: 'running', doing: 'saving' });
    const saved = await defaultsStep(plan);
    if (!mine()) return;
    setStep(key, 'defaults', saved);
  }
  const done = runs.get(key)!;
  setRun(key, { ...done, running: false });
  void loadTicketWork(key, t.ref);
  if (!Object.values(done.steps).every((s) => SETTLED.has(s.kind))) return;
  runs.delete(key);
  await openTicketThread(thread);
}

/** Opens a thread of the ticket in a new pane, since the surface that
 * asked may be gone by now; a failure says why. */
export async function openTicketThread(thread: Thread): Promise<void> {
  try {
    if (thread.projectId) revealProject(thread.projectId);
    await mountThreadForToolPane(thread, null);
  } catch (err) {
    addErrorToast(`Could not open the thread: ${userFacingError(err)}`, err);
  }
}

onTrackerTicketClosed((key) => {
  reads.delete(key);
  runs.delete(key);
});

export function resetTicketWorkForTest(): void {
  reads.clear();
  asking.clear();
  runs.clear();
  nextRequest = 0;
}
