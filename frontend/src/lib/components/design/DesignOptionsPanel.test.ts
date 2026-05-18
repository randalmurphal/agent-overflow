import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import DesignOptionsPanel from './DesignOptionsPanel.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { makePanelContext } from '../../stores/rhsPanelSlot.svelte';
import type { Thread } from '../../types/models';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Design thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    projectId: 'proj-design',
    mode: 'design',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane() {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(makeThread());
  pane.setActiveOptionSet({
    setId: 'set-1',
    optionPaths: ['options/set-1/alpha', 'options/set-1/beta/'],
  });
  return pane;
}

describe('<DesignOptionsPanel>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('renders option iframes for the active option set', async () => {
    const pane = await buildPane();
    const { container, getAllByTestId } = render(DesignOptionsPanel, {
      props: { ctx: makePanelContext(pane) },
    });

    expect(getAllByTestId('design-option-card')).toHaveLength(2);
    const iframeSrcs = [...container.querySelectorAll('iframe')]
      .map((iframe) => iframe.getAttribute('src'));
    expect(iframeSrcs).toEqual([
      '/design/thread-1/options/set-1/alpha/?cb=0',
      '/design/thread-1/options/set-1/beta/?cb=0',
    ]);
  });

  it('refreshes options from the backend and reloads unchanged iframes', async () => {
    const pane = await buildPane();
    const latest = setBindingMock('LatestDesignOptionSet', async () => ({
      setId: 'set-2',
      optionIds: ['gamma'],
    }));
    const { container, getByRole } = render(DesignOptionsPanel, {
      props: { ctx: makePanelContext(pane) },
    });

    await fireEvent.click(getByRole('button', { name: 'Refresh options' }));

    await waitFor(() => expect(latest).toHaveBeenCalledWith('thread-1'));
    await waitFor(() => {
      const iframe = container.querySelector('iframe');
      expect(iframe?.getAttribute('src')).toBe('/design/thread-1/options/set-2/gamma/?cb=1');
    });
  });

  it('sends a structured pick message and clears the active option set', async () => {
    const pane = await buildPane();
    const send = setBindingMock('SendMessage', async () => {});
    const dismiss = setBindingMock('DismissDesignOptionSet', async () => {});
    const { getAllByTestId } = render(DesignOptionsPanel, {
      props: { ctx: makePanelContext(pane) },
    });

    await fireEvent.click(getAllByTestId('design-option-pick')[0]);

    await waitFor(() => expect(send).toHaveBeenCalled());
    expect(send.mock.calls[0][0]).toBe('thread-1');
    expect(send.mock.calls[0][1]).toContain('"kind": "option_chosen"');
    expect(send.mock.calls[0][1]).toContain('"optionId": "alpha"');
    expect(send.mock.calls[0][2]).toEqual([]);
    expect(dismiss).toHaveBeenCalledWith('thread-1', 'set-1');
    expect(pane.activeOptionSet).toBeNull();
  });

  it('dismisses the option set locally without sending a message', async () => {
    const pane = await buildPane();
    const send = setBindingMock('SendMessage', async () => {});
    const { getByRole } = render(DesignOptionsPanel, {
      props: { ctx: makePanelContext(pane) },
    });

    await fireEvent.click(getByRole('button', { name: 'Dismiss' }));

    expect(send).not.toHaveBeenCalled();
    expect(pane.activeOptionSet).toBeNull();
  });
});
