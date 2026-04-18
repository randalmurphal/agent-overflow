import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';

import EffortMenu from './EffortMenu.svelte';
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
    reasoningEffort: 'high',
    fastMode: false,
    contextWindow: 1000000,
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

describe('<EffortMenu>', () => {
  beforeEach(() => resetBindingMocks());

  it('renders effort + context on Claude threads', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    const label = getByTestId('composer-effort-trigger').textContent ?? '';
    expect(label).toMatch(/High/);
    expect(label).toMatch(/1M/);
  });

  it('hides context on Codex threads', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    const label = getByTestId('composer-effort-trigger').textContent ?? '';
    expect(label).toMatch(/High/);
    expect(label).not.toMatch(/1M|200k/);
  });

  it('opens the menu and calls UpdateThreadReasoningEffort on row click', async () => {
    const pane = await buildPane(makeThread({ reasoningEffort: 'medium' }));
    const updated = makeThread({ reasoningEffort: 'low' });
    setBindingMock('UpdateThreadReasoningEffort', async () => updated);
    const { getByTestId, findByRole } = render(EffortMenu, { props: { pane } });

    await fireEvent.click(getByTestId('composer-effort-trigger'));
    const lowRow = await findByRole('menuitem', { name: /Low/ });
    await fireEvent.click(lowRow);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('UpdateThreadReasoningEffort')!.mock.calls[0]).toEqual([
      'thread-1',
      'low',
    ]);
  });

  it('calls UpdateThreadFastMode when toggling Fast Mode', async () => {
    const pane = await buildPane(makeThread({ fastMode: false }));
    setBindingMock('UpdateThreadFastMode', async () => makeThread({ fastMode: true }));
    const { getByTestId, findAllByRole } = render(EffortMenu, { props: { pane } });

    await fireEvent.click(getByTestId('composer-effort-trigger'));
    const rows = await findAllByRole('menuitem', { name: /^On$/ });
    await fireEvent.click(rows[0]);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('UpdateThreadFastMode')!.mock.calls[0]).toEqual([
      'thread-1',
      true,
    ]);
  });

  it('disables the 200k/1M rows when provider=codex', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const { getByTestId, findAllByRole } = render(EffortMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-effort-trigger'));
    const rows = await findAllByRole('menuitem', { name: /^(200k|1M)$/ });
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row.getAttribute('aria-disabled')).toBe('true');
    }
  });
});
