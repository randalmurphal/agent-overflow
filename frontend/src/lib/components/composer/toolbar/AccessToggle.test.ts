import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import AccessToggle from './AccessToggle.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Project, RuntimeMode, Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import { buildPane as buildRegisteredPane } from '../../../../test/helpers/chat';

function makeThread(runtimeMode: RuntimeMode): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    runtimeMode,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function buildPane(mode: RuntimeMode) {
  return buildRegisteredPane(makeThread(mode));
}

function makeProject(): Project {
  return {
    id: 'project-1',
    path: '/tmp/project',
    name: 'Project',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

describe('<AccessToggle>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('renders the current tier label', async () => {
    const pane = await buildPane('approval-required');
    const { getByTestId } = render(AccessToggle, { props: { pane } });
    expect(getByTestId('composer-access-toggle').textContent ?? '').toMatch(/Supervised/);
  });

  it('persists a selected mode on a materialized thread', async () => {
    const pane = await buildPane('approval-required');
    const updated = makeThread('auto-accept-edits');
    const update = setBindingMock('UpdateThreadRuntimeMode', async () => updated);
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: /Auto-accept edits/ }));

    expect(update).toHaveBeenCalledWith('thread-1', 'auto-accept-edits');
    await waitFor(() => {
      expect(getByTestId('composer-access-toggle').getAttribute('data-mode')).toBe(
        'auto-accept-edits',
      );
    });
    expect(pane.thread?.runtimeMode).toBe('auto-accept-edits');
  });

  it('updates new-thread defaults on a placeholder', async () => {
    const pane = createThreadPane();
    pane.startDraftPlaceholder(makeProject(), 'chat', {
      provider: 'claude',
      model: 'claude-sonnet-4-6',
      reasoningEffort: '',
      fastMode: false,
      contextWindow: 200000,
      runtimeMode: 'approval-required',
      branch: 'main',
      workspacePath: '/tmp/project',
    });
    const update = setBindingMock('UpdateNewThreadDefaults', async () => ({
      provider: 'claude',
      model: 'claude-sonnet-4-6',
      reasoningEffort: '',
      fastMode: false,
      contextWindow: 200000,
      runtimeMode: 'auto-accept-edits',
      branch: 'main',
      workspacePath: '/tmp/project',
    }));
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: /Auto-accept edits/ }));

    expect(update).toHaveBeenCalledWith(expect.objectContaining({
      projectId: 'project-1',
      provider: 'claude',
      model: 'claude-sonnet-4-6',
      runtimeMode: 'auto-accept-edits',
    }));
    await waitFor(() => {
      expect(getByTestId('composer-access-toggle').getAttribute('data-mode')).toBe(
        'auto-accept-edits',
      );
    });
    expect(pane.threadId).toBeNull();
  });

  it('does not persist a no-op current mode selection', async () => {
    const pane = await buildPane('approval-required');
    setBindingMock('UpdateThreadRuntimeMode', async () => makeThread('approval-required'));
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: /Supervised/ }));

    expect(getBindingMock('UpdateThreadRuntimeMode')).not.toHaveBeenCalled();
  });

  it('exposes the current mode as a data attribute', async () => {
    const pane = await buildPane('auto-accept-edits');
    const { getByTestId } = render(AccessToggle, { props: { pane } });
    expect(getByTestId('composer-access-toggle').getAttribute('data-mode')).toBe(
      'auto-accept-edits',
    );
  });

  it('shows tier descriptions in the access menu', async () => {
    const pane = await buildPane('full-access');
    const { getByText, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));

    expect(getByText('Ask before commands and file changes.')).toBeTruthy();
    expect(getByText('Auto-approve edits, ask before other actions.')).toBeTruthy();
    expect(getByText('Allow commands and edits without prompts.')).toBeTruthy();
  });
});
