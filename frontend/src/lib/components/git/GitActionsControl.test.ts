import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import GitActionsControl from './GitActionsControl.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { GitStatus } from '../../types/git';
import type { Thread } from '../../types/models';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';

// Force the transport status mirror to "connected" so the subscribe
// $effect runs. The real store reads from wsClient, which is never
// brought up in jsdom-style tests.
vi.mock('../../stores/transportStatus.svelte', () => ({
  getTransportStatus: () => ({ status: 'connected', nextAttemptAt: null }),
  retryTransport: () => {},
  resetTransportStatusForTest: () => {},
}));

// Svelte transitions poke Element.animate on mount; jsdom lacks it.
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

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
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

// installSubscribeMock wires up the subscribe/unsubscribe pair so a
// test gets the same subscription ID and can drive subsequent
// "git:status" events through emitWailsEvent. Returns the assigned id
// + a counter for assertion convenience.
function installSubscribeMock(initial: GitStatus, id = 'sub-1') {
  const subscribeFn = setBindingMock(
    'GitStatusSubscribe',
    async () => ({ id, status: initial }),
  );
  setBindingMock('GitStatusUnsubscribe', async () => {});
  return { id, subscribeFn };
}

describe('<GitActionsControl> subscribe model', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    await loadSettings();
  });

  it('renders nothing when the workspace is not a git repo', async () => {
    const pane = await buildPane();
    installSubscribeMock(status({ isRepo: false, branch: '' }));
    const { queryByRole, queryByTestId, container } = render(GitActionsControl, { props: { pane } });
    await flush();
    expect(queryByRole('menuitem', { name: /Ship Changes/i })).toBeNull();
    expect(queryByTestId('git-actions-error')).toBeNull();
    expect(container.querySelector('button[aria-label="More git actions"]')).toBeNull();
  });

  it('shows the retry affordance when GitStatusSubscribe rejects', async () => {
    const pane = await buildPane();
    setBindingMock('GitStatusSubscribe', async () => {
      throw new Error('ENOENT git');
    });
    setBindingMock('GitStatusUnsubscribe', async () => {});
    const { findByTestId } = render(GitActionsControl, { props: { pane } });
    const errorButton = await findByTestId('git-actions-error');
    expect(errorButton).toBeInTheDocument();
  });

  it('renders the Ship Changes menu entry in a valid repo', async () => {
    const pane = await buildPane();
    installSubscribeMock(status({ isRepo: true, hasChanges: true }));
    const { container, queryByTestId, findByRole } = render(GitActionsControl, { props: { pane } });
    await flush();

    expect(queryByTestId('git-actions-error')).toBeNull();

    const trigger = container.querySelector<HTMLButtonElement>('button[aria-label="More git actions"]');
    expect(trigger).not.toBeNull();
    await fireEvent.click(trigger!);
    const shipRow = await findByRole('menuitem', { name: /Ship Changes/i });
    expect(shipRow).toBeInTheDocument();
  });

  it('updates label when a "git:status" event arrives for the active subscription', async () => {
    const pane = await buildPane();
    const { id } = installSubscribeMock(status({ hasChanges: true }));
    const { container } = render(GitActionsControl, { props: { pane } });
    await flush();

    const primary = container.querySelector<HTMLButtonElement>('div.flex > button:first-of-type');
    expect(primary?.textContent?.trim()).toBe('Commit');

    // Backend pushes a status update reflecting "no changes, branch
    // is up to date". The component should re-render to "Commit"
    // (disabled, "No changes to commit") since hasChanges flipped false.
    emitWailsEvent('git:status', {
      subscriptionId: id,
      status: status({ hasChanges: false, aheadCount: 2 }),
    });
    await flush();

    expect(primary?.textContent?.trim()).toBe('Push');
  });

  it('ignores "git:status" events targeted at a different subscription', async () => {
    const pane = await buildPane();
    const { id } = installSubscribeMock(status({ hasChanges: true }));
    const { container } = render(GitActionsControl, { props: { pane } });
    await flush();

    const primary = container.querySelector<HTMLButtonElement>('div.flex > button:first-of-type');
    expect(primary?.textContent?.trim()).toBe('Commit');

    // Stale event from a different subscription must NOT update us.
    emitWailsEvent('git:status', {
      subscriptionId: `not-${id}`,
      status: status({ hasChanges: false, aheadCount: 5 }),
    });
    await flush();

    expect(primary?.textContent?.trim()).toBe('Commit');
  });

  it('does NOT re-subscribe when pane.replaceThread updates token usage', async () => {
    // Regression test for the per-token flicker. The old code tracked
    // pane.threadId in a $effect; every pane.replaceThread() call (for
    // token-usage / mode / hasIncompleteTurn updates during a turn)
    // re-fired the effect, wiped status, and re-fetched. The new
    // $derived(gitCwd) value-equality must short-circuit those re-runs.
    const thread = makeThread({ workspacePath: '/workspace' });
    const pane = await buildPane(thread);
    const { subscribeFn } = installSubscribeMock(status({ hasChanges: true }));
    render(GitActionsControl, { props: { pane } });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(1);

    // Simulate a per-turn usage event reassigning pane.thread without
    // changing the workspace path. With the old effect this would have
    // wiped status and called GetGitStatus again; with the new
    // subscribe-on-cwd model, nothing changes for git status.
    pane.replaceThread({ ...thread, lastTokenUsage: '{"used":1234}' });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(getBindingMock('GitStatusUnsubscribe')).not.toHaveBeenCalled();
  });

  it('resubscribes when the workspace path changes', async () => {
    const thread = makeThread({ workspacePath: '/workspace' });
    const pane = await buildPane(thread);
    const { subscribeFn } = installSubscribeMock(status());
    render(GitActionsControl, { props: { pane } });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(1);

    // Switching to a worktree path should release the old sub and
    // open a new one — different cwd, different watcher.
    pane.replaceThread({ ...thread, worktreePath: '/wt/branch-a' });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(2);
    expect(getBindingMock('GitStatusUnsubscribe')).toHaveBeenCalledTimes(1);
  });

  it('releases orphan subscription when cwd changes mid-Subscribe', async () => {
    // The $effect's async-IIFE pattern: if the effect re-runs while
    // a Subscribe RPC is still in flight, the freshly-created
    // subscription would orphan unless the cancelled-guard releases
    // it via Unsubscribe. Without the eager release, the only safety
    // net is the connection-drop cleanup at app shutdown — fine in
    // production but a leak across thread switches in unit-test
    // scope.
    const thread = makeThread({ workspacePath: '/workspace' });
    const pane = await buildPane(thread);

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

    render(GitActionsControl, { props: { pane } });
    await flush();

    // First subscribe is in flight (firstPromise hasn't resolved).
    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(unsubscribeFn).not.toHaveBeenCalled();

    // Trigger effect re-run via cwd change. The cleanup function
    // sets `cancelled = true` for the still-awaiting first call.
    pane.replaceThread({ ...thread, worktreePath: '/wt/branch-b' });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(2);

    // Resolve the orphaned first subscribe. The cancelled-guard
    // should release it via Unsubscribe rather than leaking.
    resolveFirst({ id: 'orphan-1', status: status() });
    await flush();

    const unsubCalls = unsubscribeFn.mock.calls.map((c) => c[0]);
    expect(unsubCalls).toContain('orphan-1');
  });

  // NOTE: A test for "transport disconnect → connect re-subscribes"
  // would belong here, but the vi.mock factory above pins
  // getTransportStatus() to a constant snapshot. Making it reactive
  // would require a $state-backed shim, which can't live in a plain
  // .ts file. The reactive path is exercised in production via
  // transportStatus.svelte.ts (the real $state); the contract here
  // is that the $effect tracks `transportConnected`, which the
  // existing tests indirectly cover by passing 'connected' at mount
  // time and observing subscribe/unsubscribe behavior.
});

