// RuntimeModePicker — three-click flow + error surface + idempotent selection.
//
// The backend is mocked at the bindings layer; these tests focus on the
// picker's state machine (open/applying/closed) and on the optimistic
// pane + threads-store patch that happens synchronously after a
// successful SetThreadRuntimeMode call.

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import RuntimeModePicker from './RuntimeModePicker.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread, RuntimeMode } from '../../types/models';
import { resetBindingMocks, setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';

function seededPane(runtimeMode: RuntimeMode): ReturnType<typeof createThreadPane> {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);

  const pane = createThreadPane();
  const thread: Thread = {
    id: 'thread-1',
    title: 'Rune thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    interactionMode: 'default',
    runtimeMode,
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
  void pane.switchThread(thread);
  return pane;
}

describe('RuntimeModePicker', () => {
  beforeEach(() => {
    resetBindingMocks();
    // Silence expected console.error from the failure-path test. The
    // threads store doesn't expose a reset; it's hydrated fresh per
    // pane by the bindings we mock, so no cross-test contamination.
    vi.stubGlobal('console', { ...console, error: vi.fn() });
  });

  it('renders the current badge text based on the thread runtime mode', async () => {
    const pane = seededPane('approval-required');
    const { getByTestId } = render(RuntimeModePicker, { pane });
    await tick();

    expect(getByTestId('runtime-mode-trigger').textContent).toContain('Safe');
  });

  it('falls back to "Default" when the thread has no runtimeMode set', async () => {
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
    setBindingMock('ListPayloadMetas', async () => []);
    const pane = createThreadPane();
    const thread: Thread = {
      id: 'thread-no-rm',
      title: 'Legacy thread',
      provider: 'claude',
      workspacePath: '/tmp',
      projectPath: '/tmp',
      interactionMode: 'default',
      // runtimeMode intentionally absent to model a legacy fixture.
      model: 'claude-sonnet-4-6',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    void pane.switchThread(thread);

    const { getByTestId } = render(RuntimeModePicker, { pane });
    await tick();

    expect(getByTestId('runtime-mode-trigger').textContent).toContain('Default');
  });

  it('opens the listbox and shows all three modes', async () => {
    const pane = seededPane('full-access');
    const { getByTestId } = render(RuntimeModePicker, { pane });
    await tick();

    await fireEvent.click(getByTestId('runtime-mode-trigger'));
    await tick();

    expect(getByTestId('runtime-mode-listbox')).toBeDefined();
    expect(getByTestId('runtime-mode-option-approval-required')).toBeDefined();
    expect(getByTestId('runtime-mode-option-auto-accept-edits')).toBeDefined();
    expect(getByTestId('runtime-mode-option-full-access')).toBeDefined();
  });

  it('calls SetThreadRuntimeMode and patches the pane optimistically', async () => {
    const pane = seededPane('full-access');
    setBindingMock('SetThreadRuntimeMode', async (_id, mode) => ({
      threadId: 'thread-1',
      runtimeMode: mode,
      needsReconnect: false,
    }));

    const { getByTestId } = render(RuntimeModePicker, { pane });
    await tick();
    await fireEvent.click(getByTestId('runtime-mode-trigger'));
    await tick();
    await fireEvent.click(getByTestId('runtime-mode-option-auto-accept-edits'));

    await waitFor(() => {
      expect(pane.thread?.runtimeMode).toBe('auto-accept-edits');
    });
    const binding = getBindingMock('SetThreadRuntimeMode');
    expect(binding).toBeDefined();
    expect(binding!).toHaveBeenCalledWith('thread-1', 'auto-accept-edits');
  });

  it('skips the round-trip when the user re-picks the current mode', async () => {
    const pane = seededPane('auto-accept-edits');
    setBindingMock('SetThreadRuntimeMode', async (_id, mode) => ({
      threadId: 'thread-1',
      runtimeMode: mode,
      needsReconnect: false,
    }));

    const { getByTestId } = render(RuntimeModePicker, { pane });
    await tick();
    await fireEvent.click(getByTestId('runtime-mode-trigger'));
    await tick();
    await fireEvent.click(getByTestId('runtime-mode-option-auto-accept-edits'));
    await tick();

    const binding = getBindingMock('SetThreadRuntimeMode');
    expect(binding?.mock.calls.length ?? 0).toBe(0);
  });

  it('surfaces backend errors without leaving the button stuck in applying state', async () => {
    const pane = seededPane('approval-required');
    setBindingMock('SetThreadRuntimeMode', async () => {
      throw new Error('bind-fail');
    });

    const { getByTestId } = render(RuntimeModePicker, { pane });
    await tick();
    await fireEvent.click(getByTestId('runtime-mode-trigger'));
    await tick();
    await fireEvent.click(getByTestId('runtime-mode-option-full-access'));

    await waitFor(() => {
      const trigger = getByTestId('runtime-mode-trigger') as HTMLButtonElement;
      expect(trigger.disabled).toBe(false);
    });
    // Pane mode did NOT flip — optimistic update only runs on success.
    expect(pane.thread?.runtimeMode).toBe('approval-required');
  });
});
