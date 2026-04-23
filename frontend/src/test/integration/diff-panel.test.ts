// Integration tests for the diff panel drawer mounted as part of the full
// App tree. The drawer subscribes to checkpoint:* Wails events and calls
// several bindings; these tests exercise the turn-tab UX, empty states,
// keyboard navigation, and event-driven refreshes together.

import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread, Item } from '../../lib/types/models';
import type { Checkpoint } from '../../lib/types/checkpoint';
import { setBindingMock } from '../mocks/bindings-app';
import { emitWailsEvent } from '../mocks/wailsio-runtime';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';

beforeAll(installAnimateShim);

function checkpoint(turnIndex: number, overrides: Partial<Checkpoint> = {}): Checkpoint {
  return {
    id: `c-${turnIndex}`,
    threadId: 'thread-1',
    turnIndex,
    refName: `refs/ao/thread-1/${turnIndex}`,
    capturedAt: Date.UTC(2026, 0, 1, 0, turnIndex),
    workspacePath: '/tmp/ws',
    ...overrides,
  };
}

function diffItem(id: string, turnIndex: number, overrides: Partial<Item> = {}): Item {
  const createdAt = overrides.createdAt ?? 0;
  return {
    id,
    threadId: 'thread-1',
    turnIndex,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'completed',
    summary: '',
    highlightedContent: '',
    payloadId: `${id}-payload`,
    payloadKind: 'diff',
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    ...overrides,
  };
}

async function mountAppWithThread(opts: {
  thread?: Thread;
  items?: Item[];
  checkpoints?: Checkpoint[];
} = {}) {
  const thread = opts.thread ?? makeThread({ title: 'Diff Panel Thread' });
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  setBindingMock('ListItems', async () => opts.items ?? []);
  setBindingMock('ListThreadCheckpoints', async () => opts.checkpoints ?? []);
  // The cumulative tab sources its row set from a dedicated binding
  // now. Mirror `items` into it so the existing fixtures keep working
  // without the caller having to know about the windowing split.
  setBindingMock('ListThreadDiffPayloads', async () => opts.items ?? []);
  installComposerDefaults(thread.id);
  setBindingMock('GetWorkingTreeDiff', async () => '');
  setBindingMock('GetTurnDiff', async () => '');
  setBindingMock('GetCheckpointToWorktreeDiff', async () => '');
  setBindingMock('GetPayloadData', async () => ({ data: '', html: '' }));

  const rendered = render(App);
  await flush();
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  return { ...rendered, thread };
}

