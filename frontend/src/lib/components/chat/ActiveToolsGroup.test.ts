import { describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ActiveToolsGroup from './ActiveToolsGroup.svelte';
import type { WorkEntryData } from '../../types/models';

function entry(overrides: Partial<WorkEntryData> & { id: string; type: string }): WorkEntryData {
  return {
    status: 'running',
    ...overrides,
  } as WorkEntryData;
}

describe('<ActiveToolsGroup>', () => {
  it('renders nothing when there are no entries', () => {
    const { container } = render(ActiveToolsGroup, { props: { entries: [] } });
    // The top-level conditional returns nothing; the mounted root is empty.
    expect(container.textContent?.trim() ?? '').toBe('');
  });

  it('renders a single entry inline without the collapse chip', () => {
    const { queryByTestId, getByText } = render(ActiveToolsGroup, {
      props: {
        entries: [entry({ id: 'a', type: 'Bash' })],
      },
    });
    expect(queryByTestId('active-tools-chip')).toBeNull();
    expect(getByText('Bash')).toBeInTheDocument();
  });

  it('collapses 2+ entries into a chip summarising the work', () => {
    const { getByTestId, queryByTestId } = render(ActiveToolsGroup, {
      props: {
        entries: [
          entry({ id: 'a', type: 'Read' }),
          entry({ id: 'b', type: 'Grep' }),
          entry({ id: 'c', type: 'Bash' }),
        ],
      },
    });
    const chip = getByTestId('active-tools-chip');
    expect(chip).toBeInTheDocument();
    expect(chip.textContent ?? '').toMatch(/Running 3 tools/);
    expect(chip.textContent ?? '').toMatch(/Read.*Grep.*Bash/);
    expect(chip.getAttribute('aria-expanded')).toBe('false');
    // Children are hidden by default.
    expect(queryByTestId('active-tools-children')).toBeNull();
  });

  it('expands the group when the chip is clicked', async () => {
    const { getByTestId, getByText } = render(ActiveToolsGroup, {
      props: {
        entries: [
          entry({ id: 'a', type: 'Read' }),
          entry({ id: 'b', type: 'Grep' }),
        ],
      },
    });
    const chip = getByTestId('active-tools-chip');
    await fireEvent.click(chip);
    expect(chip.getAttribute('aria-expanded')).toBe('true');
    expect(getByTestId('active-tools-children')).toBeInTheDocument();
    expect(getByText('Read')).toBeInTheDocument();
    expect(getByText('Grep')).toBeInTheDocument();
  });

  it('keyboard Enter and Space toggle the chip', async () => {
    const { getByTestId, queryByTestId } = render(ActiveToolsGroup, {
      props: {
        entries: [
          entry({ id: 'a', type: 'Read' }),
          entry({ id: 'b', type: 'Grep' }),
        ],
      },
    });
    const chip = getByTestId('active-tools-chip');
    await fireEvent.keyDown(chip, { key: 'Enter' });
    expect(chip.getAttribute('aria-expanded')).toBe('true');
    expect(getByTestId('active-tools-children')).toBeInTheDocument();
    await fireEvent.keyDown(chip, { key: ' ' });
    expect(chip.getAttribute('aria-expanded')).toBe('false');
    expect(queryByTestId('active-tools-children')).toBeNull();
  });

  it('does not render the chip when the set shrinks back to one tool', async () => {
    const { rerender, queryByTestId, getByText } = render(ActiveToolsGroup, {
      props: {
        entries: [
          entry({ id: 'a', type: 'Read' }),
          entry({ id: 'b', type: 'Grep' }),
        ],
      },
    });
    expect(queryByTestId('active-tools-chip')).not.toBeNull();
    await rerender({ entries: [entry({ id: 'a', type: 'Read' })] });
    expect(queryByTestId('active-tools-chip')).toBeNull();
    expect(getByText('Read')).toBeInTheDocument();
  });

  it('shows children re-collapsed when a fresh streak arrives after shrinking', async () => {
    const initial = [
      entry({ id: 'a', type: 'Read' }),
      entry({ id: 'b', type: 'Grep' }),
    ];
    const { rerender, getByTestId, queryByTestId } = render(ActiveToolsGroup, {
      props: { entries: initial },
    });

    // User expands the chip explicitly.
    await fireEvent.click(getByTestId('active-tools-chip'));
    expect(getByTestId('active-tools-chip').getAttribute('aria-expanded')).toBe('true');

    // Tools finish down to one — chip disappears.
    await rerender({ entries: [entry({ id: 'a', type: 'Read' })] });
    expect(queryByTestId('active-tools-chip')).toBeNull();

    // A new streak of 2+ arrives — chip re-appears collapsed.
    await rerender({
      entries: [
        entry({ id: 'c', type: 'Write' }),
        entry({ id: 'd', type: 'Bash' }),
      ],
    });
    const chip = getByTestId('active-tools-chip');
    expect(chip.getAttribute('aria-expanded')).toBe('false');
  });

  it('auto-collapses when the set drops to a single tool, even after user expanded', async () => {
    const { rerender, getByTestId, queryByTestId } = render(ActiveToolsGroup, {
      props: {
        entries: [
          entry({ id: 'a', type: 'Read' }),
          entry({ id: 'b', type: 'Grep' }),
        ],
      },
    });
    await fireEvent.click(getByTestId('active-tools-chip'));
    expect(getByTestId('active-tools-chip').getAttribute('aria-expanded')).toBe('true');
    await rerender({ entries: [] });
    expect(queryByTestId('active-tools-chip')).toBeNull();
    expect(queryByTestId('active-tools-children')).toBeNull();
  });

  it('group role and aria label are preserved so the area is discoverable', () => {
    const { getByRole } = render(ActiveToolsGroup, {
      props: { entries: [entry({ id: 'a', type: 'Bash' })] },
    });
    expect(getByRole('group', { name: /Active tool calls/i })).toBeInTheDocument();
  });
});
