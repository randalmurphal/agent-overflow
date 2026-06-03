import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import GitActionsControl from './GitActionsControl.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { GitStatus } from '../../types/git';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// GitActionsControl is now a pure consumer of the pane-owned gitStatus slot —
// it owns no subscription. The slot's subscribe / retry / branch-persist /
// event-routing behavior is covered in stores/gitStatus.svelte.test.ts; these
// tests drive rendering by setting the slot status directly via
// `pane.gitStatus.set(...)` / `.setError(...)`.

// Pin transport connected for the createThreadPane import chain (the real
// store reads from wsClient, never initialised in test scope).
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
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListThreadCheckpoints', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  registerPaneForTest(pane.paneId, pane);
  return pane;
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

describe('<GitActionsControl> consumer rendering', () => {
  beforeEach(async () => {
    resetPanesForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    await loadSettings();
  });

  it('renders nothing when no status has been observed yet', async () => {
    const pane = await buildPane();
    const { container, queryByTestId } = render(GitActionsControl, { props: { pane } });
    await flush();
    expect(queryByTestId('git-actions-error')).toBeNull();
    expect(container.querySelector('button[aria-label="More git actions"]')).toBeNull();
  });

  it('renders nothing when the workspace is not a git repo', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({ isRepo: false, branch: '' }));
    const { container, queryByTestId } = render(GitActionsControl, { props: { pane } });
    await flush();
    expect(queryByTestId('git-actions-error')).toBeNull();
    expect(container.querySelector('button[aria-label="More git actions"]')).toBeNull();
  });

  it('shows the retry affordance when the slot reports an error', async () => {
    const pane = await buildPane();
    pane.gitStatus.setError(true);
    const { findByTestId } = render(GitActionsControl, { props: { pane } });
    expect(await findByTestId('git-actions-error')).toBeInTheDocument();
  });

  it('retry button asks the slot to refresh', async () => {
    const pane = await buildPane();
    pane.gitStatus.setError(true);
    const refreshNow = vi.spyOn(pane.gitStatus, 'refreshNow').mockResolvedValue();
    const { findByTestId } = render(GitActionsControl, { props: { pane } });
    await fireEvent.click(await findByTestId('git-actions-error'));
    expect(refreshNow).toHaveBeenCalled();
  });

  it('renders the split button + Ship Changes menu entry in a valid repo', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({ isRepo: true, hasChanges: true }));
    const { container, queryByTestId, findByRole } = render(GitActionsControl, { props: { pane } });
    await flush();

    expect(queryByTestId('git-actions-error')).toBeNull();
    const trigger = container.querySelector<HTMLButtonElement>('button[aria-label="More git actions"]');
    expect(trigger).not.toBeNull();
    await fireEvent.click(trigger!);
    expect(await findByRole('menuitem', { name: /Ship Changes/i })).toBeInTheDocument();
  });

  it('reflects the primary action label for the observed status', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({ hasChanges: true }));
    const { container } = render(GitActionsControl, { props: { pane } });
    await flush();

    const primary = container.querySelector<HTMLButtonElement>('div.flex > button:first-of-type');
    expect(primary?.textContent?.trim()).toBe('Commit');

    // A new observed status (no changes, ahead of upstream) re-renders the
    // same primary button in place.
    pane.gitStatus.set(status({ hasChanges: false, aheadCount: 2 }));
    await flush();
    expect(primary?.textContent?.trim()).toBe('Push');
  });
});

describe('<GitActionsControl> forge labels', () => {
  beforeEach(async () => {
    resetPanesForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    await loadSettings();
  });

  it('renders "Create PR" for github forge', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({ forge: 'github', branch: 'feature', isDefaultBranch: false }));
    const { findByLabelText, getByText } = render(GitActionsControl, { props: { pane } });
    await fireEvent.click(await findByLabelText('More git actions'));
    await flush();
    expect(getByText('Create PR')).toBeTruthy();
  });

  it('renders "Create MR" for gitlab forge', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({ forge: 'gitlab', branch: 'feature', isDefaultBranch: false }));
    const { findByLabelText, getByText } = render(GitActionsControl, { props: { pane } });
    await fireEvent.click(await findByLabelText('More git actions'));
    await flush();
    expect(getByText('Create MR')).toBeTruthy();
  });

  it('disables the Create PR menu item when the forge is unsupported', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({ forge: '', branch: 'feature', isDefaultBranch: false }));
    const { findByLabelText, getByText } = render(GitActionsControl, { props: { pane } });
    await fireEvent.click(await findByLabelText('More git actions'));
    await flush();
    const item = getByText('Create PR').closest('[role="menuitem"]');
    expect(item?.getAttribute('aria-disabled')).toBe('true');
  });
});
