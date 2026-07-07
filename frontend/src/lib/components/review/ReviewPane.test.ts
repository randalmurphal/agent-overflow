import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import ReviewPane from './ReviewPane.svelte';
import type { PanelContext } from '../../stores/panelContext.svelte';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { resetForTest as resetDiffReviewCommentsForTest } from '../../stores/diffReviewComments.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import type { DiffReviewComment, DiffReviewCommentInput, PRDetail, Thread } from '../../types/models';
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
  resetDiffReviewCommentsForTest();
  // The mount/reload PR probe reads both; defaults resolve to "no PR".
  setBindingMock('GetThread', async () => ({ id: 'thread-1', workspacePath: '/repo' }));
  setBindingMock('GetGitStatus', async () => ({}));
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
    // The lockfile default-collapses to its header alone; the source
    // file renders its lines.
    expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
    expect(view.getByText('+new')).toBeInTheDocument();
    expect(view.getByText('-old')).toBeInTheDocument();

    await fireEvent.click(view.getAllByTestId('review-file-header-path')[0]!);
    expect(view.queryAllByTestId('review-line-block')).toHaveLength(0);
    expect(view.getAllByTestId('review-file-header')).toHaveLength(2);

    await fireEvent.click(view.getAllByTestId('review-file-header-path')[0]!);
    expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
  });

  it('toggles collapse-all/expand-all from the toolbar', async () => {
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
    });

    const toggle = view.getByTestId('review-collapse-all-toggle');
    expect(toggle).toHaveAccessibleName('Collapse all files');

    await fireEvent.click(toggle);
    expect(view.queryAllByTestId('review-line-block')).toHaveLength(0);
    expect(view.getAllByTestId('review-file-header')).toHaveLength(2);
    expect(toggle).toHaveAccessibleName('Expand all files');

    await fireEvent.click(toggle);
    // Expand-all overrides the lockfile's default collapse too.
    expect(view.getAllByTestId('review-line-block')).toHaveLength(2);
    expect(toggle).toHaveAccessibleName('Collapse all files');
  });

  it('surfaces comments in the rail tab, tree badges, and toolbar tally', async () => {
    const draft: DiffReviewComment = {
      id: 'draft-1',
      threadId: 'thread-1',
      scope: 'workspace',
      sourceKey: 'source',
      filePath: 'src/app.ts',
      status: 'draft',
      newLine: 1,
      side: 'new',
      selectedText: '',
      body: 'needs a guard here',
      createdAt: 1,
      updatedAt: 1,
    };
    setBindingMock('ListDiffReviewComments', async () => [draft]);

    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getByTestId('review-comment-tally')).toHaveTextContent('1 draft');
    });

    // Files tab: the commented file carries a count badge.
    expect(view.getByTestId('review-tree-comment-count')).toHaveTextContent('1');

    // Tally opens the Comments tab; the draft is listed with its snippet.
    await fireEvent.click(view.getByTestId('review-comment-tally'));
    const items = view.getAllByTestId('review-comments-item');
    expect(items).toHaveLength(1);
    expect(items[0]).toHaveTextContent('You');
    expect(items[0]).toHaveTextContent('needs a guard here');

    // Clicking the item stages + consumes the row-key jump without errors.
    await fireEvent.click(items[0]!);
    expect(view.getByTestId('review-comments-list')).toBeInTheDocument();

    // Tabs switch back to the file tree.
    await fireEvent.click(view.getByTestId('review-rail-tab-files'));
    expect(view.getByTestId('review-tree-search')).toBeInTheDocument();
  });

  it('applies the extension filter to the diff when the dropdown toggle is checked', async () => {
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getAllByTestId('review-file-header')).toHaveLength(2);
    });

    // Distinct paths, because the sticky overlay can duplicate the top
    // file's header row.
    const headerPaths = () =>
      [...new Set(view.getAllByTestId('review-file-header').map((node) => node.getAttribute('data-path')))];

    await fireEvent.click(view.getByTestId('review-tree-ext-trigger'));
    await fireEvent.click(view.getByRole('menuitem', { name: /^\.ts/ }));
    // Rail-only by default: the diff still shows both files.
    expect(headerPaths()).toHaveLength(2);

    await fireEvent.click(view.getByRole('menuitem', { name: /apply filter to diff/i }));
    expect(headerPaths()).toEqual(['src/app.ts']);
    expect(view.getByTestId('review-diff-stats').textContent).toContain('1 file');

    // Unchecking restores the full diff; the rail filter stays active.
    await fireEvent.click(view.getByRole('menuitem', { name: /apply filter to diff/i }));
    expect(headerPaths()).toHaveLength(2);
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

  it('carries the PR state + branch refs in the toolbar, not a second header stats line', async () => {
    const detail: PRDetail = {
      number: 5,
      title: 'Add feature',
      body: '',
      authorLogin: 'octocat',
      state: 'open',
      draft: false,
      headRefName: 'feature',
      baseRefName: 'main',
      headSHA: 'sha-a',
      url: 'https://github.com/owner/repo/pull/5',
      // A distinctive additions count that appears nowhere else, so a leaked
      // header "+99" would be unambiguous.
      additions: 99,
      deletions: 0,
      changedFiles: 1,
      viewerIsAuthor: false,
      reviewDecision: '',
      latestReviews: [],
      checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
      mergeability: 'clean',
    };
    setBindingMock('SubscribePRUpdates', async (threadId: string, pr: unknown) => ({
      id: 'sub-1',
      threadId,
      pr,
      detail,
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setBindingMock('GetPRDiff', async () => patch());
    setBindingMock('ListPRReviewThreads', async () => []);

    const ctx: PanelContext = {
      ...makeCtx(),
      thread: {
        prRef: JSON.stringify({ Forge: 'github', Namespace: 'owner', Repo: 'repo', Number: 5 }),
        workspacePath: '/repo',
      } as unknown as Thread,
    };
    const view = render(ReviewPane, { ctx });

    // The PR scope option only appears once the thread's prRef resolves.
    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    await fireEvent.change(view.getByTestId('review-scope-select'), { target: { value: 'pr' } });

    await waitFor(() => {
      expect(view.getByTestId('review-pr-header')).toBeInTheDocument();
    });

    // Toolbar now owns the state badge + branch refs.
    const meta = view.getByTestId('review-pr-meta');
    expect(meta.textContent).toContain('open');
    expect(meta.textContent).toContain('main ← feature');

    // The header no longer duplicates the branch refs or the PR-detail +/- stats.
    const header = view.getByTestId('review-pr-header');
    expect(header.querySelector('[data-testid="review-pr-meta"]')).toBeNull();
    expect(header.textContent).not.toContain('main ← feature');
    expect(header.textContent).not.toContain('+99');
  });

  it("disables approve/request-changes on the viewer's own GitHub PR, keeps comment", async () => {
    const detail: PRDetail = {
      number: 5,
      title: 'Add feature',
      body: '',
      authorLogin: 'octocat',
      state: 'open',
      draft: false,
      headRefName: 'feature',
      baseRefName: 'main',
      headSHA: 'sha-a',
      url: 'https://github.com/owner/repo/pull/5',
      additions: 3,
      deletions: 0,
      changedFiles: 1,
      // GitHub rejects approve/request-changes on a PR you authored.
      viewerIsAuthor: true,
      reviewDecision: '',
      latestReviews: [],
      checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
      mergeability: 'clean',
    };
    setBindingMock('SubscribePRUpdates', async (threadId: string, pr: unknown) => ({
      id: 'sub-1',
      threadId,
      pr,
      detail,
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setBindingMock('GetPRDiff', async () => patch());
    setBindingMock('ListPRReviewThreads', async () => []);
    // A pending PR-scope draft makes the send strip (and verdict buttons) render.
    setBindingMock('ListDiffReviewComments', async (_threadId: never, scope: never) =>
      scope === 'pr'
        ? [
            {
              id: 'pr-draft-1',
              threadId: 'thread-1',
              scope: 'pr',
              sourceKey: 'pr:github:owner/repo:5',
              filePath: 'src/app.ts',
              status: 'draft',
              side: 'new',
              newLine: 1,
              selectedText: '',
              body: 'nit',
              createdAt: 1,
              updatedAt: 1,
            },
          ]
        : [],
    );

    const ctx: PanelContext = {
      ...makeCtx(),
      thread: {
        prRef: JSON.stringify({ Forge: 'github', Namespace: 'owner', Repo: 'repo', Number: 5 }),
        workspacePath: '/repo',
      } as unknown as Thread,
    };
    const view = render(ReviewPane, { ctx });

    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    await fireEvent.change(view.getByTestId('review-scope-select'), { target: { value: 'pr' } });

    // Retarget the send from the agent to the PR to reveal the verdict row.
    await waitFor(() => {
      expect(view.getByTestId('review-send-strip')).toBeInTheDocument();
    });
    const targetSelect = view.getByTestId('review-send-strip').querySelector('select')!;
    await fireEvent.change(targetSelect, { target: { value: 'pr' } });

    await waitFor(() => {
      expect(view.getByRole('button', { name: 'Approve' })).toBeInTheDocument();
    });
    expect(view.getByRole('button', { name: 'Approve' })).toBeDisabled();
    expect(view.getByRole('button', { name: 'Request changes' })).toBeDisabled();
    // A comment-only review is always allowed, even on your own PR.
    expect(view.getByRole('button', { name: 'Comment' })).toBeEnabled();
  });

  it('keeps the scope selector enabled while a slow PR load is in flight', async () => {
    setBindingMock('SubscribePRUpdates', async (threadId: string, pr: unknown) => ({
      id: 'sub-1',
      threadId,
      pr,
      detail: null,
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    // The PR diff never resolves — a hung gh/glab call must not lock
    // the user out of switching back to a local scope.
    setBindingMock('GetPRDiff', () => new Promise<string>(() => {}));
    setBindingMock('ListPRReviewThreads', async () => []);

    const ctx: PanelContext = {
      ...makeCtx(),
      thread: {
        prRef: JSON.stringify({ Forge: 'github', Namespace: 'owner', Repo: 'repo', Number: 5 }),
        workspacePath: '/repo',
      } as unknown as Thread,
    };
    const view = render(ReviewPane, { ctx });

    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    const select = view.getByTestId('review-scope-select');
    await fireEvent.change(select, { target: { value: 'pr' } });

    // Still loading (the diff hangs) — the selector must stay usable.
    expect(select).toBeEnabled();
    await fireEvent.change(select, { target: { value: 'workspace' } });
    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
  });
});
