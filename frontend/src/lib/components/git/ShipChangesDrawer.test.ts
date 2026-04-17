import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ShipChangesDrawer from './ShipChangesDrawer.svelte';
import { createShipChangesState } from '../../stores/shipChanges.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { GitActionResult, GitStatus } from '../../types/git';
import { setBindingMock } from '../../../test/mocks/bindings-app';

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
    ...overrides,
  };
}

async function buildPane() {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  // Seed threadId via the internal switchThread fake.
  await pane.switchThread({
    id: 't-1',
    title: 't',
    provider: 'claude',
    workspacePath: '',
    projectPath: '',
    model: 'm',
    interactionMode: 'default',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  });
  return pane;
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
    setBindingMock('GetGitStatus', async () => status({ hasChanges: true }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    expect(await findByTestId('ship-changes-step-commit')).toBeInTheDocument();
  });

  it('lands on the push step when there are no changes but commits ahead', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ hasChanges: false, aheadCount: 3 }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    expect(await findByTestId('ship-changes-step-push')).toBeInTheDocument();
  });

  it('lands on the PR step when branch is clean and up to date with upstream', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ hasChanges: false, aheadCount: 0 }));
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    expect(await findByTestId('ship-changes-step-pr')).toBeInTheDocument();
  });

  it('calls GitCommit with trimmed subject/body and advances to push', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ hasChanges: true }));
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

  it('surfaces a commit error inline and offers a retry', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ hasChanges: true }));
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
    setBindingMock('GetGitStatus', async () => status({ hasChanges: true }));
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
    setBindingMock('GetGitStatus', async () => status({ hasChanges: false, aheadCount: 1 }));
    const push = setBindingMock('GitPush', async () => ({ action: 'push' } as GitActionResult));
    const { findByTestId, getByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => {} },
    });
    await findByTestId('ship-changes-step-push');
    await fireEvent.click(getByTestId('ship-changes-push-submit'));
    await flush(10);
    expect(push.mock.calls.length).toBe(1);
    expect(await findByTestId('ship-changes-step-pr')).toBeInTheDocument();
  });

  it('surfaces a push error and allows retry', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ hasChanges: false, aheadCount: 1 }));
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
    setBindingMock('GetGitStatus', async () => status({ hasChanges: false, aheadCount: 0 }));
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
    const url = await findByTestId('ship-changes-pr-url');
    expect(url.getAttribute('href')).toBe('https://github.com/owner/repo/pull/42');
  });

  it('surfaces a PR error and allows retry', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ hasChanges: false, aheadCount: 0 }));
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
    setBindingMock('GetGitStatus', async () => status({
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
    expect(queryByTestId('ship-changes-pr-title')).toBeNull();
    expect(queryByTestId('ship-changes-pr-submit')).toBeNull();
  });

  it('Close button calls onClose', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ hasChanges: true }));
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
    setBindingMock('GetGitStatus', async () => status({ hasChanges: true }));
    let closed = 0;
    const { findByTestId } = render(ShipChangesDrawer, {
      props: { open: true, pane, onClose: () => { closed += 1; } },
    });
    const backdrop = await findByTestId('ship-changes-backdrop');
    await fireEvent.keyDown(backdrop, { key: 'Escape' });
    expect(closed).toBe(1);
  });

  it('honours an externally-provided state store', async () => {
    const pane = await buildPane();
    setBindingMock('GetGitStatus', async () => status({ hasChanges: true }));
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
    setBindingMock('GetGitStatus', async () => status({ hasChanges: true }));
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
});
