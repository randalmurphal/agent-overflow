// Tag rows, detached-HEAD labels and tag refs in the branch picker.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import BranchPicker from './BranchPicker.svelte';
import { TAG_ROW_CAP } from './branchPickerRows';
import type { Thread } from '../../../types/models';
import type { GitStatus } from '../../../types/git';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';
import {
  enterCreateBranchMode,
  resetForTest as resetWorktreeIntent,
  setThreadEnvMode,
  worktreeIntentForThread,
} from '../../../stores/worktreeIntent.svelte';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../../test/helpers/chat';
import { recentBranchSelections, recordBranchSelection } from '../../../stores/branchMru';
import { resetAppStorageForTest } from '../../../stores/appStorage';
import { __resetGitStatusStoreForTest, __seedGitStatusForTest } from '../../../stores/gitStatusStore.svelte';
import { resetPanesForTest } from '../../../stores/panes.svelte';
import { __setTransportHelloForTest } from '../../../stores/transportStatus.svelte';
import { getToasts, resetToastsForTest } from '../../../stores/toast.svelte';
import { idleWorkspaceActivity } from '../../../../test/helpers/workspaceLock';
import { createThreadPane } from '../../../stores/thread.svelte';
import { checkoutPickerRow } from './branchPickerCheckout';

const WS = { projectId: 'project-1', workspacePath: '/repo' };
const SHA = 'abc1234def5678900000000000000000000000000';

function hello(capabilities: string[]): void {
  __setTransportHelloForTest({
    protocolVersion: 1, capabilities, backendId: '', backendName: '',
    serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  });
}

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true, branch: 'main', isDefaultBranch: true, hasChanges: false,
    insertions: 0, deletions: 0, fileCount: 0, hasUpstream: true,
    aheadCount: 0, behindCount: 0, hasOriginRemote: true, ...overrides,
  };
}

const detachedAt = (tag?: string): GitStatus =>
  status({ branch: '', isDefaultBranch: false, hasUpstream: false, tag, headCommit: SHA });

async function buildPane(branch: string, overrides: Partial<Thread> = {}) {
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  return buildRegisteredPane(makeBaseThread({
    workspacePath: '/repo', projectPath: '/repo', projectId: 'project-1', model: 'm', branch, ...overrides,
  }));
}

const BRANCHES = [
  { name: 'main', isCurrent: true, isDefault: true, date: 100 },
  { name: 'feat/new', isCurrent: false, isDefault: false, date: 300 },
  { name: 'feat/old', isCurrent: false, isDefault: false, date: 50 },
];
const TAGS = [
  { name: 'v3', date: 400 },
  { name: 'v2', date: 200 },
  { name: 'v1', date: 10 },
];

function mockLists(tags: unknown = TAGS): void {
  setBindingMock('GitListBranches', async () => BRANCHES);
  setBindingMock('GitListTags', async () => tags);
  setBindingMock('GitMaybeFetchRemotes', async () => false);
  setBindingMock('UpdateThreadBranch', async () => []);
}

function rowTexts(getAllByRole: (role: string) => HTMLElement[]): string[] {
  return getAllByRole('menuitem').map((row) => row.textContent?.replace(/\s+/g, ' ').trim() ?? '');
}

async function openWithTags(pane: Awaited<ReturnType<typeof buildPane>>) {
  const view = render(BranchPicker, { props: { pane } });
  await fireEvent.click(view.getByTestId('branch-picker-trigger'));
  await view.findByRole('menuitem', { name: /^v1/ });
  return view;
}

