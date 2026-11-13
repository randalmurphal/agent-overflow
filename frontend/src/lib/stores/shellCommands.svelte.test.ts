import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ShellCommandEvent } from '../types/events';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { DisconnectedError } from '../transport/wsClient';
import {
  OUTPUT_TAIL_CHARS,
  answerShellCommand,
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
const AnswerThreadShellCommand = vi.fn();

const THREAD = 't1';

function started(runId = 'run-1', command = 'make test'): ShellCommandEvent {
  return { phase: 'started', threadId: THREAD, runId, command, startedAt: 1_000 };
}
function output(seq: number, chunk: string, runId = 'run-1'): ShellCommandEvent {
  return { phase: 'output', threadId: THREAD, runId, seq, chunk };
}
function terminal(terminalSeq: number, text: string, runId = 'run-1'): ShellCommandEvent {
  return { phase: 'terminal', threadId: THREAD, runId, terminalSeq, terminal: text };
}
function finished(state: string, runId = 'run-1', error = ''): ShellCommandEvent {
  return { phase: 'finished', threadId: THREAD, runId, state, error, exitCode: 0, finishedAt: 2_000 };
}
function snapshot(runId: string, text: string, outputSeq: number, term?: { terminal: string; terminalSeq: number }) {
  return [{ threadId: THREAD, runId, command: 'make test', state: 'running', output: text, outputSeq, startedAt: 1_000, ...term }];
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
  AnswerThreadShellCommand.mockReset();
  setBindingMock('AnswerThreadShellCommand', (...args: unknown[]) => AnswerThreadShellCommand(...args));
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
    applyShellCommandEvent(finished('failed', 'run-1', 'The command finished, but its output was not sent: boom'));
    expect(getShellRun(THREAD)).toMatchObject({
      state: 'failed', command: 'make test', output: '',
      error: 'The command finished, but its output was not sent: boom',
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

  it('shows a live frame past a hole at once and fills the hole from the snapshot', async () => {
    applyShellCommandEvent(started());
    applyShellCommandEvent(output(1, 'a\n'));
    const pending = deferred<ReturnType<typeof snapshot>>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    applyShellCommandEvent(output(3, 'c\n'));
    expect(getShellRun(THREAD)).toMatchObject({ output: 'a\nc\n', outputSeq: 3 });
    // A frame behind the read in flight is replayed over the snapshot.
    applyShellCommandEvent(output(4, 'd\n'));
    pending.resolve(snapshot('run-1', 'a\nb\nc\n', 3));
    await vi.waitFor(() => expect(getShellRun(THREAD)?.output).toBe('a\nb\nc\nd\n'));
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
  });

  it('keeps the stream moving past a hole when the read for it fails', async () => {
    applyShellCommandEvent(started());
    applyShellCommandEvent(output(1, 'a\n'));
    const pending = deferred<never>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    applyShellCommandEvent(output(3, 'c\n'));
    applyShellCommandEvent(output(4, 'd\n'));
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    pending.reject(new Error('socket closed'));
    await vi.waitFor(() => expect(getShellRun(THREAD)?.output).toBe('a\nc\nd\n'));
    warn.mockRestore();
    applyShellCommandEvent(output(5, 'e\n'));
    expect(getShellRun(THREAD)).toMatchObject({ state: 'running', output: 'a\nc\nd\ne\n', outputSeq: 5 });
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
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

  it('applies a replayed frame past a hole instead of reading again', async () => {
    const pending = deferred<ReturnType<typeof snapshot>>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    const hydration = hydrateShellRun(THREAD);
    applyShellCommandEvent(output(3, 'c\n'));
    pending.resolve(snapshot('run-1', 'a\n', 1));
    await hydration;
    expect(getShellRun(THREAD)?.output).toBe('a\nc\n');
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
  });

  it('does not read again for a replayed frame of a run the snapshot does not name', async () => {
    const pending = deferred<ReturnType<typeof snapshot>>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    const hydration = hydrateShellRun(THREAD);
    applyShellCommandEvent(output(1, 'x\n', 'run-2'));
    pending.resolve(snapshot('run-1', 'a\n', 1));
    await hydration;
    expect(getShellRun(THREAD)).toMatchObject({ runId: 'run-1', output: 'a\n' });
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

  it('forgets a read in flight on reset', async () => {
    const pending = deferred<ReturnType<typeof snapshot>>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    const hydration = hydrateShellRun(THREAD);
    resetShellRunsForTest();
    pending.resolve(snapshot('run-1', 'stale\n', 1));
    await hydration;
    expect(getShellRun(THREAD)).toBeNull();
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
    // A failure notice has nothing on the backend to re-read.
    applyShellCommandEvent({ ...finished('failed', 'run-7', 'boom'), threadId: 't2' });
    GetThreadShellCommands.mockResolvedValue([]);
    resyncShellRuns();
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
    expect(GetThreadShellCommands).toHaveBeenCalledWith(THREAD);
    await vi.waitFor(() => expect(getShellRun(THREAD)).toBeNull());
    expect(getShellRun('t2')?.state).toBe('failed');
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

  it('keeps a failure the stream reported when the reply names a settled run', async () => {
    const pending = deferred<{ state: string }>();
    RunThreadShellCommand.mockReturnValue(pending.promise);
    const run = runShellCommand(THREAD, 'make test');
    const runId = getShellRun(THREAD)!.runId;
    applyShellCommandEvent(finished('failed', runId, 'no shell'));
    pending.resolve({ state: 'failed' });
    await run;
    expect(getShellRun(THREAD)).toMatchObject({ state: 'failed', error: 'no shell' });
  });

  it('asks about the same run when the reply to a start was lost', async () => {
    RunThreadShellCommand.mockRejectedValueOnce(new DisconnectedError());
    await expect(runShellCommand(THREAD, 'make test')).rejects.toBeInstanceOf(DisconnectedError);
    expect(getShellRun(THREAD)).toBeNull();
    const lost = RunThreadShellCommand.mock.calls[0][1].runId;

    // The same command again names the run that may already be running.
    RunThreadShellCommand.mockResolvedValue({ state: 'running' });
    await runShellCommand(THREAD, 'make test');
    expect(RunThreadShellCommand.mock.calls[1][1]).toEqual({ runId: lost, command: 'make test' });
    expect(getShellRun(THREAD)?.runId).toBe(lost);

    // The reply settled it: the next run is a new one.
    applyShellCommandEvent(finished('sent', lost));
    await runShellCommand(THREAD, 'make test');
    expect(RunThreadShellCommand.mock.calls[2][1].runId).not.toBe(lost);
  });

  it('starts a new run after a lost reply when the command differs or was refused', async () => {
    RunThreadShellCommand.mockRejectedValueOnce(new DisconnectedError());
    await expect(runShellCommand(THREAD, 'make test')).rejects.toThrow();
    const lost = RunThreadShellCommand.mock.calls[0][1].runId;
    RunThreadShellCommand.mockResolvedValue({ state: 'running' });
    await runShellCommand(THREAD, 'make lint');
    expect(RunThreadShellCommand.mock.calls[1][1].runId).not.toBe(lost);
    // A different command forgot the lost run for good.
    applyShellCommandEvent(finished('sent', RunThreadShellCommand.mock.calls[1][1].runId));
    await runShellCommand(THREAD, 'make test');
    expect(RunThreadShellCommand.mock.calls[2][1].runId).not.toBe(lost);

    resetShellRunsForTest();
    RunThreadShellCommand.mockReset();
    RunThreadShellCommand.mockRejectedValueOnce(new Error('a command is already running'));
    await expect(runShellCommand(THREAD, 'make test')).rejects.toThrow();
    RunThreadShellCommand.mockResolvedValue({ state: 'running' });
    await runShellCommand(THREAD, 'make test');
    expect(RunThreadShellCommand.mock.calls[1][1].runId).not.toBe(RunThreadShellCommand.mock.calls[0][1].runId);
  });

  it('forgets a lost start once a frame of that run arrives', async () => {
    RunThreadShellCommand.mockRejectedValueOnce(new DisconnectedError());
    await expect(runShellCommand(THREAD, 'make test')).rejects.toThrow();
    const lost = RunThreadShellCommand.mock.calls[0][1].runId;
    // The run did start, and this client watched it end.
    applyShellCommandEvent(started(lost));
    applyShellCommandEvent(finished('sent', lost));
    RunThreadShellCommand.mockResolvedValue({ state: 'running' });
    await runShellCommand(THREAD, 'make test');
    expect(RunThreadShellCommand.mock.calls[1][1].runId).not.toBe(lost);
  });

  it('stops a run only after the backend has answered its start', async () => {
    const start = deferred<{ state: string }>();
    RunThreadShellCommand.mockReturnValue(start.promise);
    StopThreadShellCommand.mockResolvedValue(undefined);
    const run = runShellCommand(THREAD, 'make test');
    const runId = getShellRun(THREAD)!.runId;
    const stop = stopShellCommand(THREAD);
    await Promise.resolve();
    expect(getShellRun(THREAD)?.stopping).toBe(true);
    expect(StopThreadShellCommand).not.toHaveBeenCalled();
    start.resolve({ state: 'running' });
    await run;
    await stop;
    expect(StopThreadShellCommand).toHaveBeenCalledWith(THREAD, runId);
  });

  it('does not ask to stop a run whose start was refused', async () => {
    const start = deferred<never>();
    RunThreadShellCommand.mockReturnValue(start.promise);
    const run = runShellCommand(THREAD, 'make test');
    const stop = stopShellCommand(THREAD);
    start.reject(new Error('refused'));
    await expect(run).rejects.toThrow('refused');
    await stop;
    expect(StopThreadShellCommand).not.toHaveBeenCalled();
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
    GetThreadShellCommands.mockResolvedValue(snapshot('run-1', '', 0));
    pending.reject(new Error('gone'));
    await expect(stop).rejects.toThrow('gone');
    expect(getShellRun(THREAD)?.stopping).toBe(false);
    await vi.waitFor(() => expect(GetThreadShellCommands).toHaveBeenCalledWith(THREAD));
    expect(isShellRunActive(THREAD)).toBe(true);
  });

  it('drops a run that ended unseen when its stop is refused', async () => {
    applyShellCommandEvent(started());
    StopThreadShellCommand.mockRejectedValue(new Error('no command is running'));
    GetThreadShellCommands.mockResolvedValue([]);
    await expect(stopShellCommand(THREAD)).rejects.toThrow('no command is running');
    await vi.waitFor(() => expect(getShellRun(THREAD)).toBeNull());
  });

  it('does nothing when there is no running command to stop', async () => {
    await stopShellCommand(THREAD);
    expect(StopThreadShellCommand).not.toHaveBeenCalled();
  });
});

describe('terminal prompts', () => {
  it('starts a run with an empty terminal, from its start frame or this client\'s own start', async () => {
    applyShellCommandEvent(started());
    expect(getShellRun(THREAD)).toMatchObject({ terminal: '', terminalSeq: 0 });
    resetShellRunsForTest();
    RunThreadShellCommand.mockResolvedValue({ state: 'running' });
    await runShellCommand(THREAD, 'sudo true');
    expect(getShellRun(THREAD)).toMatchObject({ terminal: '', terminalSeq: 0 });
  });

  it('replaces the terminal text with each newer frame and keeps it across a start frame', () => {
    applyShellCommandEvent(started());
    const before = shellRunRevision(THREAD);
    applyShellCommandEvent(terminal(1, '[sudo] password for me: '));
    expect(getShellRun(THREAD)).toMatchObject({ terminal: '[sudo] password for me: ', terminalSeq: 1 });
    expect(shellRunRevision(THREAD)).toBeGreaterThan(before);
    applyShellCommandEvent(terminal(2, '[sudo] password for me: \nSorry, try again.\n[sudo] password for me: '));
    expect(getShellRun(THREAD)?.terminal).toBe('[sudo] password for me: \nSorry, try again.\n[sudo] password for me: ');
    // Output and the terminal count separately.
    applyShellCommandEvent(output(1, 'a\n'));
    expect(getShellRun(THREAD)).toMatchObject({ output: 'a\n', outputSeq: 1, terminalSeq: 2 });
    applyShellCommandEvent(started());
    expect(getShellRun(THREAD)?.terminalSeq).toBe(2);
  });

  it('drops a terminal frame at or below the sequence it holds', () => {
    applyShellCommandEvent(started());
    applyShellCommandEvent(terminal(3, 'newest'));
    const revision = shellRunRevision(THREAD);
    applyShellCommandEvent(terminal(2, 'older'));
    applyShellCommandEvent(terminal(3, 'same'));
    expect(getShellRun(THREAD)).toMatchObject({ terminal: 'newest', terminalSeq: 3 });
    expect(shellRunRevision(THREAD)).toBe(revision);
    expect(GetThreadShellCommands).not.toHaveBeenCalled();
  });

  it('takes the terminal from the snapshot, and defaults it when absent', async () => {
    GetThreadShellCommands.mockResolvedValue(snapshot('run-1', 'a\n', 1, { terminal: 'Password: ', terminalSeq: 4 }));
    await hydrateShellRun(THREAD);
    expect(getShellRun(THREAD)).toMatchObject({ terminal: 'Password: ', terminalSeq: 4 });
    applyShellCommandEvent(terminal(4, 'stale'));
    expect(getShellRun(THREAD)?.terminal).toBe('Password: ');

    GetThreadShellCommands.mockResolvedValue(snapshot('run-1', 'a\n', 1));
    await hydrateShellRun(THREAD);
    expect(getShellRun(THREAD)).toMatchObject({ terminal: '', terminalSeq: 0 });
  });

  it('reads the run when a terminal frame names one this client does not hold', async () => {
    GetThreadShellCommands.mockResolvedValue(snapshot('run-1', '', 0, { terminal: 'Password: ', terminalSeq: 1 }));
    applyShellCommandEvent(terminal(1, 'Password: '));
    expect(GetThreadShellCommands).toHaveBeenCalledWith(THREAD);
    await vi.waitFor(() => expect(getShellRun(THREAD)).toMatchObject({ runId: 'run-1', terminal: 'Password: ', terminalSeq: 1 }));
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
  });

  it('replays terminal frames buffered during a snapshot read over it', async () => {
    const pending = deferred<ReturnType<typeof snapshot>>();
    GetThreadShellCommands.mockReturnValue(pending.promise);
    const hydration = hydrateShellRun(THREAD);
    applyShellCommandEvent(terminal(1, 'Password: '));
    applyShellCommandEvent(terminal(3, 'Password: \nSorry.\nPassword: '));
    pending.resolve(snapshot('run-1', '', 0, { terminal: 'Password: \nSorry.', terminalSeq: 2 }));
    await hydration;
    expect(getShellRun(THREAD)).toMatchObject({ terminal: 'Password: \nSorry.\nPassword: ', terminalSeq: 3 });
    expect(GetThreadShellCommands).toHaveBeenCalledTimes(1);
  });

  it('does not carry the terminal into a failure', () => {
    applyShellCommandEvent(started());
    applyShellCommandEvent(terminal(1, 'Password: '));
    applyShellCommandEvent(finished('failed', 'run-1', 'boom'));
    expect(getShellRun(THREAD)).toMatchObject({ state: 'failed', terminal: '', terminalSeq: 0 });
  });

  it('answers the running command once, with its run id, and keeps no copy of the answer', async () => {
    applyShellCommandEvent(started());
    applyShellCommandEvent(terminal(1, 'Password: '));
    AnswerThreadShellCommand.mockResolvedValue(undefined);
    await answerShellCommand(THREAD, 'hunter2');
    expect(AnswerThreadShellCommand).toHaveBeenCalledTimes(1);
    expect(AnswerThreadShellCommand).toHaveBeenCalledWith(THREAD, { runId: 'run-1', text: 'hunter2' });
    expect(JSON.stringify(getShellRun(THREAD))).not.toContain('hunter2');
  });

  it('answers nothing without a running command', async () => {
    await answerShellCommand(THREAD, 'hunter2');
    applyShellCommandEvent(finished('failed', 'run-1', 'boom'));
    await answerShellCommand(THREAD, 'hunter2');
    expect(AnswerThreadShellCommand).not.toHaveBeenCalled();
  });

  it('rejects a failed answer without asking again', async () => {
    applyShellCommandEvent(started());
    AnswerThreadShellCommand.mockRejectedValue(new DisconnectedError());
    await expect(answerShellCommand(THREAD, 'hunter2')).rejects.toBeInstanceOf(DisconnectedError);
    await Promise.resolve();
    expect(AnswerThreadShellCommand).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(getShellRun(THREAD))).not.toContain('hunter2');
  });
});
