import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DesignPreviewPanel from './DesignPreviewPanel.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import {
  DESIGN_RELOAD_MAIN_EVENT,
  DESIGN_CAPTURE_REQUEST_EVENT,
  DESIGN_SNAPSHOTS_UPDATE_EVENT,
} from '../../stores/events';

// Mock the iframe-capture round-trip so tests don't need a real layout
// engine. requestIframeCapture returns the base64 payload directly
// after the parent-iframe postMessage exchange.
vi.mock('../../utils/captureHtml', () => ({
  requestIframeCapture: vi.fn(async () => 'ZmFrZS1wbmc='),
}));
import { requestIframeCapture } from '../../utils/captureHtml';

// Stub Element.animate (Toast subtree depends on it for transitions).
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
  setBindingMock('ListDesignSnapshots', async () => []);
  setBindingMock('EnsureDesignWorkdir', async () => {});
  const pane = createThreadPane();
  await pane.switchThread(makeThread());
  return pane;
}

// Wait for the EnsureDesignWorkdir effect to resolve and the iframe
// to mount. The component intentionally does not render the iframe
// until the workdir is confirmed (so a fresh thread doesn't 404),
// so every test that asserts on the iframe must wait for it.
async function waitForIframe(container: HTMLElement): Promise<HTMLIFrameElement> {
  return await waitFor(() => {
    const iframe = container.querySelector(
      'iframe[data-testid="design-preview-iframe"]',
    ) as HTMLIFrameElement | null;
    if (!iframe) throw new Error('iframe not yet mounted');
    return iframe;
  });
}

