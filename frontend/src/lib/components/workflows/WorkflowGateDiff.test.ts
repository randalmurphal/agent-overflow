import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import WorkflowGateDiff from './WorkflowGateDiff.svelte';
import { setPatchTextBudgetForTest } from '../../utils/patchMemory.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { WorkspaceRef } from '../../types/git';

const workspace: WorkspaceRef = { projectId: 'project-1', workspacePath: '/tmp/gate' };

function added(path: string, lines: number): string {
  return [
    `diff --git a/${path} b/${path}`,
    'new file mode 100644',
    '--- /dev/null',
    `+++ b/${path}`,
    `@@ -0,0 +1,${lines} @@`,
    ...Array.from({ length: lines }, (_, index) => `+${path} line ${index + 1}`),
  ].join('\n');
}

const patch = [added('a.go', 4), added('b.go', 4), added('c.go', 4)].join('\n') + '\n';

// The branch-base diff in 40-character chunks under one handle.
function installDiff() {
  const chunkAt = (offset: number) => {
    const next = Math.min(patch.length, offset + 40);
    return { data: patch.slice(offset, next), offset, nextOffset: next, eof: next === patch.length };
  };
  const open = setBindingMock('OpenBranchBaseDiff', async () => ({ id: 'gate-1', chunk: chunkAt(0) }));
  const read = setBindingMock('ReadReviewDiff', async (_id: string, offset: number) => chunkAt(offset));
  const release = setBindingMock('ReleaseReviewDiff', async () => undefined);
  return { open, read, release };
}

afterEach(() => setPatchTextBudgetForTest(null));

describe('<WorkflowGateDiff> past the memory budget', () => {
  it('shows a file whose text was evicted, and lets the diff go when it unmounts', async () => {
    setPatchTextBudgetForTest(100);
    const { release } = installDiff();
    const view = render(WorkflowGateDiff, { workspace, threadId: 'thread-1', baseBranch: 'main', expandFirst: false });

    await fireEvent.click(view.getByTestId('workflow-diff-load'));
    await waitFor(() => {
      expect(view.getAllByTestId('wf-diff-file')).toHaveLength(3);
    });
    expect(release).not.toHaveBeenCalled();

    await fireEvent.click(view.getAllByTestId('wf-diff-file-toggle')[2]);
    expect(await view.findByTestId('wf-diff-hunks')).toHaveTextContent('c.go line 4');

    view.unmount();
    expect(release).toHaveBeenCalledTimes(1);
    expect(release).toHaveBeenCalledWith('gate-1');
  });

  it('reads the diff again when its evicted text can no longer be read', async () => {
    setPatchTextBudgetForTest(100);
    const { open, read } = installDiff();
    const view = render(WorkflowGateDiff, { workspace, threadId: 'thread-1', baseBranch: 'main', expandFirst: false });
    await fireEvent.click(view.getByTestId('workflow-diff-load'));
    await waitFor(() => {
      expect(view.getAllByTestId('wf-diff-file')).toHaveLength(3);
    });

    read.mockImplementationOnce(async () => { throw new Error('review diff: not open'); });
    await fireEvent.click(view.getAllByTestId('wf-diff-file-toggle')[2]);

    await waitFor(() => {
      expect(open).toHaveBeenCalledTimes(2);
    });
    // The lost diff's files go while it is read again.
    await tick();
    await waitFor(() => {
      expect(view.getAllByTestId('wf-diff-file')).toHaveLength(3);
    });
    await fireEvent.click(view.getAllByTestId('wf-diff-file-toggle')[2]);
    expect(await view.findByTestId('wf-diff-hunks')).toHaveTextContent('c.go line 4');
    view.unmount();
  });
});
