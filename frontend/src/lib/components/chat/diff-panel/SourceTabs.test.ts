import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import SourceTabs from './SourceTabs.svelte';

describe('<SourceTabs>', () => {
  it('renders all three tabs when turn is visible', () => {
    const onSelect = vi.fn();
    const { getByTestId } = render(SourceTabs, {
      source: 'turn',
      turnTabVisible: true,
      onSelect,
    });
    expect(getByTestId('diff-source-tab-turn')).toBeInTheDocument();
    expect(getByTestId('diff-source-tab-worktree')).toBeInTheDocument();
    expect(getByTestId('diff-source-tab-cumulative')).toBeInTheDocument();
  });

  it('hides the turn tab when turnTabVisible is false', () => {
    const onSelect = vi.fn();
    const { queryByTestId, getByTestId } = render(SourceTabs, {
      source: 'worktree',
      turnTabVisible: false,
      onSelect,
    });
    expect(queryByTestId('diff-source-tab-turn')).toBeNull();
    expect(getByTestId('diff-source-tab-worktree')).toBeInTheDocument();
    expect(getByTestId('diff-source-tab-cumulative')).toBeInTheDocument();
  });

  it('marks the active tab with aria-selected', () => {
    const { getByTestId } = render(SourceTabs, {
      source: 'cumulative',
      turnTabVisible: true,
      onSelect: vi.fn(),
    });
    expect(getByTestId('diff-source-tab-cumulative').getAttribute('aria-selected')).toBe('true');
    expect(getByTestId('diff-source-tab-worktree').getAttribute('aria-selected')).toBe('false');
  });

  it('invokes onSelect with the tab id when clicked', async () => {
    const onSelect = vi.fn();
    const { getByTestId } = render(SourceTabs, {
      source: 'turn',
      turnTabVisible: true,
      onSelect,
    });
    await fireEvent.click(getByTestId('diff-source-tab-worktree'));
    expect(onSelect).toHaveBeenCalledWith('worktree');
  });
});
