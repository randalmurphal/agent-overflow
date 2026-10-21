// Both tabs' filter rows draw their chips from the one chip appearance,
// which carries the shared compact hit target.

import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import PipelinesFilterChips from './PipelinesFilterChips.svelte';
import PullsFilterChips from './PullsFilterChips.svelte';
import { chipClasses } from '../primitives/chipClasses';
import { DEFAULT_PIPELINE_FILTERS } from '../../stores/pullsPageFilters.svelte';

const PROJECTS = [{ id: 'p1', name: 'App' }, { id: 'p2', name: 'Widgets' }];

describe('filter chips', () => {
  it('size to the compact target Button uses', () => {
    expect(chipClasses(true).split(' ')).toContain('compact:h-9');
    expect(chipClasses(false).split(' ')).toContain('compact:h-9');
  });

  it('are the shared chip on both tabs', () => {
    const mrs = render(PullsFilterChips, {
      filter: 'mine', needsYouCount: 0, projects: PROJECTS, selectedProjects: [], onFilter: () => {}, onProjects: () => {},
    });
    expect(mrs.getByTestId('pulls-filter-mine').className).toBe(chipClasses(true));
    expect(mrs.getByTestId('pulls-filter-all').className).toBe(chipClasses(false));
    expect(mrs.getByTestId('pulls-projects-picker').className).toBe(`${chipClasses(false)} ml-auto`);
    mrs.unmount();

    const pipelines = render(PipelinesFilterChips, { filters: { ...DEFAULT_PIPELINE_FILTERS, status: 'failed' }, projects: PROJECTS, onChange: () => {} });
    expect(pipelines.getByTestId('pipelines-project-p1').className).toBe(chipClasses(true));
    expect(pipelines.getByTestId('pipelines-ref-picker').className).toBe(`${chipClasses(false)} ml-auto`);
    expect(pipelines.getByTestId('pipelines-status-picker').className).toBe(chipClasses(true));
  });
});