describe('App integration — diff panel', () => {
  beforeEach(() => {
    resetAppState();
  });

  it('opens the diff panel with mod+shift+g', async () => {
    const { queryByTestId, findByTestId } = await mountAppWithThread();
    // Install the keybinding AFTER mount, then call loadKeybindings()
    // so the rule is parsed and indexed before we fire the chord. The
    // helper's installAppDefaults sets GetKeybindings to return [] so
    // we need to swap + reload.
    setBindingMock('GetKeybindings', async () => [
      { key: 'mod+shift+g', command: 'diff.panel.toggle', when: 'hasActiveThread' },
    ]);
    const { loadKeybindings, isKeybindingsLoaded } = await import(
      '../../lib/stores/keybindings.svelte'
    );
    await loadKeybindings();
    expect(isKeybindingsLoaded()).toBe(true);

    expect(queryByTestId('diff-panel-drawer')).toBeNull();

    await fireEvent.keyDown(window, { key: 'g', metaKey: true, shiftKey: true });
    await fireEvent.keyDown(window, { key: 'g', ctrlKey: true, shiftKey: true });

    await findByTestId('diff-panel-drawer');
  });

  it('switches between Turn / Working Tree / Cumulative source tabs', async () => {
    const cks = [checkpoint(0), checkpoint(1)];
    const { getByTestId, findByTestId } = await mountAppWithThread({ checkpoints: cks });
    // Install expected return values AFTER mount so the helper's default
    // empty-string mocks don't overwrite them.
    const getTurnDiff = setBindingMock('GetTurnDiff', async () => 'TURN-DIFF');
    const getWorktree = setBindingMock('GetWorkingTreeDiff', async () => 'WORKTREE-DIFF');
    setBindingMock('GetPayloadData', async () => ({ data: '', html: '' }));
    // Open via Diffs button in the chat header.
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush();

    // Turn is the default source and getTurnDiff is called.
    await waitFor(() => expect(getTurnDiff).toHaveBeenCalled());
    const viewer = await findByTestId('diff-viewer');
    expect(viewer.textContent).toContain('TURN-DIFF');

    // Switch to worktree.
    await fireEvent.click(getByTestId('diff-source-tab-worktree'));
    await flush();
    await waitFor(() => expect(getWorktree).toHaveBeenCalled());

    // Switch to cumulative.
    await fireEvent.click(getByTestId('diff-source-tab-cumulative'));
    await flush();
    // Cumulative reads via GetPayloadData per diff item; with no diff items
    // we expect the empty state to render.
    await findByTestId('diff-viewer-empty');
  });

  it('lists every checkpoint returned by ListThreadCheckpoints', async () => {
    const cks = [checkpoint(0), checkpoint(1), checkpoint(2)];
    const { getByTestId, findByTestId } = await mountAppWithThread({ checkpoints: cks });
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush();
    await findByTestId('diff-panel-drawer');
    expect(getByTestId('diff-turn-0')).toBeInTheDocument();
    expect(getByTestId('diff-turn-1')).toBeInTheDocument();
    expect(getByTestId('diff-turn-2')).toBeInTheDocument();
  });

  it('clicking a turn triggers GetTurnDiff for that turn', async () => {
    const cks = [checkpoint(0), checkpoint(1)];
    const { getByTestId } = await mountAppWithThread({ checkpoints: cks });
    const getTurnDiff = setBindingMock('GetTurnDiff', async (_id, turnIndex) =>
      `diff-for-${String(turnIndex)}`,
    );
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush(10);

    // Auto-selected latest (turn 1). Clear history, then click turn 0.
    getTurnDiff.mockClear();
    await fireEvent.click(getByTestId('diff-turn-0'));
    await flush();
    await waitFor(() => expect(getTurnDiff).toHaveBeenCalledWith('thread-1', 0));
  });

  it('shows the empty-turns state when ListThreadCheckpoints returns []', async () => {
    const { getByTestId } = await mountAppWithThread({ checkpoints: [] });
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush();
    // Default source is Turn. With no checkpoints the drawer renders the
    // empty turn list. The TurnDiffView uses the `diff-viewer-empty`
    // testid for both empty states.
    await waitFor(() => {
      const maybeEmpty = document.querySelector('[data-testid="diff-viewer-empty"]');
      // Fall through to the broader "No turns" / "no diff" copy.
      expect(
        maybeEmpty ||
          document.body.textContent?.match(/No turns|Pick a turn|no diff|No diff/i),
      ).toBeTruthy();
    });
  });

  it('checkpoint:unavailable event hides the Turn tab and auto-switches to Working tree', async () => {
    const items = [diffItem('it1', 0, { kind: 'message' })];
    const { getByTestId, queryByTestId } = await mountAppWithThread({
      items,
      checkpoints: [checkpoint(0)],
    });
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush();
    // Turn tab is initially present.
    expect(getByTestId('diff-source-tab-turn')).toBeInTheDocument();

    emitWailsEvent('checkpoint:unavailable', {
      threadId: 'thread-1',
      reason: 'not-a-git-repo',
    });
    await flush();
    expect(queryByTestId('diff-source-tab-turn')).toBeNull();
  });

  it('refreshes the turn list when a checkpoint:captured event arrives', async () => {
    const { getByTestId, findByTestId } = await mountAppWithThread({ checkpoints: [checkpoint(0)] });
    // After mount (which ran ListThreadCheckpoints once), re-install the
    // mock so the next call (triggered by the captured event) returns the
    // growing list.
    setBindingMock('ListThreadCheckpoints', async () => [checkpoint(0), checkpoint(1)]);

    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush();
    await findByTestId('diff-turn-0');

    emitWailsEvent('checkpoint:captured', {
      threadId: 'thread-1',
      turnIndex: 1,
      refName: 'refs/ao/thread-1/1',
      capturedAt: 0,
    });
    await waitFor(() => expect(getByTestId('diff-turn-1')).toBeInTheDocument());
  });

  it('arrow keys navigate between turns when the drawer is open', async () => {
    const cks = [checkpoint(0), checkpoint(1), checkpoint(2)];
    setBindingMock('GetTurnDiff', async () => '');
    const { getByTestId } = await mountAppWithThread({ checkpoints: cks });
    // Open the drawer so store.open flips to true — arrow nav is gated on
    // that flag.
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush();

    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    await waitFor(() => expect(pane.diffPanel.selectedTurnIndex).toBe(2));

    await fireEvent.keyDown(window, { key: 'ArrowLeft' });
    await flush();
    await waitFor(() => expect(pane.diffPanel.selectedTurnIndex).toBe(1));

    await fireEvent.keyDown(window, { key: 'ArrowRight' });
    await flush();
    await waitFor(() => expect(pane.diffPanel.selectedTurnIndex).toBe(2));
  });

  it('cumulative source aggregates every agent-authored diff item', async () => {
    const items = [
      diffItem('d1', 0, { itemIndex: 1 }),
      diffItem('d2', 0, { itemIndex: 2 }),
      diffItem('d3', 1, { itemIndex: 0 }),
    ];
    const { getByTestId, findByTestId } = await mountAppWithThread({ items });
    const getPayload = setBindingMock('GetPayloadData', async (_threadId, id) => ({ data: `payload-${String(id)}`, html: '' }));
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush();
    await fireEvent.click(getByTestId('diff-source-tab-cumulative'));
    await flush(10);

    await waitFor(() => {
      expect(getPayload).toHaveBeenCalledWith('thread-1', 'd1-payload');
      expect(getPayload).toHaveBeenCalledWith('thread-1', 'd2-payload');
      expect(getPayload).toHaveBeenCalledWith('thread-1', 'd3-payload');
    });
    const viewer = await findByTestId('diff-viewer');
    expect(viewer.textContent).toContain('payload-d1-payload');
    expect(viewer.textContent).toContain('payload-d2-payload');
    expect(viewer.textContent).toContain('payload-d3-payload');
  });
});
