import { fireEvent, waitFor, within } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import { applyPRReviewUpdated } from '../../stores/eventsPRReview';
import { getToasts } from '../../stores/toast.svelte';
import { TransportError } from '../../transport/wsClient';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { PR_KEY, mountReviewPanePR, prDetailFixture, resetReviewPanePR, type ReviewPaneView } from '../../../test/helpers/reviewPanePR';
import type { PRDetail } from '../../types/models';

// The Merge section through the whole pane: the store's actions, the
// pump's frames and the section's rows together, one row of the spec's
// state matrix per test (docs/specs/pull-requests.md).

function blocked(overrides: Partial<PRDetail> = {}): PRDetail {
  return prDetailFixture({
    readiness: {
      state: 'blocked',
      blockers: [
        { kind: 'approvals', text: 'Needs 1 more approval', detail: '1 of 2' },
        { kind: 'checks', text: 'Checks must pass', detail: 'unit · running', url: 'https://ci.example/run/1' },
      ],
      settled: [{ kind: 'conflicts', text: 'No conflicts with main' }],
    },
    ...overrides,
  });
}

function frame(detail: PRDetail, seq: number): void {
  applyPRReviewUpdated({ prKey: PR_KEY, detail, threads: [], events: [], headSHA: detail.headSHA, seq });
}

// Opens the section from the refs line pill, as a reader would.
async function openMergeSection(view: ReviewPaneView): Promise<void> {
  await fireEvent.click(view.getByTestId('review-pr-readiness'));
  await waitFor(() => {
    expect(view.getByTestId('review-pr-merge').getAttribute('data-open')).toBe('true');
  });
}

async function openMergeMenu(view: ReviewPaneView): Promise<HTMLElement> {
  await fireEvent.click(view.getByTestId('review-merge-trigger'));
  return await waitFor(() => view.getByTestId('review-merge-menu'));
}

beforeEach(() => {
  resetReviewPanePR();
});

