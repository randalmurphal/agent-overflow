import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';

import Harness from './WorkspaceChangeLockHarness.svelte';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import type { Item, Thread } from '../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../test/mocks/bindings-app';
import { emitWailsEvent, resetWailsMocks } from '../../test/mocks/wailsio-runtime';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    mode: 'chat',
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function backgroundLaunch(overrides: Partial<Item> = {}): Item {
  return {
    id: 'bg-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'running',
    summary: 'Bash',
    isBackground: true,
    createdAt: 1,
    updatedAt: 1,
    ...overrides,
  };
}

async function buildPane(thread: Thread = makeThread()): Promise<ThreadPane> {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe('createWorkspaceChangeLockState', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWailsMocks();
    vi.useRealTimers();
  });

  it('locks while a turn is active', async () => {
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 1 });

    const { getByTestId } = render(Harness, { props: { pane } });
    const state = getByTestId('workspace-change-lock');

    expect(state).toHaveAttribute('data-locked', 'true');
    expect(state.getAttribute('data-reason') ?? '').toMatch(/agent is responding/);
  });

  it('locks until the initial background-task check resolves', async () => {
    let resolveTasks: (items: Item[]) => void = () => {};
    setBindingMock('ListLiveBackgroundTasks', () =>
      new Promise<Item[]>((resolve) => {
        resolveTasks = resolve;
      }),
    );
    const pane = await buildPane();

    const { getByTestId } = render(Harness, { props: { pane } });
    const state = getByTestId('workspace-change-lock');

    expect(state).toHaveAttribute('data-locked', 'true');
    expect(state.getAttribute('data-reason') ?? '').toMatch(/Checking workspace availability/);

    resolveTasks([]);

    await waitFor(() => {
      expect(state).toHaveAttribute('data-locked', 'false');
    });
  });

  it('counts running background launches as blocking', async () => {
    setBindingMock('ListLiveBackgroundTasks', async () => [backgroundLaunch()]);
    const pane = await buildPane();

    const { getByTestId } = render(Harness, { props: { pane } });
    const state = getByTestId('workspace-change-lock');

    await waitFor(() => {
      expect(state).toHaveAttribute('data-locked', 'true');
      expect(state).toHaveAttribute('data-running-background-count', '1');
      expect(state.getAttribute('data-reason') ?? '').toMatch(/background tasks/);
    });
  });

  it('counts projected running Codex subagent launches as blocking', async () => {
    setBindingMock('ListLiveBackgroundTasks', async () => [
      backgroundLaunch({
        id: 'spawn-agent',
        status: 'running',
        toolName: 'collab_agent',
        meta: JSON.stringify({
          input: {
            tool: 'spawn_agent',
            receiverThreadIds: ['child-1'],
          },
        }),
      }),
    ]);
    const pane = await buildPane(makeThread({ provider: 'codex' }));

    const { getByTestId } = render(Harness, { props: { pane } });
    const state = getByTestId('workspace-change-lock');

    await waitFor(() => {
      expect(state).toHaveAttribute('data-locked', 'true');
      expect(state).toHaveAttribute('data-running-background-count', '1');
      expect(state.getAttribute('data-reason') ?? '').toMatch(/background tasks/);
    });
  });

  it('does not count completed background launch pairs as blocking', async () => {
    setBindingMock('ListLiveBackgroundTasks', async () => [
      backgroundLaunch({ id: 'bg-1' }),
      backgroundLaunch({
        id: 'bg-1-complete',
        kind: 'tool_completion',
        status: 'completed',
        completionOf: 'bg-1',
        isBackground: true,
      }),
    ]);
    const pane = await buildPane();

    const { getByTestId } = render(Harness, { props: { pane } });
    const state = getByTestId('workspace-change-lock');

    await waitFor(() => {
      expect(state).toHaveAttribute('data-locked', 'false');
      expect(state).toHaveAttribute('data-running-background-count', '0');
    });
  });

  it('refreshes from background task events', async () => {
    vi.useFakeTimers();
    let response: Item[] = [];
    setBindingMock('ListLiveBackgroundTasks', async () => response);
    const pane = await buildPane();

    const { getByTestId } = render(Harness, { props: { pane } });
    const state = getByTestId('workspace-change-lock');
    await waitFor(() => {
      expect(state).toHaveAttribute('data-locked', 'false');
    });

    response = [backgroundLaunch()];
    emitWailsEvent('provider:background_tasks_changed', { threadId: 'thread-1' });
    await vi.advanceTimersByTimeAsync(100);

    await waitFor(() => {
      expect(state).toHaveAttribute('data-locked', 'true');
      expect(getBindingMock('ListLiveBackgroundTasks')!.mock.calls.length).toBeGreaterThanOrEqual(2);
    });
  });

  it('ignores stale refresh results after switching threads', async () => {
    let resolveFirst: (items: Item[]) => void = () => {};
    let calls = 0;
    setBindingMock('ListLiveBackgroundTasks', (threadId: unknown) => {
      calls += 1;
      if (calls === 1) {
        return new Promise<Item[]>((resolve) => {
          resolveFirst = resolve;
        });
      }
      return Promise.resolve([backgroundLaunch({ id: 'bg-2', threadId: String(threadId) })]);
    });
    const pane = await buildPane(makeThread({ id: 'thread-a' }));
    const { getByTestId } = render(Harness, { props: { pane } });
    const state = getByTestId('workspace-change-lock');

    await pane.switchThread(makeThread({ id: 'thread-b' }));
    resolveFirst([backgroundLaunch({ id: 'stale', threadId: 'thread-a' })]);

    await waitFor(() => {
      expect(state).toHaveAttribute('data-locked', 'true');
      expect(state).toHaveAttribute('data-running-background-count', '1');
    });
  });
});
