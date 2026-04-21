import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import DiffPanelDrawer from './DiffPanelDrawer.svelte';
import type { Checkpoint } from '../../types/checkpoint';
import { loadSettings } from '../../stores/settings.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
          addEventListener() {}, removeEventListener() {},
          onfinish: null, oncancel: null,
        };
      };
  }
});

function checkpoint(turnIndex: number): Checkpoint {
  return {
    id: `cp-${turnIndex}`,
    threadId: 'thread-a',
    turnIndex,
    refName: `refs/ao/thread-a/${turnIndex}`,
    capturedAt: Date.UTC(2026, 0, 1, 0, turnIndex),
    workspacePath: '/tmp/workspace',
  };
}

async function flush() {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

describe('<DiffPanelDrawer>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
    setBindingMock('ListThreadCheckpoints', async () => []);
    setBindingMock('GetTurnDiff', async () => '');
    setBindingMock('GetCheckpointToWorktreeDiff', async () => '');
    setBindingMock('GetWorkingTreeDiff', async () => '');
    setBindingMock('GetPayloadData', async () => ({ data: '', html: '' }));
    // Cumulative diffs are now sourced from a dedicated thread-wide
    // binding so the panel stays accurate with a paged timeline. Each
    // test overrides this below when it needs non-empty rows.
    setBindingMock('ListThreadDiffPayloads', async () => []);
  });

  it('loads checkpoints on mount and auto-selects the latest turn diff', async () => {
    setBindingMock('ListThreadCheckpoints', async () => [checkpoint(0), checkpoint(1)]);
    setBindingMock('GetTurnDiff', async (_threadId: string, turnIndex: number) => `turn-${turnIndex}`);
    const pane = await buildPane(makeThread({ id: 'thread-a' }));

    const { getByTestId, findByTestId } = render(DiffPanelDrawer, { props: { pane } });
    await flush();

    expect(getByTestId('diff-turn-0')).toBeInTheDocument();
    expect(getByTestId('diff-turn-1')).toBeInTheDocument();
    expect((await findByTestId('diff-viewer')).textContent).toContain('turn-1');
  });

  it('aggregates exact tool-result and diff payloads in cumulative mode', async () => {
    setBindingMock('ListThreadDiffPayloads', async () => [
      makeItem({
        id: 'tool-1',
        threadId: 'thread-a',
        kind: 'tool_call',
        payloadId: 'p1',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            availability: 'exact_patch',
            files: [{ path: 'a.ts', insertions: 3, deletions: 1 }],
          },
        }),
      }),
      makeItem({
        id: 'diff-1',
        threadId: 'thread-a',
        itemIndex: 1,
        kind: 'tool_call',
        payloadId: 'p2',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'b.ts',
          changeKind: 'modified',
          insertions: 2,
          deletions: 0,
          preview: '',
        }),
      }),
    ]);
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    const getPayload = setBindingMock('GetPayloadData', async (payloadId: string) => ({ data: `diff:${payloadId}`, html: '' }));

    const { getByTestId, findByTestId } = render(DiffPanelDrawer, { props: { pane } });
    await flush();
    await fireEvent.click(getByTestId('diff-source-tab-cumulative'));
    await flush();

    await waitFor(() => expect(getPayload).toHaveBeenCalledWith('p1'));
    expect(getPayload).toHaveBeenCalledWith('p2');
    expect((await findByTestId('diff-viewer')).textContent).toContain('diff:p1');
  });

  it('surfaces cumulative aggregation failures as an error banner', async () => {
    setBindingMock('ListThreadDiffPayloads', async () => [
      makeItem({
        id: 'tool-1',
        threadId: 'thread-a',
        kind: 'tool_call',
        payloadId: 'p1',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            availability: 'exact_patch',
            files: [{ path: 'a.ts', insertions: 1, deletions: 0 }],
          },
        }),
      }),
    ]);
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    setBindingMock('GetPayloadData', async () => {
      throw new Error('payload gone');
    });

    const { getByTestId, findByTestId } = render(DiffPanelDrawer, { props: { pane } });
    await flush();
    await fireEvent.click(getByTestId('diff-source-tab-cumulative'));

    expect((await findByTestId('diff-panel-error')).textContent).toContain('payload gone');
  });
});
