import { threadHasScope } from '../transport/entityScopes';
// Worktree setup state, keyed by the owning thread.
//
// The backend runs the project's setup recipe over a worktree it just cut
// (app_worktree_setup.go) and streams every step, its output, and its outcome
// on the `worktree:setup` channel. This store is the frontend projection of
// that stream: one keyed box, so a run streaming into one pane does not
// invalidate every sidebar row.
//
// Two things make it converge with the backend regardless of what a client
// saw:
//
//   - GetThreadWorktreeSetup is a full snapshot, and every event carries the
//     run id it belongs to. A client that missed the whole run hydrates into
//     the same state as one that watched it.
//   - Output chunks carry a monotonic per-run sequence. A chunk below the
//     folded-in high-water mark is a snapshot race and is dropped; a chunk
//     above the next expected one means frames were lost, and the snapshot is
//     re-fetched rather than leaving a hole in the transcript.
//
// Events arriving while a hydration is in flight are buffered and replayed on
// top of the snapshot (snapshotStream.ts). Without that, a `finished` frame
// landing between the backend's read and this store's apply would be
// overwritten by a snapshot that still said "running", and nothing would ever
// correct it.

import type { WorktreeSetupEvent } from '../types/events';
import {
  DismissThreadWorktreeSetup,
  GetThreadWorktreeSetup,
  RetryThreadWorktreeSetup,
} from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { createSnapshotStream } from './snapshotStream';

export type WorktreeSetupState = 'idle' | 'running' | 'failed' | 'succeeded' | 'cancelled';
export type WorktreeSetupStepStatus = 'pending' | 'running' | 'succeeded' | 'failed';

export interface WorktreeSetupStep {
  index: number;
  kind: string;
  label: string;
  argv?: string[];
}

export interface WorktreeSetupView {
  runId: string;
  state: WorktreeSetupState;
  steps: WorktreeSetupStep[];
  stepStatuses: WorktreeSetupStepStatus[];
  output: string;
  /** Highest chunk sequence folded into `output`. */
  outputSeq: number;
  error: string;
  worktreePath: string;
  startedAt: number;
  finishedAt: number;
  /**
   * Local-only: the user dismissed this failure and the backend is retiring
   * it. The panel hides at once; the backend's cancelled frame then drops the
   * view here and in every other client. Survives a hydration of the same
   * run; a different run is a new outcome and shows.
   */
  dismissed: boolean;
}

/** How long a succeeded run's card stays up before clearing itself. */
export const SUCCESS_LINGER_MS = 2500;

const views = createKeyedSignalRegistry<WorktreeSetupView | null>(null);

// Pending success auto-dismissals. The store owns them rather than the panel:
// a run that succeeds while its thread is not on screen has no panel to start
// a timer, and its view would otherwise survive until the thread is opened.
const successExpiry = new Map<string, ReturnType<typeof setTimeout>>();

const stream = createSnapshotStream<WorktreeSetupEvent, unknown>({
  name: 'worktreeSetup',
  read: (threadId) => GetThreadWorktreeSetup(threadId),
  applySnapshot,
  applyFrame: applyEvent,
});

/** Tracked read. Null means this key has nothing to show. */
export function getWorktreeSetup(key: string): WorktreeSetupView | null {
  return views.get(key);
}

/**
 * True when the key's panel should be mounted. Kept here rather than in the
 * pane so the lazy mount can gate on a cheap boolean without pulling the whole
 * view into a component that may never render it.
 *
 * A retained view is exactly a run worth showing — that is what makes this a
 * null check. In particular a `succeeded` view still counts: it is the success
 * acknowledgement, and unmounting on the state flip would take the panel down
 * before it could show it.
 */
export function hasWorktreeSetupSurface(key: string): boolean {
  return views.get(key) !== null;
}

/**
 * Retires a failed run for good. A rejected dismissal shows the failure again
 * so the user is not left believing it is gone.
 */
