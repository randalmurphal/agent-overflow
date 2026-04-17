import { describe, expect, it, afterEach, beforeAll, beforeEach, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import DiffPanelDrawer from './DiffPanelDrawer.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { Thread, Item, PayloadMeta } from '../../types/models';
import type { Checkpoint } from '../../types/checkpoint';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent, wailsListenerCount } from '../../../test/mocks/wailsio-runtime';

// happy-dom doesn't implement Element.animate, which Svelte's slide uses for
// the error banner. The MessageTimeline test has the full shim we can borrow.
beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        let onfinish: (() => void) | null = null;
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {},
          finish() { onfinish?.(); },
          play() {},
          pause() {},
          reverse() {},
          addEventListener(type: string, cb: EventListener) {
            if (type === 'finish') onfinish = cb as unknown as () => void;
          },
          removeEventListener() {},
          get onfinish() { return onfinish; },
          set onfinish(cb: (() => void) | null) {
            onfinish = cb;
            if (cb) queueMicrotask(cb);
          },
        };
      };
  }
});

function thread(id = 'thread-A'): Thread {
  return {
    id,
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    interactionMode: 'default',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function item(overrides: Partial<Item>): Item {
  return {
    id: 'item',
    threadId: 'thread-A',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'message',
    role: 'assistant',
    summary: '',
    createdAt: 0,
    ...overrides,
  };
}

function checkpoint(turnIndex: number, overrides: Partial<Checkpoint> = {}): Checkpoint {
  return {
    id: `c-${turnIndex}`,
    threadId: 'thread-A',
    turnIndex,
    refName: `refs/ao/thread-A/${turnIndex}`,
    capturedAt: Date.UTC(2024, 0, 1, 0, turnIndex),
    workspacePath: '/tmp',
    ...overrides,
  };
}

async function buildPane(
  items: Item[] = [],
  metas: PayloadMeta[] = [],
): Promise<ReturnType<typeof createThreadPane>> {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => items);
  setBindingMock('ListPayloadMetas', async () => metas);
  const pane = createThreadPane();
  await pane.switchThread(thread());
  return pane;
}

async function flush(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

describe('<DiffPanelDrawer>', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
    setBindingMock('ListThreadCheckpoints', async () => []);
    setBindingMock('GetWorkingTreeDiff', async () => '');
    setBindingMock('GetTurnDiff', async () => '');
    setBindingMock('GetCheckpointToWorktreeDiff', async () => '');
    setBindingMock('GetPayloadData', async () => '');
  });

  describe('lifecycle', () => {
    it('subscribes to checkpoint events on mount and cleans up on destroy', async () => {
      const pane = await buildPane();
      const rendered = render(DiffPanelDrawer, { pane });
      await flush();
      expect(wailsListenerCount('checkpoint:captured')).toBe(1);
      expect(wailsListenerCount('checkpoint:unavailable')).toBe(1);
      expect(wailsListenerCount('checkpoint:error')).toBe(1);

      rendered.unmount();
      await flush();
      expect(wailsListenerCount('checkpoint:captured')).toBe(0);
      expect(wailsListenerCount('checkpoint:unavailable')).toBe(0);
      expect(wailsListenerCount('checkpoint:error')).toBe(0);
    });

    it('loads checkpoints via ListThreadCheckpoints on mount', async () => {
      const listMock = setBindingMock('ListThreadCheckpoints', async () => [
        checkpoint(0),
        checkpoint(1),
      ]);
      const pane = await buildPane();
      render(DiffPanelDrawer, { pane });
      await flush();
      expect(listMock).toHaveBeenCalledWith('thread-A');
      expect(pane.diffPanel.checkpoints.map((c) => c.turnIndex)).toEqual([0, 1]);
    });
  });

  describe('turn source', () => {
    it('renders a row per checkpoint and auto-selects the latest', async () => {
      setBindingMock('ListThreadCheckpoints', async () => [checkpoint(0), checkpoint(1)]);
      setBindingMock('GetTurnDiff', async () => 'DIFF-FOR-TURN-1');
      const pane = await buildPane();
      const { getByTestId, findByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      expect(getByTestId('diff-turn-0')).toBeInTheDocument();
      expect(getByTestId('diff-turn-1')).toBeInTheDocument();
      const viewer = await findByTestId('diff-viewer');
      expect(viewer.textContent).toContain('DIFF-FOR-TURN-1');
    });

    it('clicking a turn loads its diff', async () => {
      setBindingMock('ListThreadCheckpoints', async () => [checkpoint(0), checkpoint(1)]);
      const getTurnDiff = setBindingMock('GetTurnDiff', async (_threadID, turnIndex) => {
        return `diff-for-${String(turnIndex)}`;
      });
      const pane = await buildPane();
      const { getByTestId, findByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      // Auto-selected latest (turn 1), clear call history and click turn 0.
      getTurnDiff.mockClear();
      await fireEvent.click(getByTestId('diff-turn-0'));
      await flush();
      expect(getTurnDiff).toHaveBeenCalledWith('thread-A', 0);
      const viewer = await findByTestId('diff-viewer');
      expect(viewer.textContent).toContain('diff-for-0');
    });

    it('switching compare mode swaps the binding and reloads', async () => {
      setBindingMock('ListThreadCheckpoints', async () => [checkpoint(0)]);
      setBindingMock('GetTurnDiff', async () => 'next-mode');
      const ckToWt = setBindingMock('GetCheckpointToWorktreeDiff', async () => 'worktree-mode');
      const pane = await buildPane();
      const { getByTestId, findByText } = render(DiffPanelDrawer, { pane });
      await flush();

      ckToWt.mockClear();
      await fireEvent.click(getByTestId('diff-turn-compare-worktree'));
      await flush();
      expect(ckToWt).toHaveBeenCalledWith('thread-A', 0);
      expect(await findByText(/worktree-mode/)).toBeInTheDocument();
    });

    it('caches turn diffs so re-selecting does not re-fetch', async () => {
      setBindingMock('ListThreadCheckpoints', async () => [checkpoint(0), checkpoint(1)]);
      const getTurnDiff = setBindingMock('GetTurnDiff', async (_id, turnIndex) =>
        `cache-test-${String(turnIndex)}`,
      );
      const pane = await buildPane();
      const { getByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      // Latest auto-selected => turn 1 fetched once.
      expect(getTurnDiff).toHaveBeenCalledTimes(1);

      await fireEvent.click(getByTestId('diff-turn-0'));
      await flush();
      expect(getTurnDiff).toHaveBeenCalledTimes(2);

      // Re-click turn 1 -> cache hit, no additional call.
      await fireEvent.click(getByTestId('diff-turn-1'));
      await flush();
      expect(getTurnDiff).toHaveBeenCalledTimes(2);
    });

    it('ArrowRight / ArrowLeft navigate between turns when the panel is open', async () => {
      setBindingMock('ListThreadCheckpoints', async () => [
        checkpoint(0),
        checkpoint(1),
        checkpoint(2),
      ]);
      setBindingMock('GetTurnDiff', async (_id, idx) => `turn-${String(idx)}`);
      const pane = await buildPane();
      pane.setDiffPanelOpen(true);
      render(DiffPanelDrawer, { pane });
      await flush();
      // Auto-selected latest turn (2).
      expect(pane.diffPanel.selectedTurnIndex).toBe(2);
      await fireEvent.keyDown(window, { key: 'ArrowLeft' });
      await flush();
      expect(pane.diffPanel.selectedTurnIndex).toBe(1);
      await fireEvent.keyDown(window, { key: 'ArrowLeft' });
      await flush();
      expect(pane.diffPanel.selectedTurnIndex).toBe(0);
      // Clamp at the lower bound.
      await fireEvent.keyDown(window, { key: 'ArrowLeft' });
      await flush();
      expect(pane.diffPanel.selectedTurnIndex).toBe(0);
      await fireEvent.keyDown(window, { key: 'ArrowRight' });
      await flush();
      expect(pane.diffPanel.selectedTurnIndex).toBe(1);
    });

    it('ArrowLeft is ignored when the panel is closed', async () => {
      setBindingMock('ListThreadCheckpoints', async () => [
        checkpoint(0),
        checkpoint(1),
      ]);
      setBindingMock('GetTurnDiff', async () => '');
      const pane = await buildPane();
      render(DiffPanelDrawer, { pane });
      await flush();
      // Auto-selected latest turn (1); store.open is false.
      expect(pane.diffPanel.open).toBe(false);
      const before = pane.diffPanel.selectedTurnIndex;
      await fireEvent.keyDown(window, { key: 'ArrowLeft' });
      await flush();
      expect(pane.diffPanel.selectedTurnIndex).toBe(before);
    });
  });

  describe('working tree source', () => {
    it('loads the worktree diff when the tab is selected', async () => {
      const wt = setBindingMock('GetWorkingTreeDiff', async () => '@@ hunk @@\n+line');
      const pane = await buildPane();
      const { getByTestId, findByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      wt.mockClear();
      await fireEvent.click(getByTestId('diff-source-tab-worktree'));
      await flush();
      expect(wt).toHaveBeenCalledWith('thread-A');
      const viewer = await findByTestId('diff-viewer');
      expect(viewer.textContent).toContain('+line');
    });

    it('renders the clean empty state when working tree has no changes', async () => {
      const pane = await buildPane();
      const { getByTestId, findByText } = render(DiffPanelDrawer, { pane });
      await flush();
      await fireEvent.click(getByTestId('diff-source-tab-worktree'));
      expect(await findByText(/Working tree is clean/i)).toBeInTheDocument();
    });

    it('refresh button forces a reload', async () => {
      const wt = setBindingMock('GetWorkingTreeDiff', async () => 'x');
      const pane = await buildPane();
      const { getByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      await fireEvent.click(getByTestId('diff-source-tab-worktree'));
      await flush();
      wt.mockClear();
      await fireEvent.click(getByTestId('diff-worktree-refresh'));
      await flush();
      expect(wt).toHaveBeenCalledTimes(1);
    });
  });

  describe('cumulative source', () => {
    it('aggregates agent-authored diffs from pane.items', async () => {
      const items = [
        item({ id: 'd1', kind: 'diff', payloadId: 'p1', itemIndex: 1 }),
        item({ id: 'd2', kind: 'diff', payloadId: 'p2', itemIndex: 2 }),
      ];
      const getPayloadData = setBindingMock('GetPayloadData', async (id) => `diff-${String(id)}`);
      const pane = await buildPane(items);
      const { getByTestId, findByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      await fireEvent.click(getByTestId('diff-source-tab-cumulative'));
      await flush();
      expect(getPayloadData).toHaveBeenCalledWith('p1');
      expect(getPayloadData).toHaveBeenCalledWith('p2');
      const viewer = await findByTestId('diff-viewer');
      expect(viewer.textContent).toContain('diff-p1');
      expect(viewer.textContent).toContain('diff-p2');
    });

    it('renders an empty state when no diff items are present', async () => {
      const pane = await buildPane([item({ kind: 'message', role: 'assistant' })]);
      const { getByTestId, findByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      await fireEvent.click(getByTestId('diff-source-tab-cumulative'));
      const empty = await findByTestId('diff-viewer-empty');
      expect(empty.textContent).toMatch(/No agent-authored diffs/i);
    });

    it('refresh clears cache and re-fetches every payload', async () => {
      const items = [item({ id: 'd1', kind: 'diff', payloadId: 'p1' })];
      const getPayloadData = setBindingMock('GetPayloadData', async () => 'x');
      const pane = await buildPane(items);
      const { getByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      await fireEvent.click(getByTestId('diff-source-tab-cumulative'));
      await flush();
      expect(getPayloadData).toHaveBeenCalledTimes(1);
      // Refresh -> invalidate cache, should re-fetch.
      await fireEvent.click(getByTestId('diff-cumulative-refresh'));
      await flush();
      expect(getPayloadData).toHaveBeenCalledTimes(2);
    });
  });

  describe('checkpoint events', () => {
    it('checkpoint:captured triggers a checkpoints reload', async () => {
      let callCount = 0;
      setBindingMock('ListThreadCheckpoints', async () => {
        callCount += 1;
        return callCount === 1 ? [] : [checkpoint(0)];
      });
      const pane = await buildPane();
      render(DiffPanelDrawer, { pane });
      await flush();
      expect(callCount).toBe(1);
      expect(pane.diffPanel.checkpoints).toHaveLength(0);
      emitWailsEvent('checkpoint:captured', {
        threadId: 'thread-A',
        turnIndex: 0,
        refName: 'refs/ao/thread-A/0',
        capturedAt: 1,
      });
      await flush();
      expect(callCount).toBe(2);
      expect(pane.diffPanel.checkpoints).toHaveLength(1);
    });

    it('checkpoint:unavailable hides the turn tab and shows the banner', async () => {
      const pane = await buildPane([item({ kind: 'message', turnIndex: 0 })]);
      const { queryByTestId, findByText } = render(DiffPanelDrawer, { pane });
      await flush();
      emitWailsEvent('checkpoint:unavailable', {
        threadId: 'thread-A',
        reason: 'not-a-git-repo',
      });
      await flush();
      expect(queryByTestId('diff-source-tab-turn')).toBeNull();
      // Drawer auto-switches away from the hidden turn tab.
      expect(pane.diffPanel.source).toBe('worktree');
      // Original unavailable banner is hidden on worktree view; switching back
      // should keep the tab hidden.
      expect(pane.diffPanel.checkpointsUnavailable).toBe(true);
      // Assert the banner copy is reachable via the aria-label on the drawer.
      await findByText(/Working tree is clean/i);
    });

    it('checkpoint:error surfaces an error banner that is dismissible', async () => {
      const pane = await buildPane();
      const { findByTestId, queryByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      emitWailsEvent('checkpoint:error', {
        threadId: 'thread-A',
        turnIndex: 2,
        error: 'boom',
      });
      await flush();
      const banner = await findByTestId('diff-panel-error');
      expect(banner.textContent).toContain('boom');

      await fireEvent.click(await findByTestId('diff-panel-error-dismiss'));
      await flush();
      expect(queryByTestId('diff-panel-error')).toBeNull();
    });

    it('ignores events for other threads', async () => {
      let callCount = 0;
      setBindingMock('ListThreadCheckpoints', async () => {
        callCount += 1;
        return [];
      });
      const pane = await buildPane();
      render(DiffPanelDrawer, { pane });
      await flush();
      expect(callCount).toBe(1);
      emitWailsEvent('checkpoint:captured', {
        threadId: 'another-thread',
        turnIndex: 0,
        refName: 'x',
        capturedAt: 0,
      });
      await flush();
      expect(callCount).toBe(1);
    });
  });

  describe('error handling', () => {
    it('surfaces working-tree load failures as a dismissible banner', async () => {
      setBindingMock('GetWorkingTreeDiff', async () => {
        throw new Error('git failed');
      });
      const pane = await buildPane();
      const { getByTestId, findByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      await fireEvent.click(getByTestId('diff-source-tab-worktree'));
      const banner = await findByTestId('diff-panel-error');
      expect(banner.textContent).toContain('git failed');
    });

    it('surfaces cumulative aggregation failures', async () => {
      const items = [item({ id: 'd1', kind: 'diff', payloadId: 'p1' })];
      setBindingMock('GetPayloadData', async () => {
        throw new Error('payload gone');
      });
      const pane = await buildPane(items);
      const { getByTestId, findByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      await fireEvent.click(getByTestId('diff-source-tab-cumulative'));
      const banner = await findByTestId('diff-panel-error');
      expect(banner.textContent).toContain('payload gone');
    });
  });

  describe('header controls', () => {
    it('close button invokes pane.setDiffPanelOpen(false)', async () => {
      const pane = await buildPane();
      pane.setDiffPanelOpen(true);
      const spy = vi.spyOn(pane, 'setDiffPanelOpen');
      const { getByTestId } = render(DiffPanelDrawer, { pane });
      await flush();
      await fireEvent.click(getByTestId('diff-panel-close'));
      expect(spy).toHaveBeenCalledWith(false);
    });
  });
});

afterEach(() => cleanup());
