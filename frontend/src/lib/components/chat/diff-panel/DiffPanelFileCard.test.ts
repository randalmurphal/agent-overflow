import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';
import DiffPanelFileCard from './DiffPanelFileCard.svelte';
import { parsePatchFiles } from '../../../utils/patchFiles';

describe('<DiffPanelFileCard>', () => {
  it('hides git diff metadata from the expanded body', () => {
    const [file] = parsePatchFiles(`diff --git a/test-ui.txt b/test-ui.txt
new file mode 100644
index 0000000..572d5d9
--- /dev/null
+++ b/test-ui.txt
@@ -0,0 +1,2 @@
+Line 1
+Line 2
`);

    const { container } = render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        onToggle: vi.fn(),
      },
    });

    expect(container.textContent).toContain('+Line 1');
    expect(container.textContent).toContain('+Line 2');
    expect(container.textContent).not.toContain('diff --git');
    expect(container.textContent).not.toContain('new file mode');
    expect(container.textContent).not.toContain('index 0000000');
    expect(container.textContent).not.toContain('--- /dev/null');
    expect(container.textContent).not.toContain('+++ b/test-ui.txt');
    expect(container.textContent).not.toContain('@@ -0,0 +1,2 @@');
  });

  it('shows line and file comment affordances only for commentable diffs', async () => {
    const [file] = parsePatchFiles(`diff --git a/test-ui.txt b/test-ui.txt
--- a/test-ui.txt
+++ b/test-ui.txt
@@ -1 +1 @@
-old
+new
`);
    const onCreateComment = vi.fn();

    const { rerender } = render(DiffPanelFileCard, {
      props: {
        file,
        open: true,
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        commentable: false,
        reviewScope: null,
        onToggle: vi.fn(),
        onCreateComment,
      },
    });

    expect(screen.queryByLabelText('Comment on file')).not.toBeInTheDocument();
    expect(screen.queryAllByLabelText('Add comment')).toHaveLength(0);

    await rerender({
      file,
      open: true,
      workspacePath: '/tmp/project',
      viewMode: 'stacked',
      wordWrap: false,
      commentable: true,
      reviewScope: 'workspace',
      sourceKey: 'diff-1',
      onToggle: vi.fn(),
      onCreateComment,
    });

    await fireEvent.click(screen.getAllByLabelText('Add comment')[0]);
    expect(screen.getByPlaceholderText('Comment on this line')).toBeInTheDocument();
  });
});
