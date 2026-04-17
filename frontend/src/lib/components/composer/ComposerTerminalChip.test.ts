import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ComposerTerminalChip from './ComposerTerminalChip.svelte';
import type { TerminalChip } from '../../types/draft';

function makeChip(overrides: Partial<TerminalChip> = {}): TerminalChip {
  return {
    id: 'chip-1',
    label: 'terminal',
    preview: '$ ls -la',
    content: '$ ls -la\ntotal 0',
    createdAt: 1,
    ...overrides,
  };
}

describe('<ComposerTerminalChip>', () => {
  it('shows the preview line', () => {
    const { getByText } = render(ComposerTerminalChip, {
      props: { chip: makeChip(), onRemove: vi.fn() },
    });
    expect(getByText('$ ls -la')).toBeInTheDocument();
  });

  it('renders the expanded content when `expanded` is true', () => {
    const { getByText } = render(ComposerTerminalChip, {
      props: { chip: makeChip(), expanded: true, onRemove: vi.fn() },
    });
    expect(getByText(/total 0/)).toBeInTheDocument();
  });

  it('clicking the preview toggles via onToggle', async () => {
    const onToggle = vi.fn();
    const { getByRole } = render(ComposerTerminalChip, {
      props: { chip: makeChip(), onRemove: vi.fn(), onToggle },
    });
    await fireEvent.click(getByRole('button', { name: /ls/ }));
    expect(onToggle).toHaveBeenCalledWith('chip-1');
  });

  it('remove button calls onRemove', async () => {
    const onRemove = vi.fn();
    const { getByLabelText } = render(ComposerTerminalChip, {
      props: { chip: makeChip(), onRemove },
    });
    await fireEvent.click(getByLabelText('Remove terminal context'));
    expect(onRemove).toHaveBeenCalledWith('chip-1');
  });
});
