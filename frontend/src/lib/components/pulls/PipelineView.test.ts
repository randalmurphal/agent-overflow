import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import PipelineView from './PipelineView.svelte';
import type { PipelineRowView } from './pipelineRowView';
import { createCISubjectState } from '../../stores/ciSubject.svelte';
import { __resetPipelineCIForTest, attachPipelineCI } from '../../stores/pipelineCI.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { RepoRef } from '../../utils/prReference';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// The Pipelines tab's detail, over a real CI subject state fed by mocked
// pipeline bindings: the title line, the chips, opening a job's log with
// no send, the forge link and the compact Back.

const REPO: RepoRef = { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'app' };

const ROW: PipelineRowView = {
  key: ' pipeline:gitlab:group/app:77',
  id: '77',
  status: 'running',
  name: '',
  ref: 'refs/merge-requests/12/head',
  refKind: 'merge_request',
  mrNumber: 12,
  forge: 'gitlab',
  shortSHA: 'abcdef1',
  title: 'Fix the parser',
  projectName: 'App',
  age: '5m ago',
  url: 'https://gitlab.com/group/app/-/pipelines/77',
  stages: null,
};

function mountView() {
  setBindingMock('SubscribePipeline', async () => ({
    id: 'pipe-1', key: 'gitlab:group/app:pipeline:77', error: '', errorKind: '', reserve: false, resumeAt: '', seq: 1,
    pipeline: {
      id: '77', status: 'failed', url: ROW.url,
      stages: [{ name: 'test', status: 'failed', jobs: [{ id: '20', name: 'unit', status: 'failed', logsAvailable: true }] }],
    },
  }));
  setBindingMock('UnsubscribePipeline', async () => undefined);
  const follows = setBindingMock('SetPipelineFollows', async (_id: string, jobs: string[]) => ({
    logs: Object.fromEntries(jobs.map((job) => [job, { version: 1, totalLines: 1, totalBytes: 8, stableLines: 0, sections: [], available: true, downloading: false, error: '', seq: 2 }])),
  }));
  setBindingMock('GetPRCILogLines', async () => ({ from: 0, lines: ['boom'], totalLines: 1, version: 1 }));
  const hold = attachPipelineCI(HOME_BACKEND, REPO, '77');
  const ci = createCISubjectState(() => ({ kind: 'pipeline', backend: HOME_BACKEND, repo: REPO, pipelineId: '77' }));
  const view = render(PipelineView, { row: ROW, ci });
  return { view, ci, hold, follows };
}

afterEach(() => {
  __resetPipelineCIForTest();
});

describe('<PipelineView>', () => {
  it('names the pipeline by its ref, id, live status and commit, and links it on the forge', async () => {
    const open = setBindingMock('OpenExternalURL', async () => undefined);
    const { view, hold } = mountView();
    const header = view.getByTestId('pipeline-view').querySelector('header')!;
    expect(header).toHaveTextContent('!12');
    expect(header).toHaveTextContent('#77');
    expect(header).toHaveTextContent('abcdef1');
    expect(header).toHaveTextContent('Fix the parser');
    expect(header).toHaveTextContent('5m ago');
    await waitFor(() => expect(view.getByTestId('pipeline-view-status')).toHaveTextContent('failed'));
    await fireEvent.click(view.getByTestId('pipeline-view-open-forge'));
    expect(open).toHaveBeenCalledWith(ROW.url);
    hold.release();
  });

  it('opens a job log from its chip with no send to a chat, and returns to the chips', async () => {
    const { view, ci, hold, follows } = mountView();
    expect(view.getByTestId('pipeline-view-empty')).toBeInTheDocument();
    await waitFor(() => expect(view.getAllByTestId('review-ci-chip')).toHaveLength(1));
    await fireEvent.click(view.getAllByTestId('review-ci-chip')[0]!);
    await fireEvent.click(within(document.body).getAllByTestId('review-ci-job')[0]!);
    await waitFor(() => expect(view.getByTestId('review-ci-log')).toBeInTheDocument());
    expect(follows).toHaveBeenCalledWith('pipe-1', ['20']);
    expect(view.queryByTestId('review-ci-log-send')).toBeNull();
    expect(view.queryByTestId('review-ci-section-send')).toBeNull();
    ci.closeLogView();
    await waitFor(() => expect(view.getByTestId('pipeline-view-empty')).toBeInTheDocument());
    hold.release();
  });

});
