import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import DiffPanelDrawer from './DiffPanelDrawer.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { Checkpoint } from '../../types/checkpoint';

function checkpoint(turnIndex: number): Checkpoint {
  return {
    id: `checkpoint-${turnIndex}`,
    threadId: 'thread-1',
    userItemId: `user-${turnIndex}`,
    turnIndex,
    status: 'ready',
    files: turnIndex === 0 ? [] : [{ path: 'notes.txt', kind: 'modified', additions: 1, deletions: 0 }],
    capturedAt: 1,
  };
}

describe('<DiffPanelDrawer>', () => {
  it('jumps the timeline to the selected message checkpoint', async () => {
    const thread = makeThread({ id: 'thread-1' });
    const checkpoints = [checkpoint(0), checkpoint(1)];
    const pane = await buildPane(thread);
    setBindingMock('ListThreadCheckpoints', async () => checkpoints);
    setBindingMock('GetSessionAgentDiff', async () => '');
    setBindingMock('GetMessageCheckpointDiff', async () => '');
    setBindingMock('GetWorkspaceCurrentDiff', async () => '');
    setBindingMock('ListDiffReviewComments', async () => []);
    const requestScrollToItem = vi.spyOn(pane, 'requestScrollToItem');

    const { findByTestId, getByTestId } = render(DiffPanelDrawer, { props: { pane } });
    const secondMessage = await findByTestId('diff-message-1');
    await fireEvent.click(secondMessage);
    await fireEvent.click(getByTestId('diff-message-jump'));

    expect(requestScrollToItem).toHaveBeenCalledWith('user-1', {
      behavior: 'animated',
      flash: true,
    });
  });
});
