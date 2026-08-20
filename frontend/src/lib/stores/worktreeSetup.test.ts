import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { WorktreeSetupEvent } from '../types/events';

const GetThreadWorktreeSetup = vi.fn();
const RetryThreadWorktreeSetup = vi.fn();
const GetWorkspaceWorktreeSetup = vi.fn();
const RetryWorkspaceWorktreeSetup = vi.fn();

// Spread the original: a factory listing only the four RPCs this suite drives
// turns every OTHER export of ./bindings into `undefined` for it, and the
// failure surfaces the next time something in the import graph reaches for one.
vi.mock('./bindings', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./bindings')>()),
  GetThreadWorktreeSetup: (...args: unknown[]) => GetThreadWorktreeSetup(...args),
  RetryThreadWorktreeSetup: (...args: unknown[]) => RetryThreadWorktreeSetup(...args),
  GetWorkspaceWorktreeSetup: (...args: unknown[]) => GetWorkspaceWorktreeSetup(...args),
  RetryWorkspaceWorktreeSetup: (...args: unknown[]) => RetryWorkspaceWorktreeSetup(...args),
}));

const {
  applyWorktreeSetupEvent,
  clearSettledWorktreeSetup,
  dismissWorktreeSetup,
  getWorktreeSetup,
  hasWorktreeSetupSurface,
  hydrateWorkspaceWorktreeSetup,
  hydrateWorktreeSetup,
  resetWorktreeSetupForTest,
  resyncWorkspaceWorktreeSetups,
  retryWorktreeSetup,
  showWorktreeSetup,
  workspaceSetupKey,
} = await import('./worktreeSetup.svelte');

const THREAD = 't1';

function started(runId = 'run-1'): WorktreeSetupEvent {
  return {
    phase: 'started',
    threadId: THREAD,
    runId,
    worktreePath: '/wt',
    startedAt: 1_000,
    steps: [
      { index: 0, kind: 'copy', label: 'Copy files' },
      { index: 1, kind: 'command', label: 'pnpm install', argv: ['pnpm', 'install'] },
    ],
  };
}

function output(seq: number, chunk: string, runId = 'run-1'): WorktreeSetupEvent {
  return { phase: 'output', threadId: THREAD, runId, stepIndex: 1, seq, chunk };
}

beforeEach(() => {
  resetWorktreeSetupForTest();
  GetThreadWorktreeSetup.mockReset();
  RetryThreadWorktreeSetup.mockReset();
  GetWorkspaceWorktreeSetup.mockReset();
  RetryWorkspaceWorktreeSetup.mockReset();
});

// A pre-thread run: the eager worktree apply is project-scoped, so the
// backend registers the run against the workspace and its frames carry no
// thread id.
const WS_PATH = '/wt/pre-thread';
// The key space joins with the repo's compositeKey separator (NUL), not a
// colon: the other key space is a raw thread id, and a colon is a byte an id
// may legitimately contain.
const WS_KEY = `ws\u0000${WS_PATH}`;

function unboundStarted(runId = 'run-ws'): WorktreeSetupEvent {
  return {
    phase: 'started',
    threadId: '',
    runId,
    worktreePath: WS_PATH,
    startedAt: 500,
    steps: [{ index: 0, kind: 'copy', label: 'Copy files' }],
  };
}

