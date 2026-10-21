// A row's CI color carries its status in words for assistive tech: the
// MR row's CI dot, the pipeline row's status dot, and each stage dot.

import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import PipelineRow from './PipelineRow.svelte';
import PullRow from './PullRow.svelte';
import type { PipelineRowView } from './pipelineRowView';
import type { PullRowView } from './pullRowView';

const PULL: PullRowView = {
  key: 'k5', forge: 'github', number: 5, title: 'Add feature', url: '', state: 'your_turn', group: 'needs_you',
  since: null, seen: false, projectName: 'Repo', author: 'alice', age: '2h ago', draft: false, crossRepository: false,
  ci: 'failed', approvals: null, unresolved: null,
};

const PIPELINE: PipelineRowView = {
  key: 'p77', id: '77', status: 'running', name: '', ref: 'main', refKind: 'branch', mrNumber: 0, forge: 'gitlab',
  shortSHA: 'abcdef1', title: 'Fix the parser', projectName: 'App', age: '5m ago', url: '',
  stages: [{ name: 'build', status: 'success' }, { name: 'test', status: 'failed' }], stagesError: null,
};

describe('CI status in words', () => {
  it("names an MR row's CI status, and says nothing more with none", () => {
    const view = render(PullRow, { row: PULL, selected: false, onOpen: () => {} });
    expect(view.getByRole('button', { name: /CI failed/ })).toBeInTheDocument();
    view.unmount();
    const quiet = render(PullRow, { row: { ...PULL, ci: '' }, selected: false, onOpen: () => {} });
    expect(quiet.getByRole('button', { name: /no CI/ })).toBeInTheDocument();
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
