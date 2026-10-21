// The Pull requests page over mocked bindings: a row's menu acts on its
// own MR and leaves the list on screen; a failed refresh and a failed read
// of the threads on an MR reach the page; and the header's Back is one
// ladder that closes a view open in the detail before the detail.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import PullsPane from './PullsPane.svelte';
import { tick } from 'svelte';
import { applyPullsSnapshot, selectedPR } from '../../stores/pullsPage.svelte';
import { applyPRUpdatedEvent } from '../../stores/prReviewStore.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { stepPullsBack } from '../../stores/pullsCommands.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { TransportError } from '../../transport/wsClient';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { PULLS_PANE, installPullsPaneMocks, pullItem, pullRowKey, pullsProject, pullsSnapshot, resetPullsPane } from '../../../test/helpers/pullsPane';

function mountPane() {
  return render(PullsPane, { paneId: PULLS_PANE });
}

function rowOf(view: ReturnType<typeof mountPane>, number: number): HTMLElement {
  return view.container.querySelector<HTMLElement>(`[data-testid="pull-row"][data-pr-key="${pullRowKey(number)}"]`)!;
}

async function rowsListed(view: ReturnType<typeof mountPane>): Promise<void> {
  await waitFor(() => expect(view.getAllByTestId('pull-row')).toHaveLength(2));
}

function menu(): HTMLElement {
  return document.body.querySelector<HTMLElement>('[data-popover]')!;
}

beforeEach(() => {
  resetPullsPane();
});

afterEach(() => {
  resetPullsPane();
});

describe("a row's menu", () => {
  it('acts on its own MR without opening it, so the list and the anchor stay on compact', async () => {
    setCompactLayoutForTest(true);
    const mocks = installPullsPaneMocks();
    const opened = setBindingMock('OpenPRThread', async () => {
      throw new Error('no worktree here');
    });
    const view = mountPane();
    await rowsListed(view);

    const trigger = within(rowOf(view, 6)).getByTestId('pull-row-menu');
    await fireEvent.click(trigger);

    await waitFor(() => expect(within(menu()).getByTestId('open-with-agent-new-thread')).toHaveTextContent('New thread on feature-6'));
    expect(selectedPR(PULLS_PANE)).toBeNull();
    expect(view.getByTestId('pulls-list-column')).not.toHaveClass('hidden');
    expect(view.queryByTestId('review-pane')).toBeNull();
    expect(trigger.isConnected).toBe(true);
    expect(mocks.threads).toHaveBeenCalledWith(expect.objectContaining({ headRefName: 'feature-6' }));

    await waitFor(() => expect(within(menu()).getByTestId('open-with-agent-new-thread')).not.toHaveAttribute('aria-disabled', 'true'));
    await fireEvent.click(within(menu()).getByTestId('open-with-agent-new-thread'));
    await waitFor(() => expect(opened).toHaveBeenCalledWith(expect.objectContaining({ headRefName: 'feature-6', url: 'https://github.com/owner/repo/pull/6' })));
    expect(selectedPR(PULLS_PANE)).toBeNull();
  });

  it('says a failed read of the threads on the MR, keeps New thread off, and retries', async () => {
    const mocks = installPullsPaneMocks();
    let fail = true;
    mocks.threads.mockImplementation(async () => {
      if (fail) throw new Error('database is locked');
      return [];
    });
    const view = mountPane();
    await rowsListed(view);
    await fireEvent.click(within(rowOf(view, 5)).getByTestId('pull-row-menu'));

    await waitFor(() => expect(within(menu()).getByTestId('open-with-agent-threads-error')).toHaveTextContent('database is locked'));
    expect(within(menu()).queryByTestId('open-with-agent-no-threads')).toBeNull();
    expect(within(menu()).getByTestId('open-with-agent-new-thread')).toHaveAttribute('aria-disabled', 'true');

    fail = false;
    await fireEvent.click(within(menu()).getByTestId('open-with-agent-threads-retry'));
    await waitFor(() => expect(within(menu()).getByTestId('open-with-agent-no-threads')).toBeInTheDocument());
    expect(within(menu()).queryByTestId('open-with-agent-threads-error')).toBeNull();
    expect(within(menu()).getByTestId('open-with-agent-new-thread')).not.toHaveAttribute('aria-disabled', 'true');
  });
});

describe('a failed refresh', () => {
  it('reads under the header with its details until dismissed, and again on the next failure', async () => {
    const mocks = installPullsPaneMocks();
    mocks.refresh.mockImplementation(async () => {
      throw new TransportError('method_error', 'pr list: subscription gone', {
        detail: { ref: 'abc123', method: 'RefreshPRList', at: 1, chain: ['pr list: subscription gone', 'handle 7 released'] },
      });
    });
    const view = mountPane();
    await rowsListed(view);

    await fireEvent.click(view.getByTestId('pulls-refresh'));
    const notice = await waitFor(() => view.getByTestId('pulls-refresh-error'));
    expect(notice).toHaveTextContent('Could not refresh the merge requests');
    await fireEvent.click(within(notice).getByTestId('error-details-toggle'));
    expect(within(notice).getByTestId('error-details')).toHaveTextContent('ref abc123');
    expect(within(notice).getByTestId('error-details')).toHaveTextContent('handle 7 released');

    await fireEvent.click(within(notice).getByTestId('pulls-refresh-error-dismiss'));
    expect(view.queryByTestId('pulls-refresh-error')).toBeNull();

    await fireEvent.click(view.getByTestId('pulls-refresh'));
    await waitFor(() => expect(view.getByTestId('pulls-refresh-error')).toBeInTheDocument());
  });
});