describe('workspace-keyed runs', () => {
  it('keys a frame with no thread id under the workspace', () => {
    applyWorktreeSetupEvent(unboundStarted());
    expect(workspaceSetupKey(WS_PATH)).toBe(WS_KEY);
    expect(getWorktreeSetup(WS_KEY)?.runId).toBe('run-ws');
    expect(getWorktreeSetup(WS_KEY)?.worktreePath).toBe(WS_PATH);
    // Nothing lands under a thread key: there is no thread yet.
    expect(getWorktreeSetup(THREAD)).toBeNull();
  });

  it('streams output and outcome under the workspace key', () => {
    applyWorktreeSetupEvent(unboundStarted());
    applyWorktreeSetupEvent({
      phase: 'output', threadId: '', worktreePath: WS_PATH, runId: 'run-ws',
      stepIndex: 0, seq: 1, chunk: 'copying\n',
    });
    applyWorktreeSetupEvent({
      phase: 'finished', threadId: '', worktreePath: WS_PATH, runId: 'run-ws',
      state: 'failed', error: 'copy failed', finishedAt: 900,
    });
    const view = getWorktreeSetup(WS_KEY);
    expect(view?.output).toBe('copying\n');
    expect(view?.state).toBe('failed');
    expect(view?.error).toBe('copy failed');
  });

  it('migrates the run to the thread key when the adoption frame arrives', () => {
    applyWorktreeSetupEvent(unboundStarted());
    expect(hasWorktreeSetupSurface(WS_KEY)).toBe(true);

    // CreateThread adopted the unbound run; the backend re-emits it with both
    // ids so a client that watched the workspace lands on the thread.
    applyWorktreeSetupEvent({ ...unboundStarted(), threadId: THREAD });

    expect(getWorktreeSetup(THREAD)?.runId).toBe('run-ws');
    // Exactly one card: the workspace box is dropped in the same tick.
    expect(getWorktreeSetup(WS_KEY)).toBeNull();
    expect(hasWorktreeSetupSurface(WS_KEY)).toBe(false);
  });

  it('carries the run\'s progress and the collapse across adoption', () => {
    applyWorktreeSetupEvent(unboundStarted());
    applyWorktreeSetupEvent({
      phase: 'output', threadId: '', worktreePath: WS_PATH, runId: 'run-ws',
      stepIndex: 0, seq: 1, chunk: 'copying\n',
    });
    dismissWorktreeSetup(WS_KEY);

    // The adoption frame is started-SHAPED but describes a run already in
    // flight, so it hands over the statuses, the transcript and its sequence.
    applyWorktreeSetupEvent({
      ...unboundStarted(),
      threadId: THREAD,
      stepStatuses: ['running'],
      output: 'copying\n',
      outputSeq: 1,
    });

    const view = getWorktreeSetup(THREAD);
    expect(view?.stepStatuses).toEqual(['running']);
    expect(view?.output).toBe('copying\n');
    expect(view?.outputSeq).toBe(1);
    // Adoption is not a new run: the card the user collapsed stays collapsed.
    expect(view?.dismissed).toBe(true);
    // And the next live chunk is the expected one, so no gap is reported.
    applyWorktreeSetupEvent({
      phase: 'output', threadId: THREAD, worktreePath: WS_PATH, runId: 'run-ws',
      stepIndex: 0, seq: 2, chunk: 'done\n',
    });
    expect(getWorktreeSetup(THREAD)?.output).toBe('copying\ndone\n');
    expect(GetThreadWorktreeSetup).not.toHaveBeenCalled();
  });

  it('re-snapshots workspace-keyed runs on a transport gap', async () => {
    GetWorkspaceWorktreeSetup.mockResolvedValue({
      threadId: '', runId: 'run-ws', state: 'running', worktreePath: WS_PATH,
      steps: unboundStarted().steps, stepStatuses: ['running'],
      output: 'a\n', outputSeq: 1,
    });
    await hydrateWorkspaceWorktreeSetup('project-1', WS_PATH);
    GetWorkspaceWorktreeSetup.mockClear();

    // A pre-thread run has no row, so the thread-list walk cannot reach it.
    resyncWorkspaceWorktreeSetups();
    await Promise.resolve();
    expect(GetWorkspaceWorktreeSetup).toHaveBeenCalledWith('project-1', WS_PATH);
  });

  it('hydrates the workspace snapshot and retries through the workspace RPC', async () => {
    GetWorkspaceWorktreeSetup.mockResolvedValue({
      threadId: '', runId: 'run-ws', state: 'failed', worktreePath: WS_PATH,
      steps: unboundStarted().steps, stepStatuses: ['failed'],
      output: 'boom\n', outputSeq: 1, error: 'exit 1',
    });
    await hydrateWorkspaceWorktreeSetup('project-1', WS_PATH);
    expect(GetWorkspaceWorktreeSetup).toHaveBeenCalledWith('project-1', WS_PATH);
    expect(getWorktreeSetup(WS_KEY)?.state).toBe('failed');

    RetryWorkspaceWorktreeSetup.mockResolvedValue(undefined);
    await retryWorktreeSetup(WS_KEY);
    expect(RetryWorkspaceWorktreeSetup).toHaveBeenCalledWith('project-1', WS_PATH);
    expect(RetryThreadWorktreeSetup).not.toHaveBeenCalled();
    expect(getWorktreeSetup(WS_KEY)?.state).toBe('running');
  });

  it('re-snapshots the workspace, not a thread, on a detected frame gap', async () => {
    GetWorkspaceWorktreeSetup.mockResolvedValue({
      threadId: '', runId: 'run-ws', state: 'running', worktreePath: WS_PATH,
      steps: unboundStarted().steps, stepStatuses: ['running'],
      output: 'a\nb\n', outputSeq: 2,
    });
    await hydrateWorkspaceWorktreeSetup('project-1', WS_PATH);
    GetWorkspaceWorktreeSetup.mockClear();

    applyWorktreeSetupEvent({
      phase: 'output', threadId: '', worktreePath: WS_PATH, runId: 'run-ws',
      stepIndex: 0, seq: 9, chunk: 'jumped\n',
    });

    expect(GetWorkspaceWorktreeSetup).toHaveBeenCalledWith('project-1', WS_PATH);
    expect(GetThreadWorktreeSetup).not.toHaveBeenCalled();
  });
});

