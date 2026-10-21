// The Merge requests tab's toolbar: the three menu buttons name their
// choice, the filter carries the needs-you count, picking a sort key
// again turns its order around, and the project picker keeps "every
// project" as the empty pick.

import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import PullsToolbar from './PullsToolbar.svelte';
import PipelinesFilterChips from './PipelinesFilterChips.svelte';
import { chipClasses } from '../primitives/chipClasses';
import { DEFAULT_MR_SORT, DEFAULT_PIPELINE_FILTERS, type PullsSort } from '../../stores/pullsPageFilters.svelte';

const PROJECTS = [{ id: 'p1', name: 'App' }, { id: 'p2', name: 'Widgets' }];

function mountToolbar(overrides: Partial<{ filter: 'needs_you' | 'mine' | 'reviewing' | 'all'; needsYouCount: number; projects: typeof PROJECTS; selectedProjects: string[]; sort: PullsSort }> = {}) {
  const onFilter = vi.fn();
  const onProjects = vi.fn();
  const onSort = vi.fn();
  const view = render(PullsToolbar, {
    filter: 'needs_you', needsYouCount: 3, projects: PROJECTS, selectedProjects: [], sort: DEFAULT_MR_SORT,
    onFilter, onProjects, onSort, ...overrides,
  });
  return { view, onFilter, onProjects, onSort };
}

describe('PullsToolbar', () => {
  it('names the filter with its count, the projects and the sort, and offers each choice from its menu', async () => {
    const { view, onFilter, onSort } = mountToolbar();
    const filter = view.getByTestId('pulls-filter');
    expect(filter).toHaveAttribute('aria-label', 'Filter: Needs you');
    expect(view.getByTestId('pulls-filter-needs-you-count')).toHaveTextContent('3');
    expect(view.getByTestId('pulls-projects-picker')).toHaveAttribute('aria-label', 'Projects: All projects');
    expect(view.getByTestId('pulls-sort')).toHaveAttribute('aria-label', 'Sort: Updated, Newest first');

    await fireEvent.click(filter);
    await fireEvent.click(view.getByTestId('pulls-filter-reviewing'));
    expect(onFilter).toHaveBeenCalledWith('reviewing');
    // A single choice closes its menu, and the menu opens again after.
    await waitFor(() => expect(view.queryByTestId('pulls-filter-reviewing')).toBeNull());
    await fireEvent.click(filter);
    await waitFor(() => expect(view.getByTestId('pulls-filter-mine')).toBeInTheDocument());
    await fireEvent.click(view.getByTestId('pulls-filter-mine'));
    await waitFor(() => expect(view.queryByTestId('pulls-filter-mine')).toBeNull());

    await fireEvent.click(view.getByTestId('pulls-sort'));
    await fireEvent.click(view.getByTestId('pulls-sort-title'));
    expect(onSort).toHaveBeenCalledWith({ key: 'title', reversed: false });
    await waitFor(() => expect(view.queryByTestId('pulls-sort-title')).toBeNull());
  });

  it('turns the order around when the current sort key is picked again', async () => {
    const { view, onSort } = mountToolbar({ sort: { key: 'created', reversed: false } });
    expect(view.getByTestId('pulls-sort')).toHaveAttribute('aria-label', 'Sort: Created, Newest first');
    await fireEvent.click(view.getByTestId('pulls-sort'));
    await fireEvent.click(view.getByTestId('pulls-sort-created'));
    expect(onSort).toHaveBeenCalledWith({ key: 'created', reversed: true });
  });

  it('hides the count at zero and reads All open as the unfiltered state', () => {
    const { view } = mountToolbar({ filter: 'all', needsYouCount: 0 });
    expect(view.queryByTestId('pulls-filter-needs-you-count')).toBeNull();
    expect(view.getByTestId('pulls-filter')).toHaveAttribute('aria-label', 'Filter: All open');
  });

  it('picks projects with every project as the empty pick, and names a single pick', async () => {
    const { view, onProjects } = mountToolbar();
    await fireEvent.click(view.getByTestId('pulls-projects-picker'));
    // Unticking one from "all" picks the others; the menu stays open for more.
    await fireEvent.click(view.getByTestId('pulls-project-p1'));
    expect(onProjects).toHaveBeenLastCalledWith(['p2']);
    expect(view.getByTestId('pulls-project-p2')).toBeInTheDocument();
    view.unmount();

    const picked = mountToolbar({ selectedProjects: ['p2'] });
    expect(picked.view.getByTestId('pulls-projects-picker')).toHaveAttribute('aria-label', 'Projects: Widgets');
    await fireEvent.click(picked.view.getByTestId('pulls-projects-picker'));
    // Ticking the last missing one reads as all again.
    await fireEvent.click(picked.view.getByTestId('pulls-project-p1'));
    expect(picked.onProjects).toHaveBeenLastCalledWith([]);
    await fireEvent.click(picked.view.getByTestId('pulls-projects-all'));
    expect(picked.onProjects).toHaveBeenLastCalledWith([]);
  });

  it('shows no project picker for one project', () => {
    const { view } = mountToolbar({ projects: [PROJECTS[0]!] });
    expect(view.queryByTestId('pulls-projects-picker')).toBeNull();
  });

  it('sizes its buttons to the compact target, as the Pipelines chips do', () => {
    const { view } = mountToolbar();
    for (const id of ['pulls-filter', 'pulls-projects-picker', 'pulls-sort']) {
      expect(view.getByTestId(id).className.split(' ')).toContain('compact:h-9');
    }
    const pipelines = render(PipelinesFilterChips, { filters: { ...DEFAULT_PIPELINE_FILTERS, status: 'failed' }, projects: PROJECTS, onChange: () => {} });
    expect(pipelines.getByTestId('pipelines-project-p1').className).toBe(chipClasses(true));
  });
});
