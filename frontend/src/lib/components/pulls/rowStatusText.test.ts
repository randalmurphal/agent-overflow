// A row's CI color carries its status in words for assistive tech: the
// MR row's CI dot (on the reader's own MRs only), the pipeline row's
// status dot, and each stage dot.

import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import PipelineRow from './PipelineRow.svelte';
import PullRow from './PullRow.svelte';
import type { PipelineRowView } from './pipelineRowView';
import type { PullRowView } from './pullRowView';

const PULL: PullRowView = {
  key: 'k5', forge: 'github', number: 5, title: 'Add feature', url: '', state: 'pipeline_failed', group: 'needs_you',
  label: 'Pipeline failed', tone: 'error', projectName: 'Repo', author: 'you', viewerIsAuthor: true, age: '2h', times: '', draft: false,
  crossRepository: false, ci: 'failed', approvals: null, unresolved: null,
};

const PIPELINE: PipelineRowView = {
  key: 'p77', id: '77', status: 'running', name: '', ref: 'main', refKind: 'branch', mrNumber: 0, forge: 'gitlab',
  sha: 'abcdef1', shortSHA: 'abcdef1', title: 'Fix the parser', projectName: 'App', age: '5m ago', times: '', url: '',
  stages: [{ name: 'build', status: 'success' }, { name: 'test', status: 'failed' }], stagesError: null,
};

describe('CI status in words', () => {
  it("names an MR row's CI status on the reader's own MR, and shows none otherwise", () => {
    const view = render(PullRow, { row: PULL, selected: false, onOpen: () => {} });
    expect(view.getByRole('button', { name: /CI failed/ })).toBeInTheDocument();
    view.unmount();
    const quiet = render(PullRow, { row: { ...PULL, ci: '' }, selected: false, onOpen: () => {} });
    expect(quiet.queryByTestId('pull-ci')).toBeNull();
    quiet.unmount();
    const theirs = render(PullRow, { row: { ...PULL, viewerIsAuthor: false, author: 'alice' }, selected: false, onOpen: () => {} });
    expect(theirs.queryByTestId('pull-ci')).toBeNull();
  });

  it("names a pipeline row's status and each stage's", () => {
    const view = render(PipelineRow, { row: PIPELINE, selected: false, onOpen: () => {} });
    const row = view.getByRole('button');
    expect(row).toHaveAccessibleName(/#77 running/);
    expect(row).toHaveAccessibleName(/build: passed/);
    expect(row).toHaveAccessibleName(/test: failed/);
    expect(view.getAllByRole('img').map((dot) => dot.getAttribute('aria-label'))).toEqual(['build: passed', 'test: failed']);
  });
});

describe('the age names both times', () => {
  const TIMES = 'Opened Oct 1, 2026 · Updated Oct 11, 2026, 9:05 AM';

  it('on an MR row, as hover text and in its accessible name, with one age shown', () => {
    const view = render(PullRow, { row: { ...PULL, times: TIMES }, selected: false, onOpen: () => {} });
    const age = view.getByTestId('pull-age');
    expect(age.title).toBe(TIMES);
    expect(view.getByRole('button', { name: new RegExp(TIMES) })).toBeInTheDocument();
    expect(age.querySelector('[aria-hidden="true"]')?.textContent).toBe('2h');
  });

  it('on a pipeline row', () => {
    const times = 'Created Oct 1, 2026, 9:00 AM · Updated Oct 1, 2026, 9:05 AM';
    const view = render(PipelineRow, { row: { ...PIPELINE, times }, selected: false, onOpen: () => {} });
    expect(view.getByTestId('pipeline-age').title).toBe(times);
    expect(view.getByRole('button')).toHaveAccessibleName(new RegExp(times));
  });
});
