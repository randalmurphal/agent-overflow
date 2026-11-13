// Composer shell commands in flight, keyed by the thread they run for.
//
// The backend runs one command per thread (app_shell_command.go) and streams
// it on `shell:command`. When the command exits its output is sent as a user
// message, which arrives through the ordinary item and queue streams, so a
// view exists here only while a command is running or after it failed to be
// sent. A stopped or sent run has nothing left to show and is dropped.
//
// GetThreadShellCommands is a full snapshot and output frames carry a per-run
// sequence. Frames that arrive while a snapshot is in flight are buffered and
// replayed over it (snapshotStream.ts).
//
// A command can prompt on its own terminal (sudo, ssh). A `terminal` frame
// carries the whole retained text of that terminal under its own sequence,
// and answerShellCommand types a line into it. The answer is often a
// password: it is passed to the binding and held nowhere here.

import type { ShellCommandEvent } from '../types/events';
import { threadHasScope } from '../transport/entityScopes';
import { randomId } from '../utils/randomId';
import { isTransportClassError } from './transportStatus.svelte';
import {
  AnswerThreadShellCommand,
  GetThreadShellCommands,
  RunThreadShellCommand,
  StopThreadShellCommand,
} from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { createSnapshotStream } from './snapshotStream';

export interface ShellRunView {
  runId: string;
  command: string;
  state: 'running' | 'failed';
  /** The end of the output so far, at most OUTPUT_TAIL_CHARS. */
  output: string;
  /** Highest chunk sequence folded into `output`. */
  outputSeq: number;
  /** The end of what the command wrote to its own terminal: a prompt. */
  terminal: string;
  /** Sequence of the terminal frame `terminal` reflects. */
  terminalSeq: number;
  startedAt: number;
  /** Why a finished command's output did not reach the thread. */
  error: string;
  /** Local-only: a stop was requested and the backend has not answered. */
  stopping: boolean;
}

/**
 * The live card shows a few trailing lines, so the store keeps only the end
 * of the stream. The whole output is the sent message's attachment.
 */
export const OUTPUT_TAIL_CHARS = 16 * 1024;

const views = createKeyedSignalRegistry<ShellRunView | null>(null);
// Bumped on every applied frame, so a live-state read that started before a
// frame cannot overrule it (reconcileShellRun). Held only while the thread
// has a view.
const revisions = new Map<string, number>();
// The start request in flight per thread, so Stop waits until the backend
// knows the run it is asked to stop.
const starting = new Map<string, Promise<unknown>>();
// A start whose reply was lost keeps its run id. Running the same command
// again asks the backend about that run instead of starting a second one
// (RunThreadShellCommand answers a repeated id), until a frame or a reply
// says what became of it.
const uncertainStarts = new Map<string, { command: string; runId: string }>();

type RunningCommands = Awaited<ReturnType<typeof GetThreadShellCommands>>;
const stream = createSnapshotStream<ShellCommandEvent, RunningCommands>({
  name: 'shellCommands',
  read: (threadId) => GetThreadShellCommands(threadId),
  applySnapshot,
  applyFrame: applyEvent,
});

/** Tracked read. Null means the thread has no command to show. */
export function getShellRun(threadId: string | null | undefined): ShellRunView | null {
  if (!threadId) return null;
  return views.get(threadId);
}

/** True while a command is running for the thread. */
export function isShellRunActive(threadId: string | null | undefined): boolean {
  return getShellRun(threadId)?.state === 'running';
}

export function shellRunRevision(threadId: string): number {
  return revisions.get(threadId) ?? 0;
}

function bump(threadId: string): void {
  revisions.set(threadId, shellRunRevision(threadId) + 1);
}

function tail(output: string): string {
  if (output.length <= OUTPUT_TAIL_CHARS) return output;
  const cut = output.length - OUTPUT_TAIL_CHARS;
  // Start on a line when one begins nearby, so the card never opens mid-line.
  const line = output.indexOf('\n', cut);
  return output.slice(line >= 0 && line - cut < 512 ? line + 1 : cut);
}

