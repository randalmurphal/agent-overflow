import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
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

function checkpoint(turnCount: number, overrides: Partial<Checkpoint> = {}): Checkpoint {
  return {
    id: `c-${turnCount}`,
    threadId: 'thread-1',
    turnIndex: turnCount,
    checkpointTurnCount: turnCount,
    refName: `refs/ao/thread-1/${turnCount}`,
    status: 'ready',
    // Non-baseline turns carry at least one file so DiffPanelDrawer's
    // empty-turn filter keeps the chip. Baseline is always shown.
    files: turnCount === 0
      ? []
      : [{ path: `turn-${turnCount}.ts`, kind: 'modified', additions: 1, deletions: 0 }],
    capturedAt: Date.UTC(2026, 0, 1, 0, turnCount),
    workspacePath: '/tmp/ws',
    ...overrides,
  };
}

function patch(path = 'src/file.ts', line = '+const value = 1;'): string {
  return [
    `diff --git a/${path} b/${path}`,
    `--- a/${path}`,
    `+++ b/${path}`,
    '@@ -1 +1 @@',
    line,
    '',
  ].join('\n');
}

async function mountAppWithThread(opts: {
  thread?: Thread;
  checkpoints?: Checkpoint[];
} = {}) {
  const thread = opts.thread ?? makeThread({ id: 'thread-1', title: 'Diff Panel Thread' });
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListThreadCheckpoints', async () => opts.checkpoints ?? []);
  setBindingMock('GetCheckpointRangeDiff', async (_threadId: string, from: number, to: number) =>
    patch(`range-${from}-${to}.ts`),
  );
  setBindingMock('RevertToCheckpoint', async () => undefined);
  installComposerDefaults(thread.id);

  const rendered = render(App);
  await flush();
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  return { ...rendered, thread };
}

describe('App integration - diff panel', () => {
  beforeEach(() => {
    resetAppState();
  });

  it('opens the diff panel with mod+shift+g', async () => {
    const { queryByTestId, findByTestId } = await mountAppWithThread();
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

  it('opens as a right-side checkpoint sidebar and loads the full range', async () => {
    const { getByTestId, findByTestId } = await mountAppWithThread({
      checkpoints: [checkpoint(0), checkpoint(1), checkpoint(2)],
    });
    const getRange = setBindingMock('GetCheckpointRangeDiff', async (_threadId: string, from: number, to: number) =>
      patch(`range-${from}-${to}.ts`),
    );

    await fireEvent.click(getByTestId('diff-panel-toggle'));

    const drawer = await findByTestId('diff-panel-drawer');
    expect(drawer.className).toContain('border-l');
    await waitFor(() => expect(getRange).toHaveBeenCalledWith('thread-1', 0, 2));
    expect((await findByTestId('diff-viewer')).textContent).toContain('range-0-2.ts');
    expect(getByTestId('diff-turn-1')).toBeInTheDocument();
    expect(getByTestId('diff-turn-2')).toBeInTheDocument();
  });

  it('clicking a turn loads that adjacent checkpoint range', async () => {
    const { getByTestId, findByTestId } = await mountAppWithThread({
      checkpoints: [checkpoint(0), checkpoint(1), checkpoint(2)],
    });
    const getRange = setBindingMock('GetCheckpointRangeDiff', async (_threadId: string, from: number, to: number) =>
      patch(`turn-${from}-${to}.ts`),
    );

    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await findByTestId('diff-panel-drawer');
    getRange.mockClear();
    await fireEvent.click(getByTestId('diff-turn-1'));

    await waitFor(() => expect(getRange).toHaveBeenCalledWith('thread-1', 0, 1));
    expect((await findByTestId('diff-viewer')).textContent).toContain('turn-0-1.ts');
    expect(getByTestId('diff-turn-revert')).toBeInTheDocument();
  });

  it('shows the empty checkpoint state when there are no checkpoints', async () => {
    const { getByTestId, getByText } = await mountAppWithThread({ checkpoints: [] });
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await flush();

    expect(getByText('No checkpoints yet.')).toBeInTheDocument();
  });

  it('refreshes the turn chips when checkpoint events arrive', async () => {
    let rows = [checkpoint(0), checkpoint(1)];
    const { getByTestId, queryByTestId } = await mountAppWithThread({
      checkpoints: rows,
    });
    setBindingMock('ListThreadCheckpoints', async () => rows);

    await fireEvent.click(getByTestId('diff-panel-toggle'));
    await waitFor(() => expect(getByTestId('diff-turn-1')).toBeInTheDocument());
    expect(queryByTestId('diff-turn-2')).toBeNull();

    rows = [checkpoint(0), checkpoint(1), checkpoint(2)];
    emitWailsEvent('checkpoint:captured', {
      threadId: 'thread-1',
      turnIndex: 2,
      checkpointTurnCount: 2,
      refName: 'refs/ao/thread-1/2',
      capturedAt: 0,
    });

    await waitFor(() => expect(getByTestId('diff-turn-2')).toBeInTheDocument());
  });
});
