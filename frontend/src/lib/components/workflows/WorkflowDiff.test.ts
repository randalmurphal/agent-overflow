import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { parseReviewFiles, PatchTextLost, type ReviewFile } from '../../utils/patchStore';
import { installDiagnosticsCapture } from '../../../test/helpers/diagnostics';
import WorkflowDiff from './WorkflowDiff.svelte';
import WorkflowDiffBlock from './WorkflowDiffBlock.svelte';

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
    expect(view.queryByTestId('wf-diff-hunks')).not.toBeInTheDocument();
    await view.rerender({ files, expandFirst: true });
    await waitFor(() => expect(view.getByTestId('wf-diff-hunks')).toHaveTextContent('+changed'));
    expect(view.getByTestId('wf-diff-hunks')).toHaveTextContent('@@ -1 +1 @@ -before +changed');
    await view.rerender({ files, expandFirst: false });
    expect(view.queryByTestId('wf-diff-hunks')).not.toBeInTheDocument();
  });

  it('renders a file\'s lines only when it expands', async () => {
    const view = render(WorkflowDiff, { files: filesOf() });
    expect(view.queryByTestId('wf-diff-hunks')).not.toBeInTheDocument();
    await fireEvent.click(view.getByTestId('wf-diff-file-toggle'));
    await waitFor(() => expect(view.getByTestId('wf-diff-hunks')).toHaveTextContent('+changed'));
    await fireEvent.click(view.getByTestId('wf-diff-file-toggle'));
    expect(view.queryByTestId('wf-diff-hunks')).not.toBeInTheDocument();
  });
});

describe('WorkflowDiffBlock', () => {
  const diagnostics = installDiagnosticsCapture();

  it('reports text it fails to read for any reason but lost text, which its owner handles', async () => {
    const [file] = filesOf();
    const whenResident = vi.spyOn(file.body, 'whenResident');
    const props = { file, start: 0, end: file.body.lineCount, padTop: 0, padBottom: 0, onWidth: () => {} };

    whenResident.mockRejectedValueOnce(new PatchTextLost());
    render(WorkflowDiffBlock, props);
    await waitFor(() => expect(whenResident).toHaveBeenCalledTimes(1));
    // Let the rejection be handled before asserting nothing was reported.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(await diagnostics.messages()).toEqual([]);

    whenResident.mockRejectedValueOnce(new Error('boom'));
    render(WorkflowDiffBlock, props);
    await waitFor(async () => {
      expect(await diagnostics.messages()).toEqual(['workflow diff: block text could not be read']);
    });
  });
});
