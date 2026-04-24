import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import AccessToggle from './AccessToggle.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import {
  hasRuntimeModeDraft,
  resetRuntimeModeDraftsForTest,
} from '../../../stores/runtimeModeDraft.svelte';
import type { RuntimeMode, Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

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
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(makeThread(mode));
  return pane;
}

describe('<AccessToggle>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRuntimeModeDraftsForTest();
  });

  it('renders the current tier label', async () => {
    const pane = await buildPane('approval-required');
    const { getByTestId } = render(AccessToggle, { props: { pane } });
    expect(getByTestId('composer-access-toggle').textContent ?? '').toMatch(/Supervised/);
  });

  it('stages a selected mode without calling the backend', async () => {
    const pane = await buildPane('approval-required');
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: /Auto-accept edits/ }));

    expect(getBindingMock('UpdateThreadRuntimeMode')).toBeUndefined();
    await waitFor(() => {
      expect(getByTestId('composer-access-toggle').getAttribute('data-mode')).toBe(
        'auto-accept-edits',
      );
    });
    expect(pane.thread?.runtimeMode).toBe('approval-required');
  });

  it('does not stage a no-op current mode selection', async () => {
    const pane = await buildPane('approval-required');
    const { getByRole, getByTestId } = render(AccessToggle, { props: { pane } });

    await fireEvent.click(getByTestId('composer-access-toggle'));
    await fireEvent.click(getByRole('menuitem', { name: /Supervised/ }));

    expect(hasRuntimeModeDraft(pane.thread)).toBe(false);
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
