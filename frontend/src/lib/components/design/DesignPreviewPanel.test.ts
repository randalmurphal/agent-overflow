import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DesignPreviewPanel from './DesignPreviewPanel.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import type { DesignArtifact } from '../../types/design';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';

// Stub Element.animate for Svelte transitions (inherited from Toast subtree).
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

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Design thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    interactionMode: 'design',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function makeArtifact(overrides: Partial<DesignArtifact> = {}): DesignArtifact {
  return {
    id: 'art-1',
    threadId: 'thread-1',
    title: 'Landing page v1',
    description: 'Bold hero',
    kind: 'render',
    htmlPath: '/tmp/art-1.html',
    createdAt: 100,
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

describe('<DesignPreviewPanel>', () => {
  beforeEach(() => {
    // Default: resolves with an HTML string for whatever artifact is requested.
    setBindingMock('GetDesignArtifactHTML', async () => '<html><body>ok</body></html>');
  });

  it('renders empty state when there are no artifacts', async () => {
    const pane = await buildPane();
    const { getByText, container } = render(DesignPreviewPanel, { props: { pane } });
    expect(getByText(/no design preview yet/i)).toBeInTheDocument();
    // No iframe shown yet.
    expect(container.querySelector('iframe')).toBeNull();
  });

  it('renders the latest artifact in a sandboxed iframe with fetched HTML', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact({ id: 'a-1', title: 'First', createdAt: 10 }));
    pane.appendDesignArtifact(makeArtifact({ id: 'a-2', title: 'Second', createdAt: 20 }));
    const htmlMock = setBindingMock('GetDesignArtifactHTML', async () =>
      '<html><body>second</body></html>',
    );

    const { container } = render(DesignPreviewPanel, { props: { pane } });

    // Await the fetch + re-render loop.
    await waitFor(() => {
      const iframe = container.querySelector('iframe');
      expect(iframe).not.toBeNull();
      expect(iframe?.getAttribute('srcdoc')).toContain('second');
    });

    const iframe = container.querySelector('iframe')!;
    expect(iframe.getAttribute('sandbox')).toBe('allow-scripts');
    expect(iframe.getAttribute('title')).toBe('Second');
    // Only `allow-scripts` — no `allow-same-origin`, since agent HTML is untrusted.
    expect(iframe.getAttribute('sandbox')).not.toMatch(/allow-same-origin/);
    // Fetched with the latest artifact ID.
    const calls = htmlMock.mock.calls;
    expect(calls[calls.length - 1]).toEqual(['thread-1', 'a-2']);
  });

  it('viewport toggle updates the iframe width', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact());
    const { container, getByRole } = render(DesignPreviewPanel, { props: { pane } });
    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());

    // Desktop is default — width: 100%.
    let iframe = container.querySelector('iframe')!;
    expect(iframe.style.width).toBe('100%');

    await fireEvent.click(getByRole('button', { name: /mobile/i }));
    iframe = container.querySelector('iframe')!;
    expect(iframe.style.width).toBe('375px');
    expect(pane.designViewport).toBe('mobile');

    await fireEvent.click(getByRole('button', { name: /tablet/i }));
    iframe = container.querySelector('iframe')!;
    expect(iframe.style.width).toBe('768px');
    expect(pane.designViewport).toBe('tablet');
  });

  it('switching artifact via the history dropdown re-fetches HTML', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact({ id: 'a-1', title: 'First', createdAt: 10 }));
    pane.appendDesignArtifact(makeArtifact({ id: 'a-2', title: 'Second', createdAt: 20 }));

    const htmlMock = setBindingMock('GetDesignArtifactHTML', async (_t: unknown, id: unknown) =>
      `<html><body>${id}</body></html>`,
    );

    const { container } = render(DesignPreviewPanel, { props: { pane } });
    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('a-2'));

    // Switch to the older artifact explicitly.
    const select = container.querySelector('select')!;
    await fireEvent.change(select, { target: { value: 'a-1' } });

    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('a-1'));
    expect(pane.activeArtifactId).toBe('a-1');

    // Both artifacts fetched at least once — initial load + explicit pick.
    const calledIds = htmlMock.mock.calls.map((c) => c[1]);
    expect(calledIds).toContain('a-2');
    expect(calledIds).toContain('a-1');
  });

  it('surfaces a load error via toast when GetDesignArtifactHTML rejects', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact());
    setBindingMock('GetDesignArtifactHTML', async () => {
      throw new Error('disk fire');
    });

    const { findByText, container } = render(DesignPreviewPanel, { props: { pane } });
    await findByText(/failed to load design/i);
    // No iframe while errored.
    expect(container.querySelector('iframe')).toBeNull();
  });

  it('lazy-loads: binding is not called until an artifact is present', async () => {
    const pane = await buildPane();
    render(DesignPreviewPanel, { props: { pane } });
    // Give effects a tick to settle.
    await Promise.resolve();
    await Promise.resolve();
    const mock = getBindingMock('GetDesignArtifactHTML');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls.length).toBe(0);
  });

  it('prefers the first option artifact when pendingDesignOptions is set', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact({ id: 'a-1', title: 'Standalone', createdAt: 5 }));
    pane.appendDesignArtifact(makeArtifact({ id: 'opt-art-A', title: 'Option A', createdAt: 10 }));
    pane.appendDesignArtifact(makeArtifact({ id: 'opt-art-B', title: 'Option B', createdAt: 20 }));
    pane.setDesignOptions({
      requestId: 'req-1',
      threadId: 'thread-1',
      prompt: '',
      options: [
        { id: 'opt-A', title: 'A', description: '', artifactId: 'opt-art-A' },
        { id: 'opt-B', title: 'B', description: '', artifactId: 'opt-art-B' },
      ],
    });
    const htmlMock = setBindingMock('GetDesignArtifactHTML', async () => '<html>pending</html>');

    const { container } = render(DesignPreviewPanel, { props: { pane } });
    await waitFor(() => expect(container.querySelector('iframe')).not.toBeNull());
    const calls = htmlMock.mock.calls;
    const latest = calls[calls.length - 1];
    // Preview should be the first option's artifact, not the latest in history.
    expect(latest).toEqual(['thread-1', 'opt-art-A']);
  });
});