describe('<BranchPicker> tags', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetAppStorageForTest();
    resetPanesForTest();
    __resetGitStatusStoreForTest();
    resetToastsForTest();
    hello(['git-tags.v1']);
  });

  afterEach(() => {
    __setTransportHelloForTest(null);
  });

  it('lists unpicked tags after every branch, a recent tag lifted, each marked "tag"', async () => {
    recordBranchSelection('project-1', 'refs/tags/v1');
    const pane = await buildPane('main');
    mockLists();
    const { getAllByRole, getAllByTestId } = await openWithTags(pane);

    const rows = rowTexts(getAllByRole).filter((text) => /^(main|feat|v\d)/.test(text));
    expect(rows).toEqual([
      expect.stringMatching(/^main/),
      'v1 tag',
      expect.stringMatching(/^feat\/new/),
      expect.stringMatching(/^feat\/old/),
      'v3 tag',
      'v2 tag',
    ]);
    expect(getAllByTestId('branch-picker-tag-row')).toHaveLength(3);
  });

  it('merges matching branches and tags by date while searching', async () => {
    const pane = await buildPane('main');
    mockLists();
    const { getAllByRole, getByPlaceholderText } = await openWithTags(pane);

    await fireEvent.input(getByPlaceholderText('Search branches and tags'), { target: { value: 'e' } });
    // "feat/new" (300) and "feat/old" (50) match; no tag has an "e".
    await waitFor(() => {
      expect(rowTexts(getAllByRole).filter((text) => /^(main|feat|v\d)/.test(text))).toEqual([
        expect.stringMatching(/^feat\/new/),
        expect.stringMatching(/^feat\/old/),
      ]);
    });
    await fireEvent.input(getByPlaceholderText('Search branches and tags'), { target: { value: '' } });
    await fireEvent.input(getByPlaceholderText('Search branches and tags'), { target: { value: 'v' } });
    await waitFor(() => {
      expect(rowTexts(getAllByRole).filter((text) => /^v\d/.test(text))).toEqual(['v3 tag', 'v2 tag', 'v1 tag']);
    });
  });

  it('renders at most the capped tag rows and says how many more match', async () => {
    const many = Array.from({ length: TAG_ROW_CAP + 12 }, (_, i) => ({ name: `r${i}`, date: 10_000 - i }));
    const pane = await buildPane('main');
    mockLists(many);
    const { findByTestId, getAllByTestId, getByPlaceholderText, queryByTestId } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(document.querySelector('[data-testid="branch-picker-trigger"]')!);

    const hint = await findByTestId('branch-picker-more-tags');
    expect(hint.textContent).toContain('12 more tags. Search to narrow.');
    expect(getAllByTestId('branch-picker-tag-row')).toHaveLength(TAG_ROW_CAP);

    await fireEvent.input(getByPlaceholderText('Search branches and tags'), { target: { value: 'r6' } });
    await waitFor(() => expect(queryByTestId('branch-picker-more-tags')).toBeNull());
    // r6 and r60..r61 match.
    expect(getAllByTestId('branch-picker-tag-row')).toHaveLength(3);
  });

  it('checks a tag out by its tag ref and records the ref as a recent pick', async () => {
    const pane = await buildPane('main');
    mockLists();
    const checkout = setBindingMock('GitCheckout', async () => ({
      workspacePath: '/repo', worktreePath: '', branch: '', status: detachedAt('v2'),
    }));
    const { findByRole } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /^v2/ }));

    await waitFor(() => expect(checkout).toHaveBeenCalledWith(WS, 'refs/tags/v2'));
    await waitFor(() => expect(recentBranchSelections('project-1')).toEqual(['refs/tags/v2']));
    await waitFor(() => expect(getToasts().map((t) => t.message)).toContain('Checked out v2'));
  });

  it('stages a tag as the new worktree attach target', async () => {
    const pane = await buildPane('main');
    setThreadEnvMode(pane.thread!, 'new-worktree');
    mockLists();
    const { findByRole, getByTestId } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /^v3/ }));

    expect(worktreeIntentForThread(pane.thread).attachBranch).toBe('refs/tags/v3');
    expect(getBindingMock('GitCheckout')).toBeUndefined();
    await waitFor(() => {
      expect(getByTestId('branch-picker-trigger').textContent).toContain('v3');
      expect(getByTestId('branch-picker-trigger').dataset.checkoutKind).toBe('tag');
    });
  });

  it('sets a tag as the new branch base', async () => {
    const pane = await buildPane('main');
    enterCreateBranchMode(pane.thread!, 'main');
    mockLists();
    const { findByRole, getByTestId } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /^v1/ }));

    expect(worktreeIntentForThread(pane.thread).newBranchBase).toBe('refs/tags/v1');
    await waitFor(() => expect(getByTestId('branch-picker-trigger').textContent).toContain('From v1'));
  });

  it('shows a detached tag checkout on the trigger with the tag icon and checks its row', async () => {
    __seedGitStatusForTest('/repo', detachedAt('v2'));
    const pane = await buildPane('');
    mockLists();
    const { getByTestId, findByRole } = await openWithTags(pane);

    const trigger = getByTestId('branch-picker-trigger');
    expect(trigger.textContent).toContain('v2');
    expect(trigger.dataset.checkoutKind).toBe('tag');
    expect(trigger.querySelector('.lucide-tag')).not.toBeNull();
    expect((await findByRole('menuitem', { name: /^v2/ })).textContent).toContain('✓');
    expect((await findByRole('menuitem', { name: /^v3/ })).textContent).not.toContain('✓');
  });

  it('defaults a new branch from a tag checkout to the checkout, its row selected', async () => {
    __seedGitStatusForTest('/repo', detachedAt('v2'));
    const pane = await buildPane('');
    mockLists();
    const { findByRole, getByTestId } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /New branch/ }));

    expect(worktreeIntentForThread(pane.thread)).toMatchObject({ creatingBranch: true, newBranchBase: '' });
    await waitFor(() => expect(getByTestId('branch-picker-trigger').textContent).toContain('From v2'));
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    expect((await findByRole('menuitem', { name: /^v2/ })).textContent).toContain('✓');
    expect((await findByRole('menuitem', { name: /^main/ })).textContent).not.toContain('✓');
  });

  it('names a detached HEAD at no tag by its commit, and branches from it with an empty base', async () => {
    __seedGitStatusForTest('/repo', detachedAt());
    const pane = await buildPane('');
    mockLists();
    const { getByTestId, findByRole } = await openWithTags(pane);

    const trigger = getByTestId('branch-picker-trigger');
    expect(trigger.textContent).toContain('detached at abc1234');
    expect(trigger.dataset.checkoutKind).toBe('commit');
    expect(trigger.querySelector('.lucide-git-commit-horizontal')).not.toBeNull();

    await fireEvent.click(await findByRole('menuitem', { name: /New branch/ }));
    expect(worktreeIntentForThread(pane.thread)).toMatchObject({ creatingBranch: true, newBranchBase: '' });
    await waitFor(() => expect(trigger.textContent).toContain('From abc1234'));
  });

  it('describes the "Base (with changes)" row and titles the trigger by the actual checkout', async () => {
    for (const [at, description, title] of [
      [detachedAt('v2'), 'v2', 'Tag: v2'],
      [detachedAt(), 'detached at abc1234', 'Commit: abc1234'],
      [status(), 'main', 'Branch: main'],
    ] as const) {
      resetWorktreeIntent();
      __seedGitStatusForTest('/repo', { ...at, hasChanges: true });
      const pane = await buildPane('');
      mockLists();
      const view = await openWithTags(pane);
      const trigger = view.getByTestId('branch-picker-trigger');
      expect(trigger.title).toMatch(new RegExp(`^${title}`));
      await fireEvent.click(await view.findByRole('menuitem', { name: /New branch/ }));
      await fireEvent.click(trigger);

      const base = await view.findByRole('menuitem', { name: /Base \(with changes\)/ });
      expect(base.textContent).toContain(description);
      expect(base.textContent).toContain('✓');
      view.unmount();
    }
  });

  it('reads "No branch" only when no branch, tag or commit is known', async () => {
    const pane = await buildPane('');
    const { getByTestId } = render(BranchPicker, { props: { pane } });
    expect(getByTestId('branch-picker-trigger').textContent).toContain('No branch');
    expect(getByTestId('branch-picker-trigger').dataset.checkoutKind).toBe('none');
  });

  it('names the checked-out tag from the checkout\'s answer at once', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    setBindingMock('GitCheckout', async () => {
      // The backend re-branches the row; no status has been pushed yet.
      pane.replaceThread({ ...pane.thread!, branch: '' });
      return { workspacePath: '/repo', worktreePath: '', branch: '', status: detachedAt('v3') };
    });
    const { findByRole, getByTestId } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /^v3/ }));

    const trigger = getByTestId('branch-picker-trigger') as HTMLButtonElement;
    await waitFor(() => expect(trigger.disabled).toBe(false));
    expect(trigger.textContent).toContain('v3');
    expect(trigger.dataset.checkoutKind).toBe('tag');
    expect(trigger.title).toMatch(/^Tag: v3/);
    expect(getToasts().map((t) => t.message)).toContain('Checked out v3');
    expect(getBindingMock('GetGitStatus')).toBeUndefined();
  });

  it('keeps a status pushed during the checkout over the older one the checkout answers with', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    const onBranch = { branch: 'feat/new', isDefaultBranch: false };
    setBindingMock('GitCheckout', async () => {
      // The backend's own push, with the PR lookup answered, lands first.
      __seedGitStatusForTest('/repo', status({ ...onBranch, openPrNumber: 7, openPrUrl: 'https://forge/pr/7' }));
      return {
        workspacePath: '/repo', worktreePath: '', branch: 'feat/new',
        status: status({ ...onBranch, openPrLookupPending: true }),
      };
    });
    const { findByRole, getByTestId } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /^feat\/new/ }));

    const trigger = getByTestId('branch-picker-trigger') as HTMLButtonElement;
    await waitFor(() => expect(getToasts().map((t) => t.message)).toContain('Checked out feat/new'));
    expect(trigger.textContent).toContain('feat/new');
    expect(pane.gitStatus.status?.openPrNumber).toBe(7);
    expect(pane.gitStatus.status?.openPrLookupPending).toBeFalsy();
  });

  it('keeps the status of a workspace released and reacquired during the checkout', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    const onBranch = { branch: 'feat/new', isDefaultBranch: false };
    setBindingMock('GitCheckout', async () => {
      // The only holder leaves and comes back: the fresh hold's first status
      // is the same count of applies as before, and newer than the answer.
      __resetGitStatusStoreForTest();
      __seedGitStatusForTest('/repo', status({ ...onBranch, openPrNumber: 7, openPrUrl: 'https://forge/pr/7' }));
      return {
        workspacePath: '/repo', worktreePath: '', branch: 'feat/new',
        status: status({ ...onBranch, openPrLookupPending: true }),
      };
    });
    const { findByRole } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /^feat\/new/ }));

    await waitFor(() => expect(getToasts().map((t) => t.message)).toContain('Checked out feat/new'));
    expect(pane.gitStatus.status?.openPrNumber).toBe(7);
    expect(pane.gitStatus.status?.openPrLookupPending).toBeFalsy();
  });

  it('keeps the held label for an answer without a status and names the row in the toast', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    setBindingMock('GitCheckout', async () => ({ workspacePath: '/repo', worktreePath: '', branch: '' }));
    const { findByRole, getByTestId } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /^v3/ }));

    const trigger = getByTestId('branch-picker-trigger') as HTMLButtonElement;
    await waitFor(() => expect(trigger.disabled).toBe(false));
    expect(trigger.textContent).toContain('main');
    expect(getToasts().map((t) => t.message)).toContain('Checked out v3');
  });

  it('names a draft\'s branch from the answer\'s status, and from a branch row only without one', async () => {
    const draft = () => {
      const pane = createThreadPane();
      pane.startDraftPlaceholder(
        { id: 'project-1', path: '/repo', name: 'Repo', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
        'chat',
        { provider: 'claude', model: 'm', workspacePath: '/repo', branch: 'main' },
      );
      return pane;
    };
    const row = { kind: 'branch', ref: 'feat/new', name: 'feat/new', branch: BRANCHES[1] } as const;

    const answered = draft();
    setBindingMock('GitCheckout', async () => ({
      workspacePath: '/repo', worktreePath: '', branch: '', status: status({ branch: 'feat/resolved' }),
    }));
    await checkoutPickerRow(answered, WS, row);
    expect(answered.thread?.branch).toBe('feat/resolved');

    const older = draft();
    setBindingMock('GitCheckout', async () => ({ workspacePath: '/repo', worktreePath: '', branch: '' }));
    await checkoutPickerRow(older, WS, row);
    expect(older.thread?.branch).toBe('feat/new');
  });

  it('warns with the short sha when a checkout leaves commits on no branch', async () => {
    __seedGitStatusForTest('/repo', detachedAt());
    const pane = await buildPane('');
    mockLists();
    setBindingMock('GitCheckout', async () => ({
      workspacePath: '/repo', worktreePath: '', branch: 'feat/new',
      leftBehind: { count: 3, commit: SHA },
    }));
    const { findByRole } = await openWithTags(pane);

    await fireEvent.click(await findByRole('menuitem', { name: /^feat\/new/ }));

    await waitFor(() => {
      expect(getToasts()).toContainEqual(expect.objectContaining({
        type: 'warning',
        message: 'Left 3 commits on no branch at abc1234. Create a branch from abc1234 to keep them.',
      }));
    });
  });

  it('refreshes tags from origin in place, keeping the menu open', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    let resolveRefresh: ((tags: unknown) => void) | undefined;
    const refresh = setBindingMock('GitRefreshTags', () => new Promise((resolve) => { resolveRefresh = resolve; }));
    const { findByTestId, getByTestId, findByRole, queryByRole } = await openWithTags(pane);

    const row = await findByTestId('branch-picker-refresh-tags');
    await fireEvent.click(row);
    expect(refresh).toHaveBeenCalledWith(WS);
    await waitFor(() => expect(getByTestId('branch-picker-refresh-tags').textContent).toContain('Refreshing tags'));
    expect(getByTestId('branch-picker-refresh-tags').querySelector('svg')).not.toBeNull();

    resolveRefresh!([{ name: 'v9', date: 900 }]);
    expect(await findByRole('menuitem', { name: /^v9/ })).toBeTruthy();
    expect(queryByRole('menuitem', { name: /^v1/ })).toBeNull();
    expect(getByTestId('branch-picker-refresh-tags').textContent).toContain('Refresh tags from origin');
  });

  it('puts the refresh row last inside the scrolling list', async () => {
    __seedGitStatusForTest('/repo', status());
    const many = Array.from({ length: TAG_ROW_CAP + 2 }, (_, i) => ({ name: `r${i}`, date: 10_000 - i }));
    const pane = await buildPane('main');
    mockLists(many);
    const { findByTestId, getByTestId } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    const refresh = await findByTestId('branch-picker-refresh-tags');
    const list = getByTestId('branch-picker-list');
    expect(list.classList.contains('overflow-y-auto')).toBe(true);
    expect(list.contains(refresh)).toBe(true);
    expect(list.lastElementChild).toBe(refresh);
    // After the tag rows and the "more" hint.
    const hint = getByTestId('branch-picker-more-tags');
    expect(hint.compareDocumentPosition(refresh) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('keeps the refresh row under the empty message when nothing is listed', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    const { getByTestId, findByTestId, getByPlaceholderText } = await openWithTags(pane);

    await fireEvent.input(getByPlaceholderText('Search branches and tags'), { target: { value: 'zzz' } });
    const empty = await findByTestId('branch-picker-empty');
    expect(empty.textContent?.trim()).toBe('No branches or tags');
    const refresh = getByTestId('branch-picker-refresh-tags');
    expect(getByTestId('branch-picker-list').contains(refresh)).toBe(true);
    expect(empty.compareDocumentPosition(refresh) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('offers the refresh row in a repository with no tags yet', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists([]);
    setBindingMock('GitListBranches', async () => []);
    const { getByTestId, findByTestId } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    expect((await findByTestId('branch-picker-empty')).textContent?.trim()).toBe('No branches or tags');
    expect(getByTestId('branch-picker-list').contains(getByTestId('branch-picker-refresh-tags'))).toBe(true);
  });

  it('says "No branches" when the backend lists no tags', async () => {
    hello([]);
    const pane = await buildPane('main');
    mockLists();
    setBindingMock('GitListBranches', async () => []);
    const { getByTestId, findByTestId, queryByTestId } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    expect((await findByTestId('branch-picker-empty')).textContent?.trim()).toBe('No branches');
    expect(queryByTestId('branch-picker-list')).toBeNull();
  });

  it('reports a failed tag refresh as a toast and keeps the rows', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    setBindingMock('GitRefreshTags', async () => { throw new Error('no route to origin'); });
    const { findByTestId, findByRole } = await openWithTags(pane);

    await fireEvent.click(await findByTestId('branch-picker-refresh-tags'));

    await waitFor(() => {
      expect(getToasts()).toContainEqual(expect.objectContaining({
        type: 'error',
        message: expect.stringMatching(/^Failed to refresh tags: no route to origin/i),
      }));
    });
    expect(await findByRole('menuitem', { name: /^v1/ })).toBeTruthy();
    expect(await findByTestId('branch-picker-refresh-tags')).toBeTruthy();
  });

  it('offers no refresh without an origin remote', async () => {
    __seedGitStatusForTest('/repo', status({ hasOriginRemote: false }));
    const pane = await buildPane('main');
    mockLists();
    const { queryByTestId } = await openWithTags(pane);
    expect(queryByTestId('branch-picker-refresh-tags')).toBeNull();
  });

  it('makes no tag calls and offers no tag rows against a backend without tags', async () => {
    hello([]);
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    const { getByTestId, findByRole, queryByTestId, getByPlaceholderText } = render(BranchPicker, {
      props: { pane },
    });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /^feat\/new/ });

    expect(getBindingMock('GitListTags')).toHaveBeenCalledTimes(0);
    expect(queryByTestId('branch-picker-tag-row')).toBeNull();
    expect(queryByTestId('branch-picker-refresh-tags')).toBeNull();
    expect(getByPlaceholderText('Search Branches')).toBeTruthy();
  });

  it('drops the held tags, and a read still in flight, when the backend stops serving tags', async () => {
    const pane = await buildPane('main');
    mockLists();
    const { queryByTestId, findByRole } = await openWithTags(pane);

    hello([]);
    await waitFor(() => expect(queryByTestId('branch-picker-tag-row')).toBeNull());

    // A read that answers after support is lost again must not list its tags.
    let answer: (tags: unknown) => void = () => {};
    setBindingMock('GitListTags', () => new Promise((resolve) => { answer = resolve; }));
    hello(['git-tags.v1']);
    await waitFor(() => expect(getBindingMock('GitListTags')).toHaveBeenCalledTimes(1));
    hello([]);
    answer(TAGS);
    await findByRole('menuitem', { name: /^feat\/new/ });
    await Promise.resolve();
    expect(queryByTestId('branch-picker-tag-row')).toBeNull();
  });

  it('keeps branches usable when the tag list fails', async () => {
    const pane = await buildPane('main');
    mockLists();
    setBindingMock('GitListTags', async () => { throw new Error('tags exploded'); });
    setBindingMock('GitCheckout', async () => ({ workspacePath: '/repo', worktreePath: '', branch: 'feat/new' }));
    const { getByTestId, findByRole, findByTestId } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));

    expect((await findByTestId('branch-picker-tags-error')).textContent).toMatch(/tags exploded/i);
    await fireEvent.click(await findByRole('menuitem', { name: /^feat\/new/ }));
    await waitFor(() => expect(getBindingMock('GitCheckout')).toHaveBeenCalledWith(WS, 'feat/new'));
  });

  it('reads tags once per open workspace, not again when the checked-out branch moves', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    await openWithTags(pane);
    expect(getBindingMock('GitListTags')).toHaveBeenCalledTimes(1);

    __seedGitStatusForTest('/repo', status({ branch: 'feat/new', isDefaultBranch: false }));
    await waitFor(() => expect(getBindingMock('GitListBranches')).toHaveBeenCalledTimes(2));
    expect(getBindingMock('GitListTags')).toHaveBeenCalledTimes(1);

    pane.replaceThread({ ...pane.thread!, workspacePath: '/other' });
    await waitFor(() => expect(getBindingMock('GitListTags')).toHaveBeenCalledTimes(2));
    expect(getBindingMock('GitListTags')).toHaveBeenLastCalledWith({ projectId: 'project-1', workspacePath: '/other' });
  });

  it('drops the tag list when the menu closes', async () => {
    const pane = await buildPane('main');
    mockLists();
    const { getByTestId, queryByTestId, findByRole } = await openWithTags(pane);

    await fireEvent.click(getByTestId('branch-picker-trigger'));
    // The next open's read never answers: only a list kept from before could render.
    setBindingMock('GitListTags', () => new Promise(() => {}));
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await findByRole('menuitem', { name: /^feat\/new/ });

    expect(queryByTestId('branch-picker-tag-row')).toBeNull();
  });

  it('follows a live detach of a draft whose cached branch is main', async () => {
    __seedGitStatusForTest('/repo', detachedAt('v2'));
    const pane = createThreadPane();
    pane.startDraftPlaceholder(
      { id: 'project-1', path: '/repo', name: 'Repo', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
      'chat',
      { provider: 'claude', model: 'm', workspacePath: '/repo', branch: 'main' },
    );
    mockLists();
    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });

    expect(pane.thread?.branch).toBe('main');
    const trigger = getByTestId('branch-picker-trigger');
    expect(trigger.textContent).toContain('v2');
    expect(trigger.dataset.checkoutKind).toBe('tag');

    await fireEvent.click(trigger);
    expect((await findByRole('menuitem', { name: /^v2/ })).textContent).toContain('✓');
    expect((await findByRole('menuitem', { name: /^main/ })).textContent).not.toContain('✓');
    await fireEvent.click(await findByRole('menuitem', { name: /New branch/ }));
    expect(worktreeIntentForThread(pane.thread)).toMatchObject({ creatingBranch: true, newBranchBase: '' });
    await waitFor(() => expect(trigger.textContent).toContain('From v2'));
  });

  it('follows a branch switch made outside this client', async () => {
    __seedGitStatusForTest('/repo', status({ branch: 'feat/new', isDefaultBranch: false }));
    const pane = await buildPane('main');
    mockLists();
    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });

    const trigger = getByTestId('branch-picker-trigger');
    expect(trigger.textContent).toContain('feat/new');
    await fireEvent.click(trigger);
    expect((await findByRole('menuitem', { name: /^feat\/new/ })).textContent).toContain('✓');
    await fireEvent.click(await findByRole('menuitem', { name: /New branch/ }));
    expect(worktreeIntentForThread(pane.thread)).toMatchObject({ creatingBranch: true, newBranchBase: '' });
    await waitFor(() => expect(trigger.textContent).toContain('From feat/new'));
  });

  it('drops a pending refresh when the pane moves to another workspace and lists that one', async () => {
    __seedGitStatusForTest('/repo', status());
    const pane = await buildPane('main');
    mockLists();
    setBindingMock('GitListTags', async (ws: { workspacePath: string }) =>
      ws.workspacePath === '/repo' ? TAGS : [{ name: 'other-repo-tag', date: 5 }]);
    let resolveRefresh: ((tags: unknown) => void) | undefined;
    setBindingMock('GitRefreshTags', () => new Promise((resolve) => { resolveRefresh = resolve; }));
    const { findByTestId, findByRole, queryByRole } = await openWithTags(pane);

    await fireEvent.click(await findByTestId('branch-picker-refresh-tags'));
    pane.replaceThread({ ...pane.thread!, workspacePath: '/other' });
    expect(await findByRole('menuitem', { name: /^other-repo-tag/ })).toBeTruthy();

    resolveRefresh!([{ name: 'from-repo-a', date: 900 }]);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(queryByRole('menuitem', { name: /^from-repo-a/ })).toBeNull();
    expect(queryByRole('menuitem', { name: /^v1/ })).toBeNull();
    expect(await findByRole('menuitem', { name: /^other-repo-tag/ })).toBeTruthy();
  });

  it('sends a branch named like a tag ref as a branch ref, and the tag as a tag ref', async () => {
    const pane = await buildPane('main');
    mockLists([{ name: 'v1.0', date: 10 }]);
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isCurrent: true, isDefault: true, date: 100 },
      { name: 'refs/tags/v1.0', isCurrent: false, isDefault: false, date: 50 },
    ]);
    const checkout = setBindingMock('GitCheckout', async () => ({ workspacePath: '/repo', worktreePath: '', branch: 'x' }));
    const { getByTestId, findByRole, findByTestId } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await fireEvent.click(await findByRole('menuitem', { name: /^refs\/tags\/v1\.0/ }));
    await waitFor(() => expect(checkout).toHaveBeenLastCalledWith(WS, 'refs/heads/refs/tags/v1.0'));
    await waitFor(() => expect((getByTestId('branch-picker-trigger') as HTMLButtonElement).disabled).toBe(false));

    await fireEvent.click(getByTestId('branch-picker-trigger'));
    await fireEvent.click(await findByTestId('branch-picker-tag-row'));
    await waitFor(() => expect(checkout).toHaveBeenLastCalledWith(WS, 'refs/tags/v1.0'));
  });
});