/**
 * Runs `command` in the thread's workspace. The view appears at once and the
 * backend's `started` frame confirms it; a start that fails removes it and
 * rejects, so the caller can put the text back.
 */
export async function runShellCommand(threadId: string, command: string): Promise<void> {
  const uncertain = uncertainStarts.get(threadId);
  uncertainStarts.delete(threadId);
  const runId = uncertain?.command === command ? uncertain.runId : randomId();
  views.set(threadId, {
    runId,
    command,
    state: 'running',
    output: '',
    outputSeq: 0,
    terminal: '',
    terminalSeq: 0,
    startedAt: Date.now(),
    error: '',
    stopping: false,
  });
  bump(threadId);
  const request = RunThreadShellCommand(threadId, { runId, command });
  starting.set(threadId, request);
  try {
    const run = await request;
    // The backend answered a repeated start for a run that had already
    // settled. A failure it reported on the stream stays to be read.
    if (run.state !== 'running' && isRunning(threadId, runId)) drop(threadId);
  } catch (err) {
    if (isRunning(threadId, runId)) drop(threadId);
    // The reply was lost, not refused: the command may be running.
    if (isTransportClassError(err)) uncertainStarts.set(threadId, { command, runId });
    throw err;
  } finally {
    if (starting.get(threadId) === request) starting.delete(threadId);
  }
}

function isRunning(threadId: string, runId: string): boolean {
  const view = views.get(threadId);
  return view?.runId === runId && view.state === 'running';
}

/** Stops the thread's running command. Its output is discarded. */
export async function stopShellCommand(threadId: string): Promise<void> {
  const view = views.get(threadId);
  if (!view || view.state !== 'running' || view.stopping) return;
  views.set(threadId, { ...view, stopping: true });
  try {
    // A run the backend has not registered yet cannot be stopped.
    await starting.get(threadId)?.catch(() => undefined);
    if (!isRunning(threadId, view.runId)) return;
    await StopThreadShellCommand(threadId, view.runId);
  } catch (err) {
    const current = views.get(threadId);
    if (current?.runId === view.runId && current.stopping) {
      views.set(threadId, { ...current, stopping: false });
    }
    // The run may have ended on a frame this client never received.
    void hydrateShellRun(threadId);
    throw err;
  }
}

/**
 * Types `text` and a newline into the running command's terminal. Not
 * idempotent, so it is never retried, and the text is not kept: it can be a
 * password.
 */
export async function answerShellCommand(threadId: string, text: string): Promise<void> {
  const view = views.get(threadId);
  if (!view || view.state !== 'running') return;
  await AnswerThreadShellCommand(threadId, { runId: view.runId, text });
}

/** Clears a failed run's notice. Local: the backend holds no failed run. */
export function dismissShellRun(threadId: string): void {
  if (views.get(threadId)?.state === 'failed') drop(threadId);
}

function drop(threadId: string): void {
  views.drop(threadId);
  // No entry reads as zero, which differs from every revision a view had.
  revisions.delete(threadId);
}

/**
 * Pulls the running command and reconciles. Called when the thread's live
 * state names a run this client does not hold, on a sequence gap, and on a
 * transport gap for the channel.
 */
export async function hydrateShellRun(threadId: string): Promise<void> {
  if (!threadId) return;
  // The snapshot carries local command output and rides `terminal:operate`,
  // the grant the channel itself is gated on.
  if (!threadHasScope('terminal:operate', threadId)) return;
  await stream.hydrate(threadId);
}

function applySnapshot(threadId: string, runs: RunningCommands): void {
  const run = runs[0];
  const previous = views.get(threadId);
  if (run) {
    views.set(threadId, {
      runId: run.runId,
      command: run.command,
      state: 'running',
      output: tail(run.output ?? ''),
      outputSeq: run.outputSeq ?? 0,
      terminal: run.terminal ?? '',
      terminalSeq: run.terminalSeq ?? 0,
      startedAt: run.startedAt ?? Date.now(),
      error: '',
      stopping: previous?.runId === run.runId && previous.stopping,
    });
    bump(threadId);
  } else if (previous?.state === 'running') {
    // It ended while this client was not listening. What it became, if
    // anything, is in the timeline.
    drop(threadId);
  }
}