export async function dismissWorktreeSetup(key: string): Promise<void> {
  const view = views.get(key);
  if (!view || view.state !== 'failed' || view.dismissed) return;
  views.set(key, { ...view, dismissed: true });
  try {
    await DismissThreadWorktreeSetup(key);
  } catch (err) {
    const current = views.get(key);
    if (current?.runId === view.runId && current.dismissed) {
      views.set(key, { ...current, dismissed: false });
    }
    throw err;
  }
}

/**
 * Clears a succeeded run's card SUCCESS_LINGER_MS after the store learns of
 * it, whether or not a panel is mounted. The backend has already dropped its
 * record, so there is nothing to reconcile with. The run id and state guard
 * leave a replacement run, a retry, or a failure alone: neither is the user's
 * to discard by a timeout.
 */
function scheduleSuccessExpiry(key: string, runId: string): void {
  cancelSuccessExpiry(key);
  successExpiry.set(key, setTimeout(() => {
    successExpiry.delete(key);
    const view = views.get(key);
    if (!view || view.runId !== runId || view.state !== 'succeeded') return;
    views.drop(key);
  }, SUCCESS_LINGER_MS));
}

function cancelSuccessExpiry(key: string): void {
  const timer = successExpiry.get(key);
  if (timer === undefined) return;
  clearTimeout(timer);
  successExpiry.delete(key);
}

/**
 * Re-runs the project's recipe. The optimistic local flip to `running` is
 * deliberate: the backend's `started` frame is what fills in the real run, but
 * without this the button would sit dead until it arrives. A rejected retry
 * restores the failure it was launched from, so the Retry affordance never
 * disappears on an error.
 *
 */
export async function retryWorktreeSetup(key: string): Promise<void> {
  const previous = views.get(key);
  if (previous) {
    views.set(key, {
      ...previous,
      state: 'running',
      error: '',
      dismissed: false,
      finishedAt: 0,
    });
  }
  try {
    await RetryThreadWorktreeSetup(key);
  } catch (err) {
    if (previous) views.set(key, previous);
    throw err;
  }
}

/**
 * Pulls the authoritative snapshot and reconciles. Called on pane mount for a
 * thread whose row says a setup ran, on a detected sequence gap, and on a
 * transport gap for the channel.
 */
export async function hydrateWorktreeSetup(threadId: string): Promise<void> {
  if (!threadId) return;
  // The snapshot rides `terminal:operate` — worktree setup runs commands
  // and streams their output. A session without that grant sees neither
  // the transcript nor the retry control, and this runs on pane mount, so
  // asking would be one refusal per open of any thread whose row says a
  // setup ran.
  if (!threadHasScope('terminal:operate', threadId)) return;
  await stream.hydrate(threadId);
}

function applySnapshot(key: string, snapshot: unknown): void {
  const previous = views.get(key);
  const view = viewFromSnapshot(snapshot);
  // A dismissal in flight applies to the run the user dismissed, not to a
  // different run the snapshot reports.
  if (view && previous?.dismissed && previous.runId === view.runId) view.dismissed = true;
  views.set(key, view);
  if (view?.state === 'succeeded') scheduleSuccessExpiry(key, view.runId);
}

/** Fan-in target of eventsWorktreeSetup.ts. */
export function applyWorktreeSetupEvent(evt: WorktreeSetupEvent): void {
  const threadId = evt?.threadId ?? '';
  if (!threadId) return;
  stream.push(threadId, evt);
}

// Starts no read for an event the stream is replaying over a snapshot.
function rehydrate(key: string): void {
  void hydrateWorktreeSetup(key);
}

function applyEvent(key: string, evt: WorktreeSetupEvent): void {
  switch (evt.phase) {
    case 'started': {
      const steps = evt.steps ?? [];
      views.set(key, {
        runId: evt.runId ?? '',
        state: 'running',
        steps,
        stepStatuses: steps.map(() => 'pending' as const),
        output: '',
        outputSeq: 0,
        error: '',
        worktreePath: evt.worktreePath ?? '',
        startedAt: evt.startedAt ?? 0,
        finishedAt: 0,
        dismissed: false,
      });
      return;
    }
    case 'step-started':
      patchStepStatus(key, evt, 'running');
      return;
    case 'step-finished':
      patchStepStatus(key, evt, evt.state === 'failed' ? 'failed' : 'succeeded');
      return;
    case 'output':
      appendOutput(key, evt);
      return;
    case 'finished':
      finishRun(key, evt);
      return;
    default:
      // A phase this client doesn't know is a backend/frontend version skew.
      // Loud, because silently ignoring frames is how a panel gets stuck.
      console.warn(`worktreeSetup: unknown phase "${String(evt.phase)}" for ${key}`);
  }
}

