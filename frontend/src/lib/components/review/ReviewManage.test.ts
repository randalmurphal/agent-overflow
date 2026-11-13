import { fireEvent, waitFor, within } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import { applyPRReviewUpdated } from '../../stores/eventsPRReview';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { PR_KEY, mountReviewPanePR, prDetailFixture, resetReviewPanePR, type ReviewPaneView } from '../../../test/helpers/reviewPanePR';
import type { PRDetail, PRManage, PRManageCan } from '../../types/models';

// The PR management controls through the whole pane (title bar, Details,
// Description, the Merge section): which controls show for which flags
// and states, the editors' dismissal and key rules while a write is
// pending, and what a merge sends with the commit message editor.

const NONE: PRManageCan = {
  editTitle: false,
  editDescription: false,
  reviewers: false,
  assignees: false,
  labels: false,
  close: false,
  reopen: false,
  convertToDraft: false,
  rerequest: false,
  dismiss: false,
  bypass: false,
  reviewerTeams: false,
};
const ALL: PRManageCan = Object.fromEntries(Object.keys(NONE).map((key) => [key, true])) as unknown as PRManageCan;

function manage(can: Partial<PRManageCan>, extra: Partial<PRManage> = {}): PRManage {
  return {
    reviewers: [
      { login: 'alee', name: 'Al Lee', state: 'PENDING' },
      { login: 'kchen', name: 'Kim Chen', state: 'COMMENTED' },
    ],
    assignees: [{ login: 'jdoe', name: 'Jane Doe' }],
    labels: [{ name: 'bug', color: '#d73a4a', id: 'LA_1' }],
    can: { ...NONE, ...can },
    ...extra,
  };
}

function detail(can: Partial<PRManageCan>, overrides: Partial<PRDetail> = {}, extra: Partial<PRManage> = {}): PRDetail {
  return prDetailFixture({ body: 'The body.', manage: manage(can, extra), ...overrides });
}

function frame(next: PRDetail, seq: number): void {
  applyPRReviewUpdated({ prKey: PR_KEY, detail: next, threads: [], events: [], headSHA: next.headSHA, seq });
}

/** Opens the Details section, collapsed by default. */
async function openDetails(view: ReviewPaneView): Promise<HTMLElement> {
  const section = view.getByTestId('review-pr-details');
  if (section.getAttribute('data-open') !== 'true') await fireEvent.click(section.querySelector('button[aria-expanded]')!);
  await waitFor(() => expect(section.getAttribute('data-open')).toBe('true'));
  return section;
}

function controls(view: ReviewPaneView): string[] {
  const ids = [
    'review-title-edit',
    'review-pr-state-menu',
    'review-description-edit',
    'review-details-edit-reviewers',
    'review-details-edit-assignees',
    'review-details-edit-labels',
    'review-details-rerequest',
  ];
  return ids.filter((id) => view.queryAllByTestId(id).length > 0);
}

beforeEach(() => {
  resetReviewPanePR();
});

