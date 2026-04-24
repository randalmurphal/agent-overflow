import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import DiffPanelDrawer from './DiffPanelDrawer.svelte';
import type { Checkpoint } from '../../types/checkpoint';
import type { Item } from '../../types/models';
import { loadSettings } from '../../stores/settings.svelte';
import { buildPane, emitItemEventUpsert, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
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
  let cleanupEvents: () => void;

  beforeEach(async () => {
    resetWailsMocks();
    resetBindingMocks();
    cleanupEvents = setupEventListeners();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
    setBindingMock('ListThreadCheckpoints', async () => []);
    setBindingMock('GetTurnDiff', async () => '');
    setBindingMock('GetCheckpointToWorktreeDiff', async () => '');
    setBindingMock('GetWorkingTreeDiff', async () => '');
    setBindingMock('GetPayloadData', async () => ({ data: '' }));
    // Cumulative diffs are now sourced from a dedicated thread-wide
    // binding so the panel stays accurate with a paged timeline. Each
    // test overrides this below when it needs non-empty rows.
    setBindingMock('ListThreadDiffPayloads', async () => []);
  });

  afterEach(() => {
    cleanup();
    cleanupEvents?.();
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
    const getPayload = setBindingMock('GetPayloadData', async (_threadId: string, payloadId: string) => ({ data: `diff:${payloadId}` }));

    const { getByTestId, findByTestId } = render(DiffPanelDrawer, { props: { pane } });
    await flush();
    await fireEvent.click(getByTestId('diff-source-tab-cumulative'));
    await flush();

    await waitFor(() => expect(getPayload).toHaveBeenCalledWith('thread-a', 'p1'));
    expect(getPayload).toHaveBeenCalledWith('thread-a', 'p2');
    expect((await findByTestId('diff-viewer')).textContent).toContain('diff:p1');
  });

  it('re-fetches diff items on diff-kind upsert and ignores plain tool_results', async () => {
    // The debounced refresh listener must fire for diff payloads
    // always and for tool_result payloads only when their meta carries
    // `inlineDiff.availability=="exact_patch"` — matching the SQL
    // filter. Mismatched events must not provoke a fetch.
    let responses: Item[] = [];
    let calls = 0;
    setBindingMock('ListThreadDiffPayloads', async () => {
      calls += 1;
      return responses;
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    render(DiffPanelDrawer, { props: { pane } });
    await flush();
    expect(calls).toBe(1); // initial mount fetch

    // Plain tool_result with no inlineDiff meta — must NOT refetch.
    emitItemEventUpsert(makeItem({
      id: 'plain',
      threadId: 'thread-a',
      kind: 'tool_completion',
      payloadKind: 'tool_result',
      payloadMeta: '{}',
    }));
    await new Promise((r) => setTimeout(r, 150));
    expect(calls).toBe(1);

    // tool_result carrying inlineDiff meta — triggers refetch.
    responses = [
      makeItem({
        id: 'inline',
        threadId: 'thread-a',
        kind: 'tool_completion',
        payloadId: 'pi',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            availability: 'exact_patch',
            files: [{ path: 'c.ts', insertions: 1, deletions: 0 }],
          },
        }),
      }),
    ];
    emitItemEventUpsert(responses[0]);
    await waitFor(() => expect(calls).toBeGreaterThanOrEqual(2), { timeout: 500 });
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

  // Stage 4 refactor: DiffPanelDrawer composes from the shared Drawer
  // primitive rather than hand-rolling its own <aside> chrome. The
  // panel stays non-resizable (fixed 340px) — that's by design for
  // this surface, which has its own internal tabs/scroll.
  it('composes its chrome via the Drawer primitive at a fixed height', async () => {
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    const { container } = render(DiffPanelDrawer, { props: { pane } });
    await flush();
    const drawerEl = container.querySelector('[data-drawer-position="bottom"]') as HTMLElement;
    expect(drawerEl).not.toBeNull();
    expect(drawerEl.style.height).toBe('340px');
    // Resizable=false so no separator/handle is rendered inside the
    // diff panel specifically.
    const handle = drawerEl.querySelector('[role="separator"][aria-orientation="horizontal"]');
    expect(handle).toBeNull();
  });
});
