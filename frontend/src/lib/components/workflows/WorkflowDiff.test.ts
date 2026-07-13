import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import type { PatchFile } from '../../utils/patchFiles';
import WorkflowDiff from './WorkflowDiff.svelte';

const files: PatchFile[] = [{
  path: 'app.go', kind: 'modified', additions: 1, deletions: 0,
  lines: [{ content: '+changed', type: 'add' }],
}];

describe('WorkflowDiff', () => {
  it('expands and collapses the first file from the Enter-controlled prop', async () => {
    const view = render(WorkflowDiff, { files, expandFirst: false });
    expect(view.queryByTestId('wf-diff-hunks')).not.toBeInTheDocument();
    await view.rerender({ files, expandFirst: true });
    expect(view.getByTestId('wf-diff-hunks')).toHaveTextContent('changed');
    await view.rerender({ files, expandFirst: false });
    expect(view.queryByTestId('wf-diff-hunks')).not.toBeInTheDocument();
  });

  it('loads hunks only when their file expands', async () => {
    const summary = [{ ...files[0], lines: [] }];
    const onLoadFile = vi.fn(async () => files[0]);
    const view = render(WorkflowDiff, { files: summary, onLoadFile });
    expect(onLoadFile).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('wf-diff-file-toggle'));
    expect(onLoadFile).toHaveBeenCalledWith('app.go');
    expect(await view.findByTestId('wf-diff-hunks')).toHaveTextContent('changed');
  });
});