describe('which management controls show', () => {
  it('shows none without the flags, and every one with them', async () => {
    let view = await mountReviewPanePR(detail({}));
    await openDetails(view);
    expect(controls(view)).toEqual([]);
    view.unmount();
    resetReviewPanePR();
    view = await mountReviewPanePR(detail(ALL));
    await openDetails(view);
    expect(controls(view)).toEqual([
      'review-title-edit',
      'review-pr-state-menu',
      'review-description-edit',
      'review-details-edit-reviewers',
      'review-details-edit-assignees',
      'review-details-edit-labels',
      'review-details-rerequest',
    ]);
    // Only the settled review has a re-request.
    expect(within(view.getByTestId('review-pr-details')).getAllByTestId('review-details-rerequest')).toHaveLength(1);
  });

  it('decides each row on its own flag', async () => {
    const view = await mountReviewPanePR(detail({ labels: true }));
    await openDetails(view);
    expect(controls(view)).toEqual(['review-details-edit-labels']);
  });

  it('has no menu on a merged PR and nothing at all from a backend without management', async () => {
    let view = await mountReviewPanePR(detail(ALL, { state: 'merged', readiness: { state: 'merged', blockers: [], settled: [] } }));
    expect(view.queryByTestId('review-pr-state-menu')).toBeNull();
    view.unmount();
    resetReviewPanePR();
    view = await mountReviewPanePR(prDetailFixture({ body: 'The body.' }));
    expect(view.queryByTestId('review-pr-details')).toBeNull();
    expect(controls(view)).toEqual([]);
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    const menu = await waitFor(() => view.getByTestId('review-merge-menu'));
    expect(within(menu).queryByTestId('review-merge-edit-message')).toBeNull();
  });

  it('never activates a state item on a held key, and keeps navigating on a held arrow', async () => {
    const setState = setBindingMock('SetPRState', async () => ({ seq: 0 }));
    const view = await mountReviewPanePR(detail({ close: true, convertToDraft: true }));
    await fireEvent.click(view.getByTestId('review-pr-state-menu'));
    const items = await waitFor(() => within(view.getByTestId('review-pr-state-items')).getAllByRole('menuitem'));
    items[0].focus();
    await fireEvent.keyDown(items[0], { key: 'Enter', repeat: true });
    await fireEvent.keyDown(items[0], { key: ' ', repeat: true });
    expect(setState).not.toHaveBeenCalled();
    await fireEvent.keyDown(items[0], { key: 'ArrowDown', repeat: true });
    expect(document.activeElement).toBe(items[1]);
    await fireEvent.keyDown(items[1], { key: 'Enter' });
    await waitFor(() => expect(setState).toHaveBeenCalledWith(expect.anything(), 'close'));
  });

  it('offers the state items the flags and state allow', async () => {
    const view = await mountReviewPanePR(detail({ close: true, convertToDraft: true }));
    await fireEvent.click(view.getByTestId('review-pr-state-menu'));
    const items = await waitFor(() => view.getByTestId('review-pr-state-items'));
    expect(within(items).getAllByRole('menuitem').map((item) => item.textContent?.trim())).toEqual(['Convert to draft', 'Close']);
  });

  it('opens Details collapsed, summing the rows up in its header, and shows a failed read as its sentence', async () => {
    let view = await mountReviewPanePR(detail(ALL));
    let section = view.getByTestId('review-pr-details');
    expect(section.getAttribute('data-open')).toBe('false');
    expect(within(section).getByTestId('review-details-summary')).toHaveTextContent('2 reviewers · 1 assignee · 1 label');
    view.unmount();
    resetReviewPanePR();
    view = await mountReviewPanePR(detail(ALL, {}, { reviewers: [], assignees: [], labels: [] }));
    section = view.getByTestId('review-pr-details');
    expect(section.getAttribute('data-open')).toBe('false');
    expect(within(section).queryByTestId('review-details-summary')).toBeNull();
    view.unmount();
    resetReviewPanePR();
    view = await mountReviewPanePR(detail(NONE, {}, { reviewers: [], assignees: [], labels: [], unavailable: 'GitLab did not answer the management read.' }));
    section = view.getByTestId('review-pr-details');
    expect(section.getAttribute('data-open')).toBe('false');
    expect(within(section).getByTestId('review-details-summary')).toHaveTextContent('unavailable');
    await fireEvent.click(section.querySelector('button[aria-expanded]')!);
    expect(within(section).getByTestId('review-details-unavailable')).toHaveTextContent('GitLab did not answer the management read.');
    expect(within(section).queryByTestId('review-details-rows')).toBeNull();
  });

  it('offers Dismiss per change request and Bypass on the changes-requested row', async () => {
    const blocked = (can: Partial<PRManageCan>) => detail(can, {
      readiness: {
        state: 'blocked',
        blockers: [{ kind: 'changes_requested', text: 'Changes requested', changeRequests: [{ reviewId: 'R1', authorLogin: 'kchen' }, { reviewId: 'R2', authorLogin: 'alee' }], bypassable: true }],
        settled: [],
      },
    });
    let view = await mountReviewPanePR(blocked({ dismiss: true }));
    await fireEvent.click(view.getByTestId('review-pr-readiness'));
    await waitFor(() => expect(view.getAllByTestId('review-merge-row-dismiss')).toHaveLength(2));
    expect(view.queryByTestId('review-merge-row-bypass')).toBeNull();
    view.unmount();
    resetReviewPanePR();
    view = await mountReviewPanePR(blocked({ bypass: true }));
    await fireEvent.click(view.getByTestId('review-pr-readiness'));
    await waitFor(() => expect(view.getByTestId('review-merge-row-bypass')).toBeInTheDocument());
    expect(view.queryByTestId('review-merge-row-dismiss')).toBeNull();
  });
});

