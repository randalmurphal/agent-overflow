import { describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import DiffPanelHeaderBar from './DiffPanelHeaderBar.svelte';

describe('<DiffPanelHeaderBar>', () => {
  it('renders messages and workspace tabs without the redundant session tab', () => {
    const { getByRole, queryByRole } = render(DiffPanelHeaderBar, {
      props: {
        totals: { files: 2, additions: 3, deletions: 1 },
        viewMode: 'stacked',
        setViewMode: vi.fn(),
        wordWrap: false,
        setWordWrap: vi.fn(),
        tabMode: 'messages',
        setTabMode: vi.fn(),
        onClose: vi.fn(),
      },
    });

    expect(getByRole('tab', { name: 'Messages' })).toBeInTheDocument();
    expect(getByRole('tab', { name: 'Workspace' })).toBeInTheDocument();
    expect(queryByRole('tab', { name: 'Session' })).toBeNull();
  });
});
