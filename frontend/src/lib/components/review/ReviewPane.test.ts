import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import ReviewPane from './ReviewPane.svelte';
import type { PanelContext } from '../../stores/panelContext.svelte';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import type { DiffReviewComment, DiffReviewCommentInput } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

function makeCtx(): PanelContext {
  return {
    paneId: 'source-pane',
    threadId: 'thread-1',
    thread: null,
    workspacePath: '/repo',
    designViewport: 'desktop',
    activeOptionSet: null,
    close() {},
    replaceThread() {},
    async switchThread() {},
    setDesignViewport() {},
    setActiveOptionSet() {},
    async refreshDesignOptions() {},
  };
}

function patch(): string {
  return [
    'diff --git a/src/app.ts b/src/app.ts',
    'index 1111111..2222222 100644',
    '--- a/src/app.ts',
    '+++ b/src/app.ts',
    '@@ -1 +1 @@',
    '-old',
    '+new',
    'diff --git a/pnpm-lock.yaml b/pnpm-lock.yaml',
    'index 3333333..4444444 100644',
    '--- a/pnpm-lock.yaml',
    '+++ b/pnpm-lock.yaml',
    '@@ -1 +1 @@',
    '-old',
    '+new',
  ].join('\n');
}

beforeEach(() => {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  setBindingMock('GetWorkspaceCurrentDiff', async () => patch());
  setBindingMock('GetSessionAgentDiff', async () => '');
  setBindingMock('GetBranchBaseDiff', async () => '');
  setBindingMock('GetMessageCheckpointDiff', async () => '');
  setBindingMock('ListThreadCheckpoints', async () => []);
  setBindingMock('GitListBranches', async () => [{ name: 'main', isCurrent: false, isDefault: true }]);
  setBindingMock('ListDiffReviewComments', async () => []);
  setBindingMock('CreateDiffReviewComment', async () => ({}));
  setBindingMock('UpdateDiffReviewComment', async () => ({}));
  setBindingMock('DeleteDiffReviewComment', async () => undefined);
  setBindingMock('SendDiffReviewComments', async () => ({}));
});

describe('<ReviewPane>', () => {
  it('renders the virtualized diff body and toggles collapse', async () => {
    const view = render(ReviewPane, { ctx: makeCtx() });

    await waitFor(() => {
      expect(view.getAllByTestId('review-file-header-path').map((node) => node.textContent)).toEqual([
        expect.stringContaining('src/app.ts'),
        expect.stringContaining('pnpm-lock.yaml'),
      ]);
    });
    expect(view.getAllByTestId('review-file-header')).toHaveLength(2);
    // Toolbar totals: 2 files, +1/-1 each.
    expect(view.getByTestId('review-diff-stats').textContent).toContain('2 files');
    expect(view.getByTestId('review-diff-stats').textContent).toContain('+2');
    expect(view.getByTestId('review-diff-stats').textContent).toContain('-2');
    // The lockfile default-collapses; the source file renders its lines.
    expect(view.getAllByTestId('review-file-collapsed')).toHaveLength(1);
    expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
    expect(view.getByText('+new')).toBeInTheDocument();
    expect(view.getByText('-old')).toBeInTheDocument();

    await fireEvent.click(view.getAllByTestId('review-file-header-path')[0]!);
    expect(view.queryAllByTestId('review-line-block')).toHaveLength(0);
    expect(view.getAllByTestId('review-file-collapsed')).toHaveLength(2);

    await fireEvent.click(view.getAllByTestId('review-file-collapsed')[0]!);
    expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
  });

  it('switches to split view and back', async () => {
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
    });

    await fireEvent.click(view.getByTestId('review-split-toggle'));
    const block = view.getByTestId('review-line-block');
    // Split view renders the del/add pair side by side on one visual row.
    expect(block.querySelectorAll('.w-1\\/2')).toHaveLength(2);

    await fireEvent.click(view.getByTestId('review-split-toggle'));
    expect(view.getByTestId('review-line-block').querySelectorAll('.w-1\\/2')).toHaveLength(0);
  });

  it('creates a gutter draft and sends the draft comments', async () => {
    let comments: DiffReviewComment[] = [];
    setBindingMock('ListDiffReviewComments', async () => comments);
    const create = setBindingMock('CreateDiffReviewComment', async (...args: never[]) => {
      const [threadId, input] = args as unknown as [string, DiffReviewCommentInput];
      const comment: DiffReviewComment = {
        id: 'comment-1',
        threadId,
        scope: input.scope,
        sourceKey: input.sourceKey,
        filePath: input.filePath,
        status: 'draft',
        oldLine: input.oldLine,
        newLine: input.newLine,
        side: input.side,
        selectedText: input.selectedText,
        body: input.body,
        createdAt: 1,
        updatedAt: 1,
      };
      comments = [comment];
      return comment;
    });
    const send = setBindingMock('SendDiffReviewComments', async () => {
      comments = comments.map((comment) => ({ ...comment, status: 'sent' as const }));
      return {};
    });

    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
    });

    await fireEvent.mouseOver(view.getByTestId('review-line-block'));
    await fireEvent.click(view.getAllByTestId('review-add-comment')[0]!);
    const editor = view.getByTestId('review-draft-editor');
    expect(editor).toBeInTheDocument();
    const textarea = editor.querySelector('textarea')!;
    const addButton = Array.from(editor.querySelectorAll('button'))
      .find((button) => button.textContent?.trim() === 'Add comment')!;

    await fireEvent.input(textarea, {
      target: { value: 'Please revisit this line.' },
    });
    await fireEvent.click(addButton);

    await waitFor(() => {
      expect(create).toHaveBeenCalled();
      expect(view.getByTestId('review-comment-thread')).toBeInTheDocument();
      expect(view.getByText('Please revisit this line.')).toBeInTheDocument();
      expect(view.getByTestId('review-send-strip')).toBeInTheDocument();
    });

    await fireEvent.click(view.getByRole('button', { name: 'Send comments' }));

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'workspace', expect.stringMatching(/^fnv1a:/), ['comment-1'], { pr: undefined });
      expect(view.queryByTestId('review-send-strip')).not.toBeInTheDocument();
    });
  });
});