describe("where a review action's failure shows", () => {
  const REFUSED = 'GitHub refused this because the login cannot dismiss reviews here.';
  const changeRequests = [{ reviewId: 'R1', authorLogin: 'kchen' }];
  function blocked(can: Partial<PRManageCan>, requests = changeRequests): PRDetail {
    return detail(can, {
      readiness: { state: 'blocked', blockers: [{ kind: 'changes_requested', text: 'Changes requested', changeRequests: requests, bypassable: true }], settled: [] },
    });
  }
  function cleared(can: Partial<PRManageCan>): PRDetail {
    return detail(can, { readiness: { state: 'ready', blockers: [], settled: [] } });
  }
  async function openMerge(view: ReviewPaneView): Promise<HTMLElement> {
    await fireEvent.click(view.getByTestId('review-pr-readiness'));
    return waitFor(() => view.getByTestId('review-pr-merge'));
  }
  function blockerRow(view: ReviewPaneView): HTMLElement {
    return view.getAllByTestId('review-merge-blocker').find((row) => row.dataset.kind === 'changes_requested')!;
  }

  it('shows a refused Bypass on the changes-requested row and gives focus back to Bypass', async () => {
    setBindingMock('BypassPRRequestedChanges', async () => { throw new Error(REFUSED); });
    const view = await mountReviewPanePR(blocked({ bypass: true }));
    await openMerge(view);
    const bypass = await waitFor(() => view.getByTestId('review-merge-row-bypass'));
    bypass.focus();
    await fireEvent.click(bypass);
    await waitFor(() => expect(within(blockerRow(view)).getByTestId('review-merge-change-request-error-message')).toHaveTextContent(REFUSED));
    await waitFor(() => expect(document.activeElement).toBe(view.getByTestId('review-merge-row-bypass')));
  });

  it('shows a refused Dismiss on the changes-requested row, with the popover closed and focus on its Dismiss', async () => {
    setBindingMock('DismissPRReview', async () => { throw new Error(REFUSED); });
    const view = await mountReviewPanePR(blocked({ dismiss: true }));
    await openMerge(view);
    await fireEvent.click(await waitFor(() => view.getByTestId('review-merge-row-dismiss')));
    const reason = await waitFor(() => view.getByTestId('review-dismiss-reason'));
    await fireEvent.input(reason, { target: { value: 'Addressed' } });
    await fireEvent.click(view.getByTestId('review-dismiss-confirm'));
    await waitFor(() => expect(within(blockerRow(view)).getByTestId('review-merge-change-request-error-message')).toHaveTextContent(REFUSED));
    expect(view.queryByTestId('review-dismiss-popover')).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(view.getByTestId('review-merge-row-dismiss')));
  });

  it("gives focus to the Merge header once a dismissal's answer takes its Dismiss away", async () => {
    setBindingMock('DismissPRReview', async () => ({ seq: 2 }));
    const view = await mountReviewPanePR(blocked({ dismiss: true }));
    const section = await openMerge(view);
    await fireEvent.click(await waitFor(() => view.getByTestId('review-merge-row-dismiss')));
    await fireEvent.input(await waitFor(() => view.getByTestId('review-dismiss-reason')), { target: { value: 'Addressed' } });
    await fireEvent.click(view.getByTestId('review-dismiss-confirm'));
    await waitFor(() => expect(view.getByTestId('review-dismiss-confirm')).toHaveAttribute('aria-busy', 'true'));
    frame(cleared({ dismiss: true }), 2);
    await waitFor(() => expect(view.queryByTestId('review-merge-row-dismiss')).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(section.querySelector('button[aria-expanded]')));
  });

  it("gives focus to the Merge header once a Bypass's answer takes its button away", async () => {
    setBindingMock('BypassPRRequestedChanges', async () => ({ seq: 2 }));
    const view = await mountReviewPanePR(blocked({ bypass: true }));
    const section = await openMerge(view);
    const bypass = await waitFor(() => view.getByTestId('review-merge-row-bypass'));
    bypass.focus();
    await fireEvent.click(bypass);
    await waitFor(() => expect(view.getByTestId('review-merge-row-bypass')).toHaveAttribute('aria-busy', 'true'));
    frame(cleared({ bypass: true }), 2);
    await waitFor(() => expect(view.queryByTestId('review-merge-row-bypass')).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(section.querySelector('button[aria-expanded]')));
  });

  it('shows a refused re-request on the Reviewers row, not as a toast', async () => {
    setBindingMock('RerequestPRReview', async () => { throw new Error(REFUSED); });
    const view = await mountReviewPanePR(detail(ALL));
    const details = await openDetails(view);
    await fireEvent.click(view.getByTestId('review-details-rerequest'));
    await waitFor(() => expect(within(details).getByTestId('review-details-error-reviewers-message')).toHaveTextContent(REFUSED));
    expect(view.queryByText(/Re-requesting the review failed/)).toBeNull();
  });
});