describe('<ReviewMergeSection>', () => {
  it('leads the overview with the readiness, each blocker beside its fix, and the settled rows', async () => {
    const view = await mountReviewPanePR(blocked());
    const section = view.getByTestId('review-pr-merge');
    // The overview's first section, closed like the others until asked.
    expect(view.getByTestId('review-overview').firstElementChild).toBe(section);
    expect(section.getAttribute('data-open')).toBe('false');
    expect(within(section).getByTestId('review-merge-pill')).toHaveTextContent('Blocked · 2');
    // The refs line says the same thing and jumps here.
    const refsPill = view.getByTestId('review-pr-readiness');
    expect(refsPill).toHaveTextContent('Blocked · 2');
    expect(refsPill.getAttribute('data-state')).toBe('blocked');
    await fireEvent.click(refsPill);
    await waitFor(() => {
      expect(section.getAttribute('data-open')).toBe('true');
    });
    const blockers = within(section).getAllByTestId('review-merge-blocker');
    expect(blockers.map((row) => row.getAttribute('data-kind'))).toEqual(['approvals', 'checks']);
    expect(blockers[0]).toHaveTextContent('Needs 1 more approval');
    expect(blockers[0]).toHaveTextContent('1 of 2');
    expect(within(blockers[0]).getByTestId('review-merge-row-approve')).toHaveTextContent('Approve');
    expect(within(blockers[1]).getByTestId('review-merge-row-pipeline')).toHaveTextContent('Open pipeline');
    const settled = within(section).getAllByTestId('review-merge-settled');
    expect(settled).toHaveLength(1);
    expect(settled[0]).toHaveTextContent('No conflicts with main');
    // Blocked, the button's first action is auto-merge and "Merge now"
    // names what it waits for.
    expect(view.getByTestId('review-merge-trigger')).toHaveTextContent('Auto-merge when ready');
    const menu = await openMergeMenu(view);
    const now = within(menu).getByTestId('review-merge-now');
    expect(now.getAttribute('aria-disabled')).toBe('true');
    expect(now).toHaveTextContent('Needs 1 more approval · Checks must pass');
    expect(within(menu).getByTestId('review-merge-auto').getAttribute('aria-disabled')).toBeNull();
  });

  it('approves with the head the pane holds, stays pending until the frame lands, then offers Unapprove', async () => {
    const approve = setBindingMock('ApprovePR', async () => undefined);
    const view = await mountReviewPanePR(blocked());
    await openMergeSection(view);
    const button = view.getByTestId('review-merge-approve');
    expect(button).toHaveTextContent('Approve');
    await fireEvent.click(button);
    expect(approve).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), 'sha-a');
    // The forge accepted; the pump has not answered yet: still pending.
    await waitFor(() => {
      expect(approve).toHaveBeenCalledTimes(1);
    });
    expect(view.getByTestId('review-merge-approve')).toBeDisabled();
    expect(view.getByTestId('review-merge-trigger')).toBeDisabled();
    const approved = blocked({
      merge: { ...blocked().merge, viewerApproved: true },
      readiness: { state: 'blocked', blockers: [{ kind: 'checks', text: 'Checks must pass' }], settled: [{ kind: 'approvals', text: 'Approved by you' }] },
    });
    frame(approved, 2);
    await waitFor(() => {
      expect(view.getByTestId('review-merge-approve')).toHaveTextContent('Unapprove');
    });
    expect(view.getByTestId('review-merge-approve')).toBeEnabled();
    expect(view.getByTestId('review-merge-pill')).toHaveTextContent('Blocked · 1');
    expect(view.getByTestId('review-merge-you-approved')).toBeInTheDocument();
    const unapprove = setBindingMock('UnapprovePR', async () => undefined);
    await fireEvent.click(view.getByTestId('review-merge-approve'));
    await waitFor(() => {
      expect(unapprove).toHaveBeenCalledTimes(1);
    });
  });

  it('shows a refused action as one sentence with details and re-reads the PR', async () => {
    setBindingMock('ApprovePR', async () => {
      throw new TransportError('forge_refused', 'You cannot approve your own pull request.', { detail: { ref: 'abc', method: 'ApprovePR', at: 1, chain: ['You cannot approve your own pull request.', 'forge POST: HTTP 422'] } });
    });
    const view = await mountReviewPanePR(blocked());
    await fireEvent.click(view.getByTestId('review-merge-approve'));
    await waitFor(() => {
      expect(getToasts().at(-1)?.message).toBe('Approve failed: You cannot approve your own pull request.');
    });
    expect(getToasts().at(-1)?.captured).toBeDefined();
    expect(getToasts().at(-1)?.action).toBeUndefined();
    expect(view.getByTestId('review-merge-approve')).toBeEnabled();
    await waitFor(() => {
      expect(getBindingMock('GetPRDetail')).toHaveBeenCalled();
    });
  });

  it('merges with the method and toggles the menu shows, then follows the frame to merged', async () => {
    const merge = setBindingMock('MergePR', async () => ({ merged: true, autoMerge: false, sha: 'm1' }));
    const view = await mountReviewPanePR(prDetailFixture());
    await openMergeSection(view);
    const trigger = view.getByTestId('review-merge-trigger');
    expect(trigger).toHaveTextContent('Merge');
    expect(view.getByTestId('review-pr-readiness')).toHaveTextContent('Ready to merge');
    let menu = await openMergeMenu(view);
    // The project's methods, its default checked; a pick keeps the menu.
    expect(within(menu).getByTestId('review-merge-method-merge')).toHaveTextContent('project default');
    expect(within(menu).getByTestId('review-merge-method-merge')).toHaveTextContent('✓');
    await fireEvent.click(within(menu).getByTestId('review-merge-method-squash'));
    expect(within(menu).getByTestId('review-merge-method-squash')).toHaveTextContent('✓');
    expect(within(menu).getByTestId('review-merge-delete-branch').getAttribute('aria-checked')).toBe('false');
    await fireEvent.click(within(menu).getByTestId('review-merge-delete-branch'));
    expect(within(menu).getByTestId('review-merge-delete-branch').getAttribute('aria-checked')).toBe('true');
    expect(within(menu).getByTestId('review-merge-auto')).toHaveTextContent('already ready');
    await fireEvent.click(within(menu).getByTestId('review-merge-now'));
    expect(merge).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), { method: 'squash', headSHA: 'sha-a', deleteBranch: true, autoMerge: false });
    await waitFor(() => {
      expect(view.queryByTestId('review-merge-menu')).toBeNull();
    });
    expect(trigger).toBeDisabled();
    frame(prDetailFixture({
      state: 'merged',
      mergedAt: '2026-10-20T10:00:00Z',
      mergedBy: 'octocat',
      readiness: { state: 'merged', blockers: [], settled: [] },
      merge: { ...prDetailFixture().merge, viewerCanMerge: false, viewerCanApprove: false },
    }), 2);
    await waitFor(() => {
      expect(view.getByTestId('review-merge-pill')).toHaveTextContent('Merged');
    });
    expect(view.queryByTestId('review-merge-trigger')).toBeNull();
    expect(view.queryByTestId('review-merge-approve')).toBeNull();
    const row = view.getByTestId('review-merge-settled');
    expect(row.getAttribute('data-kind')).toBe('merged');
    expect(row).toHaveTextContent('Merged by octocat');
    menu = view.queryByTestId('review-merge-menu') as HTMLElement;
    expect(menu).toBeNull();
  });

  it('offers Reload when the head moved under the merge', async () => {
    setBindingMock('MergePR', async () => {
      throw new TransportError('forge_stale_head', 'The branch moved after you loaded this PR. Reload and try again.');
    });
    const view = await mountReviewPanePR(prDetailFixture());
    const menu = await openMergeMenu(view);
    await fireEvent.click(within(menu).getByTestId('review-merge-now'));
    await waitFor(() => {
      expect(getToasts().at(-1)?.message).toBe('Merge failed: The branch moved after you loaded this PR. Reload and try again.');
    });
    expect(getToasts().at(-1)?.action?.label).toBe('Reload');
    expect(view.getByTestId('review-merge-trigger')).toBeEnabled();
  });

  it('arms auto-merge from a blocked PR and shows the armed request with its cancel', async () => {
    const merge = setBindingMock('MergePR', async () => ({ merged: false, autoMerge: true }));
    const view = await mountReviewPanePR(blocked());
    await openMergeSection(view);
    const menu = await openMergeMenu(view);
    await fireEvent.click(within(menu).getByTestId('review-merge-auto'));
    expect(merge).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), { method: 'merge', headSHA: 'sha-a', deleteBranch: false, autoMerge: true });
    frame(blocked({
      merge: {
        ...blocked().merge,
        autoMerge: { method: 'squash', byLogin: 'octocat', byName: 'Octo Cat', at: '2026-10-20T10:00:00Z', waitsFor: 'the checks pass', canCancel: true },
      },
    }), 2);
    await waitFor(() => {
      expect(view.getByTestId('review-merge-trigger')).toHaveTextContent('Auto-merge set');
    });
    const armed = view.getByTestId('review-merge-armed');
    expect(armed).toHaveTextContent('Auto-merge set by Octo Cat · Squash and merge');
    expect(armed).toHaveTextContent('Merges once the checks pass');
    const cancel = setBindingMock('CancelPRAutoMerge', async () => undefined);
    await fireEvent.click(within(armed).getByTestId('review-merge-row-cancel-auto'));
    await waitFor(() => {
      expect(cancel).toHaveBeenCalledTimes(1);
    });
    // Pending until the pump answers; the forge still reports it armed.
    expect(view.getByTestId('review-merge-trigger')).toBeDisabled();
    frame(blocked({
      merge: {
        ...blocked().merge,
        autoMerge: { method: 'squash', byLogin: 'octocat', byName: 'Octo Cat', at: '2026-10-20T10:00:00Z', waitsFor: 'the checks pass', canCancel: true },
      },
    }), 3);
    await waitFor(() => {
      expect(view.getByTestId('review-merge-trigger')).toBeEnabled();
    });
    const reopened = await openMergeMenu(view);
    expect(within(reopened).getByTestId('review-merge-cancel-auto')).toBeInTheDocument();
    expect(within(reopened).queryByTestId('review-merge-auto')).toBeNull();
  });

  it('hides the actions a viewer cannot take and says why', async () => {
    const view = await mountReviewPanePR(blocked({
      merge: {
        ...blocked().merge,
        viewerCanMerge: false,
        viewerCanApprove: false,
        cannotMergeReason: 'You do not have write access to owner/repo.',
        cannotApproveReason: 'You cannot approve your own pull request.',
      },
    }));
    expect(view.queryByTestId('review-merge-trigger')).toBeNull();
    expect(view.queryByTestId('review-merge-approve')).toBeNull();
    await fireEvent.click(view.getByTestId('review-pr-readiness'));
    await waitFor(() => {
      expect(view.getByTestId('review-merge-cannot')).toHaveTextContent('You do not have write access to owner/repo.');
    });
    // The approvals blocker row carries no Approve; the reason is the row.
    const approvals = view.getAllByTestId('review-merge-blocker')[0];
    expect(within(approvals).queryByTestId('review-merge-row-approve')).toBeNull();
    expect(view.queryByTestId('review-merge-cannot-approve')).toBeNull();
  });

  it('offers the fixes of the rebase and draft blockers', async () => {
    const update = setBindingMock('UpdatePRBranch', async () => undefined);
    const ready = setBindingMock('MarkPRReady', async () => undefined);
    const view = await mountReviewPanePR(blocked({
      readiness: { state: 'draft', blockers: [{ kind: 'draft', text: 'Still a draft' }, { kind: 'rebase', text: 'Behind main' }], settled: [] },
    }));
    expect(view.getByTestId('review-pr-readiness')).toHaveTextContent('Draft');
    await fireEvent.click(view.getByTestId('review-pr-readiness'));
    const rows = await waitFor(() => view.getAllByTestId('review-merge-blocker'));
    await fireEvent.click(within(rows[0]).getByTestId('review-merge-row-ready'));
    await waitFor(() => {
      expect(ready).toHaveBeenCalledTimes(1);
    });
    // One action at a time: the second fix waits for the first's frame.
    expect(within(rows[1]).getByTestId('review-merge-row-rebase')).toBeDisabled();
    frame(blocked({ readiness: { state: 'blocked', blockers: [{ kind: 'rebase', text: 'Behind main' }], settled: [] } }), 2);
    await waitFor(() => {
      expect(view.getAllByTestId('review-merge-blocker')).toHaveLength(1);
    });
    await fireEvent.click(within(view.getAllByTestId('review-merge-blocker')[0]).getByTestId('review-merge-row-rebase'));
    await waitFor(() => {
      expect(update).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), 'sha-a');
    });
  });

  it('lists forge events in the conversation and counts them, without repeating a verdict', async () => {
    const detail = prDetailFixture({
      latestReviews: [{ authorLogin: 'alice', authorName: 'Alice', state: 'APPROVED', submittedAt: '2026-10-20T09:00:00Z', body: '', commitSHA: 'sha-a' }],
    });
    const view = await mountReviewPanePR(detail, {
      events: [
        { id: 'e1', kind: 'approved', actorLogin: 'alice', actorName: 'Alice', at: '2026-10-20T09:00:10Z' },
        { id: 'e2', kind: 'marked_ready', actorLogin: 'octocat', at: '2026-10-20T08:00:00Z', text: 'marked this pull request as ready for review' },
        { id: 'e3', kind: 'force_pushed', actorLogin: 'octocat', at: '2026-10-20T07:00:00Z', ref: 'abcdef1234567' },
      ],
    });
    const conversation = view.getByTestId('review-pr-conversation');
    expect(within(conversation).getByText('3')).toBeInTheDocument();
    await fireEvent.click(conversation.querySelector('button')!);
    const rows = await waitFor(() => view.getAllByTestId('review-conversation-event'));
    expect(rows.map((row) => row.getAttribute('data-kind'))).toEqual(['marked_ready', 'force_pushed']);
    expect(rows[0]).toHaveTextContent('octocat');
    expect(rows[0]).toHaveTextContent('marked as ready');
    expect(rows[1]).toHaveTextContent('force-pushed');
    expect(rows[1]).toHaveTextContent('abcdef1');
    expect(view.getAllByTestId('review-conversation-verdict')).toHaveLength(1);
  });

  it('shows a forbidden pump failure in the login banner', async () => {
    const view = await mountReviewPanePR(prDetailFixture());
    applyPRReviewUpdated({ prKey: PR_KEY, error: 'Your github.com token lacks the `repo` scope, so the app cannot read owner/repo#5.', errorKind: 'forbidden', seq: 2 });
    await waitFor(() => {
      expect(view.getByTestId('review-pr-setup')).toHaveTextContent('lacks the `repo` scope');
    });
    expect(view.queryByTestId('review-pr-update-error')).toBeNull();
  });
});
