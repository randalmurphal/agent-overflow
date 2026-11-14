import { beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';

import ChatHeaderFactsLine from './ChatHeaderFactsLine.svelte';
import type { GitStatus } from '../../types/git';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane as buildRegisteredPane, makeThread } from '../../../test/helpers/chat';
import { idleWorkspaceActivity } from '../../../test/helpers/workspaceLock';
import { __resetGitStatusStoreForTest, __seedGitStatusForTest } from '../../stores/gitStatusStore.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetForTest as resetWorktreeIntent } from '../../stores/worktreeIntent.svelte';

const SHA = '9f8e7d6c5b4a39281706f5e4d3c2b1a098765432';

function detached(tag?: string): GitStatus {
  return {
    isRepo: true, branch: '', isDefaultBranch: false, hasChanges: false,
    insertions: 0, deletions: 0, fileCount: 0, hasUpstream: false,
    aheadCount: 0, behindCount: 0, hasOriginRemote: true, tag, headCommit: SHA,
  };
}

async function renderAt(status: GitStatus | null, branch = '') {
  if (status) __seedGitStatusForTest('/repo', status);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  setBindingMock('UpdateThreadBranch', async () => []);
  const pane = await buildRegisteredPane(makeThread({
    workspacePath: '/repo', projectPath: '/repo', projectId: 'project-1', branch,
  }));
  const view = render(ChatHeaderFactsLine, { props: { pane } });
  await tick();
  return view.getByTestId('chat-header-branch');
}

describe('<ChatHeaderFactsLine> branch segment', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetWorktreeIntent();
    __resetGitStatusStoreForTest();
  });

  it('names a detached HEAD at no tag by its commit, with the commit icon', async () => {
    const segment = await renderAt(detached());
    expect(segment).toHaveTextContent('detached at 9f8e7d6');
    expect(segment.dataset.checkoutKind).toBe('commit');
    expect(segment.querySelector('.lucide-git-commit-horizontal')).not.toBeNull();
  });

  it('names a detached tag checkout by its tag, with the tag icon', async () => {
    const segment = await renderAt(detached('v2.4.0'));
    expect(segment).toHaveTextContent('v2.4.0');
    expect(segment.dataset.checkoutKind).toBe('tag');
    expect(segment.getAttribute('title')).toBe('Tag: v2.4.0');
    expect(segment.querySelector('.lucide-tag')).not.toBeNull();
  });

  it('names a branch with the branch icon', async () => {
    const segment = await renderAt(null, 'main');
    expect(segment).toHaveTextContent('main');
    expect(segment.dataset.checkoutKind).toBe('branch');
    expect(segment.querySelector('.lucide-git-branch')).not.toBeNull();
  });
});
