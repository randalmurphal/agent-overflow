import { render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import PipelineRow from './PipelineRow.svelte';
import type { PipelineRowView } from './pipelineRowView';

// One Pipelines tab row's stage summary: a dot per stage once read, and a
// failed read in their place.

const ROW: PipelineRowView = {
  key: ' pipeline:gitlab:group/app:77',
  id: '77',
  status: 'running',
  name: '',
  ref: 'main',
  refKind: 'branch',
  mrNumber: 0,
  forge: 'gitlab',
  shortSHA: 'abcdef1',
  title: 'Fix the parser',
  projectName: 'App',
  age: '5m ago',
  url: 'https://gitlab.com/group/app/-/pipelines/77',
  stages: null,
  stagesError: null,
};

describe('<PipelineRow>', () => {
  it('shows a dot per stage read, and a failed stage read with its reason instead', () => {
    const view = render(PipelineRow, { row: { ...ROW, stages: [{ name: 'build', status: 'success' }, { name: 'test', status: 'failed' }] }, selected: false, onOpen: vi.fn() });
    const stages = view.getByTestId('pipeline-stages');
    expect(stages.querySelectorAll('span.rounded-full')).toHaveLength(2);
    expect(stages.title).toBe('build: success\ntest: failed');
    expect(view.queryByTestId('pipeline-stages-error')).toBeNull();

    view.rerender({ row: { ...ROW, stagesError: 'GitLab did not answer.' }, selected: false, onOpen: vi.fn() });
    const failed = view.getByTestId('pipeline-stages-error');
    expect(failed.textContent).toBe('Stages unavailable');
    expect(failed.title).toBe('GitLab did not answer.');
    expect(view.getByTestId('pipeline-stages').querySelectorAll('span.rounded-full')).toHaveLength(0);
  });
});
