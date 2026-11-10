import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import { parseReviewFiles, type ReviewFile } from '../../utils/patchStore';
import WorkflowDiff from './WorkflowDiff.svelte';

const disposers: ReviewFile[][] = [];

afterEach(() => {
  for (const files of disposers.splice(0)) files[0]?.body.segments[0]?.store.dispose();
});

function filesOf(): ReviewFile[] {
  const files = parseReviewFiles('diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1 @@\n-before\n+changed\n');
  disposers.push(files);
  return files;
}

describe('WorkflowDiff', () => {
  it('expands and collapses the first file from the Enter-controlled prop', async () => {
    const files = filesOf();
    const view = render(WorkflowDiff, { files, expandFirst: false });
    expect(view.queryByTestId('patch-lines')).not.toBeInTheDocument();
    await view.rerender({ files, expandFirst: true });
    await waitFor(() => expect(view.getByTestId('patch-lines')).toHaveTextContent('+changed'));
    expect(view.getByTestId('patch-lines')).toHaveTextContent('@@ -1 +1 @@ -before +changed');
    await view.rerender({ files, expandFirst: false });
    expect(view.queryByTestId('patch-lines')).not.toBeInTheDocument();
  });

  it('renders a file\'s lines only when it expands', async () => {
    const view = render(WorkflowDiff, { files: filesOf() });
    expect(view.queryByTestId('patch-lines')).not.toBeInTheDocument();
    await fireEvent.click(view.getByTestId('wf-diff-file-toggle'));
    await waitFor(() => expect(view.getByTestId('patch-lines')).toHaveTextContent('+changed'));
    await fireEvent.click(view.getByTestId('wf-diff-file-toggle'));
    expect(view.queryByTestId('patch-lines')).not.toBeInTheDocument();
  });
});
