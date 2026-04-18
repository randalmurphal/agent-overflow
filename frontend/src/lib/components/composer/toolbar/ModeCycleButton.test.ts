import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';

import ModeCycleButton from './ModeCycleButton.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe('<ModeCycleButton>', () => {
  beforeEach(() => resetBindingMocks());

  it('renders the current mode label', async () => {
    const pane = await buildPane(makeThread({ mode: 'plan' }));
    const { getByTestId } = render(ModeCycleButton, { props: { pane } });
    expect(getByTestId('composer-mode-cycle').textContent ?? '').toMatch(/plan/i);
  });

  it('advances chat → plan and replaces the pane/thread row', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat' }));
    const updated: Thread = { ...makeThread({ mode: 'plan' }) };
    setBindingMock('UpdateThreadMode', async () => updated);
    const { getByTestId } = render(ModeCycleButton, { props: { pane } });

    await fireEvent.click(getByTestId('composer-mode-cycle'));
    await Promise.resolve();
    await Promise.resolve();

    const call = getBindingMock('UpdateThreadMode')!.mock.calls[0];
    expect(call).toEqual(['thread-1', 'plan']);
    expect(pane.thread?.mode).toBe('plan');
  });

  it('wraps design → chat', async () => {
    const pane = await buildPane(makeThread({ mode: 'design' }));
    const updated: Thread = { ...makeThread({ mode: 'chat' }) };
    setBindingMock('UpdateThreadMode', async () => updated);
    const { getByTestId } = render(ModeCycleButton, { props: { pane } });
    await fireEvent.click(getByTestId('composer-mode-cycle'));

    const call = getBindingMock('UpdateThreadMode')!.mock.calls[0];
    expect(call).toEqual(['thread-1', 'chat']);
  });

  it('falls back to chat on unknown/discussion mode', async () => {
    const pane = await buildPane(makeThread({ mode: 'discussion' }));
    const updated: Thread = { ...makeThread({ mode: 'chat' }) };
    setBindingMock('UpdateThreadMode', async () => updated);
    const { getByTestId } = render(ModeCycleButton, { props: { pane } });
    await fireEvent.click(getByTestId('composer-mode-cycle'));

    const call = getBindingMock('UpdateThreadMode')!.mock.calls[0];
    expect(call).toEqual(['thread-1', 'chat']);
  });
});
