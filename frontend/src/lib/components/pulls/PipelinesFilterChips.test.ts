import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import PipelinesFilterChips from './PipelinesFilterChips.svelte';
import { DEFAULT_PIPELINE_FILTERS } from '../../stores/pullsPageFilters.svelte';

// The Pipelines tab's filter row: project chips toggle (never to none),
// the ref kind and status pickers are one choice each and close on the
// pick.

const PROJECTS = [{ id: 'p1', name: 'App' }, { id: 'p2', name: 'Widgets' }];

describe('PipelinesFilterChips', () => {
  it('a project chip narrows to the others, and the last one stays on', async () => {
    const onChange = vi.fn();
    const { getByTestId, rerender } = render(PipelinesFilterChips, { filters: DEFAULT_PIPELINE_FILTERS, projects: PROJECTS, onChange });
    await fireEvent.click(getByTestId('pipelines-project-p1'));
    expect(onChange).toHaveBeenLastCalledWith({ projects: ['p2'] });
    await rerender({ filters: { ...DEFAULT_PIPELINE_FILTERS, projects: ['p2'] }, projects: PROJECTS, onChange });
    expect(getByTestId('pipelines-project-p1').getAttribute('aria-pressed')).toBe('false');
    await fireEvent.click(getByTestId('pipelines-project-p2'));
    expect(onChange).toHaveBeenCalledTimes(1);
    // Picking the last one back reads as every project.
    await fireEvent.click(getByTestId('pipelines-project-p1'));
    expect(onChange).toHaveBeenLastCalledWith({ projects: [] });
  });

  it('the ref picker is one choice and closes on the pick', async () => {
    const onChange = vi.fn();
    const { getByTestId, queryByTestId } = render(PipelinesFilterChips, { filters: DEFAULT_PIPELINE_FILTERS, projects: PROJECTS, onChange });
    const picker = getByTestId('pipelines-ref-picker');
    expect(picker.textContent).toContain('Any ref');
    await fireEvent.click(picker);
    await waitFor(() => expect(queryByTestId('pipelines-ref-tag')).not.toBeNull());
    expect(picker.getAttribute('aria-expanded')).toBe('true');
    await fireEvent.click(getByTestId('pipelines-ref-tag'));
    expect(onChange).toHaveBeenLastCalledWith({ refKind: 'tag' });
    await waitFor(() => expect(queryByTestId('pipelines-ref-tag')).toBeNull());
    expect(picker.getAttribute('aria-expanded')).toBe('false');
  });

  it('the status picker reads the picked status and closes on the pick', async () => {
    const onChange = vi.fn();
    const { getByTestId, queryByTestId } = render(PipelinesFilterChips, { filters: { ...DEFAULT_PIPELINE_FILTERS, status: 'failed' }, projects: PROJECTS, onChange });
    const picker = getByTestId('pipelines-status-picker');
    expect(picker.textContent).toContain('Failed');
    await fireEvent.click(picker);
    await waitFor(() => expect(queryByTestId('pipelines-status-all')).not.toBeNull());
    expect(getByTestId('pipelines-status-failed').getAttribute('aria-checked')).toBe('true');
    await fireEvent.click(getByTestId('pipelines-status-all'));
    expect(onChange).toHaveBeenLastCalledWith({ status: '' });
    await waitFor(() => expect(queryByTestId('pipelines-status-all')).toBeNull());
  });
});
