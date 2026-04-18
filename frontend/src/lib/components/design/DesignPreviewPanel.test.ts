import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DesignPreviewPanel from './DesignPreviewPanel.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import type { DesignArtifact } from '../../types/design';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';

// Mock the screenshot utility so tests don't need a real canvas renderer
// or a headless browser. The export-flow tests drive the mock directly.
vi.mock('../../utils/captureHtml', () => ({
  captureHtmlToPng: vi.fn(async () => new Blob(['fake-png'], { type: 'image/png' })),
  blobToBase64: vi.fn(async () => 'ZmFrZS1wbmc='),
}));
import { captureHtmlToPng, blobToBase64 } from '../../utils/captureHtml';

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
    // Source threads need a projectId so the export-to-thread flow can
    // forward it to CreateThread. All post-Wave-1 threads carry one.
    projectId: 'proj-design',
    mode: 'design',
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

describe('<DesignPreviewPanel> — export to new thread', () => {
  beforeEach(() => {
    setBindingMock('GetDesignArtifactHTML', async () => '<html><body>landing</body></html>');
    vi.mocked(captureHtmlToPng).mockClear();
    vi.mocked(blobToBase64).mockClear();
  });

  it('hides the Export button when there is no active artifact', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(DesignPreviewPanel, { props: { pane } });
    expect(queryByTestId('design-export-to-thread')).toBeNull();
  });

  it('captures the HTML, creates a new thread, uploads the PNG, seeds the draft, and switches', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact({ id: 'a-1', title: 'Hero v2', createdAt: 10 }));

    const created = { id: 'new-thread', provider: 'claude', workspacePath: '/tmp', model: 'claude-sonnet-4-6' };
    setBindingMock('CreateThread', async () => created);
    const uploadMock = setBindingMock('UploadAttachment', async () => ({
      id: 'att-1',
      threadId: 'new-thread',
      filename: 'design-a-1.png',
      mimeType: 'image/png',
      size: 8,
      relativePath: 'new-thread/att-1.png',
      createdAt: 1,
    }));
    const saveDraftMock = setBindingMock('SaveDraft', async () => {});
    setBindingMock('StartSession', async () => {});

    const { getByTestId, findByTestId } = render(DesignPreviewPanel, { props: { pane } });
    // Wait for the iframe to load (fetchedHtml populated) so the button is enabled.
    await findByTestId('design-export-to-thread');
    await waitFor(() =>
      expect((getByTestId('design-export-to-thread') as HTMLButtonElement).disabled).toBe(false),
    );
    await fireEvent.click(getByTestId('design-export-to-thread'));

    await waitFor(() => expect(vi.mocked(captureHtmlToPng)).toHaveBeenCalled());
    // HTML captured from the preview state.
    expect(vi.mocked(captureHtmlToPng).mock.calls[0][0]).toContain('landing');

    const createCalls = (getBindingMock('CreateThread') as ReturnType<typeof vi.fn> | undefined)?.mock.calls ?? [];
    await waitFor(() => expect(createCalls.length).toBeGreaterThan(0));
    // CreateThread now takes a CreateThreadOptions struct. Export-to-thread
    // forwards the source thread's project, provider, and model; mode is
    // reset to 'default' for the spun-off thread.
    expect(createCalls[0][0]).toEqual({
      projectId: 'proj-design',
      provider: 'claude',
      model: 'claude-sonnet-4-6',
      mode: 'default',
    });

    await waitFor(() => expect(uploadMock).toHaveBeenCalled());
    expect(uploadMock.mock.calls[0][0]).toBe('new-thread');
    expect(uploadMock.mock.calls[0][1]).toBe('design-a-1.png');
    expect(uploadMock.mock.calls[0][2]).toBe('image/png');

    await waitFor(() => expect(saveDraftMock).toHaveBeenCalled());
    const draftArgs = saveDraftMock.mock.calls[0];
    expect(draftArgs[0]).toBe('new-thread');
    expect(String(draftArgs[1])).toContain('Hero v2');
    expect(draftArgs[2]).toEqual(['att-1']);

    // Pane switched to the new thread.
    await waitFor(() => expect(pane.threadId).toBe('new-thread'));
  });

  it('still creates the thread when the screenshot upload fails', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact({ id: 'a-2', title: 'CTA block' }));

    setBindingMock('CreateThread', async () => ({
      id: 'new-thread',
      provider: 'claude',
      workspacePath: '/tmp',
      model: 'claude-sonnet-4-6',
    }));
    setBindingMock('UploadAttachment', async () => {
      throw new Error('disk full');
    });
    const saveDraftMock = setBindingMock('SaveDraft', async () => {});
    setBindingMock('StartSession', async () => {});

    const { getByTestId, findByTestId } = render(DesignPreviewPanel, { props: { pane } });
    await findByTestId('design-export-to-thread');
    await waitFor(() =>
      expect((getByTestId('design-export-to-thread') as HTMLButtonElement).disabled).toBe(false),
    );
    await fireEvent.click(getByTestId('design-export-to-thread'));

    await waitFor(() => expect(saveDraftMock).toHaveBeenCalled());
    // No attachment id available — the draft should still be seeded, just
    // without the image reference in the attachments array.
    expect(saveDraftMock.mock.calls[0][2]).toEqual([]);
    await waitFor(() => expect(pane.threadId).toBe('new-thread'));
  });

  it('ignores rapid second clicks while an export is already in flight', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact({ id: 'a-dbl', title: 'Nav' }));
    let release: (blob: Blob) => void = () => {};
    const pending = new Promise<Blob>((r) => {
      release = (b) => r(b);
    });
    vi.mocked(captureHtmlToPng).mockImplementationOnce(async () => pending);
    const createMock = setBindingMock('CreateThread', async () => ({
      id: 'new-thread', provider: 'claude', workspacePath: '/tmp', model: 'm',
    }));
    setBindingMock('UploadAttachment', async () => ({
      id: 'a', threadId: 'new-thread', filename: 'x', mimeType: 'image/png', size: 0, relativePath: '', createdAt: 0,
    }));
    setBindingMock('SaveDraft', async () => {});
    setBindingMock('StartSession', async () => {});

    const { getByTestId, findByTestId } = render(DesignPreviewPanel, { props: { pane } });
    const btn = (await findByTestId('design-export-to-thread')) as HTMLButtonElement;
    await waitFor(() => expect(btn.disabled).toBe(false));
    // Three rapid clicks: the disabled guard + early-return should
    // collapse them to one actual export.
    void fireEvent.click(btn);
    void fireEvent.click(btn);
    void fireEvent.click(btn);
    await waitFor(() => expect(vi.mocked(captureHtmlToPng)).toHaveBeenCalledTimes(1));
    release(new Blob(['x'], { type: 'image/png' }));
    await waitFor(() => expect(pane.threadId).toBe('new-thread'));
    expect(createMock).toHaveBeenCalledTimes(1);
  });

  it('surfaces an error to the pane when CreateThread fails', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact({ id: 'a-3', title: 'Hero' }));
    setBindingMock('CreateThread', async () => { throw new Error('backend down'); });

    const { getByTestId, findByTestId } = render(DesignPreviewPanel, { props: { pane } });
    await findByTestId('design-export-to-thread');
    await waitFor(() =>
      expect((getByTestId('design-export-to-thread') as HTMLButtonElement).disabled).toBe(false),
    );
    await fireEvent.click(getByTestId('design-export-to-thread'));
    await waitFor(() => expect(pane.error).toMatch(/export design/i));
  });

  it('button shows "Exporting…" label while the export is in flight', async () => {
    const pane = await buildPane();
    pane.appendDesignArtifact(makeArtifact({ id: 'a-4', title: 'Nav' }));
    let release: () => void = () => {};
    const pending = new Promise<void>((r) => { release = r; });
    vi.mocked(captureHtmlToPng).mockImplementationOnce(async () => {
      await pending;
      return new Blob(['x'], { type: 'image/png' });
    });
    setBindingMock('CreateThread', async () => ({ id: 'new-thread', provider: 'claude', workspacePath: '/tmp', model: 'm' }));
    setBindingMock('UploadAttachment', async () => ({ id: 'a', threadId: 'new-thread', filename: 'x', mimeType: 'image/png', size: 0, relativePath: '', createdAt: 0 }));
    setBindingMock('SaveDraft', async () => {});
    setBindingMock('StartSession', async () => {});

    const { getByTestId, findByTestId } = render(DesignPreviewPanel, { props: { pane } });
    const btn = (await findByTestId('design-export-to-thread')) as HTMLButtonElement;
    await waitFor(() => expect(btn.disabled).toBe(false));
    void fireEvent.click(btn);
    await waitFor(() => expect(btn.textContent).toMatch(/exporting/i));
    release();
    // After the export completes the pane switches to the new thread,
    // which has no artifacts → the button unmounts. Either outcome is
    // proof the in-flight state resolved: the button stopped showing
    // "Exporting…" OR the button is gone entirely.
    await waitFor(
      () => {
        const stillThere = document.querySelector('[data-testid="design-export-to-thread"]');
        if (!stillThere) return;
        expect(stillThere.textContent).not.toMatch(/exporting/i);
      },
      { timeout: 2000 },
    );
  });
});
