import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import TurnList from './TurnList.svelte';
import type { Checkpoint } from '../../../types/checkpoint';

function cp(turnIndex: number, capturedAt = Date.now()): Checkpoint {
  return {
    id: `c-${turnIndex}`,
    threadId: 't-1',
    turnIndex,
    refName: `refs/ao/t-1/${turnIndex}`,
    capturedAt,
    workspacePath: '/ws',
  };
}

describe('<TurnList>', () => {
  it('renders a row per checkpoint', () => {
    const { getByTestId } = render(TurnList, {
      checkpoints: [cp(0), cp(1), cp(2)],
      selectedTurnIndex: null,
      onSelect: vi.fn(),
    });
    expect(getByTestId('diff-turn-0')).toBeInTheDocument();
    expect(getByTestId('diff-turn-1')).toBeInTheDocument();
    expect(getByTestId('diff-turn-2')).toBeInTheDocument();
  });

  it('shows an empty-state message when no checkpoints are provided', () => {
    const { getByText } = render(TurnList, {
      checkpoints: [],
      selectedTurnIndex: null,
      onSelect: vi.fn(),
    });
    expect(getByText(/No turns checkpointed/i)).toBeInTheDocument();
  });

  it('marks the selected turn with aria-current', () => {
    const { getByTestId } = render(TurnList, {
      checkpoints: [cp(0), cp(1)],
      selectedTurnIndex: 1,
      onSelect: vi.fn(),
    });
    expect(getByTestId('diff-turn-1').getAttribute('aria-current')).toBe('true');
    expect(getByTestId('diff-turn-0').getAttribute('aria-current')).toBe('false');
  });

  it('fires onSelect with the turn index on click', async () => {
    const onSelect = vi.fn();
    const { getByTestId } = render(TurnList, {
      checkpoints: [cp(0), cp(1)],
      selectedTurnIndex: null,
      onSelect,
    });
    await fireEvent.click(getByTestId('diff-turn-1'));
    expect(onSelect).toHaveBeenCalledWith(1);
  });
});