describe('event projection', () => {
  it('has nothing to show for a thread that never ran a setup', () => {
    expect(getWorktreeSetup(THREAD)).toBeNull();
    expect(hasWorktreeSetupSurface(THREAD)).toBe(false);
  });

  it('builds the run from the started frame with every step pending', () => {
    applyWorktreeSetupEvent(started());
    const view = getWorktreeSetup(THREAD);
    expect(view?.state).toBe('running');
    expect(view?.runId).toBe('run-1');
    expect(view?.stepStatuses).toEqual(['pending', 'pending']);
    expect(hasWorktreeSetupSurface(THREAD)).toBe(true);
  });

  it('tracks per-step transitions', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'step-started', threadId: THREAD, runId: 'run-1', stepIndex: 0 });
    expect(getWorktreeSetup(THREAD)?.stepStatuses).toEqual(['running', 'pending']);
    applyWorktreeSetupEvent({
      phase: 'step-finished', threadId: THREAD, runId: 'run-1', stepIndex: 0, state: 'succeeded',
    });
    applyWorktreeSetupEvent({
      phase: 'step-finished', threadId: THREAD, runId: 'run-1', stepIndex: 1, state: 'failed',
    });
    expect(getWorktreeSetup(THREAD)?.stepStatuses).toEqual(['succeeded', 'failed']);
  });

  it('appends output in sequence order', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent(output(1, 'a\n'));
    applyWorktreeSetupEvent(output(2, 'b\n'));
    const view = getWorktreeSetup(THREAD);
    expect(view?.output).toBe('a\nb\n');
    expect(view?.outputSeq).toBe(2);
  });

  // A chunk at or below the folded-in high-water mark is a snapshot race, not
  // new output. Appending it would duplicate the transcript.
  it('drops a chunk it has already folded in', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent(output(1, 'a\n'));
    applyWorktreeSetupEvent(output(1, 'a\n'));
    expect(getWorktreeSetup(THREAD)?.output).toBe('a\n');
  });

  it('re-fetches the snapshot when frames were lost', async () => {
    GetThreadWorktreeSetup.mockResolvedValue({
      threadId: THREAD, runId: 'run-1', state: 'running',
      steps: started().steps, stepStatuses: ['succeeded', 'running'],
      output: 'a\nb\nc\n', outputSeq: 3,
    });
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent(output(1, 'a\n'));
    applyWorktreeSetupEvent(output(3, 'c\n'));
    expect(GetThreadWorktreeSetup).toHaveBeenCalledWith(THREAD);
    await vi.waitFor(() => {
      expect(getWorktreeSetup(THREAD)?.output).toBe('a\nb\nc\n');
    });
  });

  it('hydrates for a run it never saw start', async () => {
    GetThreadWorktreeSetup.mockResolvedValue({
      threadId: THREAD, runId: 'run-9', state: 'failed',
      steps: started('run-9').steps, stepStatuses: ['succeeded', 'failed'],
      output: 'boom\n', outputSeq: 4, error: 'exit 1',
    });
    applyWorktreeSetupEvent(output(5, 'orphan\n', 'run-9'));
    await vi.waitFor(() => {
      expect(getWorktreeSetup(THREAD)?.runId).toBe('run-9');
    });
  });
});

