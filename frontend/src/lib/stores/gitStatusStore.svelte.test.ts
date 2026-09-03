import { describe, expect, it, beforeEach, vi } from 'vitest';
import { tick } from 'svelte';
import { createThreadPane } from './thread.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import {
  __resetGitStatusStoreForTest,
  __seedGitStatusForTest,
  attachGitStatus,
  gitStatusKeys,
  peekGitStatus,
  peekGitStatusError,
  refreshGitStatus,
} from './gitStatusStore.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import type { GitStatus, WorkspaceRef } from '../types/git';
import type { Thread } from '../types/models';
import { composeWorkspaceKey, workspaceKeyForThread } from '../utils/workspaceKey';
import { HOME_BACKEND } from '../transport/backendKey';
import { __resetEntityIndexForTest } from '../transport/entityIndex';
import { __attachBackendForTest, detachBackend } from '../transport/backends';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { applyTransportGap } from './eventsTransportGap';

// The store is keyed by `${backendId} ${path}` (utils/workspaceKey.ts), so
// the PATH an RPC is called with and the KEY an entry is held under are two
// different strings. A single-backend client keys under HOME_BACKEND, the
// empty string, and these tests pin that spelling.
// A stand-in connection for a second attached backend. This suite's RPCs
// all go through the bindings mock, so nothing is dialled: what the entry
// needs is a client the registry can install standing subscriptions on.
function fakeBackendClient(): never {
  return {
    callByID: async () => null,
    callByName: async () => null,
    subscribe: () => () => undefined,
    // The three per-connection frames the registry restates on every
    // composition change; a pane mounted under this suite reaches them.
    setWatchedThreads: () => undefined,
    setPresence: () => undefined,
    setLease: () => undefined,
    installStepUpProver: () => undefined,
    getStatus: () => ({ status: 'connected', nextAttemptAt: null }),
    onStatusChange: () => () => undefined,
    getHello: () => null,
    onHelloChange: () => () => undefined,
    close: () => undefined,
  } as never;
}

const WORKSPACE_PATH = '/workspace';
const OTHER_WORKSPACE_PATH = '/workspace-two';
const WORKSPACE = composeWorkspaceKey(HOME_BACKEND, WORKSPACE_PATH);
const OTHER_WORKSPACE = composeWorkspaceKey(HOME_BACKEND, OTHER_WORKSPACE_PATH);
const PROJECT = 'project-1';
const CWD = '/canonical/workspace';

/** The wire spelling of a checkout — what every source-time RPC takes. The
 *  entity KEY is still the path alone, so a ref built for a key must carry
 *  that same path. */