describe('the editors while a write is pending', () => {
  it('saves the title on Enter but never on a held key, and Escape does nothing while it saves', async () => {
    const edit = setBindingMock('EditPR', async () => ({ seq: 2 }));
    const view = await mountReviewPanePR(detail(ALL));
    await fireEvent.click(view.getByTestId('review-title-edit'));
    const input = view.getByTestId('review-title-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Renamed' } });
    await fireEvent.keyDown(input, { key: 'Enter', repeat: true });
    expect(edit).not.toHaveBeenCalled();
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(edit).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), { title: 'Renamed' }));
    // While it saves, Escape is not taken: it passes on, as in the
    // comment editors.
    expect(await fireEvent.keyDown(input, { key: 'Escape' })).toBe(true);
    expect(view.getByTestId('review-title-input')).toBeInTheDocument();
    expect(view.getByTestId('review-title-cancel')).toBeDisabled();
    expect(view.getByTestId('review-title-cancel').getAttribute('title')).toBe('Still saving this change.');
    frame(detail(ALL, { title: 'Renamed' }), 2);
    await waitFor(() => expect(view.queryByTestId('review-title-input')).toBeNull());
    expect(view.getByTestId('review-pr-title')).toHaveTextContent('Renamed');
  });

  it('keeps the description editor while it saves, and closes it on the answer', async () => {
    const edit = setBindingMock('EditPR', async () => ({ seq: 2 }));
    const view = await mountReviewPanePR(detail(ALL));
    await fireEvent.click(view.getByTestId('review-description-edit'));
    const input = await waitFor(() => view.getByTestId('review-description-input'));
    await fireEvent.input(input, { target: { value: 'New body' } });
    await fireEvent.keyDown(input, { key: 'Enter', ctrlKey: true, repeat: true });
    expect(edit).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('review-description-save'));
    await waitFor(() => expect(edit).toHaveBeenCalledWith(expect.anything(), { description: 'New body', expectedDescription: 'The body.' }));
    expect(view.getByTestId('review-description-cancel')).toBeDisabled();
    expect(view.getByTestId('review-description-cancel').getAttribute('title')).toBe('Still saving this change.');
    expect(await fireEvent.keyDown(input, { key: 'Escape' })).toBe(true);
    expect(view.getByTestId('review-description-editor')).toBeInTheDocument();
    frame(detail(ALL, { body: 'New body' }), 2);
    await waitFor(() => expect(view.queryByTestId('review-description-editor')).toBeNull());
  });
});

