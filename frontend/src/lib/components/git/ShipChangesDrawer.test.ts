import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ShipChangesDrawer from './ShipChangesDrawer.svelte';
import { createShipChangesState } from '../../stores/shipChanges.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { getToasts } from '../../stores/toast.svelte';
import type { GitActionResult, GitStatus } from '../../types/git';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { __seedGitStatusForTest } from '../../stores/gitStatusStore.svelte';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';

// Element.animate shim for jsdom — Svelte transitions poke at it on mount.
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

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'feature',
    isDefaultBranch: false,
    hasChanges: true,
    insertions: 4,
    deletions: 1,
    fileCount: 2,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'github',
    ...overrides,
  };
}

const WORKSPACE = '/ship-workspace';

async function buildPane() {
  return buildRegisteredPane(makeBaseThread({
    id: 't-1',
    title: 't',
    workspacePath: WORKSPACE,
    projectPath: WORKSPACE,
    model: 'm',
  }));
}

/**
 * The drawer no longer fetches status: it reads the workspace's shared
 * observation and only calls GetGitStatus for the post-action refresh. So a
 * test declares the world in two places at once — the store the drawer opens
 * on, and the refresh's answer — and unless it overrides the latter, both say
 * the same thing.
 */
function installStatus(s: GitStatus): void {
  __seedGitStatusForTest(WORKSPACE, s);
  setBindingMock('GetGitStatus', async () => s);
}

async function flush(n = 6): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