describe('terminal states', () => {
  it('keeps a failure with its error', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({
      phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed',
      error: 'pnpm install failed', finishedAt: 2_000,
    });
    const view = getWorktreeSetup(THREAD);
    expect(view?.state).toBe('failed');
    expect(view?.error).toBe('pnpm install failed');
    expect(hasWorktreeSetupSurface(THREAD)).toBe(true);
  });

  it('drops a cancelled run entirely — it is neither success nor failure', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'cancelled' });
    expect(getWorktreeSetup(THREAD)).toBeNull();
  });

  it('keeps a success only until the panel clears it', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'succeeded' });
    expect(getWorktreeSetup(THREAD)?.state).toBe('succeeded');
    // Still mounted: unmounting on the state flip would take the panel down
    // before it could show — and then clear — the acknowledgement.
    expect(hasWorktreeSetupSurface(THREAD)).toBe(true);
    clearSettledWorktreeSetup(THREAD, 'run-1');
    expect(getWorktreeSetup(THREAD)).toBeNull();
    expect(hasWorktreeSetupSurface(THREAD)).toBe(false);
  });

  it('refuses to clear a running or failed run on a timeout', () => {
    applyWorktreeSetupEvent(started());
    clearSettledWorktreeSetup(THREAD, 'run-1');
    expect(getWorktreeSetup(THREAD)?.state).toBe('running');
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed' });
    clearSettledWorktreeSetup(THREAD, 'run-1');
    expect(getWorktreeSetup(THREAD)?.state).toBe('failed');
  });

  it('ignores a stale clear from a run that has been replaced', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'succeeded' });
    applyWorktreeSetupEvent(started('run-2'));
    clearSettledWorktreeSetup(THREAD, 'run-1');
    expect(getWorktreeSetup(THREAD)?.runId).toBe('run-2');
  });
});

describe('dismissal', () => {
  it('collapses and re-opens without losing the run', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed', error: 'x' });
    dismissWorktreeSetup(THREAD);
    expect(getWorktreeSetup(THREAD)?.dismissed).toBe(true);
    expect(hasWorktreeSetupSurface(THREAD)).toBe(true);
    showWorktreeSetup(THREAD);
    expect(getWorktreeSetup(THREAD)?.dismissed).toBe(false);
  });

  it('re-opens for a new run', () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed', error: 'x' });
    dismissWorktreeSetup(THREAD);
    applyWorktreeSetupEvent(started('run-2'));
    expect(getWorktreeSetup(THREAD)?.dismissed).toBe(false);
  });

  // The dismissal applied to the outcome the user saw. A second failure is a
  // new outcome, so hiding it behind the collapsed bar would swallow it.
  it('re-opens on a fresh failure of the same run', () => {
    applyWorktreeSetupEvent(started());
    dismissWorktreeSetup(THREAD);
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed', error: 'x' });
    expect(getWorktreeSetup(THREAD)?.dismissed).toBe(false);
  });
});

describe('retry', () => {
  it('flips to running optimistically and clears the previous error', async () => {
    RetryThreadWorktreeSetup.mockResolvedValue(undefined);
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed', error: 'x' });
    await retryWorktreeSetup(THREAD);
    const view = getWorktreeSetup(THREAD);
    expect(view?.state).toBe('running');
    expect(view?.error).toBe('');
  });

  // The Retry affordance must survive its own failure, or a rejected retry
  // leaves the user with a card that can no longer be retried.
  it('restores the failure when the RPC rejects', async () => {
    RetryThreadWorktreeSetup.mockRejectedValue(new Error('no worktree'));
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed', error: 'x' });
    await expect(retryWorktreeSetup(THREAD)).rejects.toThrow('no worktree');
    const view = getWorktreeSetup(THREAD);
    expect(view?.state).toBe('failed');
    expect(view?.error).toBe('x');
  });
});

