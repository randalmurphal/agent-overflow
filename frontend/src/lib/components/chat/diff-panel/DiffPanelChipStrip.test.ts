import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import DiffPanelChipStrip from './DiffPanelChipStrip.svelte';
import type { Checkpoint } from '../../../types/checkpoint';

function checkpoint(turnIndex: number): Checkpoint {
  return {
    id: `checkpoint-${turnIndex}`,
    threadId: 'thread-1',
    userItemId: `user-${turnIndex}`,
    turnIndex,
    status: 'ready',
    files: [],
    capturedAt: 1,
  };
}

describe('<DiffPanelChipStrip>', () => {
  it('renders compact message chips and keeps jump as a fixed action', async () => {
    const onSelectCheckpoint = vi.fn();
    const onJumpToCheckpoint = vi.fn();
    const { getByTestId, queryByText } = render(DiffPanelChipStrip, {
      props: {
        visibleCheckpoints: [checkpoint(0), checkpoint(1), checkpoint(2)],
        selectedUserItemId: 'user-1',
        onSelectCheckpoint,
        onJumpToCheckpoint,
      },
    });

    expect(getByTestId('diff-all-messages')).toHaveTextContent('All');
    expect(queryByText('All messages')).toBeNull();
    expect(getByTestId('diff-message-0')).toHaveTextContent('1');
    expect(getByTestId('diff-message-1')).toHaveTextContent('2');
    expect(queryByText('Message 1')).toBeNull();

    const jump = getByTestId('diff-message-jump') as HTMLButtonElement;
    const scrollStrip = getByTestId('diff-message-scroll-strip');
    const jumpSlot = getByTestId('diff-message-jump-slot');
    expect(scrollStrip).toHaveClass('overflow-x-auto');
    expect(scrollStrip).not.toContainElement(jump);
    expect(jumpSlot).toContainElement(jump);
    expect(jump).not.toBeDisabled();
    expect(jump).toHaveAttribute('title', 'Jump to message');

    await fireEvent.click(jump);
    expect(onJumpToCheckpoint).toHaveBeenCalledTimes(1);
  });

  it('disables jump when the all-messages chip is selected', () => {
    const { getByTestId } = render(DiffPanelChipStrip, {
      props: {
        visibleCheckpoints: [checkpoint(0)],
        selectedUserItemId: null,
        onSelectCheckpoint: vi.fn(),
        onJumpToCheckpoint: vi.fn(),
      },
    });

    expect(getByTestId('diff-message-jump')).toBeDisabled();
  });
});