/**
 * Resolves the view an event applies to, or requests a hydration when the
 * event describes a run this client never saw start.
 */
function currentRun(key: string, evt: WorktreeSetupEvent): WorktreeSetupView | null {
  const view = views.get(key);
  if (view && view.runId === evt.runId) return view;
  rehydrate(key);
  return null;
}

function patchStepStatus(
  key: string,
  evt: WorktreeSetupEvent,
  status: WorktreeSetupStepStatus,
): void {
  const view = currentRun(key, evt);
  if (!view) return;
  const index = evt.stepIndex ?? -1;
  if (index < 0 || index >= view.stepStatuses.length) return;
  if (view.stepStatuses[index] === status) return;
  const stepStatuses = view.stepStatuses.slice();
  stepStatuses[index] = status;
  views.set(key, { ...view, stepStatuses });
}

function appendOutput(key: string, evt: WorktreeSetupEvent): void {
  const view = currentRun(key, evt);
  if (!view) return;
  const seq = evt.seq ?? 0;
  // Already folded in by a snapshot that raced this frame.
  if (seq <= view.outputSeq) return;
  if (seq > view.outputSeq + 1) {
    // Frames were lost. Take what we have so the panel keeps moving, and pull
    // the tail — which contains the whole run — to fill the hole.
    rehydrate(key);
  }
  views.set(key, {
    ...view,
    output: view.output + (evt.chunk ?? ''),
    outputSeq: seq,
  });
}

function finishRun(key: string, evt: WorktreeSetupEvent): void {
  const view = currentRun(key, evt);
  if (!view) return;
  const state = normalizeState(evt.state);
  if (state === 'cancelled') {
    // The thread left this worktree or was torn down. Nothing to report.
    views.drop(key);
    return;
  }
  views.set(key, {
    ...view,
    state,
    error: evt.error ?? '',
    finishedAt: evt.finishedAt ?? 0,
  });
  if (state === 'succeeded') scheduleSuccessExpiry(key, view.runId);
}

function normalizeState(state: string | undefined): WorktreeSetupState {
  switch (state) {
    case 'running':
    case 'failed':
    case 'succeeded':
    case 'cancelled':
      return state;
    default:
      return 'idle';
  }
}

function viewFromSnapshot(snapshot: unknown): WorktreeSetupView | null {
  const record = snapshot as Partial<WorktreeSetupView> & { stepStatuses?: string[] } | null;
  if (!record) return null;
  const state = normalizeState(record.state);
  if (state === 'idle' || state === 'cancelled') return null;
  const steps = record.steps ?? [];
  const stepStatuses = (record.stepStatuses ?? []).map(normalizeStepStatus);
  return {
    runId: record.runId ?? '',
    state,
    steps,
    // A snapshot of a durable failure the backend restarted past carries no
    // steps; pad so the two arrays are always index-aligned for the panel.
    stepStatuses: steps.map((_, index) => stepStatuses[index] ?? 'pending'),
    output: record.output ?? '',
    outputSeq: record.outputSeq ?? 0,
    error: record.error ?? '',
    worktreePath: record.worktreePath ?? '',
    startedAt: record.startedAt ?? 0,
    finishedAt: record.finishedAt ?? 0,
    dismissed: false,
  };
}

function normalizeStepStatus(status: string): WorktreeSetupStepStatus {
  switch (status) {
    case 'running':
    case 'succeeded':
    case 'failed':
      return status;
    default:
      return 'pending';
  }
}

/** Test isolation only. */
export function resetWorktreeSetupForTest(): void {
  for (const timer of successExpiry.values()) clearTimeout(timer);
  successExpiry.clear();
  stream.reset();
  views.reset();
}
