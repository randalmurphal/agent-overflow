// The Pull requests page's list find over mocked bindings: the bar sits at
// the top of the list column, typing narrows the rows and every count the
// tab shows, an empty result says what was searched, Enter focuses the
// first row, a close restores the rows, a hidden selection stays open,
// and a press in the detail makes the detail the region find acts on.

import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import PullsPane from './PullsPane.svelte';
import {
  closePaneFind,
  getPaneFind,
  hasPaneFindProvider,
  openPaneFind,
  paneFindDetailKey,
  paneFindListKey,
  paneFindTarget,
  resetPaneFindForTest,
} from '../../stores/paneFind.svelte';
import { selectPR, selectedPR } from '../../stores/pullsPage.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installPullsPaneMocks, PULLS_PANE, pullItem, pullRowKey, pullsProject, pullsSnapshot, resetPullsPane } from '../../../test/helpers/pullsPane';

const LIST = paneFindListKey(PULLS_PANE);

function mountPane() {
  return render(PullsPane, { paneId: PULLS_PANE });
}

function listedNumbers(view: ReturnType<typeof mountPane>): string[] {
  return view.queryAllByTestId('pull-row').map((row) => row.dataset.prKey!.split(':').pop()!);
}

beforeEach(() => {
  resetPullsPane();
  resetPaneFindForTest();
  installPullsPaneMocks(pullsSnapshot([pullsProject([
    pullItem(5, { title: 'Queue: retry a failed delivery' }),
    pullItem(6, { title: 'Speed up builds' }),
    pullItem(7, { title: 'Retry webhooks', state: 'open', group: 'waiting', reason: null }),
  ])]));
});

afterEach(() => {
  resetPaneFindForTest();
  resetPullsPane();
});

describe('the list find', () => {
  it('filters the rows and every count from the bar above the list, and restores them on close', async () => {
    const view = mountPane();
    await waitFor(() => expect(listedNumbers(view)).toEqual(['6', '5']));
    expect(hasPaneFindProvider(LIST)).toBe(true);
    expect(paneFindTarget(PULLS_PANE)).toBe(LIST);
    const before = view.getAllByTestId('pull-row')[0]!.querySelector('button')!;
    before.focus();
    openPaneFind(LIST, { returnFocus: before });
    await tick();

    const column = view.getByTestId('pulls-list-column');
    const bar = view.getByTestId('pane-find-bar');
    expect(column.firstElementChild).toBe(bar);
    expect(bar.dataset.findMode).toBe('filter');
    const input = view.getByTestId('pane-find-input') as HTMLInputElement;
    expect(input.placeholder).toBe('Find in merge requests');

    await fireEvent.input(input, { target: { value: 'retry' } });
    await waitFor(() => expect(listedNumbers(view)).toEqual(['5']));
    expect(view.getByTestId('pane-find-count')).toHaveTextContent('1 match');
    expect(view.getByTestId('pulls-group-needs_you')).toHaveTextContent('1');
    expect(view.getByTestId('pulls-filter-needs-you-count')).toHaveTextContent('1');

    await fireEvent.input(input, { target: { value: 'nothing like it' } });
    await waitFor(() => expect(view.getByTestId('pulls-empty')).toHaveTextContent('No merge requests match "nothing like it".'));
    expect(view.getByTestId('pane-find-count')).toHaveTextContent('No matches');
    expect(view.queryByTestId('pulls-filter-needs-you-count')).toBeNull();

    await fireEvent.input(input, { target: { value: 'builds' } });
    await waitFor(() => expect(listedNumbers(view)).toEqual(['6']));
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(document.activeElement).toBe(view.getAllByTestId('pull-row')[0]!.querySelector('button')));

    closePaneFind(LIST);
    await waitFor(() => expect(listedNumbers(view)).toEqual(['6', '5']));
    expect(view.queryByTestId('pane-find-bar')).toBeNull();
    expect(view.getByTestId('pulls-filter-needs-you-count')).toHaveTextContent('2');
  });

  it('keeps a selection the filter hides open in the detail, and re-applies the query on a tab switch', async () => {
    setBindingMock('ListForgeProjects', async () => [{ projectId: 'project-1', name: 'Repo', path: '/src/repo', repo: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo' } }]);
    setBindingMock('ListProjectPipelines', async () => ({
      rows: [
        { id: '71', status: 'success', ref: 'main', refKind: 'branch', sha: 'a'.repeat(40), title: 'Speed up builds', createdAt: '2026-10-01T00:00:00Z', url: '' },
        { id: '72', status: 'failed', ref: 'retry-train', refKind: 'branch', sha: 'b'.repeat(40), title: 'Other', createdAt: '2026-10-02T00:00:00Z', url: '' },
      ],
      fetchedAt: '',
    }));
    setBindingMock('ReadPipelineStages', async () => ({}));
    const view = mountPane();
    await waitFor(() => expect(listedNumbers(view)).toEqual(['6', '5']));
    expect(selectPR(PULLS_PANE, pullRowKey(6))).toBe(true);
    openPaneFind(LIST);
    await tick();
    await fireEvent.input(view.getByTestId('pane-find-input'), { target: { value: 'retry' } });
    await waitFor(() => expect(listedNumbers(view)).toEqual(['5']));
    expect(selectedPR(PULLS_PANE)?.key).toBe(pullRowKey(6));
    expect(view.getByTestId('pulls-detail').querySelector('[data-testid="pulls-detail-empty"]')).toBeNull();

    await fireEvent.click(view.getByRole('radio', { name: 'Pipelines' }));
    await waitFor(() => expect(view.getAllByTestId('pipeline-row').map((row) => row.querySelector('[data-testid="pipeline-status"]')!.textContent)).toEqual([expect.stringContaining('#72')]));
    expect((view.getByTestId('pane-find-input') as HTMLInputElement).value).toBe('retry');
    expect(view.getByTestId('pane-find-input')).toHaveAttribute('placeholder', 'Find in pipelines');
    expect(view.getByTestId('pane-find-count')).toHaveTextContent('1 match');

    await fireEvent.click(view.getByRole('radio', { name: 'Merge requests' }));
    await waitFor(() => expect(listedNumbers(view)).toEqual(['5']));
  });

  it('acts on the detail once it is pressed, where it has no find', async () => {
    const view = mountPane();
    await waitFor(() => expect(listedNumbers(view)).toEqual(['6', '5']));
    await fireEvent.pointerDown(view.getByTestId('pulls-detail'));
    expect(paneFindTarget(PULLS_PANE)).toBe(paneFindDetailKey(PULLS_PANE));
    expect(hasPaneFindProvider(paneFindDetailKey(PULLS_PANE))).toBe(false);
    await fireEvent.focusIn(view.getAllByTestId('pull-row')[0]!.querySelector('button')!);
    expect(paneFindTarget(PULLS_PANE)).toBe(LIST);
    expect(getPaneFind(LIST)).toBeNull();
  });
});
