import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
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
});
