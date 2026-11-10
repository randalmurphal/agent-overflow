import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ShellCommandEvent } from '../types/events';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  OUTPUT_TAIL_CHARS,
  applyShellCommandEvent,
  dismissShellRun,
  getShellRun,
  hydrateShellRun,
  isShellRunActive,
  reconcileShellRun,
  resetShellRunsForTest,
  resyncShellRuns,
  runShellCommand,
  shellRunRevision,
  stopShellCommand,
} from './shellCommands.svelte';

const GetThreadShellCommands = vi.fn();
const RunThreadShellCommand = vi.fn();
const StopThreadShellCommand = vi.fn();

const THREAD = 't1';

function started(runId = 'run-1', command = 'make test'): ShellCommandEvent {
  return { phase: 'started', threadId: THREAD, runId, command, startedAt: 1_000 };
}
function output(seq: number, chunk: string, runId = 'run-1'): ShellCommandEvent {
  return { phase: 'output', threadId: THREAD, runId, seq, chunk };
}
function finished(state: string, runId = 'run-1', error = ''): ShellCommandEvent {
  return { phase: 'finished', threadId: THREAD, runId, state, error, exitCode: 0, finishedAt: 2_000 };
}
function snapshot(runId: string, text: string, outputSeq: number) {
  return [{ threadId: THREAD, runId, command: 'make test', state: 'running', output: text, outputSeq, startedAt: 1_000 }];
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

beforeEach(() => {
  resetShellRunsForTest();
  GetThreadShellCommands.mockReset();
  RunThreadShellCommand.mockReset();
  StopThreadShellCommand.mockReset();
  setBindingMock('GetThreadShellCommands', (...args: unknown[]) => GetThreadShellCommands(...args));
  setBindingMock('RunThreadShellCommand', (...args: unknown[]) => RunThreadShellCommand(...args));
  setBindingMock('StopThreadShellCommand', (...args: unknown[]) => StopThreadShellCommand(...args));
});

describe('event projection', () => {
  it('shows a run from its start, folds output in order and drops it when sent', () => {
    applyShellCommandEvent(started());
    expect(getShellRun(THREAD)).toMatchObject({ runId: 'run-1', command: 'make test', state: 'running', output: '' });
    expect(isShellRunActive(THREAD)).toBe(true);

    applyShellCommandEvent(output(1, 'a\n'));
    applyShellCommandEvent(output(2, 'b\n'));
    // A frame a snapshot already folded in.
    applyShellCommandEvent(output(2, 'b\n'));
    expect(getShellRun(THREAD)?.output).toBe('a\nb\n');

    applyShellCommandEvent(finished('sent'));
    expect(getShellRun(THREAD)).toBeNull();
    expect(isShellRunActive(THREAD)).toBe(false);
  });

  it('drops a stopped run and ignores frames without a thread', () => {
    applyShellCommandEvent({ ...started(), threadId: '' });
    expect(getShellRun(THREAD)).toBeNull();
    applyShellCommandEvent(started());
    applyShellCommandEvent(finished('stopped'));
    expect(getShellRun(THREAD)).toBeNull();
  });

  it('keeps a failed run as an error until it is dismissed', () => {
    applyShellCommandEvent(started());
    applyShellCommandEvent(output(1, 'partial'));
    applyShellCommandEvent(finished('failed', 'run-1', 'The command finished, but its output could not be sent: boom'));
    expect(getShellRun(THREAD)).toMatchObject({
      state: 'failed', command: 'make test', output: '',
      error: 'The command finished, but its output could not be sent: boom',
    });
    expect(isShellRunActive(THREAD)).toBe(false);
    dismissShellRun(THREAD);
    expect(getShellRun(THREAD)).toBeNull();
  });

  it('shows a failure of a run this client never saw start', () => {
    applyShellCommandEvent({ ...finished('failed', 'run-9', 'no shell'), command: 'ls' });
    expect(getShellRun(THREAD)).toMatchObject({ state: 'failed', command: 'ls', error: 'no shell' });
  });

  it('ignores the finish of a run it has already replaced', () => {
    applyShellCommandEvent(started('run-2'));
    applyShellCommandEvent(finished('sent', 'run-1'));
    expect(getShellRun(THREAD)?.runId).toBe('run-2');
  });

  it('does not let dismiss remove a running command', () => {
    applyShellCommandEvent(started());
    dismissShellRun(THREAD);
    expect(isShellRunActive(THREAD)).toBe(true);
  });

  it('keeps only the end of a long stream, starting on a line', () => {
    applyShellCommandEvent(started());
    const line = `${'x'.repeat(99)}\n`;
    let seq = 0;
    for (let written = 0; written < OUTPUT_TAIL_CHARS * 2; written += line.length) {
      applyShellCommandEvent(output(++seq, line));
    }
    const held = getShellRun(THREAD)!.output;
    expect(held.length).toBeLessThanOrEqual(OUTPUT_TAIL_CHARS);
    expect(held.length).toBeGreaterThan(OUTPUT_TAIL_CHARS - 2 * line.length);
    expect(held.startsWith('x'.repeat(99))).toBe(true);
    expect(held.endsWith(line)).toBe(true);
  });
});

describe('snapshot reconciliation', () => {
  it('re-reads the run when a frame was lost', async () => {
    applyShellCommandEvent(started());
    applyShellCommandEvent(output(1, 'a\n'));
    GetThreadShellCommands.mockResolvedValue(snapshot('run-1', 'a\nb\nc\n', 3));
    applyShellCommandEvent(output(3, 'c\n'));
    await vi.waitFor(() => expect(getShellRun(THREAD)?.output).toBe('a\nb\nc\n'));
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
    // The stream continues from the snapshot's position.
    applyShellCommandEvent(output(4, 'd\n'));
    expect(getShellRun(THREAD)?.output).toBe('a\nb\nc\nd\n');
  });

  it('learns a run from its output when the start was never seen', async () => {
    GetThreadShellCommands.mockResolvedValue(snapshot('run-1', 'a\n', 1));
    applyShellCommandEvent(output(1, 'a\n'));
    await vi.waitFor(() => expect(getShellRun(THREAD)?.runId).toBe('run-1'));
    expect(getShellRun(THREAD)?.output).toBe('a\n');
  });

  it('replays frames that arrive while the snapshot is in flight, without doubling output', async () => {
    const pending = deferred<ReturnType<typeof snapshot>>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    const hydration = hydrateShellRun(THREAD);
    applyShellCommandEvent(output(2, 'b\n'));
    applyShellCommandEvent(output(3, 'c\n'));
    pending.resolve(snapshot('run-1', 'a\nb\n', 2));
    await hydration;
    expect(getShellRun(THREAD)?.output).toBe('a\nb\nc\n');
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
  });

  it('applies a finish that lands during the snapshot read', async () => {
    applyShellCommandEvent(started());
    const pending = deferred<ReturnType<typeof snapshot>>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    const hydration = hydrateShellRun(THREAD);
    applyShellCommandEvent(finished('sent'));
    pending.resolve(snapshot('run-1', 'a\n', 1));
    await hydration;
    expect(getShellRun(THREAD)).toBeNull();
  });

  it('drops a run the backend no longer has', async () => {
    applyShellCommandEvent(started());
    GetThreadShellCommands.mockResolvedValue([]);
    await hydrateShellRun(THREAD);
    expect(getShellRun(THREAD)).toBeNull();
  });

  it('keeps what it has, and replays buffered frames, when the read fails', async () => {
    applyShellCommandEvent(started());
    const pending = deferred<never>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    const hydration = hydrateShellRun(THREAD);
    applyShellCommandEvent(output(1, 'a\n'));
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    pending.reject(new Error('socket closed'));
    await hydration;
    warn.mockRestore();
    expect(getShellRun(THREAD)).toMatchObject({ state: 'running', output: 'a\n' });
  });

  it('lets the newest of two overlapping reads decide', async () => {
    const first = deferred<ReturnType<typeof snapshot>>();
    const second = deferred<ReturnType<typeof snapshot>>();
    GetThreadShellCommands.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    const a = hydrateShellRun(THREAD);
    const b = hydrateShellRun(THREAD);
    second.resolve(snapshot('run-2', 'new\n', 1));
    await b;
    first.resolve(snapshot('run-1', 'old\n', 1));
    await a;
    expect(getShellRun(THREAD)).toMatchObject({ runId: 'run-2', output: 'new\n' });
  });
});

describe('live state', () => {
  it('reads a run the thread reports and this client does not hold', async () => {
    GetThreadShellCommands.mockResolvedValue(snapshot('run-1', 'a\n', 1));
    reconcileShellRun(THREAD, 'run-1', shellRunRevision(THREAD));
    await vi.waitFor(() => expect(getShellRun(THREAD)?.output).toBe('a\n'));
  });

  it('does not re-read a run it already holds', () => {
    applyShellCommandEvent(started());
    reconcileShellRun(THREAD, 'run-1', shellRunRevision(THREAD));
    expect(GetThreadShellCommands).not.toHaveBeenCalled();
  });

  it('drops a run the thread no longer reports, but keeps a failure notice', () => {
    applyShellCommandEvent(started());
    reconcileShellRun(THREAD, '', shellRunRevision(THREAD));
    expect(getShellRun(THREAD)).toBeNull();

    applyShellCommandEvent(started('run-2'));
    applyShellCommandEvent(finished('failed', 'run-2', 'boom'));
    reconcileShellRun(THREAD, '', shellRunRevision(THREAD));
    expect(getShellRun(THREAD)?.state).toBe('failed');
  });

  it('ignores a snapshot that a later frame has overtaken', () => {
    const atRequest = shellRunRevision(THREAD);
    applyShellCommandEvent(started());
    reconcileShellRun(THREAD, '', atRequest);
    expect(isShellRunActive(THREAD)).toBe(true);
  });

  it('re-reads only running commands after a transport gap', async () => {
    applyShellCommandEvent(started());
    GetThreadShellCommands.mockResolvedValue([]);
    resyncShellRuns([THREAD, 't2']);
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
    expect(GetThreadShellCommands).toHaveBeenCalledWith(THREAD);
    await vi.waitFor(() => expect(getShellRun(THREAD)).toBeNull());
  });
});

describe('running and stopping', () => {
  it('shows the run at once and keeps it across the start frame', async () => {
    const pending = deferred<{ state: string }>();
    RunThreadShellCommand.mockReturnValue(pending.promise);
    const run = runShellCommand(THREAD, 'make test');
    const view = getShellRun(THREAD)!;
    expect(view).toMatchObject({ command: 'make test', state: 'running', output: '' });
    expect(RunThreadShellCommand).toHaveBeenCalledWith(THREAD, { runId: view.runId, command: 'make test' });

    applyShellCommandEvent(started(view.runId));
    applyShellCommandEvent(output(1, 'a\n', view.runId));
    pending.resolve({ state: 'running' });
    await run;
    expect(getShellRun(THREAD)).toMatchObject({ runId: view.runId, output: 'a\n', startedAt: 1_000 });
  });

  it('removes the run and rejects when the start is refused', async () => {
    RunThreadShellCommand.mockRejectedValue(new Error('a command is already running'));
    await expect(runShellCommand(THREAD, 'make test')).rejects.toThrow('already running');
    expect(getShellRun(THREAD)).toBeNull();
  });

  it('leaves a newer run alone when an older start is refused late', async () => {
    const pending = deferred<never>();
    RunThreadShellCommand.mockReturnValue(pending.promise);
    const run = runShellCommand(THREAD, 'first');
    applyShellCommandEvent(started('other-run', 'second'));
    pending.reject(new Error('refused'));
    await expect(run).rejects.toThrow('refused');
    expect(getShellRun(THREAD)?.runId).toBe('other-run');
  });

  it('removes the run when a repeated request finds it already settled', async () => {
    RunThreadShellCommand.mockResolvedValue({ state: 'sent' });
    await runShellCommand(THREAD, 'make test');
    expect(getShellRun(THREAD)).toBeNull();
  });

  it('marks a stop in flight, asks once, and undoes the mark when the stop fails', async () => {
    applyShellCommandEvent(started());
    const pending = deferred<void>();
    StopThreadShellCommand.mockReturnValue(pending.promise);
    const stop = stopShellCommand(THREAD);
    expect(getShellRun(THREAD)?.stopping).toBe(true);
    await stopShellCommand(THREAD);
    expect(StopThreadShellCommand).toHaveBeenCalledTimes(1);
    expect(StopThreadShellCommand).toHaveBeenCalledWith(THREAD, 'run-1');
    pending.reject(new Error('gone'));
    await expect(stop).rejects.toThrow('gone');
    expect(getShellRun(THREAD)?.stopping).toBe(false);
  });

  it('does nothing when there is no running command to stop', async () => {
    await stopShellCommand(THREAD);
    expect(StopThreadShellCommand).not.toHaveBeenCalled();
  });
});