describe('hydration', () => {
  it('adopts a durable failure with no transcript', async () => {
    GetThreadWorktreeSetup.mockResolvedValue({
      threadId: THREAD, runId: '', state: 'failed', steps: [], stepStatuses: [],
      output: '', outputSeq: 0, worktreePath: '/wt',
    });
    await hydrateWorktreeSetup(THREAD);
    const view = getWorktreeSetup(THREAD);
    expect(view?.state).toBe('failed');
    expect(view?.steps).toEqual([]);
  });

  it('resolves an idle snapshot to nothing', async () => {
    GetThreadWorktreeSetup.mockResolvedValue({
      threadId: THREAD, runId: '', state: 'idle', steps: [], stepStatuses: [],
      output: '', outputSeq: 0,
    });
    applyWorktreeSetupEvent(started());
    await hydrateWorktreeSetup(THREAD);
    expect(getWorktreeSetup(THREAD)).toBeNull();
  });

  // The race the buffer exists for: without it, a terminal frame landing
  // between the backend's read and this store's apply would be overwritten by
  // a snapshot that still said "running", and nothing would ever correct it.
  it('replays events that arrived while the snapshot was in flight', async () => {
    let release: (value: unknown) => void = () => {};
    GetThreadWorktreeSetup.mockReturnValue(new Promise((resolve) => { release = resolve; }));
    const pending = hydrateWorktreeSetup(THREAD);

    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({
      phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed', error: 'late failure',
    });
    // Nothing applied yet — the events are buffered behind the snapshot.
    expect(getWorktreeSetup(THREAD)).toBeNull();

    release({
      threadId: THREAD, runId: 'run-1', state: 'running',
      steps: started().steps, stepStatuses: ['succeeded', 'running'],
      output: 'partial\n', outputSeq: 1,
    });
    await pending;

    const view = getWorktreeSetup(THREAD);
    expect(view?.state).toBe('failed');
    expect(view?.error).toBe('late failure');
  });

  // A failed hydration must not leave the thread buffering forever: the stream
  // would go silent with no way back.
  it('replays the buffer and resumes live events when the snapshot rejects', async () => {
    GetThreadWorktreeSetup.mockRejectedValue(new Error('offline'));
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const pending = hydrateWorktreeSetup(THREAD);
    applyWorktreeSetupEvent(started());
    await pending;
    expect(getWorktreeSetup(THREAD)?.state).toBe('running');

    applyWorktreeSetupEvent(output(1, 'live\n'));
    expect(getWorktreeSetup(THREAD)?.output).toBe('live\n');
    warn.mockRestore();
  });

  it('keeps a local dismissal across a hydration', async () => {
    applyWorktreeSetupEvent(started());
    applyWorktreeSetupEvent({ phase: 'finished', threadId: THREAD, runId: 'run-1', state: 'failed', error: 'x' });
    dismissWorktreeSetup(THREAD);
    GetThreadWorktreeSetup.mockResolvedValue({
      threadId: THREAD, runId: 'run-1', state: 'failed',
      steps: started().steps, stepStatuses: ['succeeded', 'failed'],
      output: '', outputSeq: 0, error: 'x',
    });
    await hydrateWorktreeSetup(THREAD);
    expect(getWorktreeSetup(THREAD)?.dismissed).toBe(true);
  });

  // Two panes mounting the same thread must not fight over which snapshot wins.
  it('lets the newest hydration own the outcome', async () => {
    const first = { threadId: THREAD, runId: 'run-1', state: 'running', steps: [], stepStatuses: [], output: 'first', outputSeq: 1 };
    const second = { threadId: THREAD, runId: 'run-2', state: 'failed', steps: [], stepStatuses: [], output: 'second', outputSeq: 2 };
    let releaseFirst: (value: unknown) => void = () => {};
    GetThreadWorktreeSetup
      .mockReturnValueOnce(new Promise((resolve) => { releaseFirst = resolve; }))
      .mockResolvedValueOnce(second);

    const a = hydrateWorktreeSetup(THREAD);
    const b = hydrateWorktreeSetup(THREAD);
    await b;
    releaseFirst(first);
    await a;

    expect(getWorktreeSetup(THREAD)?.runId).toBe('run-2');
  });
});