function ref(workspacePath: string): WorkspaceRef {
  return { projectId: PROJECT, workspacePath };
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    projectId: PROJECT,
    title: 'Example',
    provider: 'claude',
    workspacePath: WORKSPACE_PATH,
    projectPath: WORKSPACE_PATH,
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

// Panes are seeded with replaceThread rather than switchThread: this suite
// is about the git-status store, and a real switch would drag in the whole
// thread-load binding surface for no added coverage.
function buildPane(thread = makeThread()) {
  const pane = createThreadPane({ paneId: `pane-${thread.id}` });
  pane.replaceThread(thread);
  registerPaneForTest(pane.paneId, pane);
  return pane;
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

function installSubscribeMock(initial: GitStatus, id = 'sub-1', cwd = CWD) {
  const subscribeFn = setBindingMock('GitStatusSubscribe', async () => ({ id, cwd, status: initial }));
  const unsubscribeFn = setBindingMock('GitStatusUnsubscribe', async () => {});
  return { id, cwd, subscribeFn, unsubscribeFn };
}

beforeEach(() => {
  resetPanesForTest();
  setBindingMock('UpdateThreadBranch', async () => [makeThread({ branch: 'main' })]);
  __resetEntityIndexForTest();
});

describe('gitStatusStore — workspace keying', () => {
  it('derives the key from the thread workspace, and nothing from a workspace-less thread', () => {
    expect(workspaceKeyForThread(makeThread())).toBe(WORKSPACE);
    expect(workspaceKeyForThread(makeThread({ workspacePath: '  ' }))).toBeNull();
    expect(workspaceKeyForThread(null)).toBeNull();
  });

  it('shares ONE subscription between two attachers on the same workspace', async () => {
    buildPane();
    const { subscribeFn, unsubscribeFn } = installSubscribeMock(status({ hasChanges: true }));

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    const b = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(a.current?.hasChanges).toBe(true);
    // The regression this whole store exists for: the second consumer sees
    // the SAME observation, not a private copy it has to catch up to.
    expect(b.current).toBe(a.current);

    a.release();
    await flush();
    expect(unsubscribeFn).not.toHaveBeenCalled();
    b.release();
    await flush();
    expect(unsubscribeFn).toHaveBeenCalledWith('sub-1');
  });

  it('routes a "git:status" push by canonical cwd to every local key aliased to it', async () => {
    buildPane();
    installSubscribeMock(status({ hasChanges: true }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    // A second local spelling resolving to the same canonical cwd — the
    // alias map is what makes one wire event reach both keys.
    setBindingMock('GitStatusSubscribe', async () => ({
      id: 'sub-2',
      cwd: CWD,
      status: status({ hasChanges: true }),
    }));
    const b = attachGitStatus('/private/workspace', { workspace: ref('/private/workspace') });
    await flush();

    emitWailsEvent('git:status', { cwd: CWD, status: status({ hasChanges: false, aheadCount: 4 }) });
    await flush();

    expect(a.current?.aheadCount).toBe(4);
    expect(b.current?.aheadCount).toBe(4);
    a.release();
    b.release();
  });

  it('does NOT cross two backends that canonicalize to the SAME cwd', async () => {
    // Canonicalization happens on the backend, so a laptop and a desktop
    // holding the same checkout report the identical cwd. Routed by cwd
    // alone, one machine's push would paint the other's header — and the
    // branch reconciliation behind it would write one machine's branch onto
    // the other's rows.
    const REMOTE = 'laptop';
    const REMOTE_UUID = '99999999-8888-4777-8666-555555555555';
    // The grant set for a backend comes from the credential stored for it
    // (transport/deviceSession.ts owns this record's shape and its key).
    localStorage.setItem(
      `agent-overflow:deviceSession:${REMOTE}`,
      JSON.stringify({
        sessionId: 's-laptop',
        credential: 'c',
        expiresAtMs: Date.now() + 60_000,
        scopes: ['git:operate'],
      }),
    );
    setBackendIdentityFromBootstrap(REMOTE_UUID, 'gen-1', 'Laptop', REMOTE);
    __attachBackendForTest(
      {
        id: REMOTE,
        backendId: REMOTE_UUID,
        name: 'Laptop',
        wsUrl: 'ws://localhost:3000/ws/backend/laptop',
        bootstrapUrl: '/bootstrap/laptop.json',
      },
      fakeBackendClient(),
    );

    buildPane();
    installSubscribeMock(status({ insertions: 1 }));
    const here = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    setBindingMock('GitStatusSubscribe', async () => ({
      id: 'sub-remote',
      cwd: CWD,
      status: status({ insertions: 1 }),
    }));
    const there = attachGitStatus(composeWorkspaceKey(REMOTE, WORKSPACE_PATH), {
      workspace: { projectId: 'project-remote', workspacePath: WORKSPACE_PATH },
    });
    await flush();

    // One frame, delivered on the REMOTE connection. Same cwd, same path.
    emitWailsEvent(
      'git:status',
      { cwd: CWD, status: status({ insertions: 99 }) },
      REMOTE_UUID,
    );
    await flush();

    expect(there.current?.insertions).toBe(99);
    expect(here.current?.insertions).toBe(1);
    here.release();
    there.release();
    detachBackend(REMOTE);
    localStorage.removeItem(`agent-overflow:deviceSession:${REMOTE}`);
  });

  it('drops a "git:status" push for a cwd nothing is attached to', async () => {
    buildPane();
    installSubscribeMock(status({ insertions: 1 }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    emitWailsEvent('git:status', { cwd: '/some/other/repo', status: status({ insertions: 99 }) });
    await flush();
    expect(a.current?.insertions).toBe(1);
    a.release();
  });

  it('stops applying pushes after the last release', async () => {
    buildPane();
    installSubscribeMock(status({ insertions: 1 }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();
    a.release();
    await flush();

    emitWailsEvent('git:status', { cwd: CWD, status: status({ insertions: 99 }) });
    await flush();
    expect(peekGitStatus(WORKSPACE)).toBeNull();
  });

  // The alias map is what turns a cwd-addressed wire event back into a
  // local key. A superseded source run resolves LATE and then runs its own
  // cleanup; if that cleanup could remove an alias the live run installed,
  // this workspace would keep a subscription whose pushes route nowhere —
  // a header frozen until something re-attaches it.
  it('a superseded subscribe cannot unroute the live one', async () => {
    buildPane();
    const gates: Array<(result: unknown) => void> = [];
    const subscribeFn = setBindingMock(
      'GitStatusSubscribe',
      () => new Promise((resolve) => gates.push(resolve)),
    );
    setBindingMock('GitStatusUnsubscribe', async () => {});

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    // Supersede run 1 before it answers — a reconnect is the production
    // path that does this — then let run 2 win the race.
    __setTransportStatusForTest({ status: 'reconnecting', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(2);
    gates[1]({ id: 'sub-live', cwd: CWD, status: status({ insertions: 1 }) });
    await flush();

    gates[0]({ id: 'sub-stale', cwd: CWD, status: status({ insertions: 99 }) });
    await flush();

    // Run 1's cleanup ran (its subscription is released) and its status was
    // dropped, but the routing run 2 installed is still there.
    expect(a.current?.insertions).toBe(1);
    emitWailsEvent('git:status', { cwd: CWD, status: status({ insertions: 7 }) });
    await flush();
    expect(a.current?.insertions).toBe(7);
    a.release();
  });

  it('re-attaching after a release subscribes again', async () => {
    buildPane();
    const { subscribeFn } = installSubscribeMock(status());
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();
    a.release();
    await flush();

    const b = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(2);
    expect(b.current).not.toBeNull();
    b.release();
  });
});

describe('gitStatusStore — failure surfaces', () => {
  it('records a subscribe failure as a readable error without escalating to the pane', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const pane = buildPane();
    const setGeneralError = vi.spyOn(pane, 'setGeneralError');
    setBindingMock('GitStatusSubscribe', async () => {
      throw new Error('ENOENT git');
    });
    setBindingMock('GitStatusUnsubscribe', async () => {});

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    expect(a.error).toBe('ENOENT git');
    expect(peekGitStatusError(WORKSPACE)).toBe('ENOENT git');
    expect(setGeneralError).not.toHaveBeenCalled();
    a.release();
    vi.restoreAllMocks();
  });

  it('retries a failed subscribe on a backoff and clears the error on success', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    vi.useFakeTimers();
    try {
      buildPane();
      let calls = 0;
      setBindingMock('GitStatusSubscribe', async () => {
        calls += 1;
        if (calls === 1) throw new Error('transient');
        return { id: 'sub-retry', cwd: CWD, status: status() };
      });
      setBindingMock('GitStatusUnsubscribe', async () => {});

      const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
      await flush();
      await vi.waitFor(() => expect(a.error).toBe('transient'));

      await vi.advanceTimersByTimeAsync(3_000);
      await flush();
      await vi.waitFor(() => expect(a.error).toBeNull());
      expect(calls).toBe(2);
      a.release();
    } finally {
      vi.useRealTimers();
      vi.restoreAllMocks();
    }
  });
});

describe('gitStatusStore — transport edges', () => {
  it('suspends on disconnect and re-subscribes on reconnect', async () => {
    buildPane();
    const { subscribeFn, unsubscribeFn } = installSubscribeMock(status({ hasChanges: true }));

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(a.current?.hasChanges).toBe(true);

    __setTransportStatusForTest({ status: 'reconnecting', nextAttemptAt: null });
    await flush();
    // The observation is dropped (it can no longer be kept fresh) and the
    // handle released; nothing re-subscribes while the wire is down.
    expect(a.current).toBeNull();
    expect(unsubscribeFn).toHaveBeenCalledTimes(1);
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    // The backend released every subscription with the old socket, so the
    // reference must re-acquire without the consumer re-attaching.
    expect(subscribeFn).toHaveBeenCalledTimes(2);
    expect(a.current?.hasChanges).toBe(true);
    a.release();
  });

  it('an attach made while disconnected acquires on reconnect', async () => {
    buildPane();
    const { subscribeFn } = installSubscribeMock(status({ aheadCount: 2 }));

    __setTransportStatusForTest({ status: 'reconnecting', nextAttemptAt: null });
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();
    expect(subscribeFn).not.toHaveBeenCalled();

    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(a.current?.aheadCount).toBe(2);
    a.release();
  });

  it('drops the cwd alias on suspend so a late push cannot revive a dead key', async () => {
    buildPane();
    installSubscribeMock(status({ insertions: 3 }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    await flush();
    emitWailsEvent('git:status', { cwd: CWD, status: status({ insertions: 99 }) });
    await flush();

    expect(a.current).toBeNull();
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    a.release();
  });
});

// `git:status` emits exactly one frame per change to a checkout, so a frame
// dropped mid-connection (wsClient's forward-seq-skip detection) leaves every
// consumer stale with nothing due to correct it. Recovery is blanket because
// the gap carries no cwd — and it must not blank anything on the way.
describe('gitStatusStore — transport gap', () => {
  it('re-sources every live workspace and keeps the last status while it reloads', async () => {
    buildPane();
    buildPane(makeThread({ id: 'thread-2', workspacePath: OTHER_WORKSPACE_PATH }));
    const subscribeFn = setBindingMock('GitStatusSubscribe', async (ws: WorkspaceRef) => ({
      id: `sub-${ws.workspacePath}`,
      cwd: `/canonical${ws.workspacePath}`,
      status: status({ hasChanges: true }),
    }));
    setBindingMock('GitStatusUnsubscribe', async () => {});

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    const b = attachGitStatus(OTHER_WORKSPACE, { workspace: ref(OTHER_WORKSPACE_PATH) });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(2);
    expect(gitStatusKeys().sort()).toEqual([OTHER_WORKSPACE, WORKSPACE].sort());

    // The re-subscribe is gated so the assertions below run while the fresh
    // status is still in flight: that is the window a blanking recovery
    // would show as an empty badge on every open header.
    let openGate = (): void => {};
    const gate = new Promise<void>((resolve) => {
      openGate = resolve;
    });
    const resubscribeFn = setBindingMock('GitStatusSubscribe', async (ws: WorkspaceRef) => {
      await gate;
      return {
        id: `re-${ws.workspacePath}`,
        cwd: `/canonical${ws.workspacePath}`,
        status: status({ hasChanges: false, insertions: 7 }),
      };
    });

    applyTransportGap({ channel: 'git:status', seq: 12 });
    await flush();

    expect(resubscribeFn).toHaveBeenCalledTimes(2);
    expect(a.current?.hasChanges).toBe(true);
    expect(b.current?.hasChanges).toBe(true);

    openGate();
    await flush();
    expect(a.current?.insertions).toBe(7);
    expect(b.current?.insertions).toBe(7);

    a.release();
    b.release();
  });

  it('ignores a gap on a channel this store does not own', async () => {
    buildPane();
    const { subscribeFn } = installSubscribeMock(status({ hasChanges: true }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    applyTransportGap({ channel: 'system:stats', seq: 3 });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    a.release();
  });
});

describe('gitStatusStore — observed branch reconciliation', () => {
  it('persists an observed branch for the workspace and syncs every returned row', async () => {
    const pane = buildPane(makeThread({ branch: 'stale' }));
    const updateBranch = setBindingMock('UpdateThreadBranch', async (_workspace, branch) => [
      makeThread({ branch: branch as string }),
    ]);
    installSubscribeMock(status({ branch: 'feature/live', isDefaultBranch: false }));

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await vi.waitFor(() => {
      expect(updateBranch).toHaveBeenCalledWith(WORKSPACE_PATH, 'feature/live');
      expect(pane.thread?.branch).toBe('feature/live');
    });
    a.release();
  });

  it('syncs rows the backend hands back even when they are not the attaching thread', async () => {
    // Two threads share the worktree; the branch is a fact about the
    // checkout, so BOTH rows come back and both panes must learn it.
    const paneA = buildPane(makeThread({ id: 'thread-1', branch: 'main' }));
    const paneB = buildPane(makeThread({ id: 'thread-2', branch: 'main' }));
    setBindingMock('UpdateThreadBranch', async (_workspace, branch) => [
      makeThread({ id: 'thread-1', branch: branch as string }),
      makeThread({ id: 'thread-2', branch: branch as string }),
    ]);
    installSubscribeMock(status({ branch: 'feature/shared', isDefaultBranch: false }));

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await vi.waitFor(() => {
      expect(paneA.thread?.branch).toBe('feature/shared');
      expect(paneB.thread?.branch).toBe('feature/shared');
    });
    a.release();
  });

  it('does not re-persist when a push carries the same branch, and never for a non-repo', async () => {
    buildPane(makeThread({ branch: 'main' }));
    installSubscribeMock(status({ branch: 'main' }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    const updateBranch = setBindingMock('UpdateThreadBranch', async () => [makeThread()]);
    emitWailsEvent('git:status', { cwd: CWD, status: status({ branch: 'main', hasChanges: true }) });
    emitWailsEvent('git:status', { cwd: CWD, status: status({ isRepo: false, branch: 'ignored' }) });
    await flush();

    expect(updateBranch).not.toHaveBeenCalled();
    a.release();
  });

  // The backend answers a no-change write with no rows at all, and that is
  // the common case: every attach persists its first observation, and the
  // branch usually already matches. It must cost nothing on this side.
  it('does zero work when the backend reports no rows changed', async () => {
    const pane = buildPane(makeThread({ branch: 'main' }));
    const replaceThread = vi.spyOn(pane, 'replaceThread');
    const updateBranch = setBindingMock('UpdateThreadBranch', async () => null);
    installSubscribeMock(status({ branch: 'main' }));

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await vi.waitFor(() => expect(updateBranch).toHaveBeenCalledWith(WORKSPACE_PATH, 'main'));
    await flush();

    expect(replaceThread).not.toHaveBeenCalled();
    a.release();
    replaceThread.mockRestore();
  });

  it('collapses a burst of branch flips into the latest write per workspace', async () => {
    buildPane(makeThread({ branch: 'main' }));
    installSubscribeMock(status({ branch: 'main' }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    // One release per in-flight write, so the queue can be stepped.
    const releases: Array<() => void> = [];
    const seen: string[] = [];
    const updateBranch = setBindingMock('UpdateThreadBranch', (_workspace, branch) => {
      seen.push(branch as string);
      return new Promise((resolve) => {
        releases.push(() => resolve([makeThread({ branch: branch as string })]));
      });
    });

    emitWailsEvent('git:status', { cwd: CWD, status: status({ branch: 'a', isDefaultBranch: false }) });
    await flush();
    emitWailsEvent('git:status', { cwd: CWD, status: status({ branch: 'b', isDefaultBranch: false }) });
    emitWailsEvent('git:status', { cwd: CWD, status: status({ branch: 'c', isDefaultBranch: false }) });
    await flush();
    // Only 'a' is in flight; 'b' and 'c' collapsed into one queued write.
    expect(updateBranch).toHaveBeenCalledTimes(1);

    releases[0]();
    // 'b' is never written — an older observation must not land after a newer.
    await vi.waitFor(() => expect(seen).toEqual(['a', 'c']));
    releases[1]();
    a.release();
  });

  it('surfaces a persist failure on the panes still in that workspace, and nowhere else', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const here = buildPane(makeThread({ id: 'thread-1', branch: 'main' }));
    const elsewhere = buildPane(
      makeThread({ id: 'thread-2', branch: 'main', workspacePath: '/other', projectPath: '/other' }),
    );
    const hereError = vi.spyOn(here, 'setGeneralError');
    const elsewhereError = vi.spyOn(elsewhere, 'setGeneralError');

    setBindingMock('UpdateThreadBranch', async () => {
      throw new Error('db locked');
    });
    installSubscribeMock(status({ branch: 'feature/x', isDefaultBranch: false }));

    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await vi.waitFor(() => expect(hereError).toHaveBeenCalled());
    expect(String(hereError.mock.calls[0][0])).toContain('Failed to update thread branch');
    expect(elsewhereError).not.toHaveBeenCalled();
    a.release();
    vi.restoreAllMocks();
  });
});

describe('gitStatusStore — refreshNow', () => {
  it('applies the refreshed status through the same chokepoint (branch reconciled)', async () => {
    buildPane(makeThread({ branch: 'main' }));
    installSubscribeMock(status({ branch: 'main' }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    const updateBranch = setBindingMock('UpdateThreadBranch', async (_workspace, branch) => [
      makeThread({ branch: branch as string }),
    ]);
    setBindingMock('GetGitStatus', async () =>
      status({ branch: 'feature/refreshed', insertions: 9, isDefaultBranch: false }),
    );
    await refreshGitStatus(WORKSPACE, ref(WORKSPACE_PATH), () => WORKSPACE);

    expect(a.current?.insertions).toBe(9);
    await vi.waitFor(() => expect(updateBranch).toHaveBeenCalledWith(WORKSPACE_PATH, 'feature/refreshed'));
    a.release();
  });

  it('records a refresh failure as an error on the workspace', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    buildPane();
    installSubscribeMock(status());
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    setBindingMock('GetGitStatus', async () => {
      throw new Error('git busy');
    });
    await refreshGitStatus(WORKSPACE, ref(WORKSPACE_PATH), () => WORKSPACE);
    expect(a.error).toBe('git busy');
    a.release();
    vi.restoreAllMocks();
  });

  it('drops a refresh whose thread left the workspace mid-flight', async () => {
    const pane = buildPane(makeThread({ branch: 'main' }));
    installSubscribeMock(status({ branch: 'main' }));
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();

    const updateBranch = setBindingMock('UpdateThreadBranch', async () => [makeThread()]);
    let resolveStatus: (s: GitStatus) => void = () => {};
    setBindingMock(
      'GetGitStatus',
      () =>
        new Promise<GitStatus>((resolve) => {
          resolveStatus = resolve;
        }),
    );
    // The verifier is the caller's — createGitStatusView passes exactly
    // this, re-deriving the key from the thread it owns.
    const pending = refreshGitStatus(WORKSPACE, ref(WORKSPACE_PATH), () =>
      workspaceKeyForThread(pane.thread ?? null),
    );

    // A worktree switch lands while GetGitStatus is in flight: its answer
    // describes the NEW checkout, so applying it to the old workspace key
    // would paint the wrong status AND persist the wrong branch onto every
    // thread still in the old workspace.
    pane.replaceThread(makeThread({ workspacePath: '/moved', projectPath: '/moved' }));
    resolveStatus(status({ branch: 'feature/moved', insertions: 42, isDefaultBranch: false }));
    await pending;
    await flush();

    expect(a.current?.branch).toBe('main');
    expect(a.current?.insertions).toBe(0);
    expect(updateBranch).not.toHaveBeenCalled();
    a.release();
  });
});

describe('gitStatusStore — test-seam hygiene', () => {
  // The seam suspends the WHOLE store, so a real attach alongside it would
  // register a reference that never sources and render blank forever. It
  // has to fail loudly and name the seam, not quietly do nothing.
  it('refuses an attach for a key the seam has not seeded', () => {
    __seedGitStatusForTest(WORKSPACE, status({ hasChanges: true }));
    expect(() => attachGitStatus('/other/workspace', { workspace: ref('/other/workspace') })).toThrow(
      /__seedGitStatusForTest/,
    );
    // A seeded key still attaches and reads the seed.
    const held = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    expect(held.current?.hasChanges).toBe(true);
    held.release();
    __resetGitStatusStoreForTest();
  });

  it('reset drops every live key so a leaked entry cannot answer the next test', async () => {
    buildPane();
    installSubscribeMock(status());
    const a = attachGitStatus(WORKSPACE, { workspace: ref(WORKSPACE_PATH) });
    await flush();
    expect(gitStatusKeys()).toEqual([WORKSPACE]);

    a.release();
    __resetGitStatusStoreForTest();
    expect(gitStatusKeys()).toEqual([]);
    expect(peekGitStatus(WORKSPACE)).toBeNull();
  });
});
