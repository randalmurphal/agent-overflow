import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DesignOptionsPicker from './DesignOptionsPicker.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import type { DesignOptionsRequest } from '../../types/design';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';

// Stub Element.animate so toasts don't explode.
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

function makeThread(): Thread {
  return {
    id: 'thread-1',
    title: 'Design thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'design',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function makeRequest(overrides: Partial<DesignOptionsRequest> = {}): DesignOptionsRequest {
  return {
    requestId: 'req-1',
    threadId: 'thread-1',
    prompt: 'Pick a direction',
    options: [
      { id: 'opt-A', title: 'Bold', description: 'High-contrast headings.', artifactId: 'art-A' },
      { id: 'opt-B', title: 'Minimal', description: 'Whitespace-first.', artifactId: 'art-B' },
      { id: 'opt-C', title: 'Classic', description: '', artifactId: 'art-C' },
    ],
    ...overrides,
  };
}

async function buildPane() {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(makeThread());
  return pane;
}

describe('<DesignOptionsPicker>', () => {
  beforeEach(() => {
    setBindingMock('ChooseDesignOption', async () => {});
  });

  it('renders nothing when no request is pending', async () => {
    const pane = await buildPane();
    const { container } = render(DesignOptionsPicker, { props: { pane } });
    expect(container.textContent?.trim() ?? '').toBe('');
  });

  it('renders one button per option plus prompt and request ID fragment', async () => {
    const pane = await buildPane();
    pane.setDesignOptions(makeRequest());
    const { getByText, getAllByRole } = render(DesignOptionsPicker, { props: { pane } });

    expect(getByText(/pick a direction/i)).toBeInTheDocument();
    // All three options rendered (plus the Choose button — filter by aria-label).
    const optionButtons = getAllByRole('button').filter((b) =>
      b.getAttribute('aria-label')?.startsWith('Select design option'),
    );
    expect(optionButtons).toHaveLength(3);
    expect(getByText(/req req-1/i)).toBeInTheDocument();
  });

  it('auto-selects the first option and mirrors the selection in pane.activeArtifactId', async () => {
    const pane = await buildPane();
    pane.setDesignOptions(makeRequest());
    render(DesignOptionsPicker, { props: { pane } });
    // Effect needs a tick to settle.
    await Promise.resolve();
    await Promise.resolve();
    expect(pane.activeArtifactId).toBe('art-A');
  });

  it('selecting a different option updates pane.activeArtifactId', async () => {
    const pane = await buildPane();
    pane.setDesignOptions(makeRequest());
    const { getByLabelText } = render(DesignOptionsPicker, { props: { pane } });
    await Promise.resolve();

    await fireEvent.click(getByLabelText('Select design option Minimal'));
    await Promise.resolve();
    expect(pane.activeArtifactId).toBe('art-B');
  });

  it('calling the Choose button resolves via ChooseDesignOption with correct args', async () => {
    const pane = await buildPane();
    pane.setDesignOptions(makeRequest());
    const chooseMock = setBindingMock('ChooseDesignOption', async () => {});
    const { getByLabelText, getByRole } = render(DesignOptionsPicker, { props: { pane } });

    await fireEvent.click(getByLabelText('Select design option Classic'));
    await Promise.resolve();
    const confirmBtn = getByRole('button', { name: /choose this option/i });
    await fireEvent.click(confirmBtn);
    // Let the async handler run.
    for (let i = 0; i < 3; i++) await Promise.resolve();

    expect(chooseMock).toHaveBeenCalledTimes(1);
    expect(chooseMock.mock.calls[0]).toEqual(['thread-1', 'req-1', 'opt-C']);
  });

  it('does NOT auto-clear pendingDesignOptions on success (event router handles it)', async () => {
    const pane = await buildPane();
    pane.setDesignOptions(makeRequest());
    setBindingMock('ChooseDesignOption', async () => {});
    const { getByRole } = render(DesignOptionsPicker, { props: { pane } });

    await fireEvent.click(getByRole('button', { name: /choose this option/i }));
    for (let i = 0; i < 3; i++) await Promise.resolve();

    expect(pane.pendingDesignOptions?.requestId).toBe('req-1');
  });

  it('shows Choosing... and disables buttons while submitting', async () => {
    const pane = await buildPane();
    pane.setDesignOptions(makeRequest());
    // Block the RPC so we can observe the in-flight UI state.
    let resolve!: () => void;
    const pending = new Promise<void>((r) => { resolve = r; });
    setBindingMock('ChooseDesignOption', () => pending);

    const { getByRole, getByLabelText } = render(DesignOptionsPicker, { props: { pane } });
    const confirmBtn = getByRole('button', { name: /choose this option/i }) as HTMLButtonElement;
    await fireEvent.click(confirmBtn);
    // In-flight now.
    await Promise.resolve();
    const choosingBtn = getByRole('button', { name: /choosing/i }) as HTMLButtonElement;
    expect(choosingBtn.disabled).toBe(true);
    const optionA = getByLabelText('Select design option Bold') as HTMLButtonElement;
    expect(optionA.disabled).toBe(true);

    // Resolve and re-observe state.
    resolve();
    for (let i = 0; i < 3; i++) await Promise.resolve();
  });

  it('restores the confirm button when ChooseDesignOption rejects', async () => {
    const pane = await buildPane();
    pane.setDesignOptions(makeRequest());
    setBindingMock('ChooseDesignOption', async () => { throw new Error('rpc down'); });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { getByRole } = render(DesignOptionsPicker, { props: { pane } });
    const confirm = getByRole('button', { name: /choose this option/i }) as HTMLButtonElement;
    await fireEvent.click(confirm);
    for (let i = 0; i < 5; i++) await Promise.resolve();

    // Should be back to the pre-submit label, and clickable again.
    await waitFor(() => {
      const again = getByRole('button', { name: /choose this option/i }) as HTMLButtonElement;
      expect(again.disabled).toBe(false);
    });
    const mock = getBindingMock('ChooseDesignOption');
    expect(mock!.mock.calls.length).toBe(1);
    consoleErr.mockRestore();
  });
});