/**
 * Applies the live-state snapshot's answer to "is a command running": the id
 * of the run, or empty. Ignored when a frame arrived after the read began.
 */
export function reconcileShellRun(threadId: string, runId: string, revisionAtRequest: number): void {
  if (shellRunRevision(threadId) !== revisionAtRequest) return;
  const view = views.get(threadId);
  if (runId) {
    if (view?.runId !== runId || view.state !== 'running') void hydrateShellRun(threadId);
    return;
  }
  if (view?.state === 'running') drop(threadId);
}

/** Transport-gap recovery: the gap carries no range, so re-read every run held. */
export function resyncShellRuns(): void {
  for (const threadId of views.keys()) {
    if (views.get(threadId)?.state === 'running') void hydrateShellRun(threadId);
  }
}

/** Fan-in target of events.ts. */
export function applyShellCommandEvent(evt: ShellCommandEvent): void {
  const threadId = evt?.threadId ?? '';
  if (!threadId) return;
  stream.push(threadId, evt);
}

function applyEvent(threadId: string, evt: ShellCommandEvent): void {
  const view = views.get(threadId);
  // Any frame of the run settles what became of an uncertain start.
  if (uncertainStarts.get(threadId)?.runId === evt.runId) uncertainStarts.delete(threadId);
  switch (evt.phase) {
    case 'started': {
      // The view this client made when it asked for the run keeps its
      // output position; any other client learns the run here.
      if (view?.runId === evt.runId && view.state === 'running') {
        views.set(threadId, { ...view, command: evt.command ?? view.command, startedAt: evt.startedAt ?? view.startedAt });
      } else {
        views.set(threadId, {
          runId: evt.runId,
          command: evt.command ?? '',
          state: 'running',
          output: '',
          outputSeq: 0,
          terminal: '',
          terminalSeq: 0,
          startedAt: evt.startedAt ?? Date.now(),
          error: '',
          stopping: false,
        });
      }
      bump(threadId);
      return;
    }
    case 'output': {
      if (!view || view.runId !== evt.runId || view.state !== 'running') {
        // Output of a run this client never saw start. The stream starts no
        // read for a frame it is replaying.
        void hydrateShellRun(threadId);
        return;
      }
      const seq = evt.seq ?? 0;
      // Already folded in by a snapshot that raced this frame.
      if (seq <= view.outputSeq) return;
      // Frames were lost. Show this one so the card keeps moving, and read
      // the snapshot, which replaces the output and fills the hole.
      if (seq > view.outputSeq + 1) void hydrateShellRun(threadId);
      views.set(threadId, { ...view, output: tail(view.output + (evt.chunk ?? '')), outputSeq: seq });
      bump(threadId);
      return;
    }
    case 'terminal': {
      if (!view || view.runId !== evt.runId || view.state !== 'running') {
        void hydrateShellRun(threadId);
        return;
      }
      // Each frame is the whole text, so the newest replaces it and an
      // older one, or one a snapshot already reflects, is dropped.
      const seq = evt.terminalSeq ?? 0;
      if (seq <= view.terminalSeq) return;
      views.set(threadId, { ...view, terminal: evt.terminal ?? '', terminalSeq: seq });
      bump(threadId);
      return;
    }
    case 'finished': {
      if (view && view.runId !== evt.runId) return;
      if (evt.state === 'failed') {
        views.set(threadId, {
          runId: evt.runId,
          command: view?.command ?? evt.command ?? '',
          state: 'failed',
          output: '',
          outputSeq: 0,
          terminal: '',
          terminalSeq: 0,
          startedAt: view?.startedAt ?? 0,
          error: evt.error || 'The command could not be run.',
          stopping: false,
        });
        bump(threadId);
        return;
      }
      if (view) drop(threadId);
      return;
    }
    default:
      console.warn(`shellCommands: unknown phase "${String(evt.phase)}" for ${threadId}`);
  }
}

/** Test seam: forget every run. */
export function resetShellRunsForTest(): void {
  views.reset();
  revisions.clear();
  starting.clear();
  uncertainStarts.clear();
  stream.reset();
}