describe('<ShipChangesDrawer>', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    await loadSettings();
  });

  it('does not render when `open` is false', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(ShipChangesDrawer, {
      props: { open: false, pane, onClose: () => {} },
    });
    expect(queryByTestId('ship-changes-drawer')).toBeNull();
  });

  it('loads git status and lands on the commit step when there are changes', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    expect(await findByTestId('ship-changes-step-commit')).toBeInTheDocument();
  });

  it('lands on the push step when there are no changes but commits ahead', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: false, aheadCount: 3 }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    expect(await findByTestId('ship-changes-step-push')).toBeInTheDocument();
  });

  it('lands on the PR step when branch is clean and up to date with upstream', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: false, aheadCount: 0 }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    expect(await findByTestId('ship-changes-step-pr')).toBeInTheDocument();
  });

  it('calls GitCommit with trimmed subject/body and advances to push', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    const commit = setBindingMock('GitCommit', async () => ({
      action: 'commit',
      commitSha: 'sha-123',
    } as GitActionResult));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const subject = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: '  Ship it  ' } });
    await flush();
    await fireEvent.click(getByTestId('ship-changes-commit-submit'));
    await flush(10);

    expect(commit.mock.calls.length).toBe(1);
    expect(commit.mock.calls[0][1]).toBe('Ship it');
    expect(commit.mock.calls[0][2]).toBe('');
    // After commit, push step is visible.
    expect(await findByTestId('ship-changes-step-push')).toBeInTheDocument();
  });

  it('refreshes the pane git-status slot after a successful commit', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    // The post-action refresh is the only GetGitStatus call now, so it can
    // state the world the commit left behind unconditionally.
    setBindingMock('GetGitStatus', async () => status({ hasChanges: false, aheadCount: 1 }));
    setBindingMock('GitCommit', async () => ({
      action: 'commit',
      commitSha: 'sha-123',
    } as GitActionResult));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const subject = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'Refresh status' } });
    await flush();
    await fireEvent.click(getByTestId('ship-changes-commit-submit'));
    await flush(10);

    expect(pane.gitStatus.status?.hasChanges).toBe(false);
    expect(pane.gitStatus.status?.aheadCount).toBe(1);
  });

  it('surfaces a commit error inline and offers a retry', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    setBindingMock('GitCommit', async () => { throw new Error('pre-commit failed'); });
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const subject = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'x' } });
    await flush();
    await fireEvent.click(getByTestId('ship-changes-commit-submit'));
    await flush(10);

    const errEl = await findByTestId('ship-changes-commit-error');
    expect(errEl.textContent).toMatch(/pre-commit failed/);
    // Retry button lets the user edit and try again.
    expect(getByTestId('ship-changes-commit-retry')).toBeInTheDocument();
  });

  it('skipCommit button jumps the user to the push step', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    await findByTestId('ship-changes-step-commit');
    await fireEvent.click(getByTestId('ship-changes-commit-skip'));
    await flush();
    expect(await findByTestId('ship-changes-step-push')).toBeInTheDocument();
  });

  it('calls GitPush and advances to PR step on success', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: false, aheadCount: 1 }));
    setBindingMock('GetGitStatus', async () => status({ hasChanges: false, aheadCount: 0 }));
    const push = setBindingMock('GitPush', async () => ({ action: 'push' } as GitActionResult));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    await findByTestId('ship-changes-step-push');
    await fireEvent.click(getByTestId('ship-changes-push-submit'));
    await flush(10);
    expect(push.mock.calls.length).toBe(1);
    expect(pane.gitStatus.status?.aheadCount).toBe(0);
    expect(await findByTestId('ship-changes-step-pr')).toBeInTheDocument();
  });

  it('surfaces a push error and allows retry', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: false, aheadCount: 1 }));
    setBindingMock('GitPush', async () => ({ action: 'push', error: 'auth required' } as GitActionResult));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    await findByTestId('ship-changes-step-push');
    await fireEvent.click(getByTestId('ship-changes-push-submit'));
    await flush(10);

    const errEl = await findByTestId('ship-changes-push-error');
    expect(errEl.textContent).toMatch(/auth required/);
    expect(getByTestId('ship-changes-push-retry')).toBeInTheDocument();
  });

  it('calls GitCreatePR with the trimmed title/body and shows the URL', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: false, aheadCount: 0 }));
    setBindingMock('GetGitStatus', async () => status({
      hasChanges: false,
      aheadCount: 0,
      openPrUrl: 'https://github.com/owner/repo/pull/42',
      openPrNumber: 42,
    }));
    const createPR = setBindingMock('GitCreatePR', async () => ({
      action: 'pr',
      prUrl: 'https://github.com/owner/repo/pull/42',
    } as GitActionResult));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const title = await findByTestId('ship-changes-pr-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: '  Add widget  ' } });
    await flush();
    await fireEvent.click(getByTestId('ship-changes-pr-submit'));
    await flush(10);

    expect(createPR.mock.calls.length).toBe(1);
    expect(createPR.mock.calls[0][1]).toBe('Add widget');
    expect(createPR.mock.calls[0][2]).toBe('');
    expect(pane.gitStatus.status?.openPrUrl).toBe('https://github.com/owner/repo/pull/42');
    const url = await findByTestId('ship-changes-pr-url');
    expect(url.getAttribute('href')).toBe('https://github.com/owner/repo/pull/42');
  });

  it('surfaces a PR error and allows retry', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: false, aheadCount: 0 }));
    setBindingMock('GitCreatePR', async () => ({ action: 'pr', error: 'gh not installed' } as GitActionResult));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const title = await findByTestId('ship-changes-pr-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Add widget' } });
    await flush();
    await fireEvent.click(getByTestId('ship-changes-pr-submit'));
    await flush(10);

    const errEl = await findByTestId('ship-changes-pr-error');
    expect(errEl.textContent).toMatch(/gh not installed/);
    expect(getByTestId('ship-changes-pr-retry')).toBeInTheDocument();
  });

  it('shows an existing PR link instead of the form when one is already open', async () => {
    const pane = await buildPane();
    installStatus(status({
      hasChanges: false,
      aheadCount: 0,
      openPrUrl: 'https://github.com/o/r/pull/7',
      openPrNumber: 7,
    }));
    const { findByTestId, queryByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    // pr.done lands us here; no title form, no submit.
    await findByTestId('ship-changes-step-pr');
    await flush();
    const url = await findByTestId('ship-changes-pr-url');
    expect(url.getAttribute('href')).toBe('https://github.com/o/r/pull/7');
    expect(url.textContent).toBe('https://github.com/o/r/pull/7');
    expect(queryByTestId('ship-changes-pr-title')).toBeNull();
    expect(queryByTestId('ship-changes-pr-submit')).toBeNull();
  });

  it('Close button calls onClose', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    let closed = 0;
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => { closed += 1; } },
    });
    const btn = await findByTestId('ship-changes-close');
    await fireEvent.click(btn);
    expect(closed).toBe(1);
  });

  it('Escape key closes the drawer', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    let closed = 0;
    const { container, findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => { closed += 1; } },
    });
    // Wait for the drawer body before probing the backdrop so the
    // dialog has actually mounted.
    await findByTestId('ship-changes-drawer');
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    await fireEvent.keyDown(backdrop, { key: 'Escape' });
    expect(closed).toBe(1);
  });

  it('honours an externally-provided state store', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    const external = createShipChangesState();
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {}, state: external },
    });
    await findByTestId('ship-changes-step-commit');
    // The drawer must have mutated the *external* state, proving the prop
    // was respected.
    expect(external.threadId).toBe('t-1');
  });

  it('renders the 3-step indicator with Commit active on entry', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const steps = await findByTestId('ship-changes-steps');
    expect(steps).toBeInTheDocument();
    // Commit step is active, others pending.
    expect(steps.querySelector('[data-step="commit"]')?.getAttribute('data-state')).toBe('active');
    expect(steps.querySelector('[data-step="push"]')?.getAttribute('data-state')).toBe('pending');
    expect(steps.querySelector('[data-step="pr"]')?.getAttribute('data-state')).toBe('pending');
  });

  // Bug C2 regression: closing the drawer while a commit is in flight used
  // to leave the wizard in an illegal state because completeCommit's phase
  // guard threw when called from the post-reset 'idle' phase. Worse, the
  // outer catch block called failCommit which threw a second illegal
  // transition error that escaped as an unhandled rejection. The fix is a
  // generation counter captured at the start of each side effect and
  // checked before any state mutation.
  // Bug C2 regression: closing the drawer while a commit is in flight used
  // to leave the wizard in an illegal state because completeCommit's phase
  // guard threw when called from the post-reset 'idle' phase. Worse, the
  // outer catch block called failCommit which threw a second illegal
  // transition error that escaped as an unhandled rejection. The fix is a
  // generation counter captured at the start of each side effect and
  // checked before any state mutation.
  //
  // Detection strategy: spy on the wizard's failCommit method. Without
  // the fix, the handler's catch block invokes failCommit when
  // completeCommit throws (because the phase is 'idle' post-reset);
  // the spy's call count tells us the handler tried to mutate state
  // after the close. With the fix, the handler bails on the generation
  // mismatch before touching state.
  it('ignores a commit result that lands after the drawer was closed', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));

    let resolveCommit!: (value: GitActionResult) => void;
    const commitPromise = new Promise<GitActionResult>((r) => { resolveCommit = r; });
    setBindingMock('GitCommit', () => commitPromise);

    const external = createShipChangesState();
    // Wrap the state's failCommit / completeCommit so we can detect any
    // call after the drawer was closed.
    const realComplete = external.completeCommit.bind(external);
    const realFail = external.failCommit.bind(external);
    const completeCalls: string[] = [];
    const failCalls: string[] = [];
    external.completeCommit = (sha: string) => { completeCalls.push(sha); realComplete(sha); };
    external.failCommit = (err: string) => { failCalls.push(err); realFail(err); };

    const { findByTestId, getByTestId, rerender } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {}, state: external },
    });
    const subject = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'mid-flight' } });
    await flush();
    await fireEvent.click(getByTestId('ship-changes-commit-submit'));
    await flush();
    expect(external.phase).toBe('commit.busy');

    // Close the drawer before the commit resolves.
    await rerender({ open: false, pane, onClose: () => {}, state: external });
    await flush();
    expect(external.phase).toBe('idle');

    // Snapshot call counts before resolving so we can assert the resolve
    // doesn't trigger new state mutations.
    const completeBefore = completeCalls.length;
    const failBefore = failCalls.length;

    // Now resolve the commit: the drawer is gone and the result must be
    // dropped silently without trying to advance the state machine.
    resolveCommit({ action: 'commit', commitSha: 'abcdef0' } as GitActionResult);
    await flush(30);

    // Neither completeCommit nor failCommit should have been attempted.
    expect(completeCalls.length).toBe(completeBefore);
    expect(failCalls.length).toBe(failBefore);
    expect(external.phase).toBe('idle');
    expect(external.commitSubject).toBe('');
    expect(external.commitSha).toBeNull();
    expect(external.error).toBeNull();
  });

  it('ignores a GitCommit rejection that lands after the drawer was closed', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));

    let rejectCommit!: (err: unknown) => void;
    const commitPromise = new Promise<GitActionResult>((_, r) => { rejectCommit = r; });
    setBindingMock('GitCommit', () => commitPromise);

    const external = createShipChangesState();
    const realFail = external.failCommit.bind(external);
    const failCalls: string[] = [];
    external.failCommit = (err: string) => { failCalls.push(err); realFail(err); };

    const { findByTestId, getByTestId, rerender } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {}, state: external },
    });
    const subject = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'oops' } });
    await flush();
    await fireEvent.click(getByTestId('ship-changes-commit-submit'));
    await flush();
    expect(external.phase).toBe('commit.busy');

    await rerender({ open: false, pane, onClose: () => {}, state: external });
    await flush();
    expect(external.phase).toBe('idle');

    const failBefore = failCalls.length;
    // Rejection after close must bail silently — failCommit must NOT be
    // called (it would throw on the 'idle' phase).
    rejectCommit(new Error('late failure'));
    await flush(30);
    expect(failCalls.length).toBe(failBefore);
    expect(external.phase).toBe('idle');
    expect(external.error).toBeNull();
  });

  // Bug C3 regression: an in-progress merge/rebase/bisect blocks new commits.
  // The drawer must surface the reason and keep the Commit button disabled.
  it('disables commit and shows a merge-in-progress banner when PendingOperation=merge', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true, pendingOperation: 'merge' }));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    await findByTestId('ship-changes-step-commit');
    const banner = await findByTestId('ship-changes-pending-operation');
    expect(banner.textContent).toMatch(/merge is in progress/i);
    // Even with a subject typed, canCommit stays false -> submit stays disabled.
    const subject = getByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'not happening' } });
    await flush();
    const submit = getByTestId('ship-changes-commit-submit') as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
  });

  it('disables commit and shows a rebase-in-progress banner when PendingOperation=rebase', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true, pendingOperation: 'rebase' }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    await findByTestId('ship-changes-step-commit');
    const banner = await findByTestId('ship-changes-pending-operation');
    expect(banner.textContent).toMatch(/rebase is in progress/i);
  });

  it('does not render the pending-operation banner in a clean repo', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true, pendingOperation: '' }));
    const { findByTestId, queryByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    await findByTestId('ship-changes-step-commit');
    await flush();
    expect(queryByTestId('ship-changes-pending-operation')).toBeNull();
  });

  it('drops stale results from sequential commit+push after drawer is closed', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));

    let resolveCommit!: (value: GitActionResult) => void;
    const commitPromise = new Promise<GitActionResult>((r) => { resolveCommit = r; });
    setBindingMock('GitCommit', () => commitPromise);

    let pushCalls = 0;
    setBindingMock('GitPush', async () => { pushCalls += 1; return { action: 'push' } as GitActionResult; });

    const external = createShipChangesState();
    const realComplete = external.completeCommit.bind(external);
    const completeCalls: string[] = [];
    external.completeCommit = (sha: string) => { completeCalls.push(sha); realComplete(sha); };

    const { findByTestId, getByTestId, rerender } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {}, state: external },
    });
    const subject = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'stack' } });
    await flush();
    await fireEvent.click(getByTestId('ship-changes-commit-submit'));
    await flush();
    expect(external.phase).toBe('commit.busy');

    // Close the drawer while commit is still pending.
    await rerender({ open: false, pane, onClose: () => {}, state: external });
    await flush();

    const completeBefore = completeCalls.length;

    // Resolve the commit now — this must NOT trigger completeCommit or cascade into push.
    resolveCommit({ action: 'commit', commitSha: 'sha' } as GitActionResult);
    await flush(30);
    expect(completeCalls.length).toBe(completeBefore);
    expect(external.phase).toBe('idle');
    // Push binding must not have been invoked — the wizard never reached push.
    expect(pushCalls).toBe(0);
  });

  // Bug C8 regression: switching the pane's thread while the drawer was
  // open used to silently reset wizard state onto the new thread (because
  // the effect sees the new pane.threadId and calls wizard.open(newId),
  // wiping whatever subject/body the user had typed). The fix is to
  // auto-close the drawer with an info toast so the user sees what
  // happened.
  it('auto-closes with a toast when the active thread switches mid-wizard', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    let closed = 0;
    const beforeToastCount = getToasts().length;
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: {
        open: true,
        pane,
        onClose: () => { closed += 1; },
      },
    });
    const subject = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'a subject user typed' } });
    await flush();
    expect(getByTestId('ship-changes-commit-subject')).toHaveValue('a subject user typed');

    // User switches threads while the drawer is still open.
    await pane.switchThread({
      id: 't-2',
      title: 'different thread',
      provider: 'claude',
      workspacePath: '',
      projectPath: '',
      model: 'm',
      mode: 'chat',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    } as Thread);
    await flush(10);

    // onClose must have been invoked so the parent tears the drawer down.
    expect(closed).toBeGreaterThanOrEqual(1);
    // An info toast tells the user what happened.
    const newToasts = getToasts().slice(beforeToastCount);
    const toast = newToasts.find((t) => t.message.includes('Ship Changes closed'));
    expect(toast).toBeDefined();
    expect(toast?.type).toBe('info');
  });

  it('re-opening on the same thread after a close-and-reopen resets cleanly', async () => {
    const pane = await buildPane();
    installStatus(status({ hasChanges: true }));
    const { findByTestId, rerender } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const subject = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'typed then closed' } });
    await flush();

    // Close and reopen — state must be reset (the subject gone).
    await rerender({ open: false, pane, onClose: () => {} });
    await flush();
    await rerender({ open: true, pane, onClose: () => {} });
    await flush();

    const fresh = await findByTestId('ship-changes-commit-subject') as HTMLInputElement;
    expect(fresh.value).toBe('');
  });

  it('renders MR labels and "Open MR" step when forge is gitlab', async () => {
    const pane = await buildPane();
    installStatus(status({
      forge: 'gitlab',
      hasChanges: false,
      aheadCount: 0,
    }));
    const { findByTestId, getByText } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    await findByTestId('ship-changes-step-pr');
    await flush();
    expect(getByText('Open Merge Request')).toBeTruthy();
    const submit = await findByTestId('ship-changes-pr-submit');
    expect(submit.textContent).toMatch(/Create MR/);
  });

  it('renders self-hosted notice when forge is unknown', async () => {
    const pane = await buildPane();
    installStatus(status({
      forge: '',
      hasChanges: false,
      aheadCount: 0,
    }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const note = await findByTestId('ship-changes-pr-unsupported');
    expect(note.textContent).toMatch(/not GitHub or GitLab/);
  });

  it('blocks PR creation when checking for an existing MR failed', async () => {
    const pane = await buildPane();
    installStatus(status({
      forge: 'gitlab',
      hasChanges: false,
      aheadCount: 0,
      openPrLookupError: 'glab auth required',
    }));
    const { findByTestId, queryByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    const note = await findByTestId('ship-changes-pr-lookup-error');
    expect(note.textContent).toMatch(/Could not check existing MR/);
    expect(queryByTestId('ship-changes-pr-submit')).toBeNull();
    expect(queryByTestId('ship-changes-pr-title')).toBeNull();
  });
});
