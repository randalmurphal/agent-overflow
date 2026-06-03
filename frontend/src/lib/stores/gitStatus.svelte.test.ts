import { describe, expect, it, beforeEach, vi } from 'vitest';
import { tick } from 'svelte';
import { createThreadPane } from './thread.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { loadSettings } from './settings.svelte';
import type { GitStatus } from '../types/git';
import type { Thread } from '../types/models';
import type { ThreadPane } from './thread.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';

// The slot takes `connected` as an explicit attach arg, so it never reads the
// transport store directly — but the createThreadPane import chain might. Pin
// it connected so nothing in that chain reaches for the (never-initialised)
// wsClient in test scope.
vi.mock('./transportStatus.svelte', () => ({
  getTransportStatus: () => ({ status: 'connected', nextAttemptAt: null }),
  retryTransport: () => {},
  resetTransportStatusForTest: () => {},
}));

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Example',
    provider: 'claude',
    workspacePath: '/workspace',
    projectPath: '/workspace',
    model: 'claude-sonnet-4-6',
    mode: 'chat',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'main',
    isDefaultBranch: true,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'github',
    ...overrides,
  };
}

async function buildPane(thread = makeThread()) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListThreadCheckpoints', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  registerPaneForTest(pane.paneId, pane);
  return pane;
}

