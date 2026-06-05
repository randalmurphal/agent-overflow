import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import DiffPanelDrawer from './DiffPanelDrawer.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { Checkpoint } from '../../types/checkpoint';
import type { GitStatus } from '../../types/git';

function checkpoint(turnIndex: number): Checkpoint {
  return {
    id: `checkpoint-${turnIndex}`,
    threadId: 'thread-1',
    userItemId: `user-${turnIndex}`,
    turnIndex,
    status: 'ready',
    files: turnIndex === 0 ? [] : [{ path: 'notes.txt', kind: 'modified', additions: 1, deletions: 0 }],
    capturedAt: 1,
  };
}

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'feature/workspace',
    isDefaultBranch: false,
    hasChanges: true,
    insertions: 1,
    deletions: 0,
    fileCount: 1,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'github',
    ...overrides,
  };
}

describe('<DiffPanelDrawer>', () => {
  it('jumps the timeline to the selected message checkpoint', async () => {
    const thread = makeThread({ id: 'thread-1' });
    const checkpoints = [checkpoint(0), checkpoint(1)];
    const pane = await buildPane(thread);
    setBindingMock('ListThreadCheckpoints', async () => checkpoints);
    setBindingMock('GetSessionAgentDiff', async () => '');
    setBindingMock('GetMessageCheckpointDiff', async () => '');
    setBindingMock('GetWorkspaceCurrentDiff', async () => '');
    setBindingMock('ListDiffReviewComments', async () => []);
    const requestScrollToItem = vi.spyOn(pane, 'requestScrollToItem');

    const { findByTestId, getByTestId } = render(DiffPanelDrawer, { props: { pane } });
    const secondMessage = await findByTestId('diff-message-1');
    await fireEvent.click(secondMessage);
    await fireEvent.click(getByTestId('diff-message-jump'));

    expect(requestScrollToItem).toHaveBeenCalledWith('user-1', { flash: true });
  });

  // Regression: when a non-empty session diff produced a real
  // `diffSourceKey`, the load-comments effect ran `refreshDiffReviewComments`
  // which synchronously read+wrote the same `commentsBySource` SvelteMap
  // entry, tripping Svelte's effect_update_depth_exceeded guard. The store
  // now stages fetch bookkeeping in a non-reactive map and only writes the
  // reactive cache after the await — keeping the effect a single-step.
  it('loads comments without infinite-looping on a non-empty session diff', async () => {
    const thread = makeThread({ id: 'thread-1' });
    const pane = await buildPane(thread);
    const sessionDiff = `diff --git a/notes.txt b/notes.txt
index 0000000..1111111 100644
--- a/notes.txt
+++ b/notes.txt
@@ -1 +1,2 @@
 first
+second
`;
    setBindingMock('ListThreadCheckpoints', async () => [checkpoint(0)]);
    setBindingMock('GetSessionAgentDiff', async () => sessionDiff);
    setBindingMock('GetMessageCheckpointDiff', async () => '');
    setBindingMock('GetWorkspaceCurrentDiff', async () => '');
    const list = vi.fn(async () => []);
    setBindingMock('ListDiffReviewComments', list);

    const { findByTestId } = render(DiffPanelDrawer, { props: { pane } });

    await findByTestId('diff-viewer');
    await waitFor(() => {
      expect(list).toHaveBeenCalled();
    });
    expect(list.mock.calls.length).toBeLessThan(5);
  });

  it('reloads the workspace diff when git status changes', async () => {
    const thread = makeThread({ id: 'thread-1' });
    const pane = await buildPane(thread);
    pane.diffPanel.setTabMode('workspace');
    pane.gitStatus.set(status({ hasChanges: true, insertions: 1, fileCount: 1 }));
    const workspaceDiff = `diff --git a/notes.txt b/notes.txt
index 0000000..1111111 100644
--- a/notes.txt
+++ b/notes.txt
@@ -1 +1,2 @@
 first
+second
`;
    let diffCalls = 0;
    const diff = vi.fn(async () => {
      diffCalls += 1;
      return diffCalls === 1 ? workspaceDiff : '';
    });
    setBindingMock('ListThreadCheckpoints', async () => []);
    setBindingMock('GetSessionAgentDiff', async () => '');
    setBindingMock('GetMessageCheckpointDiff', async () => '');
    setBindingMock('GetWorkspaceCurrentDiff', diff);
    setBindingMock('ListDiffReviewComments', async () => []);

    const { findByTestId, findByText } = render(DiffPanelDrawer, { props: { pane } });
    await findByTestId('diff-viewer');

    pane.gitStatus.set(status({
      hasChanges: false,
      insertions: 0,
      deletions: 0,
      fileCount: 0,
    }));

    await waitFor(() => {
      expect(diff).toHaveBeenCalledTimes(2);
    });
    expect(await findByText('No changes in this range.')).toBeInTheDocument();
  });
});
