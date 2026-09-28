import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ReviewPane from './ReviewPane.svelte';
import type { PanelContext } from '../../stores/panelContext.svelte';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { resetForTest as resetDiffReviewCommentsForTest } from '../../stores/diffReviewComments.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import type { DiffReviewComment, DiffReviewCommentInput, PRDetail, Thread } from '../../types/models';
import { setBindingMock, setReviewDiffMock } from '../../../test/mocks/bindings-app';
import { applyPRReviewUpdated } from '../../stores/eventsPRReview';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import { resetDiffSpanCacheForTest } from '../../utils/diffSpanCache.svelte';
import { resetSyntaxClassNamesForTest } from '../../utils/syntaxSpans';
import { __seedGitStatusForTest } from '../../stores/gitStatusStore.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';

function makeCtx(): PanelContext {
  return makeStubPanelContext();
}

/** The source pane's workspace has PR #5 open, which is how a review pane
 *  finds its PR. */
function seedSourcePanePR(): void {
  const pane = createThreadPane({ paneId: 'source-pane' });
  pane.replaceThread({
    id: 'thread-1',
    title: 'Review',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  });
  registerPaneForTest('source-pane', pane);
  __seedGitStatusForTest('/repo', {
    isRepo: true,
    branch: 'feature',
    isDefaultBranch: false,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'github',
    openPrUrl: 'https://github.com/owner/repo/pull/5',
    openPrNumber: 5,
  });
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
  resetPanesForTest();
  setBindingMock('GetGitStatus', async () => ({}));
  // Seeding a workspace status runs the shared store's branch reconciliation.
  setBindingMock('UpdateThreadBranch', async () => []);
  setReviewDiffMock('OpenWorkspaceDiff', async () => patch());
  setReviewDiffMock('OpenBranchBaseDiff', async () => '');
  setBindingMock('ListBranchCommits', async () => []);
  setReviewDiffMock('OpenCommitDiff', async () => '');
  setBindingMock('ListPRCommits', async () => []);
  setReviewDiffMock('OpenPRCommitDiff', async () => '');
  setBindingMock('ListThreadEditDiffs', async () => ({ entries: [], turnLabels: [] }));
  setReviewDiffMock('OpenTurnEditsDiff', async () => '');
  setReviewDiffMock('OpenEditDiff', async () => '');
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
    // The +/- prefix renders in its own tinted span, so match on the
    // row's combined text rather than a single text node.
    const blockText = view.getAllByTestId('review-line-block')[0]!.textContent ?? '';
    expect(blockText).toContain('+new');
    expect(blockText).toContain('-old');

    await fireEvent.click(view.getAllByTestId('review-file-header-path')[0]!);
    expect(view.queryAllByTestId('review-line-block')).toHaveLength(0);
    expect(view.getAllByTestId('review-file-header')).toHaveLength(2);

    await fireEvent.click(view.getAllByTestId('review-file-header-path')[0]!);
    expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
  });

  // A draft placeholder has a synthetic thread row and NO thread id: it names
  // a checkout, so the checkout scopes are its whole vocabulary. The Edits
  // scope is the thread's own history, so it must not be offered at all —
  // an offered-then-refused option is a click that does nothing.
  it('offers only the checkout scopes on a draft placeholder', async () => {
    const ctx = makeStubPanelContext({
      threadId: null,
      thread: {
        id: 'draft:source-pane:project-1:chat:1',
        projectId: 'project-1',
        workspacePath: '/repo',
      } as Thread,
    });
    const view = render(ReviewPane, { ctx });

    await waitFor(() => {
      expect(view.getAllByTestId('review-file-header')).toHaveLength(2);
    });
    const select = view.getByTestId('review-scope-select') as HTMLSelectElement;
    expect([...select.options].map((o) => o.value)).toEqual(['workspace', 'branch']);
    expect(select.disabled).toBe(false);
  });

  // The header badge is a toggle, but re-clicking it to close is not
  // discoverable; the toolbar X is the visible way out and routes through
  // the shell-injected close, the same path the badge and chord use.
  it('closes the panel from the toolbar X', async () => {
    const close = vi.fn();
    const view = render(ReviewPane, { ctx: makeStubPanelContext({ close }) });
    await waitFor(() => {
      expect(view.getAllByTestId('review-file-header')).toHaveLength(2);
    });

    await fireEvent.click(view.getByTestId('review-close'));
    expect(close).toHaveBeenCalledTimes(1);
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
    // Distinct paths, because the sticky overlay can duplicate the top
    // file's header row.
    const headerPaths = () =>
      [...new Set(view.getAllByTestId('review-file-header').map((node) => node.getAttribute('data-path')))];
    await waitFor(() => {
      expect(headerPaths()).toEqual(['src/app.ts', 'pnpm-lock.yaml']);
    });

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

  it('toggles hide-whitespace from the toolbar and re-requests the diff', async () => {
    const calls: boolean[] = [];
    setReviewDiffMock('OpenWorkspaceDiff', async (...args: never[]) => {
      const [, ignoreWhitespace] = args as unknown as [string, boolean];
      calls.push(ignoreWhitespace);
      // The -w patch drops the whitespace-only file.
      return ignoreWhitespace ? patch().split('diff --git a/pnpm-lock.yaml')[0]!.trimEnd() : patch();
    });

    const view = render(ReviewPane, { ctx: makeCtx() });
    // Distinct paths: the sticky overlay can duplicate the top file's header.
    const headerPaths = () =>
      [...new Set(view.getAllByTestId('review-file-header').map((node) => node.getAttribute('data-path')))];
    await waitFor(() => {
      expect(headerPaths()).toEqual(['src/app.ts', 'pnpm-lock.yaml']);
    });

    const toggle = view.getByTestId('review-ignore-whitespace-toggle');
    expect(toggle).toHaveAttribute('aria-pressed', 'false');
    expect(calls).toEqual([false]);
    // Disabled mid-load so a second flip can't race the reload it starts.
    await waitFor(() => {
      expect(toggle).toBeEnabled();
    });

    await fireEvent.click(toggle);
    await waitFor(() => {
      expect(headerPaths()).toEqual(['src/app.ts']);
    });
    expect(toggle).toHaveAttribute('aria-pressed', 'true');
    expect(calls).toEqual([false, true]);

    await waitFor(() => {
      expect(toggle).toBeEnabled();
    });
    await fireEvent.click(toggle);
    await waitFor(() => {
      expect(headerPaths()).toEqual(['src/app.ts', 'pnpm-lock.yaml']);
    });
    expect(toggle).toHaveAttribute('aria-pressed', 'false');
    expect(calls).toEqual([false, true, false]);
  });

  it('disables hide-whitespace for a diff source that cannot honor it', async () => {
    setBindingMock('ListThreadEditDiffs', async () => ({
      entries: [{
        itemId: 'item-1',
        payloadId: 'payload-1',
        turnIndex: 0,
        title: 'Edit',
        paths: ['src/app.ts'],
        insertions: 1,
        deletions: 1,
        createdAt: 1,
      }],
      turnLabels: [{ turnIndex: 0, label: 'turn' }],
    }));
    setReviewDiffMock('OpenTurnEditsDiff', async () => patch());
    setBindingMock('VerifyEditDiffs', async () => ({ verified: [] }));

    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getByTestId('review-ignore-whitespace-toggle')).toBeEnabled();
    });

    await fireEvent.change(view.getByTestId('review-scope-select'), { target: { value: 'edits' } });
    await waitFor(() => {
      expect(view.getByTestId('review-ignore-whitespace-toggle')).toBeDisabled();
    });
    expect(view.getByTestId('review-ignore-whitespace-toggle')).toHaveAttribute(
      'title',
      'Hide whitespace changes — not available for this diff',
    );
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

  it('withholds the gutter affordance and disables Send without threads:operate', async () => {
    // Drafting and sending both write under `threads:operate` on the
    // thread's computer. The diff and existing comments still render.
    const draft: DiffReviewComment = {
      id: 'comment-1',
      threadId: 'thread-1',
      scope: 'workspace',
      sourceKey: 'fnv1a:0',
      filePath: 'src/app.ts',
      status: 'draft',
      oldLine: undefined,
      newLine: 1,
      side: 'new',
      selectedText: 'new',
      body: 'Please revisit this line.',
      createdAt: 1,
      updatedAt: 1,
    };
    setBindingMock('ListDiffReviewComments', async () => [draft]);
    const send = setBindingMock('SendDiffReviewComments', vi.fn(async () => ({})));
    await pairViewOnly();
    try {
      const view = render(ReviewPane, { ctx: makeCtx() });
      await waitFor(() => {
        expect(view.getAllByTestId('review-line-block').length).toBeGreaterThan(0);
        expect(view.getByTestId('review-send-strip')).toBeInTheDocument();
      });

      for (const block of view.getAllByTestId('review-line-block')) {
        await fireEvent.mouseOver(block);
      }
      expect(view.queryAllByTestId('review-add-comment')).toHaveLength(0);

      const sendButton = view.getByRole('button', { name: 'Send comments' }) as HTMLButtonElement;
      expect(sendButton.disabled).toBe(true);
      expect(sendButton.title).toBe('Not granted to this device');
      await fireEvent.click(sendButton);
      expect(send).not.toHaveBeenCalled();
    } finally {
      resetToLocalPage();
    }
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
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: 'github:owner/repo:5',
      detail,
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setBindingMock('ListPRReviewThreads', async () => []);

    seedSourcePanePR();
    const ctx = makeCtx();
    const view = render(ReviewPane, { ctx });

    // The PR scope option only appears once the workspace's open PR resolves.
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
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: 'github:owner/repo:5',
      detail,
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setReviewDiffMock('OpenPRDiff', async () => patch());
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

    seedSourcePanePR();
    const ctx = makeCtx();
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

  it('selecting a PR commit hides the PR submit target and verdict row', async () => {
    const commitSHA = 'b'.repeat(40);
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
      viewerIsAuthor: false,
      reviewDecision: '',
      latestReviews: [],
      checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
      mergeability: 'clean',
    };
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: 'github:owner/repo:5',
      detail,
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setBindingMock('ListPRReviewThreads', async () => []);
    setBindingMock('ListPRCommits', async () => [
      { sha: commitSHA, shortSha: 'bbbbbbb', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setReviewDiffMock('OpenPRCommitDiff', async () => patch());
    // Echo the requested sourceKey so a draft exists (and the send strip
    // renders) in both the whole-PR and single-commit views.
    setBindingMock('ListDiffReviewComments', async (_threadId: never, scope: string, sourceKey: string) =>
      scope === 'pr'
        ? [
            {
              id: 'pr-draft-1',
              threadId: 'thread-1',
              scope,
              sourceKey,
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

    seedSourcePanePR();
    const ctx = makeCtx();
    const view = render(ReviewPane, { ctx });

    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    await fireEvent.change(view.getByTestId('review-scope-select'), { target: { value: 'pr' } });

    await waitFor(() => {
      expect(view.getByTestId('review-commit-select')).toBeInTheDocument();
      expect(view.getByTestId('review-send-strip')).toBeInTheDocument();
    });
    const strip = view.getByTestId('review-send-strip');
    await fireEvent.change(strip.querySelector('select')!, { target: { value: 'pr' } });
    await waitFor(() => {
      expect(view.getByRole('button', { name: 'Approve' })).toBeInTheDocument();
    });

    await fireEvent.change(view.getByTestId('review-commit-select'), { target: { value: commitSHA } });
    // The single-commit view can only send to the agent: the target
    // select and the PR verdict row both leave the strip.
    await waitFor(() => {
      expect(view.getByTestId('review-send-strip').querySelector('select')).toBeNull();
    });
    expect(view.queryByRole('button', { name: 'Approve' })).toBeNull();

    // Back to "All commits" restores the PR target (still selected).
    await fireEvent.change(view.getByTestId('review-commit-select'), { target: { value: '' } });
    await waitFor(() => {
      expect(view.getByTestId('review-send-strip').querySelector('select')).not.toBeNull();
      expect(view.getByRole('button', { name: 'Approve' })).toBeInTheDocument();
    });
  });

  it('edits scope shows the turn-grouped selector and switches to a single edit', async () => {
    setBindingMock('ListThreadEditDiffs', async () => ({
      entries: [
        { itemId: 'tool:1', payloadId: 'pl-1', turnIndex: 1, title: 'Edited parser.go', paths: ['parser.go'], insertions: 1, deletions: 0, createdAt: 1 },
        { itemId: 'tool:2a', payloadId: 'pl-2a', turnIndex: 2, title: 'Edited lexer.go', paths: ['lexer.go'], insertions: 2, deletions: 1, createdAt: 2 },
        { itemId: 'tool:2b', payloadId: 'pl-2b', turnIndex: 2, title: 'Edited lexer.go', paths: ['lexer.go'], insertions: 1, deletions: 1, createdAt: 3 },
      ],
      turnLabels: [
        { turnIndex: 1, label: 'fix the parser' },
        { turnIndex: 2, label: 'now the lexer' },
      ],
    }));
    setReviewDiffMock('OpenTurnEditsDiff', async () => patch());
    const payload = setReviewDiffMock('OpenEditDiff', async () => patch());

    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    await fireEvent.change(view.getByTestId('review-scope-select'), { target: { value: 'edits' } });

    await waitFor(() => {
      expect(view.getByTestId('review-edit-select')).toBeInTheDocument();
    });
    const select = view.getByTestId('review-edit-select') as HTMLSelectElement;
    // Default: the latest turn's whole set, grouped under its prompt.
    expect(select.value).toBe('turn:2');
    const groups = [...select.querySelectorAll('optgroup')].map((group) => group.label);
    expect(groups).toEqual(['fix the parser', 'now the lexer']);

    await fireEvent.change(select, { target: { value: 'item:tool:2a' } });
    await waitFor(() => {
      expect(payload).toHaveBeenCalledWith('thread-1', 'pl-2a');
    });
  });

  it('keeps the scope selector enabled while a slow PR load is in flight', async () => {
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: 'github:owner/repo:5',
      detail: null,
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    // The PR diff never resolves — a hung fetch must not lock
    // the user out of switching back to a local scope.
    setReviewDiffMock('OpenPRDiff', () => new Promise<string>(() => {}));
    setBindingMock('ListPRReviewThreads', async () => []);

    seedSourcePanePR();
    const ctx = makeCtx();
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

  it('shows a failing PR poll as state, without disturbing the diff', async () => {
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
      additions: 1,
      deletions: 0,
      changedFiles: 1,
      viewerIsAuthor: false,
      reviewDecision: '',
      latestReviews: [],
      checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
      mergeability: 'clean',
    };
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: 'github:owner/repo:5',
      detail,
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setBindingMock('ListPRReviewThreads', async () => []);

    seedSourcePanePR();
    const ctx = makeCtx();
    const view = render(ReviewPane, { ctx });
    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    await fireEvent.change(view.getByTestId('review-scope-select'), { target: { value: 'pr' } });
    await waitFor(() => {
      expect(view.getByTestId('review-pr-header')).toBeInTheDocument();
    });

    applyPRReviewUpdated({ prKey: 'github:owner/repo:5', error: 'gh api rate limit exceeded' });

    await waitFor(() => {
      expect(view.getByTestId('review-pr-update-error').textContent).toContain('rate limit');
    });
    // Errors are user-facing state, and this one is about the PR data, not
    // the diff: the header and the rendered patch stay put.
    expect(view.getByTestId('review-pr-header')).toBeInTheDocument();
    expect(view.queryByTestId('review-error')).not.toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------
// Syntax colors across rebuilds. The fake highlighter derives each
// row's spans from its text, so a row painted with another row's spans
// fails exactly like a row that went plain.
// ---------------------------------------------------------------------

const FAKE_CLASS_COUNT = 997;
const FAKE_CLASS_NAMES = ['none', ...Array.from({ length: FAKE_CLASS_COUNT }, (_, index) => `c${index + 1}`)];

/** The text a row's spans cover, as DiffLineContent slices it: add and
 * del rows drop their prefix, context rows keep the leading space. Null
 * for rows the backend receives as non-content lines (hunk headers, and
 * conflict markers and folds, which go out as `\` lines). */
function spanBody(row: string): string | null {
  const prefix = row.charAt(0);
  if (prefix === '+' || prefix === '-') return row.slice(1);
  return prefix === ' ' ? row : null;
}

/** One run per character, classed by the character and a hash of the
 * whole body, so no other text renders with these colors. Spaces stay
 * plain. */
function fakeRuns(body: string): number[] {
  let hash = 0;
  for (const char of body) hash = (hash * 31 + char.charCodeAt(0)) % FAKE_CLASS_COUNT;
  const runs: number[] = [];
  for (const char of body) {
    runs.push(1, char === ' ' ? 0 : 1 + ((char.charCodeAt(0) * 31 + hash) % FAKE_CLASS_COUNT));
  }
  return runs;
}

/** Adjacent runs of one class merge, as the renderer merges them. */
function coloredRuns(parts: { cls: string; text: string }[]): string[] {
  const merged: { cls: string; text: string }[] = [];
  for (const part of parts) {
    const last = merged.at(-1);
    if (last && last.cls === part.cls) last.text += part.text;
    else merged.push({ ...part });
  }
  return merged.filter((run) => run.cls !== '').map((run) => `${run.cls}:${run.text}`);
}

/** The colors a row renders with when painted with its own spans. */
function ownColors(row: string): string[] {
  const body = spanBody(row);
  if (body === null) return [];
  const runs = fakeRuns(body);
  return coloredRuns([...body].map((text, index) => {
    const id = runs[index * 2 + 1];
    return { cls: id === 0 ? '' : `syntax-${FAKE_CLASS_NAMES[id]}`, text };
  }));
}

/** Every rendered code cell (both sides in split view): its text and
 * the colored runs it shows. */
function renderedRows(container: HTMLElement): { text: string; colors: string[] }[] {
  const cells = container.querySelectorAll('[data-testid="review-line-block"] span.min-w-0.flex-1');
  return Array.from(cells, (cell) => ({
    text: cell.textContent ?? '',
    colors: coloredRuns(Array.from(cell.children, (child) => ({
      cls: /\bsyntax-\S+/.exec(child.className)?.[0] ?? '',
      text: child.textContent ?? '',
    }))),
  }));
}

function rowTexts(container: HTMLElement): string[] {
  return renderedRows(container).map((row) => row.text);
}

/** Every row shows exactly its own text's colors. */
function expectOwnColors(container: HTMLElement): void {
  const rows = renderedRows(container);
  expect(rows.length).toBeGreaterThan(0);
  for (const row of rows) expect(row.colors, row.text).toEqual(ownColors(row.text));
}

/** The texts of the rows shown colored now. */
function coloredTexts(container: HTMLElement): Set<string> {
  return new Set(renderedRows(container).filter((row) => row.colors.length > 0).map((row) => row.text));
}

/** While fresh spans are held: every row whose text was shown colored
 * keeps exactly that text's colors, and no other row is colored. */
function expectColorsKept(container: HTMLElement, colored: ReadonlySet<string>): void {
  const rows = renderedRows(container);
  let kept = 0;
  for (const row of rows) {
    const was = colored.has(row.text);
    if (was) kept += 1;
    expect(row.colors, row.text).toEqual(was ? ownColors(row.text) : []);
  }
  expect(kept).toBeGreaterThan(0);
}

/** Installs the fake on every highlight RPC. The returned `hold` parks
 * every request made after it until its release is called. */
function installFakeHighlighter(): () => () => void {
  resetDiffSpanCacheForTest();
  resetSyntaxClassNamesForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => FAKE_CLASS_NAMES);
  let held: Promise<void> | null = null;
  const highlight = async (req: unknown) => {
    const gate = held;
    if (gate) await gate;
    const { patch } = req as { patch: string };
    return {
      lines: patch.split('\n').map((row) => {
        const body = spanBody(row);
        return body === null ? {} : { r: fakeRuns(body) };
      }),
      incomplete: false,
    };
  };
  setBindingMock('HighlightPatchWithContext', async (_ws, req) => highlight(req));
  setBindingMock('HighlightPatch', async (req) => highlight(req));
  return () => {
    let release!: () => void;
    held = new Promise<void>((resolve) => { release = resolve; });
    return () => {
      held = null;
      release();
    };
  };
}

function sourceLine(lineNo: number): string {
  return `const row${lineNo} = compute(${lineNo * 7}, "v${lineNo}");`;
}

function prDetailFor(headSHA: string): PRDetail {
  return {
    number: 5,
    title: 'Add feature',
    body: '',
    authorLogin: 'octocat',
    state: 'open',
    draft: false,
    headRefName: 'feature',
    baseRefName: 'main',
    headSHA,
    url: 'https://github.com/owner/repo/pull/5',
    additions: 1,
    deletions: 1,
    changedFiles: 2,
    viewerIsAuthor: false,
    reviewDecision: '',
    latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'conflicts',
  };
}

function pushTo(headSHA: string): void {
  applyPRReviewUpdated({ prKey: 'github:owner/repo:5', detail: prDetailFor(headSHA), threads: [], headSHA });
}

/** Renders the pane on a PR thread and enters pr scope at head `sha-a`. */
async function renderPRScope() {
  setBindingMock('SubscribePRUpdates', async () => ({
    id: 'sub-1',
    prKey: 'github:owner/repo:5',
    detail: prDetailFor('sha-a'),
    threads: [],
    headSHA: 'sha-a',
  }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setBindingMock('ListPRReviewThreads', async () => []);
  setBindingMock('GetPRCIJobs', async () => ({ status: '', stages: [] }));
  seedSourcePanePR();
  const view = render(ReviewPane, {
    ctx: makeCtx(),
  });
  await waitFor(() => {
    expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
  });
  await fireEvent.change(view.getByTestId('review-scope-select'), { target: { value: 'pr' } });
  await waitFor(() => {
    expect(view.getByTestId('review-pr-header')).toBeInTheDocument();
  });
  return view;
}

const CONFLICT_TREE = {
  conflicted: true,
  treeOID: 'tree-1',
  baseLabel: 'origin/main',
  headLabel: 'feature',
  paths: ['main.go'],
  messages: [],
};

/** Five conflict regions with a foldable unchanged run before each:
 * fold ids 0-4, the first hiding lines 1-7 and fold n lines 100n+4 to
 * 100n+9. `theirs` is the first region's head side. */
function conflictContent(theirs: string): string {
  const lines = Array.from({ length: 10 }, (_, index) => sourceLine(index + 1));
  for (let region = 1; region <= 5; region += 1) {
    if (region > 1) lines.push(...Array.from({ length: 12 }, (_, index) => sourceLine(region * 100 - 100 + index + 1)));
    lines.push('<<<<<<< ours', `return ours(${region});`, '=======', region === 1 ? theirs : `return theirs(${region});`, '>>>>>>> theirs');
  }
  return lines.join('\n');
}

/** Opens the conflict view and waits for its colors to land. */
async function openColoredConflicts(view: Awaited<ReturnType<typeof renderPRScope>>): Promise<void> {
  await fireEvent.click(view.getByRole('button', { name: 'View conflicts' }));
  await waitFor(() => {
    expect(view.getAllByTestId('review-conflict-fold')).toHaveLength(5);
    expect(rowTexts(view.container)).toContain('+return theirs(1);');
    expectOwnColors(view.container);
  });
}

describe('<ReviewPane> syntax colors across rebuilds', () => {
  it.each(['stacked', 'split'] as const)('keeps colored lines colored through an expansion burst (%s)', async (mode) => {
    const hold = installFakeHighlighter();
    setReviewDiffMock('OpenWorkspaceDiff', async () => [
      'diff --git a/src/app.ts b/src/app.ts',
      'index 1111111..2222222 100644',
      '--- a/src/app.ts',
      '+++ b/src/app.ts',
      '@@ -150,2 +150,2 @@',
      ` ${sourceLine(150)}`,
      '-let removed = beta();',
      '+let added = gamma();',
    ].join('\n'));
    setBindingMock('GetDiffContextLines', async (_ws, req) => {
      const { startLine, endLine } = req as { startLine: number; endLine: number };
      return {
        lines: Array.from({ length: endLine - startLine + 1 }, (_, index) => sourceLine(startLine + index)),
        startLine,
        eof: false,
        totalLines: 0,
      };
    });
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain('+let added = gamma();');
      expectOwnColors(view.container);
    });
    if (mode === 'split') {
      await fireEvent.click(view.getByTestId('review-split-toggle'));
      expect(view.getByTestId('review-line-block').querySelectorAll('.w-1\\/2').length).toBeGreaterThan(0);
      expectOwnColors(view.container);
    }

    // The first click's colors land.
    await fireEvent.click(view.getByTestId('review-gap-expand-up'));
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain(` ${sourceLine(130)}`);
      expectOwnColors(view.container);
    });
    const colored = coloredTexts(view.container);
    expect(colored).toContain(` ${sourceLine(149)}`);

    // Four more clicks, every fresh result held: each rebuild keeps the
    // colors on screen, the first click's lines included, and only the
    // lines it fetched render plain.
    const release = hold();
    for (const first of [110, 90, 70, 50]) {
      await fireEvent.click(view.getByTestId('review-gap-expand-up'));
      await waitFor(() => {
        expect(rowTexts(view.container)).toContain(` ${sourceLine(first)}`);
      });
      expectColorsKept(view.container, colored);
    }
    release();
    await waitFor(() => {
      expectOwnColors(view.container);
    });
  });

  it('keeps colored lines colored while a conflict fold expands', async () => {
    const hold = installFakeHighlighter();
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setBindingMock('GetPRMergeConflicts', async () => CONFLICT_TREE);
    setBindingMock('GetMergeConflictFile', async () => conflictContent('return theirs(1);'));
    const view = await renderPRScope();
    await openColoredConflicts(view);

    // The first fold's colors land.
    await fireEvent.click(view.getAllByTestId('review-conflict-fold')[0]!);
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain(` ${sourceLine(1)}`);
      expectOwnColors(view.container);
    });
    const colored = coloredTexts(view.container);

    // Four more folds, every fresh result held.
    const release = hold();
    for (const revealed of [104, 204, 304, 404]) {
      await fireEvent.click(view.getAllByTestId('review-conflict-fold')[0]!);
      await waitFor(() => {
        expect(rowTexts(view.container)).toContain(` ${sourceLine(revealed)}`);
      });
      expectColorsKept(view.container, colored);
    }
    expect(view.queryAllByTestId('review-conflict-fold')).toHaveLength(0);
    release();
    await waitFor(() => {
      expectOwnColors(view.container);
    });
  });

  it('keeps colored conflict lines colored while a push recomputes the merge', async () => {
    const hold = installFakeHighlighter();
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setBindingMock('GetPRMergeConflicts', async () => CONFLICT_TREE);
    setBindingMock('GetMergeConflictFile', async () => conflictContent('return theirs(1);'));
    const view = await renderPRScope();
    await openColoredConflicts(view);
    const colored = coloredTexts(view.container);

    const release = hold();
    setBindingMock('GetMergeConflictFile', async () => conflictContent('return pushed(1);'));
    pushTo('sha-b');
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain('+return pushed(1);');
    });
    expectColorsKept(view.container, colored);
    release();
    await waitFor(() => {
      expectOwnColors(view.container);
    });
  });

  it('keeps colored lines colored while a reload after a push re-highlights', async () => {
    const hold = installFakeHighlighter();
    const diffAt = (added: string) => [
      'diff --git a/src/app.ts b/src/app.ts',
      'index 1111111..2222222 100644',
      '--- a/src/app.ts',
      '+++ b/src/app.ts',
      '@@ -10,3 +10,3 @@',
      ` ${sourceLine(10)}`,
      '-let removed = beta();',
      `+${added}`,
      ` ${sourceLine(12)}`,
      'diff --git a/src/util.ts b/src/util.ts',
      'index 3333333..4444444 100644',
      '--- a/src/util.ts',
      '+++ b/src/util.ts',
      '@@ -4,2 +4,2 @@',
      ` ${sourceLine(4)}`,
      '-export const unused = 1;',
      '+export const used = 2;',
    ].join('\n');
    setReviewDiffMock('OpenPRDiff', async () => diffAt('let added = gamma();'));
    const view = await renderPRScope();
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain('+let added = gamma();');
      expectOwnColors(view.container);
    });
    const colored = coloredTexts(view.container);

    // The push changes one line of one file. Primed spans are keyed by
    // the head, so the reload misses every file's exact result.
    setReviewDiffMock('OpenPRDiff', async () => diffAt('let added = delta();'));
    pushTo('sha-b');
    await waitFor(() => {
      expect(view.getByTestId('review-pr-stale')).toBeInTheDocument();
    });
    const release = hold();
    await fireEvent.click(view.getByTestId('review-pr-stale').querySelector('button')!);
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain('+let added = delta();');
    });
    expectColorsKept(view.container, colored);
    release();
    await waitFor(() => {
      expectOwnColors(view.container);
    });
  });

  it('keeps colored lines colored while a refresh that changed one hunk re-highlights', async () => {
    const hold = installFakeHighlighter();
    const diffWith = (secondHunkAdd: string) => [
      'diff --git a/src/app.ts b/src/app.ts',
      'index 1111111..2222222 100644',
      '--- a/src/app.ts',
      '+++ b/src/app.ts',
      '@@ -10,3 +10,3 @@',
      ` ${sourceLine(10)}`,
      '-let removed = beta();',
      '+let added = gamma();',
      ` ${sourceLine(12)}`,
      '@@ -40,3 +40,3 @@',
      ` ${sourceLine(40)}`,
      '-return legacy(total);',
      `+${secondHunkAdd}`,
      ` ${sourceLine(42)}`,
    ].join('\n');
    setReviewDiffMock('OpenWorkspaceDiff', async () => diffWith('return modern(total);'));
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain('+return modern(total);');
      expectOwnColors(view.container);
    });
    const colored = coloredTexts(view.container);

    setReviewDiffMock('OpenWorkspaceDiff', async () => diffWith('return modern(total, cap);'));
    const release = hold();
    await fireEvent.click(view.getByTestId('review-reload'));
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain('+return modern(total, cap);');
    });
    expectColorsKept(view.container, colored);
    release();
    await waitFor(() => {
      expectOwnColors(view.container);
    });
  });

  it('keeps colored lines colored while hide-whitespace re-highlights the diff', async () => {
    const hold = installFakeHighlighter();
    setReviewDiffMock('OpenWorkspaceDiff', async (...args: never[]) => {
      const [, ignoreWhitespace] = args as unknown as [unknown, boolean];
      return [
        'diff --git a/src/app.ts b/src/app.ts',
        'index 1111111..2222222 100644',
        '--- a/src/app.ts',
        '+++ b/src/app.ts',
        '@@ -10,4 +10,4 @@',
        ` ${sourceLine(10)}`,
        ...(ignoreWhitespace
          ? ['   return total;']
          : ['-  return   total;', '+  return total;']),
        '-let removed = beta();',
        '+let added = gamma();',
        ` ${sourceLine(13)}`,
      ].join('\n');
    });
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain('+  return total;');
      expectOwnColors(view.container);
    });
    const colored = coloredTexts(view.container);
    const toggle = view.getByTestId('review-ignore-whitespace-toggle');
    await waitFor(() => {
      expect(toggle).toBeEnabled();
    });

    const release = hold();
    await fireEvent.click(toggle);
    await waitFor(() => {
      expect(rowTexts(view.container)).toContain('   return total;');
    });
    expectColorsKept(view.container, colored);
    release();
    await waitFor(() => {
      expectOwnColors(view.container);
    });
  });
});
