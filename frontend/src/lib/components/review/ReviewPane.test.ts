import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ReviewPane from './ReviewPane.svelte';
import type { PanelContext } from '../../stores/panelContext.svelte';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { resetForTest as resetDiffReviewCommentsForTest } from '../../stores/diffReviewComments.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import type { CIPipeline, DiffReviewComment, DiffReviewCommentInput, PRDetail, ReviewThread, Thread } from '../../types/models';
import { getBindingMock, setBindingMock, setReviewDiffMock } from '../../../test/mocks/bindings-app';
import { applyPRReviewCILog, applyPRReviewCIUpdated, applyPRReviewUpdated } from '../../stores/eventsPRReview';
import { formatTimeOfDay } from '../../utils/format';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import { adoptDiffSpanOwner, evictDiffSpansForThread, resetDiffSpanCacheForTest } from '../../utils/diffSpanCache.svelte';
import { resetSyntaxClassNamesForTest } from '../../utils/syntaxSpans';
import { __seedGitStatusForTest } from '../../stores/gitStatusStore.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';

// The view toggles (whitespace, wrap, split) are checkbox rows of the
// toolbar's "..." menu, which closes on every pick: open it, find the row.
/** Opens a toolbar menu by its trigger and picks the row the matcher
 *  accepts; the pick closes the menu. */
async function pickFromMenu(
  view: { getByTestId: (id: string) => HTMLElement; getAllByTestId: (id: string) => HTMLElement[] },
  triggerId: string,
  optionId: string,
  match: (row: HTMLElement) => boolean = () => true,
): Promise<void> {
  const trigger = view.getByTestId(triggerId);
  await waitFor(() => {
    expect(trigger).toBeEnabled();
  });
  if (trigger.getAttribute('aria-expanded') !== 'true') await fireEvent.click(trigger);
  const row = await waitFor(() => {
    const found = view.getAllByTestId(optionId).find(match);
    if (!found) throw new Error(`no  row matched`);
    return found;
  });
  await fireEvent.click(row);
  await waitFor(() => {
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
  });
}

async function pickScope(
  view: { getByTestId: (id: string) => HTMLElement; getAllByTestId: (id: string) => HTMLElement[] },
  scope: string,
): Promise<void> {
  await pickFromMenu(view, 'review-scope-select', `review-scope-option-${scope}`);
}