describe('viewed marks', () => {
  it('marks nothing while the Pipelines tab shows, and the review once it shows again', async () => {
    installPullsPaneMocks();
    const mark = setBindingMock('MarkPRViewed', async () => null);
    const view = mountPane();
    await rowsListed(view);
    await fireEvent.click(rowOf(view, 5).querySelector('button')!);
    await waitFor(() => expect(mark).toHaveBeenCalledTimes(1));
    expect(mark).toHaveBeenLastCalledWith(expect.objectContaining({ Number: 5 }), 'sha-5');

    // The MR stays selected under the Pipelines tab; a comment and a push land on it.
    await fireEvent.click(view.getByRole('radio', { name: 'Pipelines' }));
    await waitFor(() => expect(view.queryByTestId('review-pane')).toBeNull());
    applyPullsSnapshot({ ...pullsSnapshot([pullsProject([pullItem(5, { headSHA: 'sha-9', comments: 1 }), pullItem(6)])]), seq: 2 }, HOME_BACKEND);
    applyPRUpdatedEvent({ prKey: 'github:owner/repo:5', detail: null, threads: [], headSHA: 'sha-9', seq: 2 });
    for (let i = 0; i < 8; i += 1) await tick();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(mark).toHaveBeenCalledTimes(1);

    await fireEvent.click(view.getByRole('radio', { name: 'Merge requests' }));
    await waitFor(() => expect(mark).toHaveBeenCalledTimes(2));
    expect(mark).toHaveBeenLastCalledWith(expect.objectContaining({ Number: 5 }), 'sha-9');
  });
});

describe('Back on compact', () => {
  it('closes an open pipeline log before the pipeline, with no Back of its own', async () => {
    setCompactLayoutForTest(true);
    installPullsPaneMocks();
    const view = mountPane();
    await rowsListed(view);
    await fireEvent.click(view.getByRole('radio', { name: 'Pipelines' }));
    const pipelineRow = await waitFor(() => view.getByTestId('pipeline-row'));
    await fireEvent.click(pipelineRow.querySelector('button')!);
    await waitFor(() => expect(view.getAllByTestId('review-ci-chip')).toHaveLength(1));
    await fireEvent.click(view.getAllByTestId('review-ci-chip')[0]!);
    await fireEvent.click(within(document.body).getAllByTestId('review-ci-job')[0]!);
    await waitFor(() => expect(view.getByTestId('review-ci-log')).toBeInTheDocument());

    expect(view.queryByTestId('review-ci-log-back')).toBeNull();
    await fireEvent.click(view.getByTestId('pulls-back'));
    await waitFor(() => expect(view.getByTestId('pipeline-view-empty')).toBeInTheDocument());
    expect(view.getByTestId('pipeline-view')).toBeInTheDocument();

    await fireEvent.click(view.getByTestId('pulls-back'));
    await waitFor(() => expect(view.queryByTestId('pipeline-view')).toBeNull());
    expect(view.getByTestId('pulls-list-column')).not.toHaveClass('hidden');
    expect(stepPullsBack()).toBe(false);
  });

  it('closes the conflict view before the merge request, with no Back of its own', async () => {
    setCompactLayoutForTest(true);
    installPullsPaneMocks();
    const view = mountPane();
    await rowsListed(view);
    await fireEvent.click(rowOf(view, 5).querySelector('button')!);
    const conflicts = await waitFor(() => view.getByRole('button', { name: 'View conflicts' }));
    await fireEvent.click(conflicts);
    await waitFor(() => expect(view.getByTestId('review-conflicts-empty')).toBeInTheDocument());

    expect(view.queryByTestId('review-conflicts-back')).toBeNull();
    await fireEvent.click(view.getByTestId('pulls-back'));
    await waitFor(() => expect(view.queryByTestId('review-conflicts-empty')).toBeNull());
    expect(selectedPR(PULLS_PANE)?.key).toBe(pullRowKey(5));

    await fireEvent.click(view.getByTestId('pulls-back'));
    await waitFor(() => expect(selectedPR(PULLS_PANE)).toBeNull());
  });

  it('leaves the inner Back controls on desktop', async () => {
    installPullsPaneMocks();
    const view = mountPane();
    await rowsListed(view);
    await fireEvent.click(rowOf(view, 5).querySelector('button')!);
    await fireEvent.click(await waitFor(() => view.getByRole('button', { name: 'View conflicts' })));
    await waitFor(() => expect(view.getByTestId('review-conflicts-back')).toBeInTheDocument());
    expect(view.queryByTestId('pulls-back')).toBeNull();
  });
});
