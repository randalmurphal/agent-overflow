import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';

import AccessToggle from './AccessToggle.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
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
  beforeEach(() => resetBindingMocks());

  it('renders the current tier label', async () => {
    const pane = await buildPane('approval-required');
    const { getByTestId } = render(AccessToggle, { props: { pane } });
    expect(getByTestId('composer-access-toggle').textContent ?? '').toMatch(/Approval/);
  });

  it('cycles approval-required → auto-accept-edits', async () => {
    const pane = await buildPane('approval-required');
    const updated = makeThread('auto-accept-edits');
    setBindingMock('UpdateThreadRuntimeMode', async () => updated);
    const { getByTestId } = render(AccessToggle, { props: { pane } });
    await fireEvent.click(getByTestId('composer-access-toggle'));
    await Promise.resolve();
    await Promise.resolve();

    const call = getBindingMock('UpdateThreadRuntimeMode')!.mock.calls[0];
    expect(call).toEqual(['thread-1', 'auto-accept-edits']);
    expect(pane.thread?.runtimeMode).toBe('auto-accept-edits');
  });

  it('wraps full-access → approval-required', async () => {
    const pane = await buildPane('full-access');
    const updated = makeThread('approval-required');
    setBindingMock('UpdateThreadRuntimeMode', async () => updated);
    const { getByTestId } = render(AccessToggle, { props: { pane } });
    await fireEvent.click(getByTestId('composer-access-toggle'));

    const call = getBindingMock('UpdateThreadRuntimeMode')!.mock.calls[0];
    expect(call).toEqual(['thread-1', 'approval-required']);
  });

  it('exposes the current mode as a data attribute', async () => {
    const pane = await buildPane('auto-accept-edits');
    const { getByTestId } = render(AccessToggle, { props: { pane } });
    expect(getByTestId('composer-access-toggle').getAttribute('data-mode')).toBe(
      'auto-accept-edits',
    );
  });
});