async function openViewOption(view: { getByTestId: (id: string) => HTMLElement }, testId: string): Promise<HTMLElement> {
  const trigger = view.getByTestId('review-view-options');
  if (trigger.getAttribute('aria-expanded') !== 'true') await fireEvent.click(trigger);
  return await waitFor(() => view.getByTestId(testId));
}

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
    const trigger = view.getByTestId('review-scope-select');
    expect(trigger).toBeEnabled();
    expect(trigger).toHaveTextContent('Workspace');
    await fireEvent.click(trigger);
    await waitFor(() => {
      expect(view.getByTestId('review-scope-option-workspace')).toBeInTheDocument();
    });
    const options = [...document.querySelectorAll('[data-testid^="review-scope-option-"]')]
      .map((row) => row.getAttribute('data-testid')?.replace('review-scope-option-', ''));
    expect(options).toEqual(['workspace', 'branch']);
    expect(view.getByTestId('review-scope-option-workspace').getAttribute('aria-checked')).toBeNull();
    expect(view.getByTestId('review-scope-option-workspace')).toHaveTextContent('✓');
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

  it('counts drafts in the tree badge and the toolbar tally', async () => {
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
    // The commented file carries a count badge; a draft is not unresolved.
    // The toolbar carries no comment tally: the counts live on the tree,
    // the Conversation header and the title bar's stepper.
    const badge = await waitFor(() => view.getByTestId('review-tree-comment-count'));
    expect(view.queryByTestId('review-comment-tally')).toBeNull();
    expect(badge).toHaveTextContent('1');
    expect(badge.classList.contains('text-warning')).toBe(false);
    expect(view.getByTestId('review-tree-search')).toBeInTheDocument();
  });

  it('PR scope renders the overview row and the tally opens its conversation', async () => {
    const detail: PRDetail = {
      number: 5,
      title: 'Add feature',
      body: 'Why this change exists.',
      authorLogin: 'octocat',
      state: 'open',
      draft: false,
      headRefName: 'feature',
      baseRefName: 'main',
      baseRefSHA: 'base-main',
      headSHA: 'sha-a',
      url: 'https://github.com/owner/repo/pull/5',
      additions: 1,
      deletions: 1,
      changedFiles: 1,
      viewerIsAuthor: false,
      reviewDecision: '',
      latestReviews: [],
      checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
      mergeability: 'clean',
    };
    const threads: ReviewThread[] = [{
      id: 't-1',
      path: 'src/app.ts',
      line: 1,
      side: 'right',
      isResolvable: true,
      isResolved: false,
      isOutdated: false,
      comments: [{ authorLogin: 'alice', authorName: 'Alice Doe', body: '**Guard this.** It can be null.', createdAt: '2026-01-01T00:00:00Z', databaseID: 1 }],
    }];
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: 'github:owner/repo:5',
      detail,
      threads,
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setBindingMock('ListPRReviewThreads', async () => threads);

    seedSourcePanePR();
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    await pickScope(view, 'pr');
    await waitFor(() => {
      expect(view.getByTestId('review-pr-header')).toBeInTheDocument();
      expect(view.getByTestId('review-overview')).toBeInTheDocument();
    });

    // Both sections start folded; the Conversation header carries the
    // count and opens at the thread's card. The toolbar has no tally.
    expect(view.getByTestId('review-pr-description').getAttribute('data-open')).toBe('false');
    expect(view.getByTestId('review-pr-conversation').getAttribute('data-open')).toBe('false');
    expect(view.queryByTestId('review-conversation-thread')).toBeNull();
    expect(view.queryByTestId('review-comment-tally')).toBeNull();
    await fireEvent.click(view.getByTestId('review-pr-conversation').querySelector('button')!);
    await waitFor(() => {
      expect(view.getByTestId('review-pr-conversation').getAttribute('data-open')).toBe('true');
    });
    const card = view.getByTestId('review-conversation-thread');
    expect(card.getAttribute('data-state')).toBe('unresolved');
    expect(card).toHaveTextContent('Alice Doe');
    expect(card).toHaveTextContent('@alice');
    expect(view.getByTestId('review-conversation-open-count')).toHaveTextContent('1 unresolved');

    // The file's badge tints for its unresolved thread.
    expect(view.getByTestId('review-tree-comment-count').classList.contains('text-warning')).toBe(true);
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

    // The toggle is a checkbox row of the view-options menu, which closes
    // on every pick.
    let toggle = await openViewOption(view, 'review-ignore-whitespace-toggle');
    expect(toggle).toHaveAttribute('aria-checked', 'false');
    expect(calls).toEqual([false]);
    // Disabled mid-load so a second flip can't race the reload it starts.
    await waitFor(() => {
      expect(toggle).not.toHaveAttribute('aria-disabled', 'true');
    });

    await fireEvent.click(toggle);
    await waitFor(() => {
      expect(headerPaths()).toEqual(['src/app.ts']);
    });
    expect(calls).toEqual([false, true]);

    toggle = await openViewOption(view, 'review-ignore-whitespace-toggle');
    expect(toggle).toHaveAttribute('aria-checked', 'true');
    await waitFor(() => {
      expect(toggle).not.toHaveAttribute('aria-disabled', 'true');
    });
    await fireEvent.click(toggle);
    await waitFor(() => {
      expect(headerPaths()).toEqual(['src/app.ts', 'pnpm-lock.yaml']);
    });
    expect(calls).toEqual([false, true, false]);
    toggle = await openViewOption(view, 'review-ignore-whitespace-toggle');
    expect(toggle).toHaveAttribute('aria-checked', 'false');
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
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    let toggle = await openViewOption(view, 'review-ignore-whitespace-toggle');
    await waitFor(() => {
      expect(toggle).not.toHaveAttribute('aria-disabled', 'true');
    });
    await fireEvent.keyDown(toggle, { key: 'Escape' });

    await pickScope(view, 'edits');
    await waitFor(async () => {
      toggle = await openViewOption(view, 'review-ignore-whitespace-toggle');
      expect(toggle).toHaveAttribute('aria-disabled', 'true');
    });
    expect(toggle).toHaveAttribute('title', 'Not available for this diff');
  });

  it('switches to split view and back', async () => {
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getAllByTestId('review-line-block')).toHaveLength(1);
    });

    await fireEvent.click(await openViewOption(view, 'review-split-toggle'));
    const block = view.getByTestId('review-line-block');
    // Split view renders the del/add pair side by side on one visual row.
    expect(block.querySelectorAll('.w-1\\/2')).toHaveLength(2);

    await fireEvent.click(await openViewOption(view, 'review-split-toggle'));
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
      baseRefSHA: 'base-main',
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
    await pickScope(view, 'pr');

    await waitFor(() => {
      expect(view.getByTestId('review-pr-header')).toBeInTheDocument();
    });

    // The title bar owns the state pill (author line) and the branch refs
    // (refs line); the toolbar keeps only the diff's own counts.
    const header = view.getByTestId('review-pr-header');
    expect(view.getByTestId('review-pr-state').textContent?.trim()).toBe('open');
    expect(view.getByTestId('review-pr-author-line').contains(view.getByTestId('review-pr-state'))).toBe(true);
    expect(view.getByTestId('review-pr-refs').getAttribute('title')).toBe('main ← feature');
    expect(view.getByTestId('review-pr-refs-line').contains(view.getByTestId('review-pr-refs'))).toBe(true);
    expect(header.textContent).not.toContain('+99');
    expect(view.getByTestId('review-toolbar').textContent).not.toContain('main ← feature');
  });

  it('closing the pane releases its PR hold and its diff read', async () => {
    const detail = {
      number: 5,
      title: 'Add feature',
      headRefName: 'feature',
      baseRefName: 'main',
      baseRefSHA: 'base-main',
      headSHA: 'sha-a',
      url: 'https://github.com/owner/repo/pull/5',
      checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    } as unknown as PRDetail;
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: 'github:owner/repo:5',
      detail,
      threads: [],
      headSHA: 'sha-a',
    }));
    const unsubscribe = setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setBindingMock('ListPRReviewThreads', async () => []);
    // The PR diff is still reading its second chunk when the pane closes.
    const text = patch();
    setBindingMock('OpenPRDiff', async () => ({
      id: 'diff-1',
      chunk: { data: text.slice(0, 40), offset: 0, nextOffset: 40, eof: false },
      headSha: 'sha-a',
    }));
    let unblock!: () => void;
    const held = new Promise<void>((resolve) => { unblock = resolve; });
    const read = setBindingMock('ReadReviewDiff', async (_handle: string, offset: number) => {
      await held;
      return { data: text.slice(offset), offset, nextOffset: text.length, eof: true };
    });
    const release = setBindingMock('ReleaseReviewDiff', async () => undefined);

    seedSourcePanePR();
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    await pickScope(view, 'pr');
    await waitFor(() => {
      expect(read).toHaveBeenCalledWith('diff-1', 40, expect.any(Number));
    });
    expect(unsubscribe).not.toHaveBeenCalled();
    expect(release).not.toHaveBeenCalled();

    view.unmount();

    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
    unblock();
    await waitFor(() => {
      expect(release).toHaveBeenCalledWith('diff-1');
    });
    expect(read).toHaveBeenCalledTimes(1);
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
      baseRefSHA: 'base-main',
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
    await pickScope(view, 'pr');

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
      baseRefSHA: 'base-main',
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
    await pickScope(view, 'pr');

    await waitFor(() => {
      expect(view.getByTestId('review-commit-select')).toBeInTheDocument();
      expect(view.getByTestId('review-send-strip')).toBeInTheDocument();
    });
    const strip = view.getByTestId('review-send-strip');
    await fireEvent.change(strip.querySelector('select')!, { target: { value: 'pr' } });
    await waitFor(() => {
      expect(view.getByRole('button', { name: 'Approve' })).toBeInTheDocument();
    });

    // The trigger reads "All commits" and never carries a subject; the
    // menu row does, with the hash.
    expect(view.getByTestId('review-commit-select')).toHaveTextContent('All commits');
    await waitFor(() => {
      expect(view.getByTestId('review-commit-select')).toBeEnabled();
    });
    await fireEvent.click(view.getByTestId('review-commit-select'));
    const row = await waitFor(() => view.getByTestId('review-commit-option'));
    expect(row).toHaveTextContent('bbbbbbb');
    expect(row).toHaveTextContent('first');
    expect(view.getByTestId('review-commit-position')).toHaveTextContent('1 commit');
    await fireEvent.click(row);
    // The single-commit view can only send to the agent: the target
    // select and the PR verdict row both leave the strip.
    await waitFor(() => {
      expect(view.getByTestId('review-send-strip').querySelector('select')).toBeNull();
    });
    expect(view.queryByRole('button', { name: 'Approve' })).toBeNull();
    // The trigger is the ordinal and hash; the subject, author and body
    // are the commit card leading the diff, in the PR overview's place.
    const trigger = view.getByTestId('review-commit-select');
    expect(trigger).toHaveTextContent('1/1');
    expect(trigger).toHaveTextContent('bbbbbbb');
    expect(trigger).not.toHaveTextContent('first');
    expect(trigger).toHaveAttribute('title', 'bbbbbbb first');
    const card = view.getByTestId('review-commit-card');
    expect(card.getAttribute('data-sha')).toBe(commitSHA);
    expect(card).toHaveTextContent('first');
    expect(card).toHaveTextContent('r');
    expect(view.queryByTestId('review-overview')).toBeNull();
    // No peek back to an overview that is not there.
    expect(view.queryByTestId('review-overview-peek')).toBeNull();
    expect(view.getAllByTestId('review-file-header')[0].compareDocumentPosition(card) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();

    // Back to "All commits" restores the PR target (still selected) and
    // drops the card.
    await pickFromMenu(view, 'review-commit-select', 'review-commit-all');
    await waitFor(() => {
      expect(view.getByTestId('review-send-strip').querySelector('select')).not.toBeNull();
      expect(view.getByRole('button', { name: 'Approve' })).toBeInTheDocument();
    });
    expect(view.queryByTestId('review-commit-card')).toBeNull();
    expect(view.getByTestId('review-commit-select')).toHaveTextContent('All commits');
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
    await pickScope(view, 'edits');

    await waitFor(() => {
      expect(view.getByTestId('review-edit-select')).toBeInTheDocument();
    });
    const trigger = view.getByTestId('review-edit-select');
    // Default: the latest turn's whole set, named by its prompt.
    expect(trigger).toHaveTextContent('now the lexer');
    await waitFor(() => {
      expect(trigger).toBeEnabled();
    });
    await fireEvent.click(trigger);
    await waitFor(() => {
      expect(view.getAllByTestId('review-edit-option')).toHaveLength(5);
    });
    const groups = [...document.querySelectorAll('[data-menu-section-header]')].map((header) => header.textContent?.trim());
    expect(groups).toEqual(['fix the parser', 'now the lexer']);
    const rows = view.getAllByTestId('review-edit-option');
    expect(rows[2]).toHaveTextContent('All edits in turn');
    expect(rows[2]).toHaveTextContent('✓');
    expect(rows[0]).not.toHaveTextContent('✓');
    expect(rows[3]).toHaveTextContent('Edited lexer.go');
    expect(rows[3]).toHaveTextContent('+2 −1');

    await fireEvent.click(rows[3]);
    await waitFor(() => {
      expect(payload).toHaveBeenCalledWith('thread-1', 'pl-2a');
    });
    await waitFor(() => {
      expect(view.getByTestId('review-edit-select')).toHaveTextContent('Edited lexer.go');
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
    await pickScope(view, 'pr');

    // Still loading (the diff hangs) — the selector must stay usable.
    expect(select).toBeEnabled();
    await pickScope(view, 'workspace');
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
      baseRefSHA: 'base-main',
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
    await pickScope(view, 'pr');
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

    // The failure's kind picks the banner. A rate limit is a pause in the
    // warning tone, named for the PR's forge, resuming at a local time.
    const resumeAt = '2026-10-10T12:30:00Z';
    const at = formatTimeOfDay(Date.parse(resumeAt));
    applyPRReviewUpdated({ prKey: 'github:owner/repo:5', error: 'failed (id: 1)', errorKind: 'rate_limited', resumeAt });
    await waitFor(() => {
      expect(view.getByTestId('review-pr-rate-limited').textContent?.trim()).toBe(`GitHub rate limit reached. Updates resume at ${at}.`);
    });
    expect(view.getByTestId('review-pr-rate-limited').className).toContain('text-warning');
    expect(view.queryByTestId('review-pr-update-error')).not.toBeInTheDocument();

    applyPRReviewUpdated({ prKey: 'github:owner/repo:5', error: 'failed (id: 2)', errorKind: 'rate_limited', reserve: true, resumeAt });
    await waitFor(() => {
      expect(view.getByTestId('review-pr-rate-limited').textContent?.trim()).toBe(
        `GitHub rate limit nearly used up. Updates pause until ${at} so your own actions still go through.`,
      );
    });

    // A login to fix: its own message, no "Retrying".
    applyPRReviewUpdated({ prKey: 'github:owner/repo:5', error: 'GitHub CLI is not logged in. Run gh auth login.', errorKind: 'setup' });
    await waitFor(() => {
      expect(view.getByTestId('review-pr-setup').textContent?.trim()).toBe('GitHub CLI is not logged in. Run gh auth login.');
    });
    expect(view.queryByTestId('review-pr-rate-limited')).not.toBeInTheDocument();
    expect(view.queryByTestId('review-pr-update-error')).not.toBeInTheDocument();

    for (const kind of ['transient', 'forge']) {
      applyPRReviewUpdated({ prKey: 'github:owner/repo:5', error: `failed (${kind})`, errorKind: kind });
      await waitFor(() => {
        expect(view.getByTestId('review-pr-update-error').textContent?.trim()).toBe(`Retrying: failed (${kind})`);
      });
      expect(view.queryByTestId('review-pr-setup')).not.toBeInTheDocument();
    }
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
    baseRefSHA: 'base-main',
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
async function renderPRScope(ci: CIPipeline | null = null) {
  setBindingMock('SubscribePRUpdates', async () => ({
    id: 'sub-1',
    prKey: 'github:owner/repo:5',
    detail: prDetailFor('sha-a'),
    threads: [],
    headSHA: 'sha-a',
    seq: 3,
    ci,
    ciError: '',
  }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setBindingMock('ListPRReviewThreads', async () => []);
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  seedSourcePanePR();
  const view = render(ReviewPane, {
    ctx: makeCtx(),
  });
  await waitFor(() => {
    expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
  });
  await pickScope(view, 'pr');
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

describe('<ReviewPane> load failures', () => {
  it('says why a diff failed instead of "No changed files", and Retry loads it', async () => {
    let fail = true;
    const diff = setReviewDiffMock('OpenWorkspaceDiff', async () => {
      if (fail) throw new Error('git diff exited with code 128: fatal: bad object');
      return patch();
    });
    const view = render(ReviewPane, { ctx: makeCtx() });
    await waitFor(() => {
      expect(view.getByTestId('review-error-message').textContent).toContain('fatal: bad object');
    });
    expect(view.queryByTestId('review-empty')).not.toBeInTheDocument();

    fail = false;
    const calls = diff.mock.calls.length;
    await fireEvent.click(view.getByTestId('review-error-retry'));
    await waitFor(() => {
      expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
    });
    expect(diff.mock.calls.length).toBe(calls + 1);
    expect(view.queryByTestId('review-error')).not.toBeInTheDocument();
  });
});

describe('<ReviewPane> span-cache ownership', () => {
  const DRAFT_ID = 'draft:source-pane:project-1:chat:1';
  const draftCtx = () => makeStubPanelContext({
    threadId: null,
    thread: { id: DRAFT_ID, projectId: 'project-1', workspacePath: '/repo' } as Thread,
  });

  // Thread switch, delete and draft materialization address the cache by
  // row id, so the diff body must file its entries under that id.
  it.each([
    ['a thread row', makeCtx, 'thread-1', null],
    ['a draft placeholder', draftCtx, DRAFT_ID, null],
    ['a materialized draft', draftCtx, 'thread-real', DRAFT_ID],
  ] as const)('evicts the spans of %s by row id', async (_label, ctx, evictId, adoptFrom) => {
    installFakeHighlighter();
    const view = render(ReviewPane, { ctx: ctx() });
    await waitFor(() => {
      expectOwnColors(view.container);
    });
    const highlight = getBindingMock('HighlightPatchWithContext')!;
    const requests = highlight.mock.calls.length;
    expect(requests).toBeGreaterThan(0);

    if (adoptFrom) adoptDiffSpanOwner(adoptFrom, evictId);
    evictDiffSpansForThread(evictId);

    await waitFor(() => {
      expect(highlight.mock.calls.length).toBeGreaterThan(requests);
    });
  });
});

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
      await fireEvent.click(await openViewOption(view, 'review-split-toggle'));
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
    const toggle = await openViewOption(view, 'review-ignore-whitespace-toggle');
    await waitFor(() => {
      expect(toggle).not.toHaveAttribute('aria-disabled', 'true');
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

describe('<ReviewPane> CI', () => {
  const PR = 'github:owner/repo:5';

  function pipelineWith(jobStatus: string, stepStatus: string): CIPipeline {
    return {
      status: jobStatus,
      stages: [{
        name: 'test',
        status: jobStatus,
        jobs: [{
          id: '20',
          name: 'unit',
          status: jobStatus,
          logsAvailable: true,
          steps: [{ number: 1, name: 'build', status: stepStatus }],
        }],
      }],
    };
  }

  it('shows Loading checks until the first pipeline frame, then the chips', async () => {
    const view = await renderPRScope(null);
    expect(view.getByText('Loading checks…')).toBeInTheDocument();
    expect(view.queryByTestId('review-ci-chip')).toBeNull();

    applyPRReviewCIUpdated({ prKey: PR, pipeline: pipelineWith('running', 'running'), seq: 4 });
    await waitFor(() => {
      expect(view.getByTestId('review-ci-chip')).toBeInTheDocument();
    });
    expect(view.queryByText('Loading checks…')).toBeNull();
  });

  it('spins the refresh button while RefreshPRCI runs and keeps the chips', async () => {
    let finish!: () => void;
    const refresh = setBindingMock('RefreshPRCI', () => new Promise<void>((resolve) => {
      finish = resolve;
    }));
    const view = await renderPRScope(pipelineWith('running', 'running'));
    const button = view.getByRole('button', { name: 'Refresh CI status' });
    expect(button).not.toBeDisabled();

    await fireEvent.click(button);
    expect(refresh).toHaveBeenCalledWith('sub-1');
    await waitFor(() => {
      expect(button).toBeDisabled();
    });
    expect(view.getByTestId('review-ci-chip')).toBeInTheDocument();
    expect(view.queryByText('Loading checks…')).toBeNull();

    finish();
    await waitFor(() => {
      expect(button).not.toBeDisabled();
    });
  });

  it('follows an opened job: the pending line while the log is unavailable, live steps, then the log in rows', async () => {
    const view = await renderPRScope(pipelineWith('running', 'running'));
    const step = (status: string) => ({ key: 'step:1', name: 'build', status, start: 0, end: 0, lines: 0, bytes: 0 });
    const follows = setBindingMock('SetPRCILogFollows', async () => ({
      logs: {
        20: {
          version: 0, totalLines: 0, totalBytes: 0, stableLines: 0, sections: [step('running')],
          available: false, downloading: false, error: '', seq: 5,
        },
      },
    }));
    const reads = setBindingMock('GetPRCILogLines', async (_pr: unknown, _job: string, from: number) => ({
      from, lines: ['error: boom'].slice(from), totalLines: 1, version: 1,
    }));

    await fireEvent.click(view.getByTestId('review-ci-chip'));
    await fireEvent.click(await view.findByTestId('review-ci-job'));
    await waitFor(() => {
      expect(view.getByTestId('review-ci-log-pending')).toHaveTextContent('The log is available when the job completes.');
    });
    expect(follows).toHaveBeenCalledWith('sub-1', ['20']);
    expect(view.queryByTestId('review-ci-log-empty')).toBeNull();
    const stepRow = () => view.getAllByTestId('review-ci-section').find((row) => row.dataset.key === 'step:1');
    expect(stepRow()?.dataset.status).toBe('running');

    // The pump's next tick sends the rows its new pipeline gives.
    applyPRReviewCIUpdated({ prKey: PR, pipeline: pipelineWith('failed', 'failed'), seq: 6 });
    applyPRReviewCILog({
      prKey: PR, jobId: '20', seq: 7, version: 0, totalLines: 0, totalBytes: 0, stableLines: 0,
      sections: [step('failed')], available: false, downloading: false, error: '',
    });
    await waitFor(() => {
      expect(stepRow()?.dataset.status).toBe('failed');
    });
    // Completed, and the forge has not published the log yet.
    expect(view.getByTestId('review-ci-log-pending')).toHaveTextContent('The job finished; the forge has not published its log yet.');
    expect(reads).not.toHaveBeenCalled();

    const jobRow = { key: 'job', name: 'unit', status: 'failed', start: 0, end: 1, lines: 1, bytes: 12 };
    applyPRReviewCILog({
      prKey: PR, jobId: '20', seq: 8, version: 1, totalLines: 1, totalBytes: 12, stableLines: 0,
      sections: [jobRow, step('failed')], available: true, downloading: false, error: '',
    });
    // The step has no start time to split by: the lines are the job's
    // row, collapsed until the reader opens it, and nothing is read
    // until then.
    const jobSection = () => view.getAllByTestId('review-ci-section').find((row) => row.dataset.key === 'job');
    await waitFor(() => {
      expect(jobSection()?.dataset.open).toBe('false');
    });
    expect(view.queryByTestId('review-ci-log-pending')).toBeNull();
    expect(reads).not.toHaveBeenCalled();
    await fireEvent.click(within(jobSection()!).getByTestId('review-ci-section-toggle'));
    await waitFor(() => {
      expect(view.getByTestId('review-ci-log-scroll')).toHaveTextContent('error: boom');
    });
    expect(reads).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), '20', 0, 1);
    expect(jobSection()?.dataset.open).toBe('true');

    await fireEvent.click(view.getByRole('button', { name: 'Back' }));
    await waitFor(() => {
      expect(follows).toHaveBeenLastCalledWith('sub-1', []);
    });

    // Opening the log again starts collapsed.
    await fireEvent.click(view.getByTestId('review-ci-chip'));
    await fireEvent.click(await view.findByTestId('review-ci-job'));
    applyPRReviewCILog({
      prKey: PR, jobId: '20', seq: 9, version: 1, totalLines: 1, totalBytes: 12, stableLines: 0,
      sections: [jobRow, step('failed')], available: true, downloading: false, error: '',
    });
    await waitFor(() => {
      expect(jobSection()?.dataset.open).toBe('false');
    });
  });
});
