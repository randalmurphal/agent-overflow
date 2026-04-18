import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import ModelProviderMenu from './ModelProviderMenu.svelte';
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

describe('<ModelProviderMenu>', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('ListDiscussions', async () => []);
  });

  it('renders the pane provider + model on the trigger', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude', model: 'claude-haiku-4-6' }));
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    const trigger = getByTestId('composer-model-menu-trigger');
    expect(trigger.textContent ?? '').toMatch(/Claude/);
    expect(trigger.textContent ?? '').toMatch(/claude-haiku-4-6/);
  });

  it('warms the active provider cache on open', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    const modelsMock = setBindingMock('GetModelsForProvider', async () => [
      { slug: 'claude-opus-4-5', name: 'Claude Opus 4.5', provider: 'claude', capabilities: [] },
    ]);
    const { getByTestId } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));
    await waitFor(() => {
      expect(modelsMock).toHaveBeenCalled();
    });
    expect(modelsMock.mock.calls.some((c) => c[0] === 'claude')).toBe(true);
  });

  it('calls UpdateThreadProvider + UpdateThreadModel when switching providers', async () => {
    const pane = await buildPane(
      makeThread({ provider: 'claude', model: 'claude-sonnet-4-6' }),
    );
    setBindingMock('GetModelsForProvider', async (provider: unknown) => {
      if (provider === 'codex') {
        return [{ slug: 'gpt-5.4', name: 'GPT 5.4', provider: 'codex', capabilities: [] }];
      }
      return [];
    });
    const providerUpdate = makeThread({ provider: 'codex', model: 'claude-sonnet-4-6' });
    const modelUpdate = makeThread({ provider: 'codex', model: 'gpt-5.4' });
    setBindingMock('UpdateThreadProvider', async () => providerUpdate);
    setBindingMock('UpdateThreadModel', async () => modelUpdate);

    const { getByTestId, findByRole } = render(ModelProviderMenu, { props: { pane } });
    await fireEvent.click(getByTestId('composer-model-menu-trigger'));

    // Hover the Codex submenu to trigger its load.
    const codexRow = await findByRole('menuitem', { name: /Codex/i });
    await fireEvent.click(codexRow);

    const gptOption = await findByRole('menuitem', { name: /GPT 5.4/i });
    await fireEvent.click(gptOption);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('UpdateThreadProvider')!.mock.calls[0]).toEqual([
      'thread-1',
      'codex',
    ]);
    expect(getBindingMock('UpdateThreadModel')!.mock.calls[0]).toEqual([
      'thread-1',
      'gpt-5.4',
    ]);
  });
});