describe('the merge commit message', () => {
  async function openEditor(view: ReviewPaneView): Promise<void> {
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    const menu = await waitFor(() => view.getByTestId('review-merge-menu'));
    await fireEvent.click(within(menu).getByTestId('review-merge-edit-message'));
    await waitFor(() => expect(view.getByTestId('review-merge-message-title')).toHaveValue('Default title'));
  }

  async function mergeNow(view: ReviewPaneView): Promise<void> {
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    const menu = await waitFor(() => view.getByTestId('review-merge-menu'));
    await fireEvent.click(within(menu).getByTestId('review-merge-now'));
  }

  it('sends nothing while the default is untouched, and the edited message once edited', async () => {
    setBindingMock('GetPRMergeMessage', async () => ({ title: 'Default title', body: 'Default body' }));
    const merge = setBindingMock('MergePR', async () => ({ seq: 0, outcome: { merged: true, autoMerge: false } }));
    const view = await mountReviewPanePR(detail(ALL));
    await openEditor(view);
    await mergeNow(view);
    await waitFor(() => expect(merge).toHaveBeenCalledTimes(1));
    expect(merge.mock.calls[0][1]).not.toHaveProperty('commitMessage');
    await fireEvent.input(view.getByTestId('review-merge-message-title'), { target: { value: 'Mine' } });
    expect(view.getByTestId('review-merge-message-state')).toHaveTextContent('edited');
    await mergeNow(view);
    await waitFor(() => expect(merge).toHaveBeenCalledTimes(2));
    expect(merge.mock.calls[1][1]).toEqual(expect.objectContaining({ commitMessage: { title: 'Mine', body: 'Default body' } }));
  });

  it('takes focus once the default has loaded, unless the reader moved on', async () => {
    let answer!: (value: { title: string; body: string }) => void;
    setBindingMock('GetPRMergeMessage', () => new Promise((resolve) => { answer = resolve; }));
    let view = await mountReviewPanePR(detail(ALL));
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    let menu = await waitFor(() => view.getByTestId('review-merge-menu'));
    await fireEvent.click(within(menu).getByTestId('review-merge-edit-message'));
    const title = await waitFor(() => view.getByTestId('review-merge-message-title'));
    expect(title).toBeDisabled();
    answer({ title: 'Default title', body: '' });
    await waitFor(() => expect(title).toBeEnabled());
    await waitFor(() => expect(document.activeElement).toBe(title));
    view.unmount();
    resetReviewPanePR();
    setBindingMock('GetPRMergeMessage', () => new Promise((resolve) => { answer = resolve; }));
    view = await mountReviewPanePR(detail(ALL));
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    menu = await waitFor(() => view.getByTestId('review-merge-menu'));
    await fireEvent.click(within(menu).getByTestId('review-merge-edit-message'));
    const elsewhere = view.getByTestId('review-merge-approve');
    elsewhere.focus();
    answer({ title: 'Default title', body: '' });
    await waitFor(() => expect(view.getByTestId('review-merge-message-title')).toBeEnabled());
    expect(document.activeElement).toBe(elsewhere);
  });

  it('merges with no message after a failed default read', async () => {
    setBindingMock('GetPRMergeMessage', async () => { throw new Error('GitHub did not answer. Try again in a moment.'); });
    const merge = setBindingMock('MergePR', async () => ({ seq: 0, outcome: { merged: true, autoMerge: false } }));
    const view = await mountReviewPanePR(detail(ALL));
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    const menu = await waitFor(() => view.getByTestId('review-merge-menu'));
    await fireEvent.click(within(menu).getByTestId('review-merge-edit-message'));
    await waitFor(() => expect(view.getByTestId('review-merge-message-error')).toBeInTheDocument());
    await mergeNow(view);
    await waitFor(() => expect(merge).toHaveBeenCalledTimes(1));
    expect(merge.mock.calls[0][1]).toEqual(expect.objectContaining({ autoMerge: false }));
    expect(merge.mock.calls[0][1]).not.toHaveProperty('commitMessage');
  });

  it('arms auto-merge with the edited message', async () => {
    setBindingMock('GetPRMergeMessage', async () => ({ title: 'Default title', body: 'Default body' }));
    const merge = setBindingMock('MergePR', async () => ({ seq: 0, outcome: { merged: false, autoMerge: true } }));
    const view = await mountReviewPanePR(detail(ALL, {
      readiness: { state: 'blocked', blockers: [{ kind: 'checks', text: 'Checks running' }], settled: [] },
    }));
    await openEditor(view);
    await fireEvent.input(view.getByTestId('review-merge-message-title'), { target: { value: 'Mine' } });
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    const menu = await waitFor(() => view.getByTestId('review-merge-menu'));
    await fireEvent.click(within(menu).getByTestId('review-merge-auto'));
    await waitFor(() => expect(merge).toHaveBeenCalledTimes(1));
    expect(merge.mock.calls[0][1]).toEqual(expect.objectContaining({ autoMerge: true, commitMessage: { title: 'Mine', body: 'Default body' } }));
  });

  async function pickAndEdit(view: ReviewPaneView, method: string): Promise<void> {
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    await fireEvent.click(await waitFor(() => view.getByTestId(`review-merge-method-${method}`)));
    if (!view.queryByTestId('review-merge-menu')) await fireEvent.click(view.getByTestId('review-merge-trigger'));
    await fireEvent.click(await waitFor(() => view.getByTestId('review-merge-edit-message')));
    await waitFor(() => expect(view.getByTestId('review-merge-message-title')).toHaveValue(`Default ${method}`));
    await fireEvent.input(view.getByTestId('review-merge-message-title'), { target: { value: 'Mine' } });
  }

  function withMethods(methods: string[]): PRDetail {
    return detail(ALL, { merge: { ...prDetailFixture().merge, methods } });
  }

  it('moves an edited message to the default method when its method goes, and merges with it', async () => {
    setBindingMock('GetPRMergeMessage', async (_pr: unknown, method: string) => ({ title: `Default ${method}`, body: 'Body' }));
    const merge = setBindingMock('MergePR', async () => ({ seq: 0, outcome: { merged: true, autoMerge: false } }));
    const view = await mountReviewPanePR(withMethods(['merge', 'squash']));
    await pickAndEdit(view, 'squash');
    frame(withMethods(['merge']), 2);
    await waitFor(() => expect(view.getByTestId('review-merge-message-notice')).toHaveTextContent('Squash is no longer allowed here. This message will be used for the merge commit.'));
    expect(view.getByTestId('review-merge-message')).toHaveTextContent('Merge commit');
    expect(view.getByTestId('review-merge-message-state')).toHaveTextContent('edited');
    expect(view.getByTestId('review-merge-message-title')).toHaveValue('Mine');
    await mergeNow(view);
    await waitFor(() => expect(merge).toHaveBeenCalledTimes(1));
    expect(merge.mock.calls[0][1]).toEqual(expect.objectContaining({ method: 'merge', commitMessage: { title: 'Mine', body: 'Body' } }));
  });

  it('keeps an edited message on screen when the new method makes no commit, and merges without it', async () => {
    setBindingMock('GetPRMergeMessage', async (_pr: unknown, method: string) => ({ title: `Default ${method}`, body: 'Body' }));
    const merge = setBindingMock('MergePR', async () => ({ seq: 0, outcome: { merged: true, autoMerge: false } }));
    const view = await mountReviewPanePR(withMethods(['squash', 'rebase']));
    await pickAndEdit(view, 'squash');
    frame(withMethods(['rebase']), 2);
    await waitFor(() => expect(view.getByTestId('review-merge-message-notice')).toHaveTextContent('Squash is no longer allowed here. Rebase makes no commit, so this message will not be used.'));
    expect(view.getByTestId('review-merge-message-title')).toHaveValue('Mine');
    await mergeNow(view);
    await waitFor(() => expect(merge).toHaveBeenCalledTimes(1));
    expect(merge.mock.calls[0][1]).toEqual(expect.objectContaining({ method: 'rebase' }));
    expect(merge.mock.calls[0][1]).not.toHaveProperty('commitMessage');
  });

  it('is not offered for a rebase', async () => {
    const view = await mountReviewPanePR(detail(ALL, { merge: { ...prDetailFixture().merge, methods: ['rebase'] } }));
    await fireEvent.click(view.getByTestId('review-merge-trigger'));
    const menu = await waitFor(() => view.getByTestId('review-merge-menu'));
    expect(within(menu).queryByTestId('review-merge-edit-message')).toBeNull();
  });
});
