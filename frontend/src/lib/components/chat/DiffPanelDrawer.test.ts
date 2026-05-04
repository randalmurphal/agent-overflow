import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import DiffPanelDrawer from './DiffPanelDrawer.svelte';
import type { Checkpoint } from '../../types/checkpoint';
import { loadSettings } from '../../stores/settings.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent, resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
import { setupEventListeners } from '../../stores/events';

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

function checkpoint(turnCount: number): Checkpoint {
  return {
    id: `cp-${turnCount}`,
    threadId: 'thread-a',
    checkpointTurnCount: turnCount,
    status: 'ready',
    // Non-baseline checkpoints carry at least one file so the empty-turn
    // filter (DiffPanelDrawer hides chips for turns with zero changes)
    // doesn't drop them. Baseline (count=0) is always kept as the
    // reference point regardless of file count.
    files: turnCount === 0
      ? []
      : [{ path: `turn-${turnCount}.ts`, kind: 'modified', additions: 1, deletions: 0 }],
    toolPaths: turnCount === 0 ? [] : [`turn-${turnCount}.ts`],
    capturedAt: Date.UTC(2026, 0, 1, 0, turnCount),
  };
}

function patch(path = 'src/file.ts', body = '+const value = 1;'): string {
  return [
    `diff --git a/${path} b/${path}`,
    `--- a/${path}`,
    `+++ b/${path}`,
    '@@ -1 +1 @@',
    body,
    '',
  ].join('\n');
}

async function flush() {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

describe('<DiffPanelDrawer>', () => {
  let cleanupEvents: () => void;

  beforeEach(async () => {
    resetWailsMocks();
    resetBindingMocks();
    cleanupEvents = setupEventListeners();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
    setBindingMock('ListThreadCheckpoints', async () => [checkpoint(0), checkpoint(1), checkpoint(2)]);
    setBindingMock('GetCheckpointRangeDiff', async (_threadId: string, from: number, to: number) =>
      patch(`turn-${from}-${to}.ts`),
    );
    setBindingMock('RevertToCheckpoint', async () => undefined);
    setBindingMock('GetPayloadData', async () => ({ data: '' }));
  });

  afterEach(() => {
    cleanup();
    cleanupEvents?.();
  });

  it('loads checkpoints and renders the all-turn checkpoint diff', async () => {
    const getRange = setBindingMock('GetCheckpointRangeDiff', async (_threadId: string, from: number, to: number) =>
      patch(`range-${from}-${to}.ts`),
    );
    const pane = await buildPane(makeThread({ id: 'thread-a' }));

    const { getByTestId, findByTestId } = render(DiffPanelDrawer, { props: { pane } });

    await waitFor(() => expect(getRange).toHaveBeenCalledWith('thread-a', 0, 2));
    expect(getByTestId('diff-turn-1')).toBeInTheDocument();
    expect(getByTestId('diff-turn-2')).toBeInTheDocument();
    expect((await findByTestId('diff-viewer')).textContent).toContain('range-0-2.ts');
  });

  it('loads an adjacent checkpoint range when a turn chip is selected', async () => {
    // Patch path matches the selected checkpoint's toolPaths so the
    // per-turn filter keeps the file. Per-turn tab drops paths the
    // agent didn't write — mismatched paths would render an empty
    // viewer.
    const getRange = setBindingMock('GetCheckpointRangeDiff', async (_threadId: string, _from: number, to: number) =>
      patch(`turn-${to}.ts`),
    );
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    const { getByTestId, findByTestId } = render(DiffPanelDrawer, { props: { pane } });
    await flush();

    getRange.mockClear();
    await fireEvent.click(getByTestId('diff-turn-1'));

    await waitFor(() => expect(getRange).toHaveBeenCalledWith('thread-a', 0, 1));
    expect((await findByTestId('diff-viewer')).textContent).toContain('turn-1.ts');
    expect(getByTestId('diff-turn-revert')).toBeInTheDocument();
  });

  it('shows an empty checkpoint state before any checkpoints exist', async () => {
    setBindingMock('ListThreadCheckpoints', async () => []);
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    const { getByText } = render(DiffPanelDrawer, { props: { pane } });
    await flush();

    expect(getByText('No checkpoints yet.')).toBeInTheDocument();
  });

  it('refreshes checkpoints when checkpoint events arrive', async () => {
    let rows = [checkpoint(0), checkpoint(1)];
    setBindingMock('ListThreadCheckpoints', async () => rows);
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    const { getByTestId, queryByTestId } = render(DiffPanelDrawer, { props: { pane } });
    await waitFor(() => expect(getByTestId('diff-turn-1')).toBeInTheDocument());
    expect(queryByTestId('diff-turn-2')).toBeNull();

    rows = [checkpoint(0), checkpoint(1), checkpoint(2)];
    emitWailsEvent('checkpoint:updated', {
      threadId: 'thread-a',
      turnIndex: 2,
      checkpointTurnCount: 2,
      capturedAt: 0,
    });

    await waitFor(() => expect(getByTestId('diff-turn-2')).toBeInTheDocument());
  });

  it('surfaces checkpoint errors and closes through the pane state', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    pane.setDiffPanelOpen(true);
    const { getByTestId } = render(DiffPanelDrawer, { props: { pane } });

    emitWailsEvent('checkpoint:error', {
      threadId: 'thread-a',
      turnIndex: 1,
      checkpointTurnCount: 1,
      error: 'capture failed',
    });

    await waitFor(() => expect(getByTestId('diff-panel-error').textContent).toContain('capture failed'));
    await fireEvent.click(getByTestId('diff-panel-close'));
    expect(pane.diffPanel.open).toBe(false);
  });

  it('clicking a file editor-link does NOT toggle the file card', async () => {
    setBindingMock('GetCheckpointRangeDiff', async () => patch('src/lib/foo.ts', '+const value = 1;'));
    const openMock = setBindingMock('OpenInEditor', async () => undefined);
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    const { findAllByTestId } = render(DiffPanelDrawer, { props: { pane } });

    // Wait for FileCard rows to render after the patch parses.
    const toggles = await findAllByTestId('diff-panel-file-toggle');
    const fooToggle = toggles.find((t) => t.getAttribute('data-path') === 'src/lib/foo.ts');
    expect(fooToggle).toBeTruthy();
    // The toggle and the editor-link icon are sibling buttons under
    // the same flex header; the wrapping <section> is the closest
    // ancestor. Use that as the search root.
    const fileCard = fooToggle!.closest('section') as HTMLElement;
    const link = fileCard.querySelector('[data-testid="editor-link-icon"]') as HTMLElement;
    expect(link).not.toBeNull();

    await fireEvent.click(link);
    await waitFor(() => {
      // The pane's thread.workspacePath flows through DiffPanelDrawer
      // → DiffPanelFileCard → EditorLink so the backend can join the
      // repo-relative `src/lib/foo.ts` against it. /tmp/workspace is
      // the value makeThread's fixture sets.
      expect(openMock).toHaveBeenCalledWith('src/lib/foo.ts', 0, 0, '/tmp/workspace');
    });
  });
});
