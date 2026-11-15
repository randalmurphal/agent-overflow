// Ordering of the four writers of a workspace's git status (the subscribe
// reply, `git:status` frames, the `refreshGitStatus` answer, the checkout
// answer) by the `seq` each status carries.

import { afterEach, describe, expect, it, beforeEach, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import { resetPanesForTest } from './panes.svelte';
import {
  __resetGitStatusStoreForTest,
  __seedGitStatusForTest,
  attachGitStatus,
  type GitStatusCtx,
  peekGitStatus,
  refreshGitStatus,
  resyncGitStatusAfterGap,
  withCheckoutAnswer,
} from './gitStatusStore.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import type { GitStatus, GitWorkspaceState, WorkspaceRef } from '../types/git';
import { composeWorkspaceKey } from '../utils/workspaceKey';
import { HOME_BACKEND } from '../transport/backendKey';
import { __resetEntityIndexForTest } from '../transport/entityIndex';
import { REMOTE_BACKEND_UUID, resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';

const WORKSPACE_PATH = '/workspace';
const WORKSPACE = composeWorkspaceKey(HOME_BACKEND, WORKSPACE_PATH);
const LAPTOP_WORKSPACE = composeWorkspaceKey('laptop', WORKSPACE_PATH);
const CWD = '/canonical/workspace';
const REF: WorkspaceRef = { projectId: 'project-1', workspacePath: WORKSPACE_PATH };

// `insertions` names which status is shown; `seq` orders it.
function status(insertions: number, seq?: number): GitStatus {
  return {
    isRepo: true,
    branch: 'main',
    isDefaultBranch: true,
    hasChanges: insertions > 0,
    insertions,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'github',
    ...(seq === undefined ? {} : { seq }),
  };
}

function frame(insertions: number, seq?: number, backendId?: string): void {
  emitWailsEvent('git:status', { cwd: CWD, status: status(insertions, seq) }, backendId);
}

interface PendingSubscribe {
  resolve(insertions: number, seq?: number): void;
  reject(err: Error): void;
}

/** Every GitStatusSubscribe waits until the test answers it. */
function gateSubscribes(): PendingSubscribe[] {
  const pending: PendingSubscribe[] = [];
  setBindingMock('GitStatusSubscribe', () => new Promise((resolve, reject) => {
    const id = `sub-${pending.length}`;
    pending.push({
      resolve: (insertions, seq) => resolve({ id, cwd: CWD, status: status(insertions, seq) }),
      reject,
    });
  }));
  setBindingMock('GitStatusUnsubscribe', async () => {});
  return pending;
}

function answerSubscribes(insertions: number, seq?: number): void {
  setBindingMock('GitStatusSubscribe', async () => ({ id: 'sub', cwd: CWD, status: status(insertions, seq) }));
  setBindingMock('GitStatusUnsubscribe', async () => {});
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

function shown(key = WORKSPACE): number | undefined {
  return peekGitStatus(key)?.insertions;
}

/** Counts how often a reader of the workspace's status is woken. */
function watchReader(key = WORKSPACE): { wakes: () => number; stop: () => void } {
  let runs = 0;
  const stop = $effect.root(() => {
    $effect(() => {
      peekGitStatus(key);
      runs += 1;
    });
  });
  flushSync();
  const base = runs;
  return { wakes: () => runs - base, stop };
}

function checkoutState(insertions: number, seq?: number): GitWorkspaceState {
  return { workspacePath: WORKSPACE_PATH, worktreePath: '', branch: 'main', status: status(insertions, seq) } as GitWorkspaceState;
}

function gateRefresh(): (insertions: number, seq?: number) => void {
  let answer: (s: GitStatus) => void = () => {};
  setBindingMock('GetGitStatus', () => new Promise<GitStatus>((resolve) => {
    answer = resolve;
  }));
  return (insertions, seq) => answer(status(insertions, seq));
}

const held: Array<{ release(): void }> = [];

/** Released after each test, so a failed assertion leaks nothing into the next. */
function attach(key: string, ctx: GitStatusCtx): ReturnType<typeof attachGitStatus> {
  const attachment = attachGitStatus(key, ctx);
  held.push(attachment);
  return attachment;
}

afterEach(() => {
  for (const attachment of held.splice(0)) attachment.release();
});

beforeEach(() => {
  resetPanesForTest();
  setBindingMock('UpdateThreadBranch', async () => null);
  __resetEntityIndexForTest();
});

describe('gitStatusStore — sequenced frames', () => {
  it('drops a frame at or below the held seq without waking a reader, and applies a newer one', async () => {
    answerSubscribes(10, 10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    expect(shown()).toBe(10);
    const reader = watchReader();
    try {
      frame(5, 5);
      frame(9, 10);
      flushSync();
      expect(shown()).toBe(10);
      expect(reader.wakes()).toBe(0);

      frame(11, 11);
      flushSync();
      expect(shown()).toBe(11);
      expect(reader.wakes()).toBe(1);
    } finally {
      reader.stop();
      a.release();
    }
  });
});

describe('gitStatusStore — equal seq', () => {
  it('keeps the held value without a same-reference diagnostic or a woken reader', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    answerSubscribes(10, 10);
    attach(WORKSPACE, { workspace: REF });
    await flush();
    const reader = watchReader();
    try {
      frame(97, 10);
      setBindingMock('GetGitStatus', async () => status(98, 10));
      await refreshGitStatus(WORKSPACE, REF, () => WORKSPACE);
      await withCheckoutAnswer(REF, async () => checkoutState(99, 10));
      answerSubscribes(96, 10);
      resyncGitStatusAfterGap(HOME_BACKEND);
      await flush();
      flushSync();
      expect(shown()).toBe(10);
      expect(reader.wakes()).toBe(0);
      const messages = consoleError.mock.calls.map((call) => String(call[0]));
      expect(messages.filter((m) => m.includes('applied the same object reference'))).toEqual([]);
    } finally {
      reader.stop();
      vi.restoreAllMocks();
    }
  });

  it('the test seed replaces the value even when handed the same object twice', () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      const seeded = status(5);
      __seedGitStatusForTest(WORKSPACE, seeded);
      __seedGitStatusForTest(WORKSPACE, seeded);
      const messages = consoleError.mock.calls.map((call) => String(call[0]));
      expect(messages.filter((m) => m.includes('applied the same object reference'))).toEqual([]);
      expect(shown()).toBe(5);
    } finally {
      __resetGitStatusStoreForTest();
      vi.restoreAllMocks();
    }
  });
});

describe('gitStatusStore — sequenced answers', () => {
  it('a refresh or checkout answer equal to the held seq wakes no reader', async () => {
    answerSubscribes(10, 10);
    attach(WORKSPACE, { workspace: REF });
    await flush();
    const reader = watchReader();
    try {
      setBindingMock('GetGitStatus', async () => status(99, 10));
      await refreshGitStatus(WORKSPACE, REF, () => WORKSPACE);
      await withCheckoutAnswer(REF, async () => checkoutState(98, 10));
      flushSync();
      expect(shown()).toBe(10);
      expect(reader.wakes()).toBe(0);
    } finally {
      reader.stop();
    }
  });

  it('a refresh answer older than a frame that landed during the call does not overwrite it', async () => {
    answerSubscribes(10, 10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();

    const answer = gateRefresh();
    const pending = refreshGitStatus(WORKSPACE, REF, () => WORKSPACE);
    await flush();
    frame(12, 12);
    answer(11, 11);
    await pending;
    expect(shown()).toBe(12);
    a.release();
  });

  it('a refresh answer newer than a frame that landed during the call applies', async () => {
    answerSubscribes(10, 10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();

    const answer = gateRefresh();
    const pending = refreshGitStatus(WORKSPACE, REF, () => WORKSPACE);
    await flush();
    frame(12, 12);
    answer(13, 13);
    await pending;
    expect(shown()).toBe(13);
    a.release();
  });

  it('a checkout answer older than a frame that landed during the call does not overwrite it', async () => {
    answerSubscribes(10, 10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();

    await withCheckoutAnswer(REF, async () => {
      frame(12, 12);
      return checkoutState(11, 11);
    });
    expect(shown()).toBe(12);
    a.release();
  });

  it('a checkout answer newer than a frame that landed during the call applies', async () => {
    answerSubscribes(10, 10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();

    await withCheckoutAnswer(REF, async () => {
      frame(12, 12);
      return checkoutState(13, 13);
    });
    expect(shown()).toBe(13);
    a.release();
  });
});

describe('gitStatusStore — frames that overtake the subscribe reply', () => {
  it('applies a newer frame after the reply registers its alias', async () => {
    const subscribes = gateSubscribes();
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    frame(12, 12);
    subscribes[0].resolve(10, 10);
    await flush();
    expect(shown()).toBe(12);
    a.release();
  });

  it('keeps the reply when the frame that overtook it is older', async () => {
    const subscribes = gateSubscribes();
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    frame(8, 8);
    subscribes[0].resolve(10, 10);
    await flush();
    expect(shown()).toBe(10);
    a.release();
  });

  it('keeps a frame for a new spelling of a cwd another key already routes', async () => {
    answerSubscribes(10, 10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    const subscribes = gateSubscribes();
    const sibling = composeWorkspaceKey(HOME_BACKEND, '/private/workspace');
    attach(sibling, { workspace: { projectId: 'project-1', workspacePath: '/private/workspace' } });
    await flush();
    frame(12, 12);
    expect(shown()).toBe(12);
    subscribes[0].resolve(11, 11);
    await flush();
    expect(shown(sibling)).toBe(12);
    a.release();
  });

  it('keeps the newest frame per cwd, not the last to arrive', async () => {
    const subscribes = gateSubscribes();
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    frame(12, 12);
    frame(11, 11);
    subscribes[0].resolve(10, 10);
    await flush();
    expect(shown()).toBe(12);
    a.release();
  });

  it('keeps frames only while a subscribe is in flight on their backend', async () => {
    frame(50, 50);
    answerSubscribes(10, 10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    expect(shown()).toBe(10);
    a.release();
  });

  it('drops kept frames when the subscribe fails', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      const subscribes = gateSubscribes();
      const a = attach(WORKSPACE, { workspace: REF });
      await flush();
      frame(50, 50);
      subscribes[0].reject(new Error('refused'));
      await flush();
      expect(a.error).toBe('refused');

      resyncGitStatusAfterGap(HOME_BACKEND);
      await flush();
      subscribes[1].resolve(10, 10);
      await flush();
      expect(shown()).toBe(10);
      a.release();
    } finally {
      vi.restoreAllMocks();
    }
  });

  it('drops kept frames when the subscribe is aborted, even if its RPC never settles', async () => {
    const subscribes = gateSubscribes();
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    frame(50, 50);
    // A restart: the old process's subscribe never answers, and the new
    // process counts from one again.
    __setTransportStatusForTest({ status: 'reconnecting', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(subscribes).toHaveLength(2);
    subscribes[1].resolve(3, 3);
    await flush();
    expect(shown()).toBe(3);
    a.release();
  });

  it('never applies a kept frame to a key on another backend', async () => {
    localStorage.setItem(
      'agent-overflow:deviceSession:laptop',
      JSON.stringify({ sessionId: 's-laptop', credential: 'c', expiresAtMs: Date.now() + 60_000, scopes: ['git:operate'] }),
    );
    stageBackend();
    const subscribes = gateSubscribes();
    const home = attach(WORKSPACE, { workspace: REF });
    const laptop = attach(LAPTOP_WORKSPACE, { workspace: { projectId: 'project-remote', workspacePath: WORKSPACE_PATH } });
    try {
      await flush();
      expect(subscribes).toHaveLength(2);
      // Both machines canonicalize to the same cwd; the frame is the laptop's.
      frame(50, 50, REMOTE_BACKEND_UUID);
      subscribes[0].resolve(10, 10);
      subscribes[1].resolve(10, 10);
      await flush();
      expect(shown(WORKSPACE)).toBe(10);
      expect(shown(LAPTOP_WORKSPACE)).toBe(50);
    } finally {
      home.release();
      laptop.release();
      resetStagedBackends();
      localStorage.removeItem('agent-overflow:deviceSession:laptop');
    }
  });
});

describe('gitStatusStore — re-sourcing', () => {
  it('a transport edge accepts a reply below the seq held before it, then orders frames against it', async () => {
    answerSubscribes(100, 100);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    expect(shown()).toBe(100);

    // A restarted backend counts from one again.
    answerSubscribes(3, 3);
    __setTransportStatusForTest({ status: 'reconnecting', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(shown()).toBe(3);

    frame(2, 2);
    expect(shown()).toBe(3);
    frame(4, 4);
    expect(shown()).toBe(4);
    a.release();
  });

  it('a re-source on the same connection keeps ordering against the held status', async () => {
    answerSubscribes(10, 10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();

    const subscribes = gateSubscribes();
    resyncGitStatusAfterGap(HOME_BACKEND);
    await flush();
    expect(subscribes).toHaveLength(1);

    const answer = gateRefresh();
    const pending = refreshGitStatus(WORKSPACE, REF, () => WORKSPACE);
    await flush();
    answer(8, 8);
    await pending;
    expect(shown()).toBe(10);

    subscribes[0].resolve(9, 9);
    await flush();
    expect(shown()).toBe(10);
    frame(11, 11);
    expect(shown()).toBe(11);
    a.release();
  });

  it('a reply equal to the held seq after a failed subscribe clears the error and wakes no reader', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    let reader: ReturnType<typeof watchReader> | null = null;
    try {
      answerSubscribes(10, 10);
      const a = attach(WORKSPACE, { workspace: REF });
      await flush();

      setBindingMock('GitStatusSubscribe', async () => {
        throw new Error('refused');
      });
      resyncGitStatusAfterGap(HOME_BACKEND);
      await flush();
      expect(a.error).toBe('refused');
      expect(shown()).toBe(10);

      reader = watchReader();
      answerSubscribes(99, 10);
      resyncGitStatusAfterGap(HOME_BACKEND);
      await flush();
      flushSync();
      expect(a.error).toBeNull();
      expect(shown()).toBe(10);
      expect(reader.wakes()).toBe(0);
    } finally {
      reader?.stop();
      vi.restoreAllMocks();
    }
  });

  it('a superseded run that resolves late changes neither the status, its seq, nor the routing', async () => {
    const subscribes = gateSubscribes();
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    resyncGitStatusAfterGap(HOME_BACKEND);
    await flush();
    expect(subscribes).toHaveLength(2);

    subscribes[1].resolve(10, 10);
    await flush();
    frame(12, 12);
    expect(shown()).toBe(12);

    // The superseded run answers with a higher seq; it may not land.
    subscribes[0].resolve(50, 50);
    await flush();
    expect(shown()).toBe(12);
    frame(11, 11);
    expect(shown()).toBe(12);
    frame(13, 13);
    expect(shown()).toBe(13);
    a.release();
  });

  it('a superseded run settling late keeps the frames held for the live run', async () => {
    const subscribes = gateSubscribes();
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    resyncGitStatusAfterGap(HOME_BACKEND);
    await flush();
    frame(12, 12);
    subscribes[0].resolve(50, 50);
    await flush();
    subscribes[1].resolve(10, 10);
    await flush();
    expect(shown()).toBe(12);
    a.release();
  });
});

describe('gitStatusStore — unsequenced statuses (older backend)', () => {
  it('a frame applies whatever it follows, and one that overtakes the reply is dropped', async () => {
    const subscribes = gateSubscribes();
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    frame(50);
    subscribes[0].resolve(10);
    await flush();
    expect(shown()).toBe(10);

    frame(7);
    expect(shown()).toBe(7);
    a.release();
  });

  it('a refresh answer applies even over a frame that landed during the call', async () => {
    answerSubscribes(10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();

    const answer = gateRefresh();
    const pending = refreshGitStatus(WORKSPACE, REF, () => WORKSPACE);
    await flush();
    frame(12);
    answer(11);
    await pending;
    expect(shown()).toBe(11);
    a.release();
  });

  it('a checkout answer stands down when a status landed during the call, and applies otherwise', async () => {
    answerSubscribes(10);
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();

    await withCheckoutAnswer(REF, async () => {
      frame(12);
      return checkoutState(11);
    });
    expect(shown()).toBe(12);

    await withCheckoutAnswer(REF, async () => checkoutState(13));
    expect(shown()).toBe(13);
    a.release();
  });

  it('a checkout answer for a workspace that held no status when the call began changes nothing', async () => {
    const subscribes = gateSubscribes();
    const a = attach(WORKSPACE, { workspace: REF });
    await flush();
    await withCheckoutAnswer(REF, async () => checkoutState(13));
    expect(shown()).toBeUndefined();
    subscribes[0].resolve(10);
    await flush();
    expect(shown()).toBe(10);
    a.release();
  });
});
