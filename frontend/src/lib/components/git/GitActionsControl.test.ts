import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import GitActionsControl from './GitActionsControl.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { GitStatus } from '../../types/git';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';

// GitActionsControl is now a pure consumer of the pane-owned gitStatus slot —
// it owns no subscription. The slot's subscribe / retry / branch-persist /
// event-routing behavior is covered in stores/gitStatus.svelte.test.ts; these
// tests drive rendering by setting the slot status directly via
// `pane.gitStatus.set(...)` / `.setError(...)`.

// Pin transport connected for the createThreadPane import chain (the real
// store reads from wsClient, never initialised in test scope).
// Partial mock: only the snapshot is pinned. A whole-module factory would
// have to re-declare every export, so any new one silently becomes undefined
// here (isMethodUnavailableError did exactly that).
vi.mock('../../stores/transportStatus.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/transportStatus.svelte')>()),
  getTransportStatus: () => ({ status: 'connected', nextAttemptAt: null }),
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
  return makeBaseThread({
    title: 'Example',
    workspacePath: '/workspace',
    projectPath: '/workspace',
    ...overrides,
  });
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
  return buildRegisteredPane(thread);
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

  it('disables Remove Worktree while this pane thread is busy', async () => {
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    const pane = await buildPane(makeThread({
      workspacePath: '/workspace-wt/feat',
      worktreePath: '/workspace-wt/feat',
    }));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });
    pane.gitStatus.set(status({ isRepo: true }));
    const { container, findByRole, queryByText } = render(GitActionsControl, { props: { pane } });
    await flush();

    const trigger = container.querySelector<HTMLButtonElement>('button[aria-label="More git actions"]');
    expect(trigger).not.toBeNull();
    await fireEvent.click(trigger!);
    const item = await findByRole('menuitem', { name: /Remove Worktree/ });
    expect(item).toHaveAttribute('aria-disabled', 'true');
    expect(item).toHaveAttribute('title', expect.stringMatching(/agent is responding/));
    await fireEvent.click(item);
    // The confirm dialog must not open from a disabled item.
    expect(queryByText(/This will remove the git worktree/)).toBeNull();
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

  it('renders "Open MR" instead of "Create MR" when a GitLab MR is already open', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({
      forge: 'gitlab',
      branch: 'feature',
      isDefaultBranch: false,
      openPrUrl: 'https://gitlab.com/o/r/-/merge_requests/45',
      openPrNumber: 45,
    }));
    const { findByLabelText, getByText, queryByText } = render(GitActionsControl, { props: { pane } });
    await fireEvent.click(await findByLabelText('More git actions'));
    await flush();

    const item = getByText('Open MR').closest('[role="menuitem"]');
    expect(item?.getAttribute('aria-disabled')).toBeNull();
    expect(queryByText('Create MR')).toBeNull();
  });

  it('disables Create MR when checking for an existing MR failed', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({
      forge: 'gitlab',
      branch: 'feature',
      isDefaultBranch: false,
      openPrLookupError: 'glab auth required',
    }));
    const { findByLabelText, getByText } = render(GitActionsControl, { props: { pane } });
    await fireEvent.click(await findByLabelText('More git actions'));
    await flush();

    const item = getByText('Create MR').closest('[role="menuitem"]');
    expect(item?.getAttribute('aria-disabled')).toBe('true');
    expect(getByText(/Could not check existing MR/)).toBeTruthy();
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

describe('<GitActionsControl> header action controls are height-locked', () => {
  // The chat header is auto-height (ChatHeader `py-2` + its tallest child).
  // Every action item is locked to `h-6` (24px) so async-loaded git chrome can
  // MOUNT without changing the header height. GitActionsControl only renders
  // once `pane.gitStatus.status` arrives (async, after a thread switch); if its
  // button is 2px taller than the rest of the cluster it grows the header from
  // 41px to 43px, the `flex-1` timeline below loses 2px, and the scroll
  // controller correctly re-pins to bottom — the visible 1–2px "settle shift"
  // on cached-thread switch (see SCROLL_SETTLE_RCA.md). The split-button was
  // hand-rolled at 26px (`text-xs` 16px line + `py-1` 8px + `border` 2px); these
  // tests pin it (and the error button) to the 24px cluster height. happy-dom
  // reports zero geometry, so the contract is asserted via the height class.
  beforeEach(async () => {
    resetPanesForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    await loadSettings();
  });

  it('locks the repo split-button (primary + caret) to the h-6 cluster height', async () => {
    const pane = await buildPane();
    pane.gitStatus.set(status({ isRepo: true, hasChanges: true }));
    const { container } = render(GitActionsControl, { props: { pane } });
    await flush();

    const primary = container.querySelector<HTMLButtonElement>('div.flex > button:first-of-type');
    const caret = container.querySelector<HTMLButtonElement>('button[aria-label="More git actions"]');
    expect(primary).not.toBeNull();
    expect(caret).not.toBeNull();
    // h-6 (24px) == every other header action item (xs Button, PrBadge). The
    // explicit height IS the contract that stops the async git-status mount from
    // growing the header and reflowing the timeline. (The original bug was a
    // hand-rolled `py-1`+border button with no explicit height = 26px; we assert
    // the positive height contract, not the absence of the historical class —
    // under border-box `py-1` can't change an `h-6` box's outer height anyway.)
    expect(primary!.className).toContain('h-6');
    expect(caret!.className).toContain('h-6');
  });

  it('locks the git-error retry button to the h-6 cluster height', async () => {
    const pane = await buildPane();
    pane.gitStatus.setError(true);
    const { findByTestId } = render(GitActionsControl, { props: { pane } });
    const errorBtn = await findByTestId('git-actions-error');
    // size="xs" → h-6; size="sm" (the prior value) is h-7 (28px) and would jump
    // the header 4px on git error.
    expect(errorBtn.className).toContain('h-6');
    expect(errorBtn.className).not.toContain('h-7');
  });
});