describe('<GitActionsControl> forge labels', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    await loadSettings();
  });

  it('renders "Create PR" for github forge', async () => {
    const pane = await buildPane();
    installSubscribeMock(status({
      forge: 'github',
      branch: 'feature',
      isDefaultBranch: false,
    }));
    const { findByLabelText, getByText } = render(GitActionsControl, { props: { pane } });
    const more = await findByLabelText('More git actions');
    await fireEvent.click(more);
    await flush();
    expect(getByText('Create PR')).toBeTruthy();
  });

  it('renders "Create MR" for gitlab forge', async () => {
    const pane = await buildPane();
    installSubscribeMock(status({
      forge: 'gitlab',
      branch: 'feature',
      isDefaultBranch: false,
    }));
    const { findByLabelText, getByText } = render(GitActionsControl, { props: { pane } });
    const more = await findByLabelText('More git actions');
    await fireEvent.click(more);
    await flush();
    expect(getByText('Create MR')).toBeTruthy();
  });

  it('disables Create PR menu item when forge is unsupported', async () => {
    const pane = await buildPane();
    installSubscribeMock(status({
      forge: '',
      branch: 'feature',
      isDefaultBranch: false,
    }));
    const { findByLabelText, getByText } = render(GitActionsControl, { props: { pane } });
    const more = await findByLabelText('More git actions');
    await fireEvent.click(more);
    await flush();
    const item = getByText('Create PR').closest('[role="menuitem"]');
    expect(item?.getAttribute('aria-disabled')).toBe('true');
  });
});