// Drives the slot exactly as ChatHeaderActions does in production: pane-derived
// thread/id getters + setGeneralError as the error sink. Uses `in` checks (not
// `??`) so an explicit `null` cwd survives — it's a real disqualifier, and
// `null ?? '/default'` would silently re-qualify it. The slot subscribes to the
// 'git:status' push itself (via wailsEventOn → the mocked @wailsio/runtime bus),
// so emitWailsEvent('git:status', …) reaches it without any wiring here.
function attachSlot(
  pane: ThreadPane,
  opts: { threadId?: string | null; cwd?: string | null; connected?: boolean } = {},
): () => void {
  return pane.gitStatus.attach({
    threadId: 'threadId' in opts ? (opts.threadId ?? null) : pane.threadId,
    cwd: 'cwd' in opts ? (opts.cwd ?? null) : '/workspace',
    connected: 'connected' in opts ? (opts.connected ?? false) : true,
    getThread: () => pane.thread ?? null,
    getLiveThreadId: () => pane.threadId,
    reportError: (message: string) => pane.setGeneralError(message),
  });
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

function installSubscribeMock(initial: GitStatus, id = 'sub-1') {
  const subscribeFn = setBindingMock('GitStatusSubscribe', async () => ({ id, status: initial }));
  setBindingMock('GitStatusUnsubscribe', async () => {});
  return { id, subscribeFn };
}

describe('createGitStatusSlot — subscription lifecycle', () => {
  beforeEach(async () => {
    resetPanesForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    setBindingMock('UpdateThreadBranch', async () => makeThread({ branch: 'main' }));
    await loadSettings();
  });

  it('does not subscribe and clears status when disqualified (no cwd / disconnected)', async () => {
    const pane = await buildPane();
    const subscribeFn = setBindingMock('GitStatusSubscribe', async () => ({ id: 'x', status: status() }));
    pane.gitStatus.set(status({ hasChanges: true }));

    const cleanup = attachSlot(pane, { cwd: null });
    await flush();
    expect(subscribeFn).not.toHaveBeenCalled();
    expect(pane.gitStatus.status).toBeNull();
    cleanup();
  });

  it('applies the initial subscribed status', async () => {
    const pane = await buildPane();
    installSubscribeMock(status({ hasChanges: true, insertions: 12, deletions: 3 }));
    attachSlot(pane);
    await flush();
    expect(pane.gitStatus.status?.hasChanges).toBe(true);
    expect(pane.gitStatus.status?.insertions).toBe(12);
    expect(pane.gitStatus.statusError).toBe(false);
  });

  it('sets statusError on subscribe failure without escalating to setGeneralError', async () => {
    const pane = await buildPane();
    const setGeneralError = vi.spyOn(pane, 'setGeneralError');
    setBindingMock('GitStatusSubscribe', async () => {
      throw new Error('ENOENT git');
    });
    setBindingMock('GitStatusUnsubscribe', async () => {});
    attachSlot(pane);
    await flush();
    expect(pane.gitStatus.statusError).toBe(true);
    expect(setGeneralError).not.toHaveBeenCalled();
  });

  it('retries subscribe with backoff after a transient failure', async () => {
    vi.useFakeTimers();
    try {
      const pane = await buildPane();
      let callCount = 0;
      const subscribeFn = setBindingMock('GitStatusSubscribe', async () => {
        callCount++;
        if (callCount === 1) throw new Error('transient');
        return { id: 'sub-retry', status: status() };
      });
      setBindingMock('GitStatusUnsubscribe', async () => {});

      attachSlot(pane);
      await flush();
      await vi.waitFor(() => expect(pane.gitStatus.statusError).toBe(true));
      expect(callCount).toBe(1);

      // Advance past the 3s retry delay.
      await vi.advanceTimersByTimeAsync(3_000);
      await flush();

      await vi.waitFor(() => expect(pane.gitStatus.statusError).toBe(false));
      expect(subscribeFn).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('cancels a pending retry timer on cleanup', async () => {
    vi.useFakeTimers();
    try {
      const pane = await buildPane();
      const subscribeFn = setBindingMock('GitStatusSubscribe', async () => {
        throw new Error('always fails');
      });
      setBindingMock('GitStatusUnsubscribe', async () => {});

      const cleanup = attachSlot(pane);
      await flush();
      expect(subscribeFn).toHaveBeenCalledTimes(1);

      cleanup();
      await vi.advanceTimersByTimeAsync(10_000);
      await flush();

      // No retry fires after cleanup cancelled the timer.
      expect(subscribeFn).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('applies a "git:status" push for the active subscription', async () => {
    const pane = await buildPane();
    const { id } = installSubscribeMock(status({ hasChanges: true }));
    attachSlot(pane);
    await flush();
    expect(pane.gitStatus.status?.hasChanges).toBe(true);

    emitWailsEvent('git:status', {
      subscriptionId: id,
      status: status({ hasChanges: false, aheadCount: 2 }),
    });
    await flush();
    expect(pane.gitStatus.status?.hasChanges).toBe(false);
    expect(pane.gitStatus.status?.aheadCount).toBe(2);
  });

  it('ignores "git:status" pushes for a different subscription', async () => {
    const pane = await buildPane();
    const { id } = installSubscribeMock(status({ hasChanges: true }));
    attachSlot(pane);
    await flush();

    emitWailsEvent('git:status', {
      subscriptionId: `not-${id}`,
      status: status({ hasChanges: false, aheadCount: 5 }),
    });
    await flush();
    expect(pane.gitStatus.status?.hasChanges).toBe(true);
  });

  it('releases an orphaned subscription when cleanup races an in-flight Subscribe', async () => {
    const pane = await buildPane();

    let resolveFirst: (val: { id: string; status: GitStatus }) => void = () => {};
    const firstPromise = new Promise<{ id: string; status: GitStatus }>((r) => {
      resolveFirst = r;
    });
    let callCount = 0;
    const subscribeFn = setBindingMock('GitStatusSubscribe', (..._args: never[]) => {
      callCount += 1;
      if (callCount === 1) return firstPromise;
      return Promise.resolve({ id: 'live', status: status() });
    });
    const unsubscribeFn = setBindingMock('GitStatusUnsubscribe', async () => {});

    const cleanup = attachSlot(pane);
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(unsubscribeFn).not.toHaveBeenCalled();

    // Cleanup before the Subscribe RPC resolves marks it cancelled.
    cleanup();
    // Resolve the orphaned subscribe — the cancelled-guard must release it.
    resolveFirst({ id: 'orphan-1', status: status() });
    await flush();

    const unsubCalls = unsubscribeFn.mock.calls.map((c) => c[0]);
    expect(unsubCalls).toContain('orphan-1');
  });

  it('doubles the retry backoff on consecutive failures', async () => {
    vi.useFakeTimers();
    try {
      const pane = await buildPane();
      const subscribeFn = setBindingMock('GitStatusSubscribe', async () => {
        throw new Error('always fails');
      });
      setBindingMock('GitStatusUnsubscribe', async () => {});

      attachSlot(pane);
      await flush();
      expect(subscribeFn).toHaveBeenCalledTimes(1);

      // 1st retry at 3s — 2.9s is not enough.
      await vi.advanceTimersByTimeAsync(2_900);
      expect(subscribeFn).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(100);
      expect(subscribeFn).toHaveBeenCalledTimes(2);

      // 2nd retry at 6s (doubled) — 5.9s is not enough.
      await vi.advanceTimersByTimeAsync(5_900);
      expect(subscribeFn).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(100);
      expect(subscribeFn).toHaveBeenCalledTimes(3);

      // 3rd retry at 12s (doubled again), proving the curve, not a fixed delay.
      await vi.advanceTimersByTimeAsync(11_900);
      expect(subscribeFn).toHaveBeenCalledTimes(3);
      await vi.advanceTimersByTimeAsync(100);
      expect(subscribeFn).toHaveBeenCalledTimes(4);
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops applying git:status pushes after cleanup', async () => {
    const pane = await buildPane();
    const { id } = installSubscribeMock(status({ hasChanges: true, insertions: 1 }));
    const cleanup = attachSlot(pane);
    await flush();
    expect(pane.gitStatus.status?.insertions).toBe(1);

    cleanup();
    emitWailsEvent('git:status', {
      subscriptionId: id,
      status: status({ insertions: 99 }),
    });
    await flush();

    // cleanup() removed the listener; the post-cleanup push must be ignored.
    // A dropped `cancelEvent()` would leak a live handler mutating a detached
    // slot and otherwise pass every existing test.
    expect(pane.gitStatus.status?.insertions).toBe(1);
  });

  it('unsubscribes on disconnect and re-subscribes on reconnect', async () => {
    const pane = await buildPane();
    const { subscribeFn } = installSubscribeMock(status({ hasChanges: true }));
    const unsubscribeFn = setBindingMock('GitStatusUnsubscribe', async () => {});

    // Connected → subscribes. (Models the attach $effect's first run.)
    let cleanup = attachSlot(pane, { connected: true });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(pane.gitStatus.status?.hasChanges).toBe(true);

    // Disconnect: the $effect tears down the old attach, then re-attaches
    // disqualified (connected:false) — the subscription is released and status
    // clears.
    cleanup();
    cleanup = attachSlot(pane, { connected: false });
    await flush();
    expect(unsubscribeFn).toHaveBeenCalledTimes(1);
    expect(pane.gitStatus.status).toBeNull();

    // Reconnect: re-attach connected → fresh subscribe.
    cleanup();
    cleanup = attachSlot(pane, { connected: true });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(2);
    expect(pane.gitStatus.status?.hasChanges).toBe(true);
    cleanup();
  });
});

describe('createGitStatusSlot — observed branch persistence', () => {
  beforeEach(async () => {
    resetPanesForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    setBindingMock('UpdateThreadBranch', async () => makeThread({ branch: 'main' }));
    await loadSettings();
  });

  it('persists and applies an initial subscribed branch that differs from the thread row', async () => {
    const pane = await buildPane(makeThread({ branch: 'main' }));
    const updateBranch = setBindingMock(
      'UpdateThreadBranch',
      async (_threadId, branch) => makeThread({ branch: branch as string }),
    );
    installSubscribeMock(status({ branch: 'feature/live', isDefaultBranch: false }));
    attachSlot(pane);

    await vi.waitFor(() => {
      expect(pane.thread?.branch).toBe('feature/live');
      expect(updateBranch).toHaveBeenCalledWith('thread-1', 'feature/live');
    });
  });

  it('persists and applies branch changes from a "git:status" push', async () => {
    const pane = await buildPane(makeThread({ branch: 'main' }));
    const updateBranch = setBindingMock(
      'UpdateThreadBranch',
      async (_threadId, branch) => makeThread({ branch: branch as string }),
    );
    const { id } = installSubscribeMock(status({ branch: 'main' }));
    attachSlot(pane);
    await flush();

    emitWailsEvent('git:status', {
      subscriptionId: id,
      status: status({ branch: 'feature/external', isDefaultBranch: false }),
    });

    await vi.waitFor(() => {
      expect(pane.thread?.branch).toBe('feature/external');
      expect(updateBranch).toHaveBeenCalledWith('thread-1', 'feature/external');
    });
  });

  it('does not persist for same-branch or non-repo status updates', async () => {
    const pane = await buildPane(makeThread({ branch: 'main' }));
    const updateBranch = setBindingMock(
      'UpdateThreadBranch',
      async (_threadId, branch) => makeThread({ branch: branch as string }),
    );
    const { id } = installSubscribeMock(status({ branch: 'main' }));
    attachSlot(pane);
    await flush();

    emitWailsEvent('git:status', {
      subscriptionId: id,
      status: status({ branch: 'main', hasChanges: true }),
    });
    emitWailsEvent('git:status', {
      subscriptionId: id,
      status: status({ isRepo: false, branch: 'ignored' }),
    });
    await flush();

    expect(pane.thread?.branch).toBe('main');
    expect(updateBranch).not.toHaveBeenCalled();
  });

  it('surfaces a setGeneralError when branch persistence fails for the live thread', async () => {
    const pane = await buildPane(makeThread({ branch: 'main' }));
    const setGeneralError = vi.spyOn(pane, 'setGeneralError');
    setBindingMock('UpdateThreadBranch', async () => {
      throw new Error('db locked');
    });
    installSubscribeMock(status({ branch: 'feature/x', isDefaultBranch: false }));
    attachSlot(pane);

    await vi.waitFor(() => expect(setGeneralError).toHaveBeenCalled());
    expect(String(setGeneralError.mock.calls[0][0])).toContain('Failed to update thread branch');
  });

  it('does NOT surface a persist error once the user has switched threads (stale)', async () => {
    // The complement of the test above: the same persist failure must stay
    // silent when getLiveThreadId() no longer matches the thread the persist
    // was queued for — otherwise a thread switch would toast a branch error
    // about a thread the user already left.
    const pane = await buildPane(makeThread({ id: 'thread-1', branch: 'main' }));
    const setGeneralError = vi.spyOn(pane, 'setGeneralError');

    let liveThreadId: string | null = 'thread-1';
    let rejectPersist: (err: Error) => void = () => {};
    setBindingMock(
      'UpdateThreadBranch',
      () =>
        new Promise((_resolve, reject) => {
          rejectPersist = reject;
        }),
    );
    installSubscribeMock(status({ branch: 'feature/x', isDefaultBranch: false }));

    // Attach directly so getLiveThreadId is a mutable test double.
    pane.gitStatus.attach({
      threadId: 'thread-1',
      cwd: '/workspace',
      connected: true,
      getThread: () => pane.thread ?? null,
      getLiveThreadId: () => liveThreadId,
      reportError: (message: string) => pane.setGeneralError(message),
    });
    // Subscribe applies branch 'feature/x' (≠ 'main') → queues a persist for thread-1.
    await flush();

    // User switches away before the persist RPC settles.
    liveThreadId = 'thread-2';
    rejectPersist(new Error('db locked'));
    await flush();

    expect(setGeneralError).not.toHaveBeenCalled();
  });
});

describe('createGitStatusSlot — pure setters', () => {
  beforeEach(async () => {
    resetPanesForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    setBindingMock('UpdateThreadBranch', async () => makeThread({ branch: 'main' }));
    await loadSettings();
  });

  it('set() updates status without persisting the observed branch', async () => {
    const pane = await buildPane(makeThread({ branch: 'main' }));
    const updateBranch = setBindingMock('UpdateThreadBranch', async () => makeThread());
    pane.gitStatus.set(status({ branch: 'feature/other', insertions: 5, deletions: 2 }));
    await flush();

    expect(pane.gitStatus.status?.insertions).toBe(5);
    expect(pane.gitStatus.status?.deletions).toBe(2);
    // Pure setter: no branch reconciliation side effects.
    expect(pane.thread?.branch).toBe('main');
    expect(updateBranch).not.toHaveBeenCalled();
  });

  it('setError() toggles the flag and reset() clears both', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({ hasChanges: true }));
    pane.gitStatus.setError(true);
    expect(pane.gitStatus.statusError).toBe(true);

    pane.gitStatus.reset();
    expect(pane.gitStatus.status).toBeNull();
    expect(pane.gitStatus.statusError).toBe(false);
  });
});

describe('createGitStatusSlot — refreshNow', () => {
  beforeEach(async () => {
    resetPanesForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    setBindingMock('UpdateThreadBranch', async () => makeThread({ branch: 'main' }));
    await loadSettings();
  });

  it('no-ops when the slot has never been attached (no ctx)', async () => {
    const pane = await buildPane();
    const getStatus = setBindingMock('GetGitStatus', async () => status());
    await pane.gitStatus.refreshNow();
    expect(getStatus).not.toHaveBeenCalled();
  });

  it('applies the refreshed status and reconciles the branch', async () => {
    const pane = await buildPane(makeThread({ branch: 'main' }));
    const updateBranch = setBindingMock(
      'UpdateThreadBranch',
      async (_id, branch) => makeThread({ branch: branch as string }),
    );
    installSubscribeMock(status({ branch: 'main' }));
    attachSlot(pane);
    await flush();

    setBindingMock('GetGitStatus', async () =>
      status({ branch: 'feature/refreshed', insertions: 9, isDefaultBranch: false }),
    );
    await pane.gitStatus.refreshNow();

    await vi.waitFor(() => {
      expect(pane.gitStatus.status?.insertions).toBe(9);
      expect(pane.thread?.branch).toBe('feature/refreshed');
      expect(updateBranch).toHaveBeenCalledWith('thread-1', 'feature/refreshed');
    });
    expect(pane.gitStatus.statusError).toBe(false);
  });

  it('sets statusError when the refresh RPC fails', async () => {
    const pane = await buildPane();
    installSubscribeMock(status());
    attachSlot(pane);
    await flush();

    setBindingMock('GetGitStatus', async () => {
      throw new Error('git busy');
    });
    await pane.gitStatus.refreshNow();
    expect(pane.gitStatus.statusError).toBe(true);
  });

  it('drops a stale refresh result if the slot detaches during the await (no cross-thread persist)', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-1', branch: 'main' }));
    const updateBranch = setBindingMock(
      'UpdateThreadBranch',
      async (_id, branch) => makeThread({ branch: branch as string }),
    );
    installSubscribeMock(status({ branch: 'main' }));
    const cleanup = attachSlot(pane);
    await flush();
    expect(pane.gitStatus.status?.branch).toBe('main');

    // Slow refresh whose resolution we control.
    let resolveStatus: (s: GitStatus) => void = () => {};
    setBindingMock(
      'GetGitStatus',
      () =>
        new Promise<GitStatus>((resolve) => {
          resolveStatus = resolve;
        }),
    );
    const refreshPromise = pane.gitStatus.refreshNow(); // captures id = 'thread-1'

    // Thread switches away mid-await: cleanup detaches the slot (ctx → null).
    cleanup();
    // The slow refresh finally resolves with a DIFFERENT branch.
    resolveStatus(status({ branch: 'feature/stale', isDefaultBranch: false }));
    await refreshPromise;
    await flush();

    // The staleness guard dropped it: status not clobbered, no branch persisted.
    // Without the guard this applies a left thread's status and persists its
    // branch onto whatever getThread() now returns.
    expect(pane.gitStatus.status?.branch).toBe('main');
    expect(updateBranch).not.toHaveBeenCalled();
  });
});