describe('<DesignPreviewPanel>', () => {
  beforeEach(() => {
    vi.mocked(requestIframeCapture).mockClear();
    vi.mocked(requestIframeCapture).mockResolvedValue('ZmFrZS1wbmc=');
  });

  it('renders an iframe pointing at /design/{threadId}/main/ once the workdir is ensured', async () => {
    const pane = await buildPane();
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    const iframe = await waitForIframe(container);
    const src = iframe.getAttribute('src') ?? '';
    expect(src.startsWith('/design/thread-1/main/?cb=')).toBe(true);
    // Sandbox is locked to allow-scripts only — no allow-same-origin
    // because the agent-rendered HTML is untrusted.
    expect(iframe.getAttribute('sandbox')).toBe('allow-scripts');
    expect(iframe.getAttribute('sandbox')).not.toMatch(/allow-same-origin/);
  });

  it('does not mount the iframe until EnsureDesignWorkdir resolves', async () => {
    const pane = await buildPane();
    // Override the resolved-immediately default installed by buildPane
    // with a pending promise so we can observe the placeholder state.
    let resolveEnsure: (() => void) | null = null;
    setBindingMock(
      'EnsureDesignWorkdir',
      () => new Promise<void>((res) => {
        resolveEnsure = res;
      }),
    );
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    // Pending — iframe should not be in the DOM yet.
    expect(container.querySelector('iframe[data-testid="design-preview-iframe"]')).toBeNull();
    expect(container.textContent).toMatch(/Preparing preview/);
    resolveEnsure!();
    await waitForIframe(container);
  });

  it('refresh button bumps the cache-bust counter', async () => {
    const pane = await buildPane();
    const { container, getByTestId } = render(DesignPreviewPanel, { props: { pane } });
    const initialIframe = await waitForIframe(container);
    const initialSrc = initialIframe.getAttribute('src')!;
    await fireEvent.click(getByTestId('design-refresh'));
    await waitFor(() => {
      const next = container.querySelector('iframe')!.getAttribute('src')!;
      expect(next).not.toBe(initialSrc);
      expect(next.startsWith('/design/thread-1/main/?cb=')).toBe(true);
    });
  });

  it('viewport toggle updates the iframe width', async () => {
    const pane = await buildPane();
    const { container, getByRole } = render(DesignPreviewPanel, { props: { pane } });
    let iframe = await waitForIframe(container);
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

  it('responds to the reload-main event by bumping the cache-bust', async () => {
    const pane = await buildPane();
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    const initialIframe = await waitForIframe(container);
    const initialSrc = initialIframe.getAttribute('src')!;

    window.dispatchEvent(
      new CustomEvent(DESIGN_RELOAD_MAIN_EVENT, { detail: { threadId: 'thread-1' } }),
    );
    await waitFor(() => {
      const next = container.querySelector('iframe')!.getAttribute('src')!;
      expect(next).not.toBe(initialSrc);
    });
  });

  it('ignores reload-main events for other threads', async () => {
    const pane = await buildPane();
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    const initialIframe = await waitForIframe(container);
    const initialSrc = initialIframe.getAttribute('src')!;
    window.dispatchEvent(
      new CustomEvent(DESIGN_RELOAD_MAIN_EVENT, { detail: { threadId: 'someone-else' } }),
    );
    // Allow effects to settle.
    await Promise.resolve();
    await Promise.resolve();
    expect(container.querySelector('iframe')!.getAttribute('src')).toBe(initialSrc);
  });

  it('forwards iframe diagnostic postMessages via IngestDiagnosticBatch (debounced)', async () => {
    const pane = await buildPane();
    const ingest = setBindingMock('IngestDiagnosticBatch', async () => {});
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    const iframe = await waitForIframe(container);
    vi.useFakeTimers();
    try {

      window.dispatchEvent(
        new MessageEvent('message', {
          data: {
            aoDesign: 'diagnostics',
            items: [
              { severity: 'error', message: 'boom', source: 'iframe', line: 12 },
              { severity: 'warn', message: 'careful' },
            ],
          },
          source: iframe.contentWindow,
        }),
      );

      // No flush yet — still inside the debounce window.
      expect(ingest).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(300);

      expect(ingest).toHaveBeenCalledTimes(1);
      const arg = ingest.mock.calls[0][0] as { threadId: string; diagnostics: unknown[] };
      expect(arg.threadId).toBe('thread-1');
      expect(arg.diagnostics).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('drops postMessages without an aoDesign tag', async () => {
    const pane = await buildPane();
    const ingest = setBindingMock('IngestDiagnosticBatch', async () => {});
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    const iframe = await waitForIframe(container);
    vi.useFakeTimers();
    try {
      window.dispatchEvent(
        new MessageEvent('message', {
          data: { irrelevant: true, items: [{ severity: 'error', message: 'spam' }] },
          source: iframe.contentWindow,
        }),
      );
      await vi.advanceTimersByTimeAsync(300);
      expect(ingest).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('drops postMessages whose source is not the mounted iframe', async () => {
    const pane = await buildPane();
    const ingest = setBindingMock('IngestDiagnosticBatch', async () => {});
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    await waitForIframe(container);
    vi.useFakeTimers();
    try {
      // Dispatch with no source — should be ignored even though shape matches.
      window.dispatchEvent(
        new MessageEvent('message', {
          data: {
            aoDesign: 'diagnostics',
            items: [{ severity: 'error', message: 'spoofed' }],
          },
        }),
      );
      await vi.advanceTimersByTimeAsync(300);
      expect(ingest).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('captures the live iframe in response to design:capture-request', async () => {
    const pane = await buildPane();
    const ingest = setBindingMock('IngestScreenshot', async () => {});
    setBindingMock('FailScreenshot', async () => {});
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    await waitForIframe(container);

    window.dispatchEvent(
      new CustomEvent(DESIGN_CAPTURE_REQUEST_EVENT, {
        detail: { threadId: 'thread-1', requestId: 'req-A' },
      }),
    );

    await waitFor(() => expect(vi.mocked(requestIframeCapture)).toHaveBeenCalled());
    await waitFor(() => expect(ingest).toHaveBeenCalled());
    expect(ingest.mock.calls[0][0]).toMatchObject({
      requestId: 'req-A',
      pngBase64: 'ZmFrZS1wbmc=',
    });
  });

  it('falls back to FailScreenshot when capture rejects', async () => {
    const pane = await buildPane();
    setBindingMock('IngestScreenshot', async () => {});
    const failMock = setBindingMock('FailScreenshot', async () => {});
    vi.mocked(requestIframeCapture).mockRejectedValueOnce(new Error('cross-origin'));
    const { container } = render(DesignPreviewPanel, { props: { pane } });
    await waitForIframe(container);

    window.dispatchEvent(
      new CustomEvent(DESIGN_CAPTURE_REQUEST_EVENT, {
        detail: { threadId: 'thread-1', requestId: 'req-B' },
      }),
    );

    await waitFor(() => expect(failMock).toHaveBeenCalled());
    expect(failMock.mock.calls[0][0]).toBe('req-B');
    expect(String(failMock.mock.calls[0][1])).toMatch(/cross-origin/);
    const ingest = getBindingMock('IngestScreenshot');
    expect(ingest!.mock.calls.length).toBe(0);
  });

  it('refreshes snapshots when the snapshots-update event fires', async () => {
    const pane = await buildPane();
    const list = setBindingMock('ListDesignSnapshots', async () => [
      { id: 's1', threadId: 'thread-1', label: 'one', dirPath: '', auto: false, createdAt: 1 },
    ]);
    render(DesignPreviewPanel, { props: { pane } });

    // Mount fires one initial fetch via $effect.
    await waitFor(() => expect(list).toHaveBeenCalled());
    const initialCalls = list.mock.calls.length;

    window.dispatchEvent(
      new CustomEvent(DESIGN_SNAPSHOTS_UPDATE_EVENT, { detail: { threadId: 'thread-1' } }),
    );
    await waitFor(() => expect(list.mock.calls.length).toBeGreaterThan(initialCalls));
  });
});
