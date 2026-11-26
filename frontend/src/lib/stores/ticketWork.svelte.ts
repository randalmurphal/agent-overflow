// A ticket's Work (docs/specs/issues.md, "Ticket"): its pull requests and
// branches as its computer reads them, the thread of an Ask, and a Start
// work run. Everything here is held by the ticket's key and goes when the
// ticket's last surface lets it go (`onTrackerTicketClosed`).
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
// under the controls of its target on a shown surface, Work's row of the
// step among them, else in a toast. A step's outcome here (`written`) is
// what the run decides by; it carries no sentence of the write's own.

import { SvelteMap } from 'svelte/reactivity';
import type { FieldValueView, Setup, SetupView, StartWorkDefaults } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { GetTicketWork, ProjectTrackerSetup, SetProjectTrackerSetup, StartTicketThread, type TicketWork } from './bindings';
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

/** The Start work defaults' place in a project's setup file: the tracker
 * site and the tracker project, named by the key's prefix. */
export interface DefaultsPlace {
  readonly tracker: string;
  readonly site: string;
  readonly project: string;
}

/** The prefix of a ticket key ("WEB" of "WEB-1"). */
export function keyPrefix(key: string): string {
  const dash = key.lastIndexOf('-');
  return dash > 0 ? key.slice(0, dash) : key;
}

/** The entry of the setup for `place`, if any. */
export function defaultsAt(setup: Setup, place: DefaultsPlace): StartWorkDefaults | null {
  return (setup.start_work ?? []).find((d) => d.tracker === place.tracker && d.site === place.site && d.project === place.project) ?? null;
}

interface DefaultsRead {
  readonly project: string;
  readonly view: SetupView | null;
  readonly loading: boolean;
  readonly error: unknown;
}

const defaultsReads = new SvelteMap<string, DefaultsRead>();

/** The setup file of the project the ticket's Start work defaults are in,
 * as the form read it; reactive. */
export function startWorkDefaults(key: string): DefaultsRead | null {
  return defaultsReads.get(key) ?? null;
}

/** Reads `project`'s setup file for the ticket's Start work form. */
export async function loadStartWorkDefaults(key: string, project: string): Promise<void> {
  defaultsReads.set(key, { project, view: defaultsReads.get(key)?.view ?? null, loading: true, error: null });
  let next: DefaultsRead;
  try {
    next = { project, view: await ProjectTrackerSetup(project), loading: false, error: null };
  } catch (err) {
    next = { project, view: null, loading: false, error: err };
  }
  if (defaultsReads.get(key)?.project === project) defaultsReads.set(key, next);
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
 * surface, else in a toast), so the step's own `text` is not shown. */
interface Written {
  readonly written?: true;
}

/** A step's outcome as the form shows it. */
export type StepOutcome =
  | { readonly kind: 'waiting' | 'running' | 'skipped' }
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

/** A tracker write's outcome as a step's. */
function writeStep(outcome: WriteOutcome | null, what: string, label: string): StepOutcome {
  if (outcome === null) return { kind: 'not_sent', text: `A change of ${label}'s ${what} is already being saved; try again once it ends.` };
  switch (outcome.kind) {
    case 'applied':
      return { kind: 'applied', note: outcome.note, written: true };
    case 'refused':
      return { kind: 'refused', text: refusalSentence(outcome.refusal, (id) => id), written: true };
    case 'unknown':
    case 'lost':
      return { kind: 'unknown', text: outcome.message, written: true };
    case 'not_sent':
      return { kind: 'not_sent', text: outcome.message, written: true };
    case 'failed':
      return { kind: 'failed', text: userFacingError(outcome.error), error: outcome.error, written: true };
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
  return writeStep(await moveTicket(t, move.id), 'status', t.label);
}

async function assignStep(t: TicketWriteContext, plan: StartWorkPlan, state: TrackerTicketState | null): Promise<StepOutcome> {
  if (!plan.assign) return { kind: 'skipped' };
  if (!plan.me) return { kind: 'not_sent', text: 'Its connection has no verified account, so who you are on the tracker is not known. Verify the connection in Settings > Issues.' };
  const detail = state?.detail;
  if (!detail) return { kind: 'not_sent', text: `${t.label} has not been read, so it was not assigned.` };
  if (detail.card.assignee?.id === plan.me) return { kind: 'already', text: 'Already yours.' };
  const value = { kind: 'user', choices: [{ id: plan.me }] } as FieldValueView;
  return writeStep(await editTicketFields(t, fieldTarget('assignee'), [{ field: 'assignee', value }]), 'assignee', t.label);
}

async function defaultsStep(key: string, plan: StartWorkPlan): Promise<StepOutcome> {
  const save = plan.save;
  if (!save) return { kind: 'skipped' };
  // The setup as the form read it, or as the last refusal answered it.
  let view = defaultsReads.get(key)?.project === save.project ? defaultsReads.get(key)?.view ?? null : null;
  try {
    view ??= await ProjectTrackerSetup(save.project);
    const kept = defaultsAt(view.setup, save.place);
    const entry: StartWorkDefaults = {
      ...save.place,
      status: plan.status?.id,
      assign: plan.assign,
      branch: save.branchTemplate ?? kept?.branch,
      repository: plan.repository === save.project ? undefined : plan.repository,
    };
    const others = (view.setup.start_work ?? []).filter((d) => d !== kept);
    const answer = await SetProjectTrackerSetup(save.project, view.revision, { ...view.setup, start_work: [...others, entry] } as Setup);
    defaultsReads.set(key, { project: save.project, view: answer.stored, loading: false, error: null });
    if (!answer.saved) return { kind: 'refused', text: `The defaults were not saved: ${answer.refused ?? 'the setup refused them'}.` };
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
    setStep(key, 'thread', { kind: 'running' });
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
  let undecided: StepOutcome | null = null;
  if (wantMove || wantAssign) {
    if (wantMove) setStep(key, 'move', { kind: 'running' });
    if (wantAssign) setStep(key, 'assign', { kind: 'running' });
    const notice = await refreshTrackerTicket(key);
    await trackerTicketSettled(key);
    undecided = unsent(unreadReason(notice, read()));
  }
  if (owed('move')) {
    const move = wantMove && undecided ? undecided : await moveStep(t, plan, read());
    if (!mine()) return;
    setStep(key, 'move', move);
    if (move.kind === 'applied') {
      await trackerTicketSettled(key);
      undecided = unsent(unreadReason(null, read()));
    } else if (move.kind === 'unknown') {
      undecided = { kind: 'not_sent', text: 'Not decided: the move is not confirmed. Try again reads the ticket first.' };
    }
  }
  if (owed('assign')) {
    const assign = wantAssign && undecided ? undecided : await assignStep(t, plan, read());
    if (!mine()) return;
    setStep(key, 'assign', assign);
  }
  if (owed('defaults')) {
    setStep(key, 'defaults', { kind: 'running' });
    const saved = await defaultsStep(key, plan);
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
  asking.delete(key);
  defaultsReads.delete(key);
  runs.delete(key);
});

export function resetTicketWorkForTest(): void {
  reads.clear();
  asking.clear();
  defaultsReads.clear();
  runs.clear();
  nextRequest = 0;
}
